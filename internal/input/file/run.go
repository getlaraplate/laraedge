package file

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	edgeinput "github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
)

// Subdirectories of the watched directory a read file is moved into with
// after_read move (spec section 5).
const (
	doneDir  = "done"
	errorDir = "error"
)

// minCommitDelay is the shortest wait between the emit of a file and its
// commit, well past the buffer's group fsync (decision P7).
const minCommitDelay = time.Second

// Outcomes of a file, logged at info.
const (
	outcomeRead      = "read"
	outcomeDuplicate = "duplicate"
	outcomeRefused   = "refused"
)

// pendingFile is a file emitted and not yet committed.
type pendingFile struct {
	info fileInfo
	key  ledgerKey
	// keyed is false for a file refused before it was read: it has no hash
	// and is not written to the ledger.
	keyed bool
	// refused files go to error/ instead of done/.
	refused bool
	emitted time.Time
}

// rememberedFile is a committed file still in the watched directory: with
// keep, or because its move failed. sub is the directory it still has to be
// moved into, empty with keep.
type rememberedFile struct {
	info fileInfo
	sub  string
}

// scanner is the state of the scanning goroutine; only that goroutine uses
// it.
type scanner struct {
	in     *Input
	emit   func(model.Record)
	life   *edgeinput.Lifecycle
	watch  *watcher
	ledger *ledger

	// pending holds the files emitted and not yet committed, by name.
	pending map[string]pendingFile
	// remembered holds the committed files still listed, by name: they are
	// not read again while their size and modification time are unchanged.
	remembered map[string]rememberedFile
	// moveFailed holds the names whose failed move was counted and logged.
	moveFailed map[string]bool
}

func newScanner(in *Input, emit func(model.Record), life *edgeinput.Lifecycle, led *ledger) *scanner {
	return &scanner{
		in:         in,
		emit:       emit,
		life:       life,
		watch:      newWatcher(in.conn),
		ledger:     led,
		pending:    map[string]pendingFile{},
		remembered: map[string]rememberedFile{},
		moveFailed: map[string]bool{},
	}
}

// run scans the directory every interval until ctx is done.
func (in *Input) run(ctx context.Context, s *scanner) {
	for {
		timer := in.clk.NewTimer(in.conn.Interval)

		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C():
			s.scan(ctx)
		}
	}
}

// commitDelay is how long a file stays pending after its emit.
func (s *scanner) commitDelay() time.Duration {
	return max(s.in.conn.Interval, minCommitDelay)
}

// scan is one scan (spec section 5): list the directory, or count the
// failure and stop there; renew the devices; commit the files emitted long
// enough ago; retry the moves that failed; prune the ledger; read the files
// that became stable.
func (s *scanner) scan(ctx context.Context) {
	in := s.in
	now := in.clk.Now()

	files, err := in.deps.list(in.conn.Directory)
	if err != nil {
		in.health.CountError(KindListing)
		in.health.SetConnected(false)
		in.log.Debug("file: the directory cannot be listed", "input", in.cfg.ID, "error", err.Error())

		return
	}
	in.health.SetConnected(true)
	for _, device := range in.devices {
		s.life.Read(device)
	}

	moved := s.commit(now, files)
	s.revisit(files, moved)

	listed := make([]fileInfo, 0, len(files))
	present := make(map[string]bool, len(files))
	for _, f := range files {
		if moved[f.Name] {
			continue
		}
		listed = append(listed, f)
		present[f.Name] = true
	}
	s.prune(now, present)

	var candidates []fileInfo
	for _, f := range s.watch.Ready(now, listed) {
		if _, ok := s.pending[f.Name]; ok {
			continue
		}
		if _, ok := s.remembered[f.Name]; ok {
			continue
		}
		candidates = append(candidates, f)
		if len(candidates) == in.conn.MaxFilesPerScan {
			break
		}
	}

	for _, f := range candidates {
		if ctx.Err() != nil {
			return
		}
		s.handle(now, f)
	}
}

