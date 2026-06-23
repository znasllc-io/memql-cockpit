package dsledit

// Local bundle workspace IO for the Editor tab's authoring mode
// (memql-cockpit#230). A "bundle" is a local directory of .memql files
// under ~/.memql/bundles/<name>/ that the user authors and later
// validates / injects into a session (C3 #231). This file is the
// on-disk surface: list / create bundles + files, read / write a
// file's source. No cluster, no gRPC -- the bundle lives on the
// operator's machine until it's explicitly validated/injected.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// bundleRoot returns the directory that holds all local bundles
// (~/.memql/bundles). Mirrors config.ConfigDir's ~/.memql resolution
// without importing it, so dsledit keeps its narrow dependency set.
func bundleRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".memql", "bundles")
	}
	return filepath.Join(home, ".memql", "bundles")
}

// safeName rejects names that aren't a single safe path segment --
// no separators, no traversal, no leading dot. Authoring writes to
// disk, so this is the guard against path-escape.
func safeName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is empty")
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("name %q must be a single path segment", name)
	}
	if name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return fmt.Errorf("name %q may not start with a dot", name)
	}
	return nil
}

// ensureMemqlExt appends ".memql" when the file name carries no
// recognized DSL extension, so the user can type "queries" and get
// "queries.memql".
func ensureMemqlExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".memql", ".tmpl":
		return name
	default:
		return name + ".memql"
	}
}

// listBundles returns the names of every local bundle directory,
// sorted. A missing root is not an error -- it just means no bundles
// authored yet.
func listBundles() ([]string, error) {
	entries, err := os.ReadDir(bundleRoot())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// createBundle makes a new empty bundle directory. Errors if the name
// is unsafe or the bundle already exists.
func createBundle(name string) error {
	if err := safeName(name); err != nil {
		return err
	}
	dir := filepath.Join(bundleRoot(), name)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("bundle %q already exists", name)
	}
	return os.MkdirAll(dir, 0o755)
}

// listBundleFiles returns the .memql/.tmpl files in a bundle, sorted.
func listBundleFiles(bundle string) ([]string, error) {
	if err := safeName(bundle); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(bundleRoot(), bundle))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".memql", ".tmpl":
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// createBundleFile creates a new empty file inside a bundle (a .memql
// extension is appended when none is given). Errors if it exists.
func createBundleFile(bundle, name string) (string, error) {
	if err := safeName(bundle); err != nil {
		return "", err
	}
	name = ensureMemqlExt(strings.TrimSpace(name))
	if err := safeName(name); err != nil {
		return "", err
	}
	path := filepath.Join(bundleRoot(), bundle, name)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("file %q already exists in bundle %q", name, bundle)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return "", err
	}
	return name, nil
}

// readBundleFile returns a bundle file's source.
func readBundleFile(bundle, name string) (string, error) {
	if err := safeName(bundle); err != nil {
		return "", err
	}
	if err := safeName(name); err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(bundleRoot(), bundle, name))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// writeBundleFile saves a bundle file's source to disk.
func writeBundleFile(bundle, name, content string) error {
	if err := safeName(bundle); err != nil {
		return err
	}
	if err := safeName(name); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundleRoot(), bundle, name), []byte(content), 0o644)
}
