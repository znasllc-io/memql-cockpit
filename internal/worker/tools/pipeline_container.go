package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

var dockerBinary = "docker"
var pipelineImageDigest = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[0-9a-f]{64}$`)

// Execution is explicit at the wire seam. In particular, an older engine's
// image-less request must not silently become a native build on somebody's Mac.
func parsePipelineExecution(args map[string]any, req *pipelineStepRequest) error {
	req.execution = argString(args, "execution")
	req.platform = argString(args, "platform")
	req.image = argString(args, "image")
	switch req.execution {
	case "native":
		if req.image != "" {
			return errors.New("pipeline_step: a native step cannot declare a container image")
		}
		if req.platform != "darwin/arm64" && req.platform != "darwin/amd64" && req.platform != "linux/arm64" && req.platform != "linux/amd64" {
			return errors.New("pipeline_step: native platform must name darwin or linux and arm64 or amd64")
		}
	case "container":
		if req.platform != "linux/arm64" && req.platform != "linux/amd64" {
			return errors.New("pipeline_step: container platform must be linux/arm64 or linux/amd64")
		}
		if !pipelineImageDigest.MatchString(req.image) {
			return errors.New("pipeline_step: a container step requires an image pinned by sha256 digest")
		}
	default:
		return errors.New("pipeline_step: execution must explicitly be native or container")
	}
	if value := args["needs"]; value != nil {
		list, ok := value.([]any)
		if !ok || len(list) > 5 {
			return errors.New("pipeline_step: needs must be a list of host requirements")
		}
		for _, item := range list {
			need, ok := item.(string)
			if !ok || !slices.Contains([]string{"docker", "gpu", "display", "macos_tooling", "user_files"}, need) {
				return errors.New("pipeline_step: unknown host requirement")
			}
			req.needs = append(req.needs, need)
		}
	}
	if req.execution == "container" && len(req.needs) > 0 {
		return errors.New("pipeline_step: host requirements do not pass through the container boundary")
	}
	// These are not implemented on the fleet yet. Refuse instead of executing
	// a different environment from the one the author declared.
	for _, key := range []string{"services", "caches"} {
		if value, present := args[key]; present {
			list, ok := value.([]any)
			if !ok || len(list) != 0 {
				return fmt.Errorf("pipeline_step: fleet %s are not supported by this worker", key)
			}
		}
	}
	return nil
}

func checkPipelineRuntime(ctx context.Context, req *pipelineStepRequest) error {
	if req.execution == "native" {
		if host := runtime.GOOS + "/" + runtime.GOARCH; req.platform != host {
			return fmt.Errorf("pipeline_step: native step requires %s; this host is %s", req.platform, host)
		}
		if slices.Contains(req.needs, "macos_tooling") && runtime.GOOS != "darwin" {
			return errors.New("pipeline_step: macOS tooling requires a Mac")
		}
		if !slices.Contains(req.needs, "docker") {
			return nil
		}
	}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := dockerOutput(probe, "info", "--format", "{{.ID}}\n{{.OSType}}/{{.Architecture}}")
	if err != nil {
		return fmt.Errorf("pipeline_step: Docker is not ready: %w", err)
	}
	daemonID, platform, ok := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if !ok || daemonID == "" {
		return errors.New("pipeline_step: Docker did not report its daemon identity")
	}
	req.dockerID = daemonID
	platform = strings.ReplaceAll(platform, "/aarch64", "/arm64")
	platform = strings.ReplaceAll(platform, "/x86_64", "/amd64")
	if req.execution == "native" {
		if platform != "linux/amd64" && platform != "linux/arm64" {
			return errors.New("pipeline_step: Docker must serve Linux containers")
		}
		return nil
	}
	if platform != req.platform {
		return fmt.Errorf("pipeline_step: Docker serves %s; step requires %s (emulation is not admitted)", platform, req.platform)
	}
	return nil
}

// No request environment reaches the Docker client. The script travels through
// stdin to the container shell, so multiline values are supported without
// putting secrets in process arguments, Docker configuration or a host file.
func containerScript(req pipelineStepRequest) string {
	env := make(map[string]string, len(req.env)+len(req.secrets))
	for k, v := range req.env {
		env[k] = v
	}
	for k, v := range req.secrets {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "export %s=%s\n", k, shellLiteral(env[k]))
	}
	fmt.Fprintf(&b, "exec /bin/sh -c %s </dev/null\n", shellLiteral(req.command))
	return b.String()
}
func shellLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func dockerCommand(args ...string) *exec.Cmd {
	cmd := exec.Command(dockerBinary, args...)
	cmd.Env = stepEnvironment(os.Environ(), nil, nil)
	return cmd
}

func dockerOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmd := dockerCommand(args...)
	// Control calls are bounded and carry no step secrets. CommandContext is
	// insufficient for a credential helper that holds stdout open after exit.
	bounded := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	bounded.Env = cmd.Env
	bounded.WaitDelay = 2 * time.Second
	out, err := bounded.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w", args[0], err)
	}
	return out, nil
}

func (r *pipelineRun) containerCommand(ctx context.Context) (code int, fail *memqlv1.Failure) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return 0, failure("exec_failed", "pipeline_step: cannot allocate container identity")
	}
	name := "memql-step-" + hex.EncodeToString(random[:])
	if r.req.dockerID == "" {
		return 0, failure("pipeline_runtime_unavailable", "pipeline_step: Docker identity is unknown")
	}
	if err := r.reservation.begin(pipelineAttemptRecord{Execution: "container", Container: name, DockerID: r.req.dockerID, Workspace: r.dir}); err != nil {
		return 0, failure("pipeline_recovery_required", err.Error())
	}
	// The name is known BEFORE create: if create's reply is lost, cleanup can
	// still address exactly this attempt. No other user's containers are swept.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := removePipelineContainer(cleanup, name); err != nil {
			code = 0
			fail = failure("pipeline_cleanup_uncertain", "pipeline_step: container cleanup could not be confirmed; do not replay this attempt until its container is reconciled: "+name)
		} else if err := r.reservation.clear(); err != nil {
			code = 0
			fail = failure("pipeline_recovery_required", "pipeline_step: container removed but its reservation could not be cleared")
		}
	}()
	if strings.ContainsAny(r.dir, ",\n\r") {
		return 0, failure("exec_failed", "pipeline_step: Docker workspace path contains an unsupported delimiter")
	}
	args := []string{"create", "--name", name, "--interactive", "--init", "--restart=no",
		"--label", "io.memql.pipeline-attempt=" + name,
		"--platform", r.req.platform, "--pull=missing", "--entrypoint", "/bin/sh",
		"--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=512",
		"--cpus=2", "--memory=2g", "--memory-swap=2g",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--mount", "type=bind,source=" + r.dir + ",target=/workspace",
		"--workdir", "/workspace", "--env", "HOME=/tmp", r.req.image}
	// Creation/pulls can stream progress, but do not contain the command or its secrets.
	result, err := runGroup(ctx, dockerCommand(args...), r.mask, r.emit)
	r.bytes.Add(result.bytes)
	if result.stopped {
		return 0, r.stopped(ctx)
	}
	if err != nil || result.exitCode != 0 {
		return 0, failure("pipeline_container_failed", "pipeline_step: Docker could not create the declared container")
	}
	cmd := dockerCommand("start", "--attach", "--interactive", name)
	cmd.Stdin = strings.NewReader(containerScript(r.req))
	result, err = runGroup(ctx, cmd, r.mask, r.emit)
	r.bytes.Add(result.bytes)
	if result.stopped {
		return 0, r.stopped(ctx)
	}
	if err != nil {
		return 0, failure("pipeline_container_uncertain", "pipeline_step: lost the container command result")
	}
	// A Docker client exit is not a build receipt. Inspect the actual container:
	// a daemon disconnect can make the client exit while work is still running.
	out, err := dockerOutput(ctx, "inspect", "--format", "{{json .State}}", name)
	if err != nil {
		return 0, failure("pipeline_container_uncertain", "pipeline_step: cannot confirm the container's terminal state")
	}
	var state struct {
		Status    string
		Running   bool
		ExitCode  int
		OOMKilled bool
		Error     string
	}
	if json.Unmarshal(out, &state) != nil || state.Running || state.Status != "exited" || state.Error != "" {
		return 0, failure("pipeline_container_uncertain", "pipeline_step: Docker has not confirmed a completed command")
	}
	if state.OOMKilled {
		if state.ExitCode == 0 {
			state.ExitCode = 137
		}
		r.note(true, "memql: container exceeded its memory limit\n")
	}
	return state.ExitCode, nil
}

func removePipelineContainer(ctx context.Context, name string) error {
	_, removeErr := dockerOutput(ctx, "rm", "--force", "--volumes", name)
	if removeErr == nil {
		return nil
	}
	// inspect's nonzero could mean a daemon outage, not absence. A successful
	// exact-name listing is required to establish absence after an uncertain rm.
	out, err := dockerOutput(ctx, "ps", "--all", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}")
	if err == nil && strings.TrimSpace(string(out)) == "" {
		return nil
	}
	return errors.New("container removal unconfirmed")
}

// A fresh process may reclaim a container attempt only on the same daemon.
// Native work may have escaped the original process group; without a durable
// host-process identity it requires operator reconciliation, never a blind retry.
func reconcilePipelineReservation(ctx context.Context, reservation *pipelineReservation) error {
	record, err := reservation.read()
	if err != nil {
		return err
	}
	if record.Execution != "container" || record.DockerID == "" || !regexp.MustCompile(`^memql-step-[0-9a-f]{32}$`).MatchString(record.Container) {
		return errPipelineUnreconciled
	}
	cleanup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := dockerOutput(cleanup, "info", "--format", "{{.ID}}")
	if err != nil || strings.TrimSpace(string(out)) != record.DockerID {
		return errors.New("interrupted container's Docker daemon is unavailable or changed; reconciliation required")
	}
	if err = removePipelineContainer(cleanup, record.Container); err != nil {
		return errPipelineUnreconciled
	}
	if filepath.IsAbs(record.Workspace) && strings.HasPrefix(filepath.Base(record.Workspace), "step-") {
		removeStepDir(record.Workspace)
	}
	return reservation.clear()
}
