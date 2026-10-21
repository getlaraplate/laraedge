package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/getlaraplate/laraedge/internal/atomicfile"
	"github.com/getlaraplate/laraedge/internal/buffer"
	"github.com/getlaraplate/laraedge/internal/model"
)

const (
	inflightFileName = "inflight"
	inflightVersion  = 1
)

// errNotLoaded is returned by the writing methods of an Inflight not loaded.
var errNotLoaded = errors.New("output: inflight not loaded")

// Inflight is the inflight file of one output, data_dir/outputs/{id}/inflight:
// the sealed batch not yet confirmed, if any, and the next source_seq. Every
// change rewrites the whole file atomically (temporary file, fsync, rename,
// fsync of the directory where supported), so after a crash the file holds
// the state before or after the change, never a mix.
//
// The next source_seq is never lost and never goes back, with one exception:
// ClearRefused hands the refused seq back (ruling R24). Load must succeed
// before any other call. An Inflight is not safe for concurrent use.
type Inflight struct {
	dir string

	loaded   bool
	nextSeq  uint64
	hasBatch bool
	batchSeq uint64
}

// NewInflight returns the inflight file of the output directory dir; nothing
// is read before Load.
func NewInflight(dir string) *Inflight {
	return &Inflight{dir: dir}
}

// Load reads the file and returns the sealed batch, nil when there is none,
// and the next source_seq, 0 for a new output. It removes a temporary file
// left by a crash during a write.
func (f *Inflight) Load() (*Batch, uint64, error) {
	tmp := filepath.Join(f.dir, inflightFileName+atomicfile.TmpSuffix)
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, 0, fmt.Errorf("output: remove a leftover temporary inflight file: %w", err)
	}

	data, err := os.ReadFile(filepath.Join(f.dir, inflightFileName))
	if errors.Is(err, fs.ErrNotExist) {
		f.loaded, f.nextSeq, f.hasBatch, f.batchSeq = true, 0, false, 0

		return nil, 0, nil
	}

	if err != nil {
		return nil, 0, fmt.Errorf("output: read inflight: %w", err)
	}

	var state inflightState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, 0, fmt.Errorf("output: parse inflight: %w", err)
	}

	if state.Version != inflightVersion {
		return nil, 0, fmt.Errorf("output: inflight has version %d, want %d", state.Version, inflightVersion)
	}

	var b *Batch
	if state.Batch != nil {
		if b, err = state.Batch.decode(); err != nil {
			return nil, 0, fmt.Errorf("output: inflight batch: %w", err)
		}

		if b.Seq == math.MaxUint64 || state.NextSeq != b.Seq+1 {
			return nil, 0, fmt.Errorf("output: inflight next_seq %d does not follow its batch seq %d", state.NextSeq, b.Seq)
		}
	}

	f.loaded, f.nextSeq = true, state.NextSeq
	f.hasBatch = b != nil
	if b != nil {
		f.batchSeq = b.Seq
	}

	return b, state.NextSeq, nil
}

// NextSeq returns the source_seq the next new batch takes.
func (f *Inflight) NextSeq() uint64 { return f.nextSeq }

// Save stores b as the sealed batch and b.Seq+1 as the next source_seq.
// While a batch is stored, b.Seq must be its seq (a 413 split keeps it): a
// new batch never replaces one not yet settled. Otherwise b.Seq must be the
// next source_seq.
func (f *Inflight) Save(b *Batch) error {
	if !f.loaded {
		return errNotLoaded
	}

	want := f.nextSeq
	if f.hasBatch {
		want = f.batchSeq
	}

	if b.Seq != want {
		return fmt.Errorf("output: save inflight batch with seq %d, want %d", b.Seq, want)
	}

	if b.Seq == math.MaxUint64 {
		return errors.New("output: source_seq exhausted")
	}

	if err := b.validate(); err != nil {
		return fmt.Errorf("output: save inflight batch: %w", err)
	}

	wb := encodeBatch(b)
	if err := f.write(inflightState{Version: inflightVersion, NextSeq: b.Seq + 1, Batch: &wb}); err != nil {
		return err
	}

	f.nextSeq, f.hasBatch, f.batchSeq = b.Seq+1, true, b.Seq

	return nil
}

// Clear removes the sealed batch and keeps the next source_seq.
func (f *Inflight) Clear() error {
	if !f.loaded {
		return errNotLoaded
	}

	if err := f.write(inflightState{Version: inflightVersion, NextSeq: f.nextSeq}); err != nil {
		return err
	}

	f.hasBatch = false

	return nil
}

