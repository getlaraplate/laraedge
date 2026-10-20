package output_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/getlaraplate/laraplate-edge/internal/buffer"
	"github.com/getlaraplate/laraplate-edge/internal/model"
	"github.com/getlaraplate/laraplate-edge/internal/output"
)

// storedBatch returns a valid batch of seq with one critical and two bulk
// records and one loss marker.
func storedBatch(seq uint64) *output.Batch {
	return &output.Batch{
		MessageID: "019a0000-0000-7000-8000-000000000001",
		Seq:       seq,
		Body:      []byte(`{"protocol":"laraplate-machine/1"}`),
		Entries: map[model.Priority][]buffer.Entry{
			model.Critical: {{Pos: 0, Next: 90, Record: sample(model.Critical, "state", 10)}},
			model.Bulk: {
				{Pos: 100, Next: 190, Record: sample(model.Bulk, "temp", 11)},
				{Pos: 190, Next: 280, Record: sample(model.Bulk, "temp", 12)},
			},
		},
		Positions: map[model.Priority]buffer.Position{model.Critical: 90, model.Bulk: 280},
		Losses:    []model.Loss{bufferFull(model.Bulk, 3, 5, 2)},
	}
}

func loadInflight(t *testing.T, dir string) (*output.Inflight, *output.Batch, uint64) {
	t.Helper()

	inf := output.NewInflight(dir)
	b, next, err := inf.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	return inf, b, next
}

func TestInflightLifecycle(t *testing.T) {
	dir := t.TempDir()

	inf, b, next := loadInflight(t, dir)
	if b != nil || next != 0 {
		t.Fatalf("Load() of a new output = %v, %d; want nil, 0", b, next)
	}

	want := storedBatch(0)
	if err := inf.Save(want); err != nil {
		t.Fatalf("Save(seq 0) = %v", err)
	}

	_, got, next := loadInflight(t, dir)
	if next != 1 {
		t.Fatalf("next_seq after Save(seq 0) = %d, want 1", next)
	}

	assertSameBatch(t, got, want)

	if err := inf.Clear(); err != nil {
		t.Fatalf("Clear() = %v", err)
	}

	inf, got, next = loadInflight(t, dir)
	if got != nil || next != 1 {
		t.Fatalf("after Clear: batch %v, next_seq %d; want nil, 1", got, next)
	}

	refused := storedBatch(1)
	if err := inf.Save(refused); err != nil {
		t.Fatalf("Save(seq 1) = %v", err)
	}

	if err := inf.ClearRefused(refused); err != nil {
		t.Fatalf("ClearRefused(seq 1) = %v", err)
	}

	_, got, next = loadInflight(t, dir)
	if got != nil || next != 1 {
		t.Fatalf("after ClearRefused(seq 1): batch %v, next_seq %d; want nil, 1 (the refused seq is reused)", got, next)
	}
}

func TestInflightRefusesSeqThatGoesBack(t *testing.T) {
	tests := []struct {
		name  string
		setup []uint64 // seqs saved and cleared in order
		save  uint64
		ok    bool
	}{
		{name: "first batch takes seq 0", save: 0, ok: true},
		{name: "first batch cannot skip", save: 1, ok: false},
		{name: "after a confirmed batch the next seq", setup: []uint64{0}, save: 1, ok: true},
		{name: "after a confirmed batch its seq is spent", setup: []uint64{0}, save: 0, ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inf, _, _ := loadInflight(t, t.TempDir())
			for _, seq := range tc.setup {
				if err := inf.Save(storedBatch(seq)); err != nil {
					t.Fatalf("Save(%d) = %v", seq, err)
				}

				if err := inf.Clear(); err != nil {
					t.Fatalf("Clear() = %v", err)
				}
			}

			err := inf.Save(storedBatch(tc.save))
			if (err == nil) != tc.ok {
				t.Fatalf("Save(seq %d) = %v, want ok %v", tc.save, err, tc.ok)
			}
		})
	}
}

func TestInflightStoredBatchBlocksANewSeq(t *testing.T) {
	dir := t.TempDir()
	inf, _, _ := loadInflight(t, dir)
	if err := inf.Save(storedBatch(0)); err != nil {
		t.Fatalf("Save(seq 0) = %v", err)
	}

	if err := inf.Save(storedBatch(1)); err == nil {
		t.Fatal("Save(seq 1) over the unconfirmed batch seq 0 = nil, want an error")
	}

	_, stored, next := loadInflight(t, dir)
	if stored == nil || stored.Seq != 0 || next != 1 {
		t.Fatalf("stored batch %v next_seq %d, want seq 0 kept and next_seq 1", stored, next)
	}
}

