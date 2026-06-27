package deploy

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestBundleVersionFromTree_DeterministicAndContentSensitive(t *testing.T) {
	tree := fstest.MapFS{
		"deployment/specs.memql":       {Data: []byte("spec requiresOwner { actor.role == \"owner\" }")},
		"deployment/automations.memql": {Data: []byte("automation deployEngineCluster { ... }")},
		"other/ignored.memql":          {Data: []byte("not part of the bundle")},
	}

	v1, err := bundleVersionFromTree(tree)
	if err != nil {
		t.Fatalf("bundleVersionFromTree: %v", err)
	}
	if !strings.HasPrefix(v1, "sha256:") {
		t.Fatalf("version %q missing sha256: prefix", v1)
	}

	// Deterministic across calls.
	v1b, _ := bundleVersionFromTree(tree)
	if v1 != v1b {
		t.Errorf("non-deterministic: %q != %q", v1, v1b)
	}

	// Changing a file OUTSIDE the deployment subtree must NOT move the hash.
	tree["other/ignored.memql"] = &fstest.MapFile{Data: []byte("changed")}
	if v, _ := bundleVersionFromTree(tree); v != v1 {
		t.Errorf("hash changed on non-bundle edit: %q != %q", v, v1)
	}

	// Changing a deployment file MUST move the hash.
	tree["deployment/specs.memql"] = &fstest.MapFile{Data: []byte("spec requiresOwner { actor.role == \"admin\" }")}
	if v, _ := bundleVersionFromTree(tree); v == v1 {
		t.Errorf("hash unchanged after bundle edit: %q", v)
	}
}

func TestBundleVersionFromTree_EmptySubtree(t *testing.T) {
	if _, err := bundleVersionFromTree(fstest.MapFS{"x/y.memql": {Data: []byte("z")}}); err == nil {
		t.Fatal("expected error for empty deployment subtree")
	}
}

func TestBundleVersion_RealTree(t *testing.T) {
	// The real embedded engine tree has dsl/deployment/specs.memql, so the
	// bundle version resolves to a stable, non-empty fingerprint.
	v, err := BundleVersion()
	if err != nil {
		t.Fatalf("BundleVersion: %v", err)
	}
	if !strings.HasPrefix(v, "sha256:") || len(v) < 10 {
		t.Fatalf("unexpected bundle version %q", v)
	}
}

func TestVersionLine(t *testing.T) {
	if got := VersionLine("0.9.0", "sha256:abc"); got != "cockpit 0.9.0 · bundle sha256:abc" {
		t.Errorf("VersionLine = %q", got)
	}
	if got := VersionLine("", "sha256:abc"); !strings.Contains(got, "cockpit dev") {
		t.Errorf("empty cockpit version should fall back to dev: %q", got)
	}
}