// ClearRefused removes the stored batch b, which the backend refused for
// good, and makes its seq the next source_seq: the backend never stored a
// refused seq and raises seq_gap only above last+1 (ruling R24).
func (f *Inflight) ClearRefused(b *Batch) error {
	if !f.loaded {
		return errNotLoaded
	}

	if !f.hasBatch || f.batchSeq != b.Seq {
		return fmt.Errorf("output: clear refused batch seq %d: not the stored batch", b.Seq)
	}

	if err := f.write(inflightState{Version: inflightVersion, NextSeq: b.Seq}); err != nil {
		return err
	}

	f.nextSeq, f.hasBatch = b.Seq, false

	return nil
}

func (f *Inflight) write(state inflightState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("output: encode inflight: %w", err)
	}

	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return fmt.Errorf("output: create %s: %w", f.dir, err)
	}

	if err := atomicfile.Write(f.dir, inflightFileName, data); err != nil {
		return fmt.Errorf("output: %w", err)
	}

	return nil
}

// inflightState is the content of the inflight file.
type inflightState struct {
	Version int            `json:"version"`
	NextSeq uint64         `json:"next_seq"`
	Batch   *inflightBatch `json:"batch"`
}

// inflightBatch is a sealed batch on disk: the body is kept as sent, the
// entries so that a 413 split can rebuild smaller envelopes (ruling R2).
type inflightBatch struct {
	MessageID string                     `json:"message_id"`
	Seq       uint64                     `json:"seq"`
	Body      []byte                     `json:"body"`
	Entries   map[string][]inflightEntry `json:"entries"`
	Positions map[string]buffer.Position `json:"positions"`
	Losses    []inflightLoss             `json:"losses"`
}

type inflightEntry struct {
	Pos    buffer.Position `json:"pos"`
	Next   buffer.Position `json:"next"`
	Record model.Record    `json:"record"`
}

type inflightLoss struct {
	Priority string    `json:"priority"`
	FromTS   time.Time `json:"from_ts"`
	ToTS     time.Time `json:"to_ts"`
	Samples  int       `json:"samples"`
	Reason   string    `json:"reason"`
}

func encodeBatch(b *Batch) inflightBatch {
	wb := inflightBatch{
		MessageID: b.MessageID,
		Seq:       b.Seq,
		Body:      b.Body,
		Entries:   map[string][]inflightEntry{},
		Positions: map[string]buffer.Position{},
		Losses:    make([]inflightLoss, 0, len(b.Losses)),
	}

	for p, es := range b.Entries {
		if len(es) == 0 {
			continue
		}

		out := make([]inflightEntry, 0, len(es))
		for _, e := range es {
			out = append(out, inflightEntry{Pos: e.Pos, Next: e.Next, Record: e.Record})
		}

		wb.Entries[p.String()] = out
	}

	for p, pos := range b.Positions {
		wb.Positions[p.String()] = pos
	}

	for _, l := range b.Losses {
		wb.Losses = append(wb.Losses, inflightLoss{Priority: l.Priority.String(), FromTS: l.FromTS, ToTS: l.ToTS, Samples: l.Samples, Reason: l.Reason})
	}

	return wb
}

func (wb *inflightBatch) decode() (*Batch, error) {
	b := &Batch{
		MessageID: wb.MessageID,
		Seq:       wb.Seq,
		Body:      wb.Body,
		Entries:   map[model.Priority][]buffer.Entry{},
		Positions: map[model.Priority]buffer.Position{},
	}

	for name, es := range wb.Entries {
		p, err := model.ParsePriority(name)
		if err != nil {
			return nil, fmt.Errorf("entries: %w", err)
		}

		out := make([]buffer.Entry, 0, len(es))
		for _, e := range es {
			out = append(out, buffer.Entry{Pos: e.Pos, Next: e.Next, Record: e.Record})
		}

		b.Entries[p] = out
	}

	for name, pos := range wb.Positions {
		p, err := model.ParsePriority(name)
		if err != nil {
			return nil, fmt.Errorf("positions: %w", err)
		}

		b.Positions[p] = pos
	}

	for _, l := range wb.Losses {
		p, err := model.ParsePriority(l.Priority)
		if err != nil {
			return nil, fmt.Errorf("losses: %w", err)
		}

		b.Losses = append(b.Losses, model.Loss{Priority: p, FromTS: l.FromTS, ToTS: l.ToTS, Samples: l.Samples, Reason: l.Reason})
	}

	if err := b.validate(); err != nil {
		return nil, err
	}

	return b, nil
}
