package genesis

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/visionarys-io/memql/component/secret"
)

const defaultGenesisName = "genesis.znas"

// HandleCommand is the entry point invoked from main.go's
// `case "genesis":`. Dispatches on the next positional arg (currently
// only `init`; siblings like `show` / `rotate` / `path` are left as
// follow-ups).
func HandleCommand(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printUsage()
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "init":
		return handleInit(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "memql-cockpit genesis: unknown subcommand %q\n", args[0])
		printUsage()
		return 1
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `Usage:
  memql-cockpit genesis init [--from <env-file>] [--manifest <path>]
                             [--out <path>] [--no-tui] [--no-rc]

  Create or update an encrypted genesis.znas from a .env file.

  Auto-detection:
    .env source:  ./.env.local then ./.env (override with --from or a
                  trailing positional argument)
    Output path:  $MEMQL_GENESIS_PATH then ~/.memql/genesis.znas
                  (override with --out)
    Manifest:     --manifest > MEMQL_MANIFEST_PATH > $MEMQL_REPO/...
                  > embedded snapshot

  First-time:
    Generates a 32-byte master key UNLESS MEMQL_MASTER_KEY is
    already set in the environment (typically via ~/.bashrc), in
    which case cockpit reuses that value so the operator doesn't
    have to update their shell config + re-source every terminal
    after a fresh init. New-key flow prints the key with a "save
    this now" gate; on a TTY you must type 'yes' before the file
    is written. Pass --no-tui to skip the gate in CI. The reuse
    path skips the gate (key is already in the operator's env).

  Update (genesis.znas already exists at the target path):
    Requires MEMQL_MASTER_KEY in env. Cockpit verifies the key opens
    the existing file, then re-encrypts the new .env content under
    the same key. Replace semantics: anything missing from the new
    .env is gone from the new genesis.

  Validation is strict-superset against the manifest: every manifest
  entry must be present in the .env. Extras above the manifest floor
  pass through silently and get encrypted along with everything else.

  Shell rc sync:
    After sealing, cockpit reconciles MEMQL_MASTER_KEY in your
    ~/.bashrc and ~/.zshrc (only those that exist) so future shells
    decrypt the envelope with the same key. Pass --no-rc to skip
    (useful in CI or when you manage your rc files elsewhere).
`)
}

