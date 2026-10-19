package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/swolley/laraplate-edge/internal/atomicfile"
)

// lastValidName is the file holding the last valid document.
const lastValidName = "last-valid.json"

// Store keeps the last valid document in data_dir/config, so the agent can
// roll back to it and start from it after a restart.
type Store struct {
	dir string
}

// NewStore returns the store of dataDir. Nothing is created until SaveValid.
func NewStore(dataDir string) *Store {
	return &Store{dir: filepath.Join(dataDir, "config")}
}

// SaveValid replaces the last valid document with doc, atomically: after a
// crash the file holds the old or the new document. doc must come from Parse.
func (s *Store) SaveValid(doc Document) error {
	if len(doc.Raw) == 0 {
		return errors.New("config: save last valid document: the document was not produced by Parse")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("config: save last valid document: %w", err)
	}
	if err := atomicfile.Write(s.dir, lastValidName, doc.Raw); err != nil {
		return fmt.Errorf("config: save last valid document: %w", err)
	}
	return nil
}

// LastValid returns the last valid document, or false when none was saved.
// A file that no longer parses is an error.
func (s *Store) LastValid() (Document, bool, error) {
	file := filepath.Join(s.dir, lastValidName)
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return Document{}, false, nil
	}
	if err != nil {
		return Document{}, false, fmt.Errorf("config: read last valid document: %w", err)
	}
	doc, msgs, err := Parse(data)
	if err != nil {
		return Document{}, false, fmt.Errorf("config: read last valid document %s: %w", file, err)
	}
	if len(msgs) > 0 {
		return Document{}, false, fmt.Errorf("config: last valid document %s is corrupt: %s", file, strings.Join(msgs, "; "))
	}
	return doc, true, nil
}
