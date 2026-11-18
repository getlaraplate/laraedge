package file

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

// pendingReport is a report emitted and not yet committed.
type pendingReport struct {
	report fileReport
	key    ledgerKey
	// keyed is false for a report refused before it was read: it has no
	// hash and is not written to the ledger.
	keyed bool
	// refused reports go to error/ instead of done/.
	refused bool
	emitted time.Time
}

// rememberedReport is a committed report still in the watched directory:
// with keep, or because a move failed. files are its files still listed,
// with the size and modification time they were read with. sub is the
// directory they still have to be moved into, empty with keep. targets are
// the paths in sub chosen for files, set only once another file of the
// report was moved, so that a pair keeps one suffix.
type rememberedReport struct {
	files   []fileInfo
	sub     string
	targets []string
}

// scanner is the state of the scanning goroutine; only that goroutine uses
// it.
type scanner struct {
	in     *Input
	emit   func(model.Record)
	life   *edgeinput.Lifecycle
	watch  *watcher
	ledger *ledger

	// pending holds the reports emitted and not yet committed, by name.
	pending map[string]pendingReport
	// remembered holds the committed reports still listed, by name: they
	// are not read again while the size and modification time of each of
	// their files are unchanged.
	remembered map[string]rememberedReport
	// moveFailed holds the report names whose failed move was counted and
	// logged.
	moveFailed map[string]bool
}

func newScanner(in *Input, emit func(model.Record), life *edgeinput.Lifecycle, led *ledger) *scanner {
	return &scanner{
		in:         in,
		emit:       emit,
		life:       life,
		watch:      newWatcher(in.conn),
		ledger:     led,
		pending:    map[string]pendingReport{},
		remembered: map[string]rememberedReport{},
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

// commitDelay is how long a report stays pending after its emit.
func (s *scanner) commitDelay() time.Duration {
	return max(s.in.conn.Interval, minCommitDelay)
}

// scan is one scan (spec section 5): list the directory, or count the
// failure and stop there; renew the devices; commit the reports emitted long
// enough ago; retry the moves that failed; prune the ledger; read the
// reports whose files became stable.
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
	present := make(map[string]time.Time, len(files))
	for _, f := range files {
		if moved[f.Name] {
			continue
		}
		listed = append(listed, f)
		present[f.Name] = f.ModTime
	}
	s.prune(now, present)

	held := s.held()
	reports, _ := reportsOf(in.conn, s.watch.Ready(now, listed), listed)
	var candidates []fileReport
	for _, r := range reports {
		if slices.ContainsFunc(r.files(), func(f fileInfo) bool { return held[f.Name] }) {
			continue
		}
		candidates = append(candidates, r)
		if len(candidates) == in.conn.MaxFilesPerScan {
			break
		}
	}

	for _, r := range candidates {
		if ctx.Err() != nil {
			return
		}
		s.handle(r)
	}
}

// held returns the names of the files of the pending and remembered
// reports: a report holding one of them is not a candidate, so a .dfd is
// never read with another .dfx while its own pair is pending or remembered.
func (s *scanner) held() map[string]bool {
	held := map[string]bool{}
	for _, p := range s.pending {
		for _, f := range p.report.files() {
			held[f.Name] = true
		}
	}
	for _, r := range s.remembered {
		for _, f := range r.files {
			held[f.Name] = true
		}
	}

	return held
}

// unchanged reports whether every file is listed with its size and
// modification time.
func unchanged(listed map[string]fileInfo, files []fileInfo) bool {
	for _, f := range files {
		l, ok := listed[f.Name]
		if !ok || l.Size != f.Size || !l.ModTime.Equal(f.ModTime) {
			return false
		}
	}

	return true
}

// commit writes the pending reports emitted at least commitDelay ago to the
// ledger, then moves them into done/ or error/ (move) or remembers them
// (keep). A report with a file no longer listed, or listed with another size
// or modification time, is only recorded: it is gone, or it changed after it
// was read and is a new candidate. It returns the names of the files moved
// out of the directory.
func (s *scanner) commit(now time.Time, files []fileInfo) map[string]bool {
	var due []pendingReport
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
			s.ledger.Add(p.key, p.report.Name())
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
		delete(s.pending, p.report.Name())
		if !unchanged(listed, p.report.files()) {
			continue
		}
		sub := doneDir
		if p.refused {
			sub = errorDir
		}
		for _, name := range s.settle(p.report, sub) {
			moved[name] = true
		}
	}

	return moved
}

// settle disposes of a committed report: with move its files are moved into
// sub, and the ones whose move failed are remembered for a later try; with
// keep it is remembered. It returns the names of the files that left the
// directory.
func (s *scanner) settle(r fileReport, sub string) []string {
	if s.in.conn.AfterRead == AfterReadKeep {
		s.remembered[r.Name()] = rememberedReport{files: r.files()}

		return nil
	}
	left, targets, moved := s.move(r.Name(), r.files(), nil, sub)
	if len(left) > 0 {
		s.remembered[r.Name()] = rememberedReport{files: left, sub: sub, targets: targets}
	}

	return moved
}