func handleInit(args []string) int {
	fs := flag.NewFlagSet("genesis init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fromFlag := fs.String("from", "", "")
	manifestArg := fs.String("manifest", "", "")
	outFlag := fs.String("out", "", "")
	noTUI := fs.Bool("no-tui", false, "")
	noRC := fs.Bool("no-rc", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) > 1 {
		fmt.Fprintln(os.Stderr, "genesis init: unexpected extra arguments")
		return 2
	}

	fromPath := *fromFlag
	if fromPath == "" && len(rest) == 1 {
		fromPath = rest[0]
	}
	if fromPath == "" {
		fromPath = resolveEnvFile()
	}
	if fromPath == "" {
		fmt.Fprintln(os.Stderr, "genesis init: no .env file found at ./.env.local or ./.env; pass --from <path>")
		return 1
	}

	outPath := *outFlag
	if outPath == "" {
		outPath = os.Getenv("MEMQL_GENESIS_PATH")
	}
	if outPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "genesis init: resolve home directory: %v\n", err)
			return 1
		}
		outPath = filepath.Join(home, ".memql", defaultGenesisName)
	}

	entries, err := ParseEnvFile(fromPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}

	manifest, err := LoadManifest(*manifestArg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}

	required := manifest.Names()
	if missing := findMissing(entries, required); len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "genesis init: .env at %s is missing manifest entries (manifest source: %s):\n", fromPath, manifest.Source)
		for _, name := range missing {
			fmt.Fprintf(os.Stderr, "  - %s\n", name)
		}
		return 1
	}

	firstTime := !fileExists(outPath)
	var masterKeyHex string
	reusedKeyFromEnv := false

	if firstTime {
		// Reuse an existing MEMQL_MASTER_KEY from the operator's
		// environment when present. The common dev workflow is to
		// delete the genesis file and re-init under the SAME key
		// already exported in ~/.bashrc -- generating a fresh key
		// here forces a bashrc edit + re-source across every open
		// terminal. Validating the shape (32-byte hex) rejects
		// garbage env values so we don't seal a genesis under a key
		// the operator can't actually reproduce.
		if envKey := strings.TrimSpace(os.Getenv(secret.EnvMasterKey)); envKey != "" {
			if verr := validateMasterKeyHex(envKey); verr != nil {
				fmt.Fprintf(os.Stderr, "genesis init: %s present in env but malformed (%v); generating a fresh key.\n", secret.EnvMasterKey, verr)
			} else {
				masterKeyHex = envKey
				reusedKeyFromEnv = true
				fmt.Fprintf(os.Stderr, "genesis init: reusing %s already set in your environment.\n", secret.EnvMasterKey)
			}
		}
		if masterKeyHex == "" {
			masterKeyHex, err = generateMasterKey()
			if err != nil {
				fmt.Fprintf(os.Stderr, "genesis init: generate master key: %v\n", err)
				return 1
			}
		}
	} else {
		masterKeyHex = os.Getenv(secret.EnvMasterKey)
		if masterKeyHex == "" {
			fmt.Fprintf(os.Stderr, "genesis init: %s exists; export %s before re-running to update it.\n", outPath, secret.EnvMasterKey)
			return 1
		}
		existing, rerr := os.ReadFile(outPath)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "genesis init: read existing %s: %v\n", outPath, rerr)
			return 1
		}
		if _, derr := secret.OpenBlob(existing); derr != nil {
			fmt.Fprintf(os.Stderr, "genesis init: %s won't decrypt with the supplied %s: %v\n", outPath, secret.EnvMasterKey, derr)
			return 1
		}
	}

	// First-time only: gate writes behind explicit confirmation that
	// the user has saved the key. Update path skips this -- they
	// already have the key (it was in their env to verify the file).
	// The env-reuse path also skips it: the key is already in the
	// operator's environment, so there's nothing new to save.
	if firstTime && !reusedKeyFromEnv {
		confirmed, cerr := ShowMasterKey(os.Stdout, os.Stdin, masterKeyHex, *noTUI)
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "genesis init: confirmation: %v\n", cerr)
			return 1
		}
		if !confirmed {
			fmt.Fprintln(os.Stderr, "Master key not confirmed -- nothing written. Re-run when ready.")
			return 1
		}
	}

	// Set MEMQL_MASTER_KEY in-process so secret.SealBlob picks up the
	// right key. In first-time mode the caller's shell has no key set;
	// in update mode this is a no-op (same value going in). Restore on
	// exit so we don't surprise downstream subcommands.
	prevKey := os.Getenv(secret.EnvMasterKey)
	if err := os.Setenv(secret.EnvMasterKey, masterKeyHex); err != nil {
		fmt.Fprintf(os.Stderr, "genesis init: set %s in-process: %v\n", secret.EnvMasterKey, err)
		return 1
	}
	defer func() { _ = os.Setenv(secret.EnvMasterKey, prevKey) }()

	// Keep the in-envelope MEMQL_MASTER_KEY in lockstep with the
	// key used to seal the envelope. The .env gets sourced after
	// decryption and overrides the operator's shell value, so a
	// mismatch silently swaps the cluster's operator key out from
	// under the seeding tool (BFF rejects the seed with
	// "master-key mismatch"). Reconciling here means there's only
	// ever one operator key to think about: the one cockpit prints.
	//
	// The source .env on disk gets the same reconciliation below,
	// after sealing succeeds, so a second run is genuinely idempotent
	// instead of re-emitting "replaced" on every invocation.
	var rec reconcileAction
	entries, rec = reconcileMasterKey(entries, masterKeyHex)

	plaintext := SerializeEntries(entries)
	envelope, err := secret.SealBlob(plaintext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "genesis init: seal: %v\n", err)
		return 1
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "genesis init: mkdir %s: %v\n", filepath.Dir(outPath), err)
		return 1
	}
	if err := writeGenesisAtomic(outPath, envelope); err != nil {
		fmt.Fprintf(os.Stderr, "genesis init: write %s: %v\n", outPath, err)
		return 1
	}

	// Persist the reconciled key back into the source .env. Doing
	// this only AFTER successful seal means a write failure here
	// can never leave the operator with a sealed envelope under a
	// key the .env doesn't carry -- the envelope is already on
	// disk by the time we touch the .env.
	if rec != reconcileNoop {
		syncSourceEnvFile(fromPath, masterKeyHex, rec)
	}

	if !*noRC {
		syncShellRCs(masterKeyHex)
	}

	extras := len(entries) - len(required)
	verb := "Wrote"
	if !firstTime {
		verb = "Updated"
	}
	fmt.Fprintf(os.Stdout, "%s %d entries to %s (%d manifest, %d extras; manifest source: %s).\n",
		verb, len(entries), outPath, len(required), extras, manifest.Source)
	return 0
}

