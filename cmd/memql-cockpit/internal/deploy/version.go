package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// bundleSubtree is the path within the engine's embedded DSL tree that
// holds the deployment bundle (specs + the future deployment capabilities,
// logic, mutations, and automations from epic memql#2212). The bundle
// version is a content hash over exactly this subtree so a `deploy` is
// reproducible from `cockpit version + bundle version`.
const bundleSubtree = "deployment"

// BundleVersion returns a deterministic content fingerprint of the
// deployment bundle embedded in the engine DSL tree. The same bundle bytes
// always hash to the same value across machines, so printing it alongside
// the cockpit version pins the procedure (handoff "Reproducibility = pin
// the procedure" decision).
//
// The hash covers every file path + content under the deployment subtree,
// in sorted path order, so it is insensitive to filesystem walk ordering.
func BundleVersion() (string, error) {
	return bundleVersionFromTree(memqldsl.Tree())
}

func bundleVersionFromTree(tree fs.FS) (string, error) {
	sub, err := fs.Sub(tree, bundleSubtree)
	if err != nil {
		return "", fmt.Errorf("open deployment subtree: %w", err)
	}

	type entry struct {
		path string
		data []byte
	}
	var entries []entry
	walkErr := fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := fs.ReadFile(sub, path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		entries = append(entries, entry{path: path, data: data})
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("walk deployment subtree: %w", walkErr)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("deployment subtree %q is empty", bundleSubtree)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })

	h := sha256.New()
	for _, e := range entries {
		// Length-prefix the path + content so distinct (path, data)
		// splits cannot collide.
		fmt.Fprintf(h, "%s\x00%d\x00", e.path, len(e.data))
		h.Write(e.data)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:16], nil
}

// VersionLine renders the reproducibility stamp printed on every deploy /
// run invocation: the cockpit binary version + the deployment bundle hash.
func VersionLine(cockpitVersion, bundleVersion string) string {
	cv := strings.TrimSpace(cockpitVersion)
	if cv == "" {
		cv = "dev"
	}
	return fmt.Sprintf("cockpit %s · bundle %s", cv, bundleVersion)
}
