package provider_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/provider"
	"github.com/getlaraplate/laraedge/internal/supervisor"
)

type fileFixture struct {
	t       *testing.T
	clk     *clock.Fake
	path    string
	applier *fakeApplier
	reload  chan struct{}
	logs    *lockedBuffer
	mtime   time.Time
}

func newFileFixture(t *testing.T, content string) *fileFixture {
	t.Helper()

	f := &fileFixture{
		t:       t,
		clk:     clock.NewFake(start),
		path:    filepath.Join(t.TempDir(), "config.json"),
		applier: newFakeApplier(),
		reload:  make(chan struct{}, 1),
		logs:    &lockedBuffer{},
		mtime:   start,
	}
	f.write(content)

	p, err := provider.NewFile(provider.FileConfig{
		Path:    f.path,
		Applier: f.applier,
		Reload:  f.reload,
		Clock:   f.clk,
		Logger:  slog.New(slog.NewJSONHandler(f.logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewFile() = %v", err)
	}
	run(t, p.Run)

	return f
}

// write replaces the file and moves its modification time forward.
func (f *fileFixture) write(content string) {
	f.t.Helper()

	if err := os.WriteFile(f.path, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.mtime = f.mtime.Add(time.Second)
	if err := os.Chtimes(f.path, f.mtime, f.mtime); err != nil {
		f.t.Fatal(err)
	}
}

// poll fires the next poll and waits until the provider waits again.
func (f *fileFixture) poll() {
	f.t.Helper()

	waitTimers(f.t, f.clk, 1)
	f.clk.Advance(provider.DefaultFileInterval)
	waitTimers(f.t, f.clk, 1)
}

func TestFileAppliesOnStartAndOnChange(t *testing.T) {
	f := newFileFixture(t, doc(1))

	if got := f.applier.next(t); got.Version != 1 {
		t.Fatalf("applied version %d on start, want 1", got.Version)
	}

	f.poll()
	if n := f.applier.count(); n != 1 {
		t.Fatalf("an unchanged file was applied again (%d applies)", n)
	}

	f.write(doc(2))
	waitTimers(t, f.clk, 1)
	f.clk.Advance(provider.DefaultFileInterval - time.Nanosecond)
	if n := f.applier.count(); n != 1 {
		t.Fatalf("the change was applied before the poll interval (%d applies)", n)
	}
	f.clk.Advance(time.Nanosecond)
	if got := f.applier.next(t); got.Version != 2 {
		t.Fatalf("applied version %d after the change, want 2", got.Version)
	}
}

func TestFileReloadSignalAppliesAgain(t *testing.T) {
	f := newFileFixture(t, doc(4))
	f.applier.next(t)

	f.reload <- struct{}{}

	if got := f.applier.next(t); got.Version != 4 {
		t.Fatalf("applied version %d on reload, want the file again (4)", got.Version)
	}
}

func TestFileRefusedDocumentsAreLoggedAndNotApplied(t *testing.T) {
	f := newFileFixture(t, doc(1))
	f.applier.next(t)

	f.write(`{"schema":"laraplate-edge-config/1","version":2,"inputs":[],"outputs":[],"bogus":"secret-value-77"}`)
	f.poll()

	if n := f.applier.count(); n != 1 {
		t.Fatalf("an invalid document was applied (%d applies)", n)
	}
	if !f.logs.has(`"level":"WARN"`, "bogus") {
		t.Fatalf("no warning naming the problem in %s", f.logs.String())
	}
	if f.logs.has("secret-value-77") {
		t.Fatalf("the logs repeat a document value: %s", f.logs.String())
	}

	f.applier.setOutcome(supervisor.Outcome{Result: supervisor.ResultRejected, Errors: []string{"version: 1 is older than the applied version 2"}})
	f.write(doc(1) + " ")
	f.poll()
	f.applier.next(t)
	if !f.logs.has(`"level":"WARN"`, "rejected", "older than the applied version") {
		t.Fatalf("no warning with the rejection messages in %s", f.logs.String())
	}

	f.applier.setOutcome(supervisor.Outcome{Result: supervisor.ResultApplied})
	f.write(doc(3))
	f.poll()
	if got := f.applier.next(t); got.Version != 3 {
		t.Fatalf("applied version %d after the fix, want 3", got.Version)
	}
}

func TestFileMissingFileIsLogged(t *testing.T) {
	f := newFileFixture(t, doc(1))
	f.applier.next(t)

	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	f.poll()

	if !f.logs.has(`"level":"WARN"`, "config.json") {
		t.Fatalf("no warning about the missing file in %s", f.logs.String())
	}

	f.write(doc(2))
	f.poll()
	if got := f.applier.next(t); got.Version != 2 {
		t.Fatalf("applied version %d once the file is back, want 2", got.Version)
	}
}

func TestApplyLastValid(t *testing.T) {
	dir := t.TempDir()
	store := config.NewStore(dir)
	a := newFakeApplier()
	logs := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))

	provider.ApplyLastValid(t.Context(), store, a, log)
	if n := a.count(); n != 0 {
		t.Fatalf("applied %d documents without a last valid one", n)
	}

	saved, msgs, err := config.Parse([]byte(doc(7)))
	if err != nil || len(msgs) > 0 {
		t.Fatal(err, msgs)
	}
	if err := store.SaveValid(saved); err != nil {
		t.Fatal(err)
	}

	provider.ApplyLastValid(t.Context(), store, a, log)
	if got := a.next(t); got.Version != 7 {
		t.Fatalf("applied version %d, want the last valid 7", got.Version)
	}
}

// TestFileAttemptedIsCalledOnceAfterTheFirstLoad covers the readiness hook
// (systemd READY=1): it follows the first load whatever its outcome, here a
// missing file, and never again.
func TestFileAttemptedIsCalledOnceAfterTheFirstLoad(t *testing.T) {
	clk := clock.NewFake(start)
	path := filepath.Join(t.TempDir(), "missing.json")
	var hook attempts
	p, err := provider.NewFile(provider.FileConfig{
		Path:      path,
		Applier:   newFakeApplier(),
		Clock:     clk,
		Logger:    slog.New(slog.DiscardHandler),
		Attempted: hook.hook,
	})
	if err != nil {
		t.Fatal(err)
	}
	run(t, p.Run)

	waitTimers(t, clk, 1)
	if got := hook.count(); got != 1 {
		t.Fatalf("Attempted called %d times after the first load, want 1", got)
	}

	if err := os.WriteFile(path, []byte(doc(1)), 0o600); err != nil {
		t.Fatal(err)
	}
	clk.Advance(provider.DefaultFileInterval)
	waitTimers(t, clk, 1)
	if got := hook.count(); got != 1 {
		t.Fatalf("Attempted called %d times after a later load, want 1", got)
	}
}

// TestFileRunAfterShutdownAppliesNothing: a SIGTERM during ApplyLastValid
// cancels ctx before Run starts; Run returns without a full Apply under a
// cancelled context.
func TestFileRunAfterShutdownAppliesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(doc(1)), 0o600); err != nil {
		t.Fatal(err)
	}

	a := newFakeApplier()
	var hook attempts
	p, err := provider.NewFile(provider.FileConfig{
		Path:      path,
		Applier:   a,
		Clock:     clock.NewFake(start),
		Logger:    slog.New(slog.DiscardHandler),
		Attempted: hook.hook,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	if n := a.count(); n != 0 {
		t.Fatalf("Run() under a cancelled context applied %d documents, want 0", n)
	}
}