// syncSourceEnvFile rewrites path so its MEMQL_MASTER_KEY line
// carries masterKeyHex. Called only when the in-memory reconcile
// changed something -- a noop here would just touch the file's
// mtime for nothing. Best-effort: a write failure prints a warning
// but doesn't abort, because the sealed envelope is already on
// disk and the operator can fix the .env by hand from the printed
// key.
//
// The on-disk rewrite preserves comments, blank lines, and
// formatting; only the MEMQL_MASTER_KEY line itself changes (or
// a fresh one appends to the end).
func syncSourceEnvFile(path, masterKeyHex string, action reconcileAction) {
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "genesis init: could not re-read %s to sync %s (%v); sealed envelope is correct, source .env still carries the old value.\n", path, secret.EnvMasterKey, err)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "genesis init: could not stat %s to sync %s (%v); sealed envelope is correct, source .env still carries the old value.\n", path, secret.EnvMasterKey, err)
		return
	}
	out, replaced, appended := RewriteMasterKeyAssignment(raw, masterKeyHex)
	if !replaced && !appended {
		// Defensive: the in-memory reconcile said the entries
		// changed but the on-disk rewrite found nothing to do.
		// Should be unreachable but doesn't deserve a hard failure.
		return
	}
	if err := writeRCAtomic(path, out, info.Mode().Perm()); err != nil {
		fmt.Fprintf(os.Stderr, "genesis init: could not write %s to sync %s (%v); sealed envelope is correct, source .env still carries the old value.\n", path, secret.EnvMasterKey, err)
		return
	}
	switch action {
	case reconcileReplaced:
		fmt.Fprintf(os.Stderr, "genesis init: updated %s in %s to match the encryption key.\n", secret.EnvMasterKey, path)
	case reconcileAdded:
		fmt.Fprintf(os.Stderr, "genesis init: added %s to %s (cluster will boot with the same key the envelope uses).\n", secret.EnvMasterKey, path)
	}
}

// syncShellRCs reconciles MEMQL_MASTER_KEY across the operator's
// shell rc files so future shells decrypt the envelope with the
// same key cockpit just sealed it under. Best-effort: per-file
// failures are reported to stderr but don't abort the run, because
// the genesis file is already written and the operator can fix
// their rc manually from the printed master key.
func syncShellRCs(masterKeyHex string) {
	paths, err := rcCandidatePaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "genesis init: resolve shell rc paths: %v (skipping rc sync)\n", err)
		return
	}
	for _, p := range paths {
		action, rerr := reconcileShellRC(p, masterKeyHex)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "genesis init: sync %s: %v\n", p, rerr)
			continue
		}
		switch action {
		case rcUpdated:
			fmt.Fprintf(os.Stderr, "genesis init: updated %s in %s\n", secret.EnvMasterKey, p)
		case rcAppended:
			fmt.Fprintf(os.Stderr, "genesis init: appended %s to %s\n", secret.EnvMasterKey, p)
		}
	}
}

func resolveEnvFile() string {
	for _, candidate := range []string{".env.local", ".env"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func generateMasterKey() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// validateMasterKeyHex checks that s looks like a generated master
// key: lowercase/uppercase hex, 64 chars (32 raw bytes). Used to
// gate the env-reuse path so we don't seal a genesis under garbage.
func validateMasterKeyHex(s string) error {
	if len(s) != 64 {
		return fmt.Errorf("expected 64 hex chars (32 bytes), got %d", len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return fmt.Errorf("not valid hex")
	}
	return nil
}

// writeGenesisAtomic writes envelope to outPath without leaving the
// destination in a half-written state if anything goes wrong. The
// temp file is created in the same directory as outPath so the
// final os.Rename is a same-filesystem move (atomic on POSIX). On
// any error before the rename, the previous genesis.znas at outPath
// is untouched; on a rename failure the temp file is removed.
func writeGenesisAtomic(outPath string, envelope []byte) error {
	dir := filepath.Dir(outPath)
	tmp, err := os.CreateTemp(dir, filepath.Base(outPath)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	// Remove the temp file on any error path. Once Rename succeeds,
	// the temp name no longer exists so this is a no-op.
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(envelope); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, outPath); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpName, outPath, err)
	}
	cleanup = false
	return nil
}

// reconcileAction describes what reconcileMasterKey did to the
// entries: nothing, replaced an existing mismatching value, or
// appended a new entry.
type reconcileAction int

const (
	reconcileNoop reconcileAction = iota
	reconcileReplaced
	reconcileAdded
)

// reconcileMasterKey makes the in-envelope MEMQL_MASTER_KEY entry
// match masterKeyHex (the key used to seal the envelope). The
// returned slice shares backing storage with the input when the
// value was replaced in place; callers should treat the input as
// consumed.
func reconcileMasterKey(entries []EnvEntry, masterKeyHex string) ([]EnvEntry, reconcileAction) {
	for i := range entries {
		if entries[i].Name != secret.EnvMasterKey {
			continue
		}
		if entries[i].Value == masterKeyHex {
			return entries, reconcileNoop
		}
		entries[i].Value = masterKeyHex
		return entries, reconcileReplaced
	}
	return append(entries, EnvEntry{Name: secret.EnvMasterKey, Value: masterKeyHex}), reconcileAdded
}

// findMissing returns the required names not present in entries,
// in input order. Empty result means the .env covers every required
// name (extras above the floor are not flagged here).
func findMissing(entries []EnvEntry, required []string) []string {
	have := make(map[string]bool, len(entries))
	for _, e := range entries {
		have[e.Name] = true
	}
	var missing []string
	for _, name := range required {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	return missing
}