func TestInflightShrunkBatchKeepsItsSeq(t *testing.T) {
	inf, _, _ := loadInflight(t, t.TempDir())
	if err := inf.Save(storedBatch(0)); err != nil {
		t.Fatalf("Save(seq 0) = %v", err)
	}

	shrunk := storedBatch(0)
	shrunk.MessageID = "019a0000-0000-7000-8000-000000000002"
	if err := inf.Save(shrunk); err != nil {
		t.Fatalf("Save(shrunk seq 0) = %v", err)
	}

	if got := inf.NextSeq(); got != 1 {
		t.Fatalf("NextSeq() = %d, want 1", got)
	}
}

func TestInflightNeedsLoad(t *testing.T) {
	inf := output.NewInflight(t.TempDir())
	calls := map[string]func() error{
		"Save":         func() error { return inf.Save(storedBatch(0)) },
		"Clear":        inf.Clear,
		"ClearRefused": func() error { return inf.ClearRefused(storedBatch(0)) },
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s before Load = nil, want an error", name)
		}
	}
}

func TestInflightLoadRefusesBadFiles(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "not JSON", content: "{"},
		{name: "unknown version", content: `{"version":2,"next_seq":0,"batch":null}`},
		{name: "next_seq does not follow the batch", content: `{"version":1,"next_seq":5,"batch":{"message_id":"m","seq":1,"body":"e30=","entries":{"bulk":[{"pos":0,"next":10,"record":{"Sample":{"Device":"d","Signal":"s","TS":"2026-10-19T08:00:00Z","Value":{"Kind":2,"Int":1},"Quality":1,"Priority":2}}}]},"positions":{"bulk":10},"losses":[]}}`},
		{name: "positions do not match the entries", content: `{"version":1,"next_seq":2,"batch":{"message_id":"m","seq":1,"body":"e30=","entries":{"bulk":[{"pos":0,"next":10,"record":{"Sample":{"Device":"d","Signal":"s","TS":"2026-10-19T08:00:00Z","Value":{"Kind":2,"Int":1},"Quality":1,"Priority":2}}}]},"positions":{"bulk":11},"losses":[]}}`},
		{name: "batch without records", content: `{"version":1,"next_seq":2,"batch":{"message_id":"m","seq":1,"body":"e30=","entries":{},"positions":{},"losses":[]}}`},
		{name: "invalid record", content: `{"version":1,"next_seq":2,"batch":{"message_id":"m","seq":1,"body":"e30=","entries":{"bulk":[{"pos":0,"next":10,"record":{}}]},"positions":{"bulk":10},"losses":[]}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "inflight"), []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, _, err := output.NewInflight(dir).Load(); err == nil {
				t.Fatal("Load() = nil, want an error")
			}
		})
	}
}

func TestInflightLoadRemovesLeftoverTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "inflight.tmp")
	if err := os.WriteFile(tmp, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	loadInflight(t, dir)
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("leftover temporary file still there: %v", err)
	}
}

func TestInflightSaveLeavesOnlyItsFile(t *testing.T) {
	dir := t.TempDir()
	inf, _, _ := loadInflight(t, dir)
	if err := inf.Save(storedBatch(0)); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}

	if strings.Join(names, ",") != "inflight" {
		t.Fatalf("files after Save = %v, want only inflight", names)
	}
}

func assertSameBatch(t *testing.T, got, want *output.Batch) {
	t.Helper()

	if got == nil {
		t.Fatal("batch is nil")
	}

	if got.MessageID != want.MessageID || got.Seq != want.Seq {
		t.Fatalf("batch %s seq %d, want %s seq %d", got.MessageID, got.Seq, want.MessageID, want.Seq)
	}

	if !bytes.Equal(got.Body, want.Body) {
		t.Fatalf("body differs:\n got %s\nwant %s", got.Body, want.Body)
	}

	if !reflect.DeepEqual(positionsOf(got), positionsOf(want)) {
		t.Fatalf("positions %v, want %v", got.Positions, want.Positions)
	}

	if !reflect.DeepEqual(entryPositions(got), entryPositions(want)) {
		t.Fatalf("entries %v, want %v", entryPositions(got), entryPositions(want))
	}

	for _, p := range []model.Priority{model.Critical, model.Bulk} {
		for i, e := range got.Entries[p] {
			w := want.Entries[p][i].Record
			if e.Record.Device() != w.Device() || !recordTS(e.Record).Equal(recordTS(w)) {
				t.Fatalf("%s entry %d record differs", p, i)
			}
		}
	}

	if !sameLosses(got.Losses, want.Losses) {
		t.Fatalf("losses %v, want %v", got.Losses, want.Losses)
	}
}

func positionsOf(b *output.Batch) map[model.Priority]buffer.Position {
	out := map[model.Priority]buffer.Position{}
	for p, pos := range b.Positions {
		out[p] = pos
	}

	return out
}

func entryPositions(b *output.Batch) map[model.Priority][]buffer.Position {
	out := map[model.Priority][]buffer.Position{}
	for p, es := range b.Entries {
		for _, e := range es {
			out[p] = append(out[p], e.Pos, e.Next)
		}
	}

	return out
}
