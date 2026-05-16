// Package dockerprobe enumerates the containers that belong to a
// memQL cluster running on the local docker daemon. The data backs
// two surfaces:
//
//   1. The "Set up local cluster" wizard uses ClusterRunning to
//      branch into a "already running" status mode instead of
//      blindly running port-in-use checks against ports owned by
//      the cluster itself.
//   2. The operating console's topology pane uses Containers to
//      augment the gRPC clusterNodes feed with infrastructure
//      services (LB, DB, IDENTITY, REDIS, ...) that don't register
//      themselves as memQL nodes.
//
// Everything here is best-effort: missing docker, missing daemon,
// or an unknown compose project label all return an empty result
// with a nil error so callers degrade gracefully (e.g. a remote
// cluster doesn't get spurious "docker not installed" warnings).
package dockerprobe

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// ProjectName is the docker-compose project label every memQL
// service ships with (set via `name: memql-cluster` at the top of
// docker-compose.full.yml). Used in `docker ps --filter label=...`
// to scope our queries to memQL containers only.
const ProjectName = "memql-cluster"

// Container is one row from the docker probe. State is the raw
// docker container state ("running", "exited", "paused", "dead",
// ...); HealthHint is parsed out of the human "Status" string when
// docker prints a "(healthy)" / "(unhealthy)" / "(starting)"
// suffix, so callers can render a more specific health colour
// without parsing the string themselves.
type Container struct {
	Name       string // e.g. "memql-bff"
	Service    string // compose service name, e.g. "bff", "nginx", "postgres"
	State      string // docker state: running / exited / paused / dead / ...
	Status     string // human status: "Up 12 minutes (healthy)" etc.
	HealthHint string // healthy / unhealthy / starting / none -- parsed from Status
}

// Running reports whether the container's state is running. Used
// by the wizard to distinguish "cluster up, ports owned by us" from
// "cluster down, real port conflict."
func (c Container) Running() bool {
	return c.State == "running"
}

// Containers returns the list of compose-managed containers for
// ProjectName. Returns (nil, nil) on any environmental failure
// (no docker, no daemon, unknown project) so callers can treat
// "empty list" as the unified "nothing to show" signal.
func Containers() ([]Container, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// `--format '{{.Names}}|{{.State}}|{{.Status}}|{{.Label}}'` keeps
	// us off the JSON path (no extra dep) while staying
	// unambiguous: the pipe never appears in any of the fields we
	// pull (names are alnum + dashes, states are single words,
	// status is human-friendly but pipe-free, labels are simple
	// strings).
	out, err := exec.CommandContext(ctx, "docker", "ps",
		"--all",
		"--filter", "label=com.docker.compose.project="+ProjectName,
		"--format", `{{.Names}}|{{.State}}|{{.Status}}|{{.Label "com.docker.compose.service"}}`,
	).Output()
	if err != nil {
		return nil, nil
	}
	return parseDockerPS(string(out)), nil
}

// ClusterRunning reports whether at least one project container is
// in the running state. Pure helper on top of Containers; lets
// callers ask the one-liner question without iterating.
func ClusterRunning() (bool, []Container, error) {
	cs, err := Containers()
	if err != nil {
		return false, nil, err
	}
	for _, c := range cs {
		if c.Running() {
			return true, cs, nil
		}
	}
	return false, cs, nil
}

func parseDockerPS(out string) []Container {
	var rows []Container
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 4 {
			continue
		}
		c := Container{
			Name:       parts[0],
			State:      parts[1],
			Status:     parts[2],
			Service:    parts[3],
			HealthHint: parseHealthHint(parts[2]),
		}
		rows = append(rows, c)
	}
	return rows
}

// parseHealthHint extracts the parenthesized "(healthy)" /
// "(unhealthy)" / "(starting)" suffix docker appends to a Status
// string when the container has a HEALTHCHECK directive. Returns
// the empty string when no parenthesized hint is present (or the
// parens hold something we don't recognize).
func parseHealthHint(status string) string {
	open := strings.LastIndex(status, "(")
	close := strings.LastIndex(status, ")")
	if open < 0 || close <= open {
		return ""
	}
	hint := strings.TrimSpace(status[open+1 : close])
	switch hint {
	case "healthy", "unhealthy", "starting", "health: starting":
		if hint == "health: starting" {
			return "starting"
		}
		return hint
	}
	return ""
}