// commit writes the pending files emitted at least commitDelay ago to the
// ledger, then moves them into done/ or error/ (move) or remembers them
// (keep). A file no longer listed, or listed with another size or
// modification time, is only recorded: it is gone, or it changed after it
// was read and is a new candidate. It returns the names moved out of the
// directory.
func (s *scanner) commit(now time.Time, files []fileInfo) map[string]bool {
	var due []pendingFile
	for _, name := range slices.Sorted(maps.Keys(s.pending)) {
		p := s.pending[name]
		if now.Sub(p.emitted) >= s.commitDelay() {
			due = append(due, p)
		}
	}
	moved := map[string]bool{}
	if len(due) == 0 {
		return moved
	}

	keyed := false
	for _, p := range due {
		if p.keyed {
			s.ledger.Add(p.key, p.info.Name)
			keyed = true
		}
	}
	if keyed {
		s.save()
	}

	listed := make(map[string]fileInfo, len(files))
	for _, f := range files {
		listed[f.Name] = f
	}
	for _, p := range due {
		delete(s.pending, p.info.Name)
		if f, ok := listed[p.info.Name]; !ok || f.Size != p.info.Size || !f.ModTime.Equal(p.info.ModTime) {
			continue
		}
		sub := doneDir
		if p.refused {
			sub = errorDir
		}
		if s.settle(p.info, sub) {
			moved[p.info.Name] = true
		}
	}

	return moved
}

// settle disposes of a committed file: with move it is moved into sub, and
// remembered for a later try when the move fails; with keep it is
// remembered. It reports whether the file left the directory.
func (s *scanner) settle(info fileInfo, sub string) bool {
	if s.in.conn.AfterRead == AfterReadKeep {
		s.remembered[info.Name] = rememberedFile{info: info}

		return false
	}
	if s.move(info.Name, sub) {
		return true
	}
	s.remembered[info.Name] = rememberedFile{info: info, sub: sub}

	return false
}