// revisit forgets the remembered reports with a file no longer listed or
// changed since (a changed report is a new candidate) and tries again the
// moves that failed. The names it moves are added to moved.
func (s *scanner) revisit(files []fileInfo, moved map[string]bool) {
	listed := make(map[string]fileInfo, len(files))
	for _, f := range files {
		listed[f.Name] = f
	}

	for _, name := range slices.Sorted(maps.Keys(s.remembered)) {
		r := s.remembered[name]
		if !unchanged(listed, r.files) {
			delete(s.remembered, name)

			continue
		}
		if r.sub == "" {
			continue
		}
		left, targets, done := s.move(name, r.files, r.targets, r.sub)
		for _, n := range done {
			moved[n] = true
		}
		if len(left) == 0 {
			delete(s.remembered, name)

			continue
		}
		r.files, r.targets = left, targets
		s.remembered[name] = r
	}

	for name := range s.moveFailed {
		_, isListed := listed[name]
		_, isRemembered := s.remembered[name]
		if !isListed && !isRemembered {
			delete(s.moveFailed, name)
		}
	}
}

// prune applies the retention to the ledger and saves it when an entry
// changed.
func (s *scanner) prune(now time.Time, present map[string]time.Time) {
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

// handle reads one report whose files are ready: refused when a file is too
// large, skipped on a read error (it is tried again at the next scan), a
// duplicate when its hash and modification time are in the ledger, otherwise
// parsed and emitted. Read and refused reports become pending.
func (s *scanner) handle(r fileReport) {
	in := s.in
	data, key, tooLarge, err := readReport(in.deps, in.conn.Directory, r, in.conn.MaxFileSize)
	if tooLarge {
		s.refuse(r, KindFileTooLarge, nil)

		return
	}
	if err != nil {
		in.log.Debug("file: the file cannot be read, trying again at the next scan", "input", in.cfg.ID, "file", r.Name(), "error", err.Error())

		return
	}

	if s.ledger.Has(key) {
		s.logFile(r.Name(), 0, outcomeDuplicate)
		s.settle(r, doneDir)

		return
	}

	samples, kinds, err := parseReport(in.plan, in.conn.Format, data, r.Primary.ModTime)
	if err != nil {
		s.refuse(r, KindMalformedFile, &key)

		return
	}
	for _, sample := range samples {
		s.emit(model.SampleRecord(sample))
	}
	for _, kind := range kinds {
		in.health.CountError(kind)
	}
	s.logFile(r.Name(), len(samples), outcomeRead)
	s.pending[r.Name()] = pendingReport{report: r, key: key, keyed: true, emitted: in.clk.Now()}
}

// refuse counts a report refused as a whole and holds it pending, so it is
// committed like a read one (decision P8). key is nil for a report not read.
func (s *scanner) refuse(r fileReport, kind string, key *ledgerKey) {
	s.in.health.CountError(kind)
	s.in.log.Info("file read", "input", s.in.cfg.ID, "file", r.Name(), "samples", 0, "outcome", outcomeRefused, "kind", kind)

	p := pendingReport{report: r, refused: true, emitted: s.in.clk.Now()}
	if key != nil {
		p.key, p.keyed = *key, true
	}
	s.pending[r.Name()] = p
}

// logFile logs the outcome of a report at info, never its contents.
func (s *scanner) logFile(name string, samples int, outcome string) {
	s.in.log.Info("file read", "input", s.in.cfg.ID, "file", name, "samples", samples, "outcome", outcome)
}

// move renames the files of the report name from the watched directory into
// its subdirectory sub, created on first use. targets are the paths chosen
// for files by an earlier try that moved another file of the report, nil
// otherwise: then one suffix free for every file is chosen, so a pair keeps
// matching names (decision Q8). A chosen path taken since gets a free name of
// its own rather than being replaced. It returns the files left in the
// directory with their targets, and the names it moved. A failure is counted
// and logged once per report name.
func (s *scanner) move(name string, files []fileInfo, targets []string, sub string) (left []fileInfo, leftTargets []string, moved []string) {
	in := s.in
	dir := filepath.Join(in.conn.Directory, sub)
	previous := targets != nil

	err := os.MkdirAll(dir, 0o755)
	if err == nil && !previous {
		names := make([]string, len(files))
		for i, f := range files {
			names[i] = f.Name
		}
		targets, err = freeNames(dir, names)
	}
	if err != nil {
		s.moveFailure(name, files[0].Name, sub, err)
		if !previous {
			targets = nil
		}

		return files, targets, nil
	}

	var failed error
	failedName := ""
	for i, f := range files {
		target, err := retarget(dir, f.Name, targets[i], previous)
		if err == nil {
			err = in.deps.rename(filepath.Join(in.conn.Directory, f.Name), target)
		}
		if err != nil {
			left = append(left, f)
			leftTargets = append(leftTargets, target)
			if failed == nil {
				failed, failedName = err, f.Name
			}

			continue
		}
		moved = append(moved, f.Name)
	}
	if failed == nil {
		delete(s.moveFailed, name)

		return nil, nil, moved
	}
	s.moveFailure(name, failedName, sub, failed)
	if !previous && len(moved) == 0 {
		leftTargets = nil
	}

	return left, leftTargets, moved
}

// retarget returns the path a file is moved to: target, or, when target was
// chosen by an earlier try (previous) and is taken since, a free name of its
// own.
func retarget(dir, name, target string, previous bool) (string, error) {
	if !previous {
		return target, nil
	}
	_, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return target, nil
	}
	if err != nil {
		return target, err
	}

	return freeName(dir, name)
}

// moveFailure counts and logs a failed move of the report name once, naming
// the file that could not be moved.
func (s *scanner) moveFailure(name, file, sub string, err error) {
	if s.moveFailed[name] {
		return
	}
	s.moveFailed[name] = true
	s.in.health.CountError(KindMoveFailed)
	s.in.log.Warn("file: the file cannot be moved after reading; it will not be read again", "input", s.in.cfg.ID, "file", file, "into", sub, "error", err.Error())
}
