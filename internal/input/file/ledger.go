package file

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/getlaraplate/laraedge/internal/atomicfile"
)

// The ledger of read files is data_dir/file/seen/{input_id}.json. Like the
// mqtt subscription set it stays outside data_dir/inputs/{input_id}, which
// the supervisor deletes when an input leaves the document.
const (
	ledgerSubdir    = "file/seen"
	ledgerExtension = ".json"
	ledgerVersion   = 1
)

// errNoDataDir is the reason the ledger is not kept.
var errNoDataDir = errors.New("file: data_dir unknown, the ledger of read files is not kept")

// ledgerKey identifies a read file: the hash of its content and its
// modification time. Modification times compare as instants, so the zone, the
// monotonic reading and the rounding of the file system do not matter once
// the value is stored.
type ledgerKey struct {
	Hash    [32]byte
	ModTime time.Time
}

// ledgerEntry is one stored key with the name it was read under.
type ledgerEntry struct {
	Hash      string     `json:"hash"`
	ModTime   time.Time  `json:"mtime"`
	Name      string     `json:"name"`
	GoneSince *time.Time `json:"gone_since,omitempty"`
}

// ledgerFile is the stored document.
type ledgerFile struct {
	Version int           `json:"version"`
	Entries []ledgerEntry `json:"entries"`
}

// ledger remembers the files already read, so a file kept in place is not
// read again (spec section 5).
type ledger struct {
	dir     string
	name    string
	entries []ledgerEntry
}

// ledgerPath is the directory and file name of the ledger of input id.
func ledgerPath(dataDir, id string) (string, string) {
	return filepath.Join(dataDir, filepath.FromSlash(ledgerSubdir)), id + ledgerExtension
}

// loadLedger reads the ledger of the input. A missing file gives an empty
// ledger. A file that does not parse is renamed
// {input_id}.json.corrupt-<unix seconds>, logged at warn, and an empty ledger
// is returned (plan decision P6).
func loadLedger(dataDir, inputID string, log *slog.Logger) (*ledger, error) {
	if dataDir == "" {
		return nil, errNoDataDir
	}

	dir, name := ledgerPath(dataDir, inputID)
	l := &ledger{dir: dir, name: name}
	file := filepath.Join(dir, name)

	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}

	var doc ledgerFile
	if err := json.Unmarshal(data, &doc); err != nil || doc.Version != ledgerVersion || !validEntries(doc.Entries) {
		moved := fmt.Sprintf("%s.corrupt-%d", file, time.Now().Unix())
		if err := os.Rename(file, moved); err != nil {
			return nil, err
		}
		log.Warn("ledger of read files is corrupt, starting with an empty one",
			"input_id", inputID, "kept_as", filepath.Base(moved))

		return l, nil
	}
	l.entries = doc.Entries

	return l, nil
}

// validEntries reports whether every stored hash is a SHA-256 in hex.
func validEntries(entries []ledgerEntry) bool {
	for _, e := range entries {
		if b, err := hex.DecodeString(e.Hash); err != nil || len(b) != 32 {
			return false
		}
	}

	return true
}

// index returns the position of the key, or -1.
func (l *ledger) index(k ledgerKey) int {
	h := hex.EncodeToString(k.Hash[:])

	return slices.IndexFunc(l.entries, func(e ledgerEntry) bool {
		return e.Hash == h && e.ModTime.Equal(k.ModTime)
	})
}

// Has reports whether the key was read.
func (l *ledger) Has(k ledgerKey) bool {
	return l.index(k) >= 0
}

// Add records the key under the file name it was read as.
func (l *ledger) Add(k ledgerKey, name string) {
	if i := l.index(k); i >= 0 {
		l.entries[i].Name = name
		l.entries[i].GoneSince = nil

		return
	}
	l.entries = append(l.entries, ledgerEntry{
		Hash:    hex.EncodeToString(k.Hash[:]),
		ModTime: k.ModTime.UTC().Round(0),
		Name:    name,
	})
}

// Prune keeps every entry whose name is still present, however old, marks the
// others gone since now, and drops those gone for longer than retention.
func (l *ledger) Prune(now time.Time, present map[string]bool, retention time.Duration) {
	now = now.UTC().Round(0)
	kept := l.entries[:0]
	for _, e := range l.entries {
		switch {
		case present[e.Name]:
			e.GoneSince = nil
		case e.GoneSince == nil:
			since := now
			e.GoneSince = &since
		case now.Sub(*e.GoneSince) > retention:
			continue
		}
		kept = append(kept, e)
	}
	l.entries = kept
}

// Save writes the ledger atomically, creating its directory.
func (l *ledger) Save() error {
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}

	entries := l.entries
	if entries == nil {
		entries = []ledgerEntry{}
	}
	data, err := json.Marshal(ledgerFile{Version: ledgerVersion, Entries: entries})
	if err != nil {
		return err
	}

	return atomicfile.Write(l.dir, l.name, data)
}
