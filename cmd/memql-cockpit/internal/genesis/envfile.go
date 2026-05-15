// Package genesis implements the `memql-cockpit genesis` subcommand
// family. The primary entry point is `memql-cockpit genesis init`,
// which reads a developer's .env file, validates it against memql's
// manifest, and seals it into ~/.memql/genesis.znas under a master
// key (generated on first run, supplied via MEMQL_MASTER_KEY on
// updates). See memql's docs/planning/genesis-implementation-plan.md
// for the full design.
package genesis

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// EnvEntry is one parsed line: a key, its value, and the original
// source line number (used in validation error messages). Order is
// preserved as the file is read; SerializeEntries re-emits in the
// same order.
type EnvEntry struct {
	Name  string
	Value string
	Line  int
}

// ParseEnvFile reads a .env file from disk. Skips comments and blank
// lines; tolerates an optional `export ` prefix; strips matching
// single or double quotes around the value. Returns a typed error
// for malformed input (missing `=`, empty key).
func ParseEnvFile(path string) ([]EnvEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("envfile: open %s: %w", path, err)
	}
	defer f.Close()
	return parseEnv(f, path)
}

func parseEnv(r io.Reader, label string) ([]EnvEntry, error) {
	var out []EnvEntry
	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("envfile: %s:%d: line lacks '=': %q", label, lineNo, line)
		}
		name := strings.TrimSpace(line[:eq])
		value := line[eq+1:]
		if name == "" {
			return nil, fmt.Errorf("envfile: %s:%d: empty key", label, lineNo)
		}
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		out = append(out, EnvEntry{Name: name, Value: value, Line: lineNo})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("envfile: %s: scan: %w", label, err)
	}
	return out, nil
}

// SerializeEntries renders entries in the same shape ParseEnvFile
// expects to read back: one `KEY=VALUE\n` per entry, in input order.
// Output is what gets sealed into the genesis envelope.
func SerializeEntries(entries []EnvEntry) []byte {
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(e.Name)
		sb.WriteByte('=')
		sb.WriteString(e.Value)
		sb.WriteByte('\n')
	}
	return []byte(sb.String())
}
