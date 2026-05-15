package genesis

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"

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
	fmt.Fprintln(os.Stderr, `Usage:
  memql-cockpit genesis init [--from <env-file>] [--manifest <path>]
                             [--out <path>] [--no-tui]

  Create or update an encrypted genesis.znas from a .env file.

  Auto-detection:
    .env source:  ./.env.local then ./.env (override with --from or a
                  trailing positional argument)
    Output path:  $MEMQL_GENESIS_PATH then ~/.memql/genesis.znas
                  (override with --out)
    Manifest:     --manifest > MEMQL_MANIFEST_PATH > $MEMQL_REPO/...
                  > embedded snapshot

  First-time:
    Generates a 32-byte master key. Cockpit prints it with a "save
    this now" gate; on a TTY you must type 'yes' before the file is
    written. Pass --no-tui to skip the gate in CI.

  Update (genesis.znas already exists at the target path):
    Requires MEMQL_MASTER_KEY in env. Cockpit verifies the key opens
    the existing file, then re-encrypts the new .env content under
    the same key. Replace semantics: anything missing from the new
    .env is gone from the new genesis.

  Validation is strict-superset against the manifest: every manifest
  entry must be present in the .env. Extras above the manifest floor
  pass through silently and get encrypted along with everything else.
`)
}

func handleInit(args []string) int {
	fs := flag.NewFlagSet("genesis init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fromFlag := fs.String("from", "", "")
	manifestArg := fs.String("manifest", "", "")
	outFlag := fs.String("out", "", "")
	noTUI := fs.Bool("no-tui", false, "")
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

	if firstTime {
		masterKeyHex, err = generateMasterKey()
		if err != nil {
			fmt.Fprintf(os.Stderr, "genesis init: generate master key: %v\n", err)
			return 1
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
	if firstTime {
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
	if err := os.WriteFile(outPath, envelope, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "genesis init: write %s: %v\n", outPath, err)
		return 1
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
