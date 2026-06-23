package dsledit

import (
	"path/filepath"
	"testing"
)

func TestSafeNameRejectsTraversal(t *testing.T) {
	bad := []string{"", "  ", "../evil", "a/b", `a\b`, ".", "..", ".hidden"}
	for _, n := range bad {
		if err := safeName(n); err == nil {
			t.Errorf("safeName(%q) = nil, want error", n)
		}
	}
	for _, n := range []string{"queries", "my-bundle", "cognition.memql"} {
		if err := safeName(n); err != nil {
			t.Errorf("safeName(%q) = %v, want nil", n, err)
		}
	}
}

func TestEnsureMemqlExt(t *testing.T) {
	cases := map[string]string{
		"queries":         "queries.memql",
		"queries.memql":   "queries.memql",
		"agentReply.tmpl": "agentReply.tmpl",
		"foo.txt":         "foo.txt.memql",
	}
	for in, want := range cases {
		if got := ensureMemqlExt(in); got != want {
			t.Errorf("ensureMemqlExt(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBundleLifecycle exercises the on-disk bundle CRUD against an
// isolated HOME.
func TestBundleLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// No bundles yet.
	if bs, err := listBundles(); err != nil || len(bs) != 0 {
		t.Fatalf("listBundles on empty = %v, %v; want [] nil", bs, err)
	}

	if err := createBundle("b1"); err != nil {
		t.Fatalf("createBundle: %v", err)
	}
	// Duplicate is rejected.
	if err := createBundle("b1"); err == nil {
		t.Fatal("createBundle duplicate should error")
	}
	// Unsafe name rejected.
	if err := createBundle("../escape"); err == nil {
		t.Fatal("createBundle with traversal name should error")
	}

	if bs, err := listBundles(); err != nil || len(bs) != 1 || bs[0] != "b1" {
		t.Fatalf("listBundles = %v, %v; want [b1]", bs, err)
	}

	// Create a file (extension auto-appended) and round-trip its content.
	name, err := createBundleFile("b1", "queries")
	if err != nil {
		t.Fatalf("createBundleFile: %v", err)
	}
	if name != "queries.memql" {
		t.Fatalf("created file name = %q, want queries.memql", name)
	}
	if files, err := listBundleFiles("b1"); err != nil || len(files) != 1 || files[0] != "queries.memql" {
		t.Fatalf("listBundleFiles = %v, %v; want [queries.memql]", files, err)
	}

	const src = "query foo {\n  args { x string }\n}"
	if err := writeBundleFile("b1", "queries.memql", src); err != nil {
		t.Fatalf("writeBundleFile: %v", err)
	}
	got, err := readBundleFile("b1", "queries.memql")
	if err != nil {
		t.Fatalf("readBundleFile: %v", err)
	}
	if got != src {
		t.Fatalf("round-trip mismatch: got %q want %q", got, src)
	}

	// Sanity: the bundle lives under ~/.memql/bundles.
	if filepath.Base(filepath.Dir(bundleRoot())) != ".memql" {
		t.Errorf("bundleRoot = %q, want under ~/.memql", bundleRoot())
	}
}
