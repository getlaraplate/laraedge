package transform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/getlaraplate/laraedge/internal/atomicfile"
	"github.com/getlaraplate/laraedge/internal/clock"
)

// stateFileName is the file of an input's persisted step state, in
// data_dir/inputs/{input_id}.
const stateFileName = "transform-state.json"

// stateVersion is the version of the state file this agent writes and reads.
const stateVersion = 1

// stateDocument is the content of the state file. A counter's key is
// "<device>/<signal>/<position>/<sha256 hex of the step's canonical JSON>"
// (P2): a changed step gets a new key and starts clean.
type stateDocument struct {
	Version  int              `json:"version"`
	Counters map[string]int64 `json:"counters"`
}

// stateFile holds the counters of one input, loaded once and written whole,
// atomically, on every Save.
type stateFile struct {
	dir string

	mu       sync.Mutex
	counters map[string]int64
	// requested holds the key of every Store the factory returned.
	requested map[string]struct{}
	// pruned is set when prune dropped a loaded key not yet written away.
	pruned bool
}

// loadStateFile reads the state file in dir. A file that cannot be read or
// parsed, or of another version, is renamed to
// transform-state.json.corrupt-<unix seconds>, logged at ERROR with the input
// id and never its content, and the counters start empty (P3).
func loadStateFile(dir, inputID string, clk clock.Clock, log *slog.Logger) *stateFile {
	s := &stateFile{dir: dir, counters: map[string]int64{}, requested: map[string]struct{}{}}
	path := filepath.Join(dir, stateFileName)

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s
	}

	if err == nil {
		if counters, ok := parseState(data); ok {
			s.counters = counters

			return s
		}
	}

	aside := fmt.Sprintf("%s.corrupt-%d", stateFileName, clk.Now().Unix())
	if renameErr := os.Rename(path, filepath.Join(dir, aside)); renameErr != nil {
		log.Error("transform: the state file cannot be read nor renamed aside; edge counters start from 0",
			"input", inputID, "error", renameErr)

		return s
	}

	log.Error("transform: the state file cannot be read and was renamed aside; edge counters start from 0",
		"input", inputID, "file", aside)

	return s
}

// parseState decodes a state file of the current version. A negative count
// cannot have been written and makes the file corrupt.
func parseState(data []byte) (map[string]int64, bool) {
	var doc stateDocument
	if err := json.Unmarshal(data, &doc); err != nil || doc.Version != stateVersion {
		return nil, false
	}

	for _, v := range doc.Counters {
		if v < 0 {
			return nil, false
		}
	}

	if doc.Counters == nil {
		doc.Counters = map[string]int64{}
	}

	return doc.Counters, true
}

// factory returns the StateFactory of one point. Its stores are lazy: a key
// enters the file only on its first Save, so a chain compiled and then
// refused, or replaced before it counted, leaves no entry. Every key handed
// out is recorded for prune.
func (s *stateFile) factory(device, signal string) StateFactory {
	return func(position int, canonical json.RawMessage) Store {
		sum := sha256.Sum256(canonical)
		key := fmt.Sprintf("%s/%s/%d/%s", device, signal, position, hex.EncodeToString(sum[:]))

		s.mu.Lock()
		s.requested[key] = struct{}{}
		s.mu.Unlock()

		return &counterStore{file: s, key: key}
	}
}

// prune drops every counter whose key no compiled chain asked for (ruling
// R11): the count of a changed or removed step must not come back when the
// document is reverted, because a resumed higher count would read as parts
// produced. It runs once all chains are compiled, before any Save.
func (s *stateFile) prune() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key := range s.counters {
		if _, ok := s.requested[key]; !ok {
			delete(s.counters, key)

			s.pruned = true
		}
	}
}

// persistPruned writes the file when prune dropped keys, so that a revert
// after a restart, before any Save, starts clean too.
func (s *stateFile) persistPruned() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.pruned {
		return nil
	}

	if err := s.writeLocked(); err != nil {
		return err
	}

	s.pruned = false

	return nil
}

func (s *stateFile) load(key string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v, ok := s.counters[key]

	return v, ok
}

// save sets the counter and writes the whole file atomically: directory
// 0700, file 0600. On a failed write the counter stays in memory, so the next
// successful Save writes it.
func (s *stateFile) save(key string, v int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.counters[key] = v

	if err := s.writeLocked(); err != nil {
		return err
	}

	s.pruned = false

	return nil
}

// writeLocked writes the whole file. The caller holds s.mu.
func (s *stateFile) writeLocked() error {
	data, err := json.Marshal(stateDocument{Version: stateVersion, Counters: s.counters})
	if err != nil {
		return fmt.Errorf("transform: encode state: %w", err)
	}

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("transform: save state: %w", err)
	}

	if err := atomicfile.Write(s.dir, stateFileName, data); err != nil {
		return fmt.Errorf("transform: save state: %w", err)
	}

	return nil
}

// RemoveStaleInputStates removes the state directory of every input that
// keep refuses: each directory directly in dataDir/inputs, named after an
// input id, with everything in it (ruling R15). Nothing else is touched: a
// file, a symbolic link or anything outside dataDir/inputs stays. It returns
// the ids removed, in name order; a missing dataDir/inputs is not an error.
// A directory that cannot be removed is reported in the error and the others
// are still removed. The caller makes sure no processor of a removed input
// is running.
func RemoveStaleInputStates(dataDir string, keep func(inputID string) bool) ([]string, error) {
	root := filepath.Join(dataDir, "inputs")

	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("transform: list input states: %w", err)
	}

	var (
		removed []string
		errs    []error
	)

	for _, e := range entries {
		if !e.IsDir() || keep(e.Name()) {
			continue
		}

		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			errs = append(errs, fmt.Errorf("transform: remove input state: %w", err))

			continue
		}

		removed = append(removed, e.Name())
	}

	return removed, errors.Join(errs...)
}

// counterStore is the Store of one step, backed by the input's state file.
type counterStore struct {
	file *stateFile
	key  string
}

// Load implements Store.
func (c *counterStore) Load() (int64, bool) { return c.file.load(c.key) }

// Save implements Store.
func (c *counterStore) Save(v int64) error { return c.file.save(c.key, v) }