// revisit forgets the remembered files no longer listed or changed since
// (a changed file is a new candidate) and tries again the moves that failed.
// The names it moves are added to moved.
func (s *scanner) revisit(files []fileInfo, moved map[string]bool) {
	listed := make(map[string]fileInfo, len(files))
	for _, f := range files {
		listed[f.Name] = f
	}
	for name := range s.moveFailed {
		if _, ok := listed[name]; !ok {
			delete(s.moveFailed, name)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(s.remembered)) {
		r := s.remembered[name]
		f, ok := listed[name]
		if !ok || f.Size != r.info.Size || !f.ModTime.Equal(r.info.ModTime) {
			delete(s.remembered, name)

			continue
		}
		if r.sub != "" && s.move(name, r.sub) {
			delete(s.remembered, name)
			moved[name] = true
		}
	}
}

// prune applies the retention to the ledger and saves it when an entry
// changed.
func (s *scanner) prune(now time.Time, present map[string]bool) {
	gone := make([]bool, len(s.ledger.entries))
	for i, e := range s.ledger.entries {
		gone[i] = e.GoneSince != nil
	}

	s.ledger.Prune(now, present, s.in.conn.SeenRetention)

	changed := len(gone) != len(s.ledger.entries)
	for i := 0; !changed && i < len(gone); i++ {
		changed = gone[i] != (s.ledger.entries[i].GoneSince != nil)
	}
	if changed {
		s.save()
	}
}

// save writes the ledger, logging a failure: the entries stay in memory and
// are written with the next change.
func (s *scanner) save() {
	if err := s.ledger.Save(); err != nil {
		s.in.log.Warn("file: the ledger of read files cannot be written", "input", s.in.cfg.ID, "error", err.Error())
	}
}

// handle reads one ready file: refused when too large, skipped on a read
// error (it is tried again at the next scan), a duplicate when its hash and
// modification time are in the ledger, otherwise parsed and emitted. Read
// and refused files become pending.
func (s *scanner) handle(now time.Time, f fileInfo) {
	in := s.in
	if f.Size > int64(in.conn.MaxFileSize) {
		s.refuse(now, f, KindFileTooLarge, nil)

		return
	}

	data, err := in.deps.readFile(filepath.Join(in.conn.Directory, f.Name))
	if err != nil {
		in.log.Debug("file: the file cannot be read, trying again at the next scan", "input", in.cfg.ID, "file", f.Name, "error", err.Error())

		return
	}
	if len(data) > in.conn.MaxFileSize {
		s.refuse(now, f, KindFileTooLarge, nil)

		return
	}

	key := ledgerKey{Hash: sha256.Sum256(data), ModTime: f.ModTime}
	if s.ledger.Has(key) {
		s.logFile(f.Name, 0, outcomeDuplicate)
		s.settle(f, doneDir)

		return
	}

	samples, kinds, err := parseCSV(in.plan, data, f.ModTime)
	if err != nil {
		s.refuse(now, f, KindMalformedFile, &key)

		return
	}
	for _, sample := range samples {
		s.emit(model.SampleRecord(sample))
	}
	for _, kind := range kinds {
		in.health.CountError(kind)
	}
	s.logFile(f.Name, len(samples), outcomeRead)
	s.pending[f.Name] = pendingFile{info: f, key: key, keyed: true, emitted: now}
}

// refuse counts a file refused as a whole and holds it pending, so it is
// committed like a read one (decision P8). key is nil for a file not read.
func (s *scanner) refuse(now time.Time, f fileInfo, kind string, key *ledgerKey) {
	s.in.health.CountError(kind)
	s.in.log.Info("file read", "input", s.in.cfg.ID, "file", f.Name, "samples", 0, "outcome", outcomeRefused, "kind", kind)

	p := pendingFile{info: f, refused: true, emitted: now}
	if key != nil {
		p.key, p.keyed = *key, true
	}
	s.pending[f.Name] = p
}

// logFile logs the outcome of a file at info, never its contents.
func (s *scanner) logFile(name string, samples int, outcome string) {
	s.in.log.Info("file read", "input", s.in.cfg.ID, "file", name, "samples", samples, "outcome", outcome)
}

// move renames the file name of the watched directory into its subdirectory
// sub, created on first use, under a free name. A failure is counted and
// logged once per file name.
func (s *scanner) move(name, sub string) bool {
	in := s.in
	dir := filepath.Join(in.conn.Directory, sub)

	err := os.MkdirAll(dir, 0o755)
	var target string
	if err == nil {
		target, err = freeName(dir, name)
	}
	if err == nil {
		err = in.deps.rename(filepath.Join(in.conn.Directory, name), target)
	}
	if err != nil {
		if !s.moveFailed[name] {
			s.moveFailed[name] = true
			in.health.CountError(KindMoveFailed)
			in.log.Warn("file: the file cannot be moved after reading; it will not be read again", "input", in.cfg.ID, "file", name, "into", sub, "error", err.Error())
		}

		return false
	}
	delete(s.moveFailed, name)

	return true
}

// maxSuffix bounds the search for a free name.
const maxSuffix = 100000

// errNoFreeName is a directory holding every suffixed name up to maxSuffix.
var errNoFreeName = errors.New("file: no free name in the target directory")

// freeName returns the path in dir under which name can be stored without
// replacing a file: name itself, else name with -1, -2, ... before the
// extension. os.Rename onto an existing name replaces it on Unix and fails
// on Windows, so the collision is resolved before the rename.
func freeName(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for n := 0; n <= maxSuffix; n++ {
		candidate := name
		if n > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, n, ext)
		}
		path := filepath.Join(dir, candidate)
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
	}

	return "", errNoFreeName
}
