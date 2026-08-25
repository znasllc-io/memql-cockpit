package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the test's own directory to the module root.
// Located by go.mod rather than a fixed "../.." so the test survives the
// package moving.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestVersionIsSettableByLdflags guards the difference between `var` and
// `const` for the version symbol.
//
// The Makefile stamps the real git tag in with `-ldflags -X
// main.version=$(VERSION)`, and -X silently does NOTHING to a const: no
// error, no warning, just a binary reporting the source string. Measured
// on this tree -- a const build with `-X main.version=STAMPED-9.9.9`
// printed `memql 0.10.0`.
//
// So every release artifact carried the hard-coded constant rather than
// the tag it was cut from, which is the opposite of what VERSIONING.md
// promises. Nothing surfaced it because the number printed was always
// plausible.
//
// Asserted against the SOURCE rather than by building with ldflags: the
// failure is a declaration keyword, and a source check names it. A
// build-and-run assertion would report "wrong version" and leave the
// reader to work out why.
func TestVersionIsSettableByLdflags(t *testing.T) {
	body := readRepoFile(t, filepath.Join("cmd", "memql", "main.go"))

	if strings.Contains(body, "const version =") {
		t.Error("version is declared const; -ldflags -X cannot set it, so release " +
			"artifacts would report the source string instead of their tag")
	}
	if !strings.Contains(body, "var version =") {
		t.Error("main.go declares no `var version =`; the Makefile's -X main.version has no target")
	}

	// The Makefile must actually stamp it, or the var buys nothing.
	if mk := readRepoFile(t, "Makefile"); !strings.Contains(mk, "-X main.version=$(VERSION)") {
		t.Error("the Makefile no longer stamps -X main.version=$(VERSION)")
	}

	// cut-release.sh must rewrite the keyword main.go declares. A
	// const-shaped regex substitutes nothing and the cut proceeds with a
	// stale version compiled in.
	cut := readRepoFile(t, filepath.Join("scripts", "release", "cut-release.sh"))
	if strings.Contains(cut, `s/^const version = `) {
		t.Error("cut-release.sh rewrites `const version`, which main.go does not declare")
	}
	if !strings.Contains(cut, `s/^var version = `) {
		t.Error("cut-release.sh does not rewrite `var version`")
	}
	// ... and must verify its own substitution, since a silent no-op here
	// is precisely how a stale version ships.
	if !strings.Contains(cut, `grep -q "^var version = `) {
		t.Error("cut-release.sh does not verify that its version substitution took effect")
	}
}

// TestBuildVariantStaysConst is the other half. buildVariant is selected
// by a build tag, not stamped, so there is nothing for -X to set and a
// const is correct. Stated so the fix above does not get "applied
// consistently" to a symbol it does not apply to.
func TestBuildVariantStaysConst(t *testing.T) {
	for _, f := range []string{"variant_headless.go", "variant_computeruse.go"} {
		body := readRepoFile(t, filepath.Join("cmd", "memql", f))
		if !strings.Contains(body, "const buildVariant = ") {
			t.Errorf("%s: buildVariant should stay a const -- it is chosen by a build tag, not stamped", f)
		}
	}
}

