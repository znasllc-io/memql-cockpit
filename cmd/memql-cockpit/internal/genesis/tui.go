package genesis

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ShowMasterKey renders the freshly-generated master key with a
// prominent "SAVE THIS NOW" framing and either prompts for an
// explicit "yes" confirmation (TTY) or just prints (non-TTY / CI).
//
// Returns:
//   - true  if confirmation succeeded (or input was non-TTY, in which
//           case there is nothing to confirm against)
//   - false if the user typed anything other than y / yes / Y / Yes
//
// forcePrintf=true forces the non-TTY path regardless of stdin.
// w is the writer the framed output lands on (typically os.Stdout);
// in is the reader the confirmation answer is read from (typically
// os.Stdin -- needs to be the *os.File so we can probe IsTerminal).
func ShowMasterKey(w io.Writer, in *os.File, masterKeyHex string, forcePrintf bool) (bool, error) {
	isTTY := !forcePrintf && term.IsTerminal(int(in.Fd()))

	bar := strings.Repeat("=", 78)
	fmt.Fprintln(w)
	fmt.Fprintln(w, bar)
	fmt.Fprintln(w, "  MEMQL_MASTER_KEY -- SAVE THIS NOW.")
	fmt.Fprintln(w, "  It will NOT be shown again. Lose it and your genesis.znas is unrecoverable.")
	fmt.Fprintln(w, bar)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "    MEMQL_MASTER_KEY=%s\n", masterKeyHex)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  1. Paste it into your password manager.")
	fmt.Fprintln(w, "  2. Export it in your shell -- e.g. add to ~/.bashrc:")
	fmt.Fprintf(w, "       export MEMQL_MASTER_KEY=%s\n", masterKeyHex)
	fmt.Fprintln(w, bar)
	fmt.Fprintln(w)

	if !isTTY {
		return true, nil
	}

	fmt.Fprint(w, "Have you saved the master key? Type 'yes' to write genesis.znas: ")
	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("ShowMasterKey: read input: %w", err)
		}
		return false, nil
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	return answer == "y" || answer == "yes", nil
}
