package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// The ledger: what this machine has already sent, per watched folder.
//
// ===========================================================================
// WHY THERE IS LOCAL STATE AT ALL
// ===========================================================================
// The engine can answer "is there a file at this (machine, path)" -- that is
// what libraryFileByUploadedFrom is for -- but it cannot answer "are the bytes
// there the bytes that are here now" without being sent them. Keeping the last
// stamp locally is what turns every sweep after the first into a few thousand
// stat calls instead of a few thousand uploads.
//
// IT IS A CACHE, NOT A SOURCE OF TRUTH. Losing it costs one expensive sweep
// and nothing else: every re-push is keyed on (machine, path), so a file the
// ledger forgot is re-sent and lands as a new VERSION of the same row rather
// than as a duplicate. That property is what lets this be a plain JSON file
// with no migration story.
//
// ONE FILE PER WATCH, named by the watch id, so a watch somebody stopped can
// be forgotten by deleting one file, and two watches sweeping concurrently
// never write the same file.

const (
	ledgerDirMode  os.FileMode = 0o700
	ledgerFileMode os.FileMode = 0o600
)

// Record is what the ledger remembers about one file.
type Record struct {
	Stamp Stamp `json:"stamp"`
	// FileID is the v1:library:file the last push produced. Kept so the
	// verify lane can report a state without asking the engine which row a
	// path belongs to on every sweep; an empty value simply means "ask".
	FileID string `json:"fileId,omitempty"`
	// PushedAtUnix is when those bytes were accepted.
	PushedAtUnix int64 `json:"pushedAtUnix,omitempty"`
	// VerifiedAtUnix is when this file's link state was last REPORTED to the
	// engine. It paces the verify lane: an unchanged file's `synced` is
	// re-stamped occasionally so "checked 3 weeks ago" stops being true, but
	// not on every sweep, or a folder of ten thousand files would be ten
	// thousand writes every few minutes forever.
	VerifiedAtUnix int64 `json:"verifiedAtUnix,omitempty"`
	// LinkState is the last state reported for this file, so the lane can
	// tell a CHANGE (report it now) from a repeat (report it on the slow
	// cadence).
	LinkState string `json:"linkState,omitempty"`
}

// Ledger is one watch's record set, held in memory and persisted whole.
//
// Persisted WHOLE rather than appended to, because it is small (one line per
// file) and because a partial write is the one failure that would matter: a
// half-written append is a corrupt file, while a rename is atomic and either
// lands or does not.
type Ledger struct {
	mu    sync.Mutex
	path  string
	files map[string]Record
}

type ledgerDoc struct {
	Version int               `json:"version"`
	Files   map[string]Record `json:"files"`
}

// LoadLedger reads the ledger for one watch, or returns an empty one.
//
// EVERY FAILURE IS AN EMPTY LEDGER, not an error. A corrupt or unreadable
// cache must not stop a backup: the cost of ignoring it is one expensive
// sweep, and the cost of refusing to run is that somebody's files stop being
// copied because a JSON file got truncated.
func LoadLedger(stateDir, watchID string) *Ledger {
	l := &Ledger{path: ledgerPath(stateDir, watchID), files: map[string]Record{}}
	data, err := os.ReadFile(l.path)
	if err != nil {
		return l
	}
	var doc ledgerDoc
	if err := json.Unmarshal(data, &doc); err != nil || doc.Version != 1 {
		return l
	}
	if doc.Files != nil {
		l.files = doc.Files
	}
	return l
}

func ledgerPath(stateDir, watchID string) string {
	return filepath.Join(stateDir, "backup", watchID+".json")
}

// Get returns the record for a path, and whether one exists.
func (l *Ledger) Get(path string) (Record, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.files[path]
	return rec, ok
}

// Put records a path.
func (l *Ledger) Put(path string, rec Record) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.files[path] = rec
}

// Paths returns every path the ledger knows, so the verify lane can notice
// the ones that are no longer at the origin.
func (l *Ledger) Paths() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.files))
	for path := range l.files {
		out = append(out, path)
	}
	return out
}

// Forget drops a path.
//
// USED FOR NOTHING THE ORIGIN DOES. A file deleted at the origin stays in the
// ledger with its state reported as origin_gone, because forgetting it would
// make the next sweep unable to tell "gone" from "never seen" -- and the whole
// invariant of this feature is that a deletion at the origin FLAGS the copy
// rather than removing it. This exists for a watch being reconfigured.
func (l *Ledger) Forget(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.files, path)
}

// Save writes the ledger atomically.
func (l *Ledger) Save() error {
	l.mu.Lock()
	doc := ledgerDoc{Version: 1, Files: make(map[string]Record, len(l.files))}
	for k, v := range l.files {
		doc.Files[k] = v
	}
	path := l.path
	l.mu.Unlock()

	body, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("backup: encode ledger: %w", err)
	}
	return writeFileAtomic(path, body, ledgerFileMode)
}

// writeFileAtomic writes through a temp file in the SAME directory and
// renames, so a reader never sees a half-written ledger and a crash mid-write
// leaves the previous one intact. Same shape as appsession's own writer; not
// shared, because importing across those two packages for one helper would
// couple two subsystems that have nothing else to say to each other.
func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, ledgerDirMode); err != nil {
		return fmt.Errorf("backup: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".ledger-*")
	if err != nil {
		return fmt.Errorf("backup: temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("backup: write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("backup: close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("backup: chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("backup: rename into %s: %w", path, err)
	}
	return nil
}

// ErrNoLedgerDir is returned when a caller asks for ledgers with no state
// directory configured, which is a wiring mistake rather than a runtime one.
var ErrNoLedgerDir = errors.New("backup: no state directory configured")