// TestReleaseArtifactNamesAgree checks the ONE set of strings that lives
// in three files: the Makefile's dist targets, install.sh's download
// URL, and release.yml's upload list.
//
// A rename applied to two of the three publishes a GREEN release whose
// installer 404s. Nothing else in CI can see it -- the release workflow
// succeeds, and the failure surfaces the next time a person runs the
// one-liner.
func TestReleaseArtifactNamesAgree(t *testing.T) {
	makefile := readRepoFile(t, "Makefile")
	installSh := readRepoFile(t, "install.sh")
	releaseYml := readRepoFile(t, filepath.Join(".github", "workflows", "release.yml"))

	if !strings.Contains(installSh, `BINARY="memql"`) {
		t.Error(`install.sh does not install BINARY="memql"`)
	}
	if !strings.Contains(makefile, `memql-$(VERSION)-$$triple.tar.gz`) {
		t.Error("Makefile dist does not package memql-<version>-<triple>.tar.gz")
	}
	if !strings.Contains(makefile, `memql-$(VERSION)-SHA256SUMS`) {
		t.Error("Makefile does not write memql-<version>-SHA256SUMS")
	}
	for _, triple := range []string{"darwin-arm64", "darwin-amd64", "linux-amd64", "linux-arm64"} {
		if !strings.Contains(makefile, "$(BIN_DIR)/memql-"+triple+" ./cmd/memql") {
			t.Errorf("Makefile does not cross-build memql-%s", triple)
		}
		if !strings.Contains(releaseYml, "bin/memql-"+triple) {
			t.Errorf("release.yml does not upload bin/memql-%s", triple)
		}
	}
	for name, body := range map[string]string{"Makefile": makefile, "install.sh": installSh, "release.yml": releaseYml} {
		if strings.Contains(body, "bin/memql-cockpit") || strings.Contains(body, "memql-cockpit-$(VERSION)") {
			t.Errorf("%s still names a pre-rename build artifact", name)
		}
	}
}

// TestInstallersInstallOneCommandName pins design D4: both build
// variants install as `memql`.
//
// The download ARTIFACT carries the variant; the installed file never
// does. Installing the computer-use build as `memql-computeruse`
// reinstates the second command name D4 retires -- and it is the name
// the README, the service unit's ExecStart, and `memql --version`'s
// variant line all assume, so the deviation is silent until an operator
// on a computer-use machine types `memql` and gets nothing.
func TestInstallersInstallOneCommandName(t *testing.T) {
	for _, f := range []string{"install-mac.sh", "install-linux.sh"} {
		body := readRepoFile(t, filepath.Join("scripts", "install", f))
		if strings.Contains(body, `friendly_name="memql-computeruse"`) {
			t.Errorf("%s installs the computer-use build under a second command name; "+
				"design D4 gives both variants the one name `memql`", f)
		}
		if !strings.Contains(body, `"$INSTALLED_COMMAND"`) {
			t.Errorf("%s does not install under lib.sh's INSTALLED_COMMAND", f)
		}
	}
	if lib := readRepoFile(t, filepath.Join("scripts", "install", "lib.sh")); !strings.Contains(lib, `INSTALLED_COMMAND="memql"`) {
		t.Error(`lib.sh does not define INSTALLED_COMMAND="memql"`)
	}
}

// TestRootInstallerMigratesTheService pins the half of the rename
// migration the `curl | sh` path was missing.
//
// It deleted the pre-rename binaries and left the pre-rename service
// loaded. That service is KeepAlive (macOS) / Restart (systemd), so it
// went on trying to exec a path that no longer existed: the worker stops,
// nothing says why, and the operator sees a machine that went offline.
// The token-driven installers under scripts/install/ always did this
// correctly -- the two paths disagreeing is the defect.
func TestRootInstallerMigratesTheService(t *testing.T) {
	body := readRepoFile(t, "install.sh")

	for _, want := range []string{
		"com.znasllc.memql-cockpit-worker", // the legacy macOS label it must retire
		"com.znasllc.memql-worker",         // the label it must register
		"memql-cockpit-worker",             // the legacy systemd unit
		"memql-worker",                     // the systemd unit it must register
		"launchctl unload",
		"systemctl --user disable",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("install.sh never mentions %q; the service migration is incomplete", want)
		}
	}

	// Order is load-bearing: re-register before deleting the old binary,
	// so a KeepAlive restart in the gap finds something to exec.
	migrate := strings.Index(body, `migrate_service "$os"`)
	remove := strings.Index(body, `remove_legacy_binaries "$bindir"`)
	if migrate < 0 || remove < 0 {
		t.Fatal("install.sh does not call both migrate_service and remove_legacy_binaries from main()")
	}
	if migrate > remove {
		t.Error("install.sh deletes the pre-rename binaries before migrating the service; " +
			"a KeepAlive restart in that gap execs a path that no longer exists")
	}
}
