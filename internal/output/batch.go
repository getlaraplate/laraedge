// Package output takes the records of one output from its disk buffer and
// delivers them to a destination. The Batcher seals them into batches, one
// laraplate-machine/1 envelope each, persisted in the inflight file before
// they are sent and resent identical until the destination answers (spec
// 6.3, 6.4).
package output

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/getlaraplate/laraedge/internal/buffer"
	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/envelope"
	"github.com/getlaraplate/laraedge/internal/model"
)

// priorities is the order of the records in a batch: critical first.
var priorities = [2]model.Priority{model.Critical, model.Bulk}

// BatchLimits bounds one batch (config max_samples, max_body, max_wait).
type BatchLimits struct {
	// MaxSamples is the most records of a batch; samples and device events
	// count alike. At least 1.
	MaxSamples int
	// MaxBody is the most bytes of an envelope. At least 1.
	MaxBody int
	// MaxWait is how long a batch with fewer than MaxSamples records waits
	// for more, from the first record seen. Zero seals at once.
	MaxWait time.Duration
}

func (l BatchLimits) validate() error {
	switch {
	case l.MaxSamples < 1:
		return fmt.Errorf("max samples %d below 1", l.MaxSamples)
	case l.MaxBody < 1:
		return fmt.Errorf("max body %d below 1", l.MaxBody)
	case l.MaxWait < 0:
		return fmt.Errorf("negative max wait %v", l.MaxWait)
	default:
		return nil
	}
}

// Batch is a sealed batch: one envelope with its source_seq, and the buffer
// records it carries. The body is sent unchanged on every retry.
type Batch struct {
	// MessageID is the envelope's message_id (UUIDv7).
	MessageID string
	// Seq is the envelope's source_seq.
	Seq uint64
	// Body is the envelope, as sent.
	Body []byte
	// Entries are the records of the batch per priority, in queue order. The
	// envelope holds the critical ones first, then the bulk ones.
	Entries map[model.Priority][]buffer.Entry
	// Positions is, per priority with records, the position just after the
	// batch's last record of that queue.
	Positions map[model.Priority]buffer.Position
	// Losses are the loss markers the envelope carries.
	Losses []model.Loss
}

// Len returns the number of records of the batch.
func (b *Batch) Len() int {
	return len(b.Entries[model.Critical]) + len(b.Entries[model.Bulk])
}

// Records returns the records of the batch in envelope order: critical
// first, then bulk, each in queue order.
func (b *Batch) Records() []model.Record {
	out := make([]model.Record, 0, b.Len())
	for _, p := range priorities {
		for _, e := range b.Entries[p] {
			out = append(out, e.Record)
		}
	}

	return out
}

// validate reports whether b is a complete sealed batch.
func (b *Batch) validate() error {
	if b.MessageID == "" {
		return errors.New("batch without message id")
	}

	if len(b.Body) == 0 {
		return errors.New("batch without body")
	}

	if b.Len() == 0 {
		return errors.New("batch without records")
	}

	for p, es := range b.Entries {
		if !p.Valid() {
			return fmt.Errorf("entries of invalid priority %d", uint8(p))
		}

		for i, e := range es {
			if e.Next <= e.Pos || (i > 0 && e.Pos < es[i-1].Next) {
				return fmt.Errorf("%s entry %d: positions %d..%d out of order", p, i, uint64(e.Pos), uint64(e.Next))
			}

			if err := e.Record.Validate(); err != nil {
				return fmt.Errorf("%s entry %d: %w", p, i, err)
			}
		}
	}

	for _, p := range priorities {
		pos, ok := b.Positions[p]
		es := b.Entries[p]
		switch {
		case len(es) == 0 && ok:
			return fmt.Errorf("%s position without records", p)
		case len(es) > 0 && (!ok || pos != es[len(es)-1].Next):
			return fmt.Errorf("%s position %d does not end the records", p, uint64(pos))
		}
	}

	if len(b.Positions) > len(b.Entries) {
		return errors.New("positions of unknown priorities")
	}

	for i, l := range b.Losses {
		if err := l.Validate(); err != nil {
			return fmt.Errorf("loss %d: %w", i, err)
		}
	}

	return nil
}

// newBatch builds a Batch from its parts, copying the slices.
func newBatch(id string, seq uint64, body []byte, entries map[model.Priority][]buffer.Entry, losses []model.Loss) *Batch {
	b := &Batch{
		MessageID: id,
		Seq:       seq,
		Body:      body,
		Entries:   map[model.Priority][]buffer.Entry{},
		Positions: map[model.Priority]buffer.Position{},
		Losses:    slices.Clone(losses),
	}

	for _, p := range priorities {
		if es := entries[p]; len(es) > 0 {
			b.Entries[p] = slices.Clone(es)
			b.Positions[p] = es[len(es)-1].Next
		}
	}

	return b
}

// Buffer is what the Batcher uses of the output's disk buffer
// (*buffer.Buffer implements it).
type Buffer interface {
	Read(p model.Priority, from buffer.Position, limit int) ([]buffer.Entry, error)
	Ack(p model.Priority, upTo buffer.Position) error
	MarkSealed(p model.Priority, upTo buffer.Position) error
	Head(p model.Priority) buffer.Position
	Changed() <-chan struct{}
	Records() uint64
	AddLoss(l model.Loss) error
	SyncLosses() error
	PendingLosses() []model.Loss
	AckLosses(n int) error
}

// MessageIDs produces envelope message ids (*envelope.IDGenerator).
type MessageIDs interface {
	New() string
}

// Batcher seals the records of one output into batches, one in flight at a
// time, so source_seq stays strictly ordered (spec 6.3). It is owned by the
// output's sending goroutine and is not safe for concurrent use; the buffer
// it reads may be appended to concurrently.
//
// The cycle is Next, then one of Confirm, Shrink or Drop; until one of them
// succeeds, Next returns the same batch, also after a restart.
type Batcher struct {
	buf      Buffer
	inflight *Inflight
	ids      MessageIDs
	clk      clock.Clock
	limits   BatchLimits
	log      *slog.Logger

	current *Batch
}

// NewBatcher loads the inflight file and returns the batcher of the output
// whose buffer is buf. A batch found in inflight is marked sealed in the
// buffer at once, so that an eviction does not count its records as lost
// (the batch carries its own copy): build the batcher before anything can
// Append to buf, that is before the router starts. A nil log means
// slog.Default().
func NewBatcher(buf Buffer, inflight *Inflight, ids MessageIDs, clk clock.Clock, limits BatchLimits, log *slog.Logger) (*Batcher, error) {
	if err := limits.validate(); err != nil {
		return nil, fmt.Errorf("output: batch limits: %w", err)
	}

	if log == nil {
		log = slog.Default()
	}

	bt := &Batcher{buf: buf, inflight: inflight, ids: ids, clk: clk, limits: limits, log: log}

	b, _, err := inflight.Load()
	if err != nil {
		return nil, err
	}

	if b != nil {
		if err := bt.markSealed(b); err != nil {
			return nil, err
		}

		bt.current = b
	}

	return bt, nil
}

// Next returns the batch to send: the sealed batch not yet settled, unchanged,
// or a new one. A new batch waits until the buffer holds a record, then until
// it holds MaxSamples records or MaxWait has passed since the first record
// was seen, whichever comes first. It takes critical records first, then
// bulk, up to MaxSamples, drops records from the tail until the envelope
// fits MaxBody, and carries the pending loss markers. A record whose envelope
// alone exceeds MaxBody can never be sent: it is dropped with a
// rejected_invalid marker (ruling R23).
//
// Sealing order: the batch takes the next source_seq and a new message id,
// is saved in inflight, and only then is marked sealed in the buffer and are
// its loss markers acknowledged. A crash before the save leaves nothing
// durable: the records are sealed again with the same, never sent, seq. A
// crash between the save and the acknowledgement leaves the markers both in
// the batch and pending: they are reported twice, never lost (ruling R40).
func (bt *Batcher) Next(ctx context.Context) (*Batch, error) {
	if bt.current != nil {
		return bt.current, nil
	}

	idle := false
	for {
		if err := bt.wait(ctx, idle); err != nil {
			return nil, err
		}

		for {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("output: next batch: %w", err)
			}

			b, retry, err := bt.seal()
			if err != nil {
				return nil, err
			}

			if b != nil {
				return b, nil
			}

			if !retry {
				bt.log.Warn("output: the buffer counts records but none is readable; waiting for the next append")
				idle = true

				break
			}
		}
	}
}

// wait blocks until a batch can be sealed, without polling: it wakes on
// appends and on the MaxWait timer. After a seal that found nothing to read
// (idle) it first waits for an append, so a count that disagrees with the
// queues never makes it spin.
func (bt *Batcher) wait(ctx context.Context, idle bool) error {
	if idle {
		select {
		case <-ctx.Done():
			return fmt.Errorf("output: next batch: %w", ctx.Err())
		case <-bt.buf.Changed():
		}
	}

	var timer clock.Timer
	var fired <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("output: next batch: %w", err)
		}

		n := bt.buf.Records()
		if n >= uint64(bt.limits.MaxSamples) {
			return nil
		}

		if n > 0 && timer == nil {
			if bt.limits.MaxWait <= 0 {
				return nil
			}

			timer = bt.clk.NewTimer(bt.limits.MaxWait)
			fired = timer.C()
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("output: next batch: %w", ctx.Err())
		case <-bt.buf.Changed():
		case <-fired:
			return nil
		}
	}
}

// seal builds, saves and returns a new batch. It returns no batch when the
// buffer holds nothing readable, and retry when it dropped a record larger
// than MaxBody, so that the next records are sealed at once.
func (bt *Batcher) seal() (*Batch, bool, error) {
	crit, err := bt.buf.Read(model.Critical, 0, bt.limits.MaxSamples)
	if err != nil {
		return nil, false, fmt.Errorf("output: read the critical queue: %w", err)
	}

	var bulk []buffer.Entry
	if rest := bt.limits.MaxSamples - len(crit); rest > 0 {
		if bulk, err = bt.buf.Read(model.Bulk, 0, rest); err != nil {
			return nil, false, fmt.Errorf("output: read the bulk queue: %w", err)
		}
	}

	if len(crit)+len(bulk) == 0 {
		return nil, false, nil
	}

	records := make([]model.Record, 0, len(crit)+len(bulk))
	for _, e := range append(slices.Clip(crit), bulk...) {
		records = append(records, e.Record)
	}

	id := bt.ids.New()
	seq := bt.inflight.NextSeq()
	sentAt := bt.clk.Now()
	losses := bt.buf.PendingLosses()

	n, k, body, err := bt.fit(id, seq, sentAt, records, losses)
	if err != nil {
		return nil, false, err
	}

	if n == 0 {
		p, e := model.Bulk, buffer.Entry{}
		if len(crit) > 0 {
			p, e = model.Critical, crit[0]
		} else {
			e = bulk[0]
		}

		return nil, true, bt.reject(p, e)
	}

	kc := min(n, len(crit))
	b := newBatch(id, seq, body, map[model.Priority][]buffer.Entry{
		model.Critical: crit[:kc],
		model.Bulk:     bulk[:n-kc],
	}, losses[:k])

	if err := bt.inflight.Save(b); err != nil {
		return nil, false, err
	}

	// The batch is sealed: what follows never undoes it.
	bt.current = b
	if err := bt.markSealed(b); err != nil {
		bt.log.Error("output: cannot mark the batch sealed in the buffer; an eviction may report its records as lost", "seq", b.Seq, "error", err)
	}

	// AckLosses after the save, never before: a crash in between reports
	// the markers twice (they are pending and in the batch), which is the
	// accepted side; markers have no identity to tell a newer equal one from
	// these (ruling R40). ErrNotDurable means applied: never repeated.
	if k > 0 {
		if err := bt.buf.AckLosses(k); err != nil {
			if errors.Is(err, buffer.ErrNotDurable) {
				bt.log.Warn("output: loss markers acknowledged, not yet durable", "seq", b.Seq, "error", err)
			} else {
				bt.log.Error("output: cannot acknowledge the loss markers of a sealed batch; they will travel again", "seq", b.Seq, "error", err)
			}
		}
	}

	return b, false, nil
}

// fit returns how many records and loss markers fit MaxBody and their
// envelope. Records are dropped from the tail first (binary search over the
// count); when even the first record does not fit with the markers, markers
// are dropped from the tail by halving. n is 0 when the first record alone
// exceeds MaxBody.
func (bt *Batcher) fit(id string, seq uint64, sentAt time.Time, records []model.Record, losses []model.Loss) (int, int, []byte, error) {
	build := func(n, k int) ([]byte, bool, error) {
		body, err := envelope.Build(id, seq, sentAt, records[:n], losses[:k])
		if err != nil {
			return nil, false, fmt.Errorf("output: build envelope: %w", err)
		}

		return body, len(body) <= bt.limits.MaxBody, nil
	}

	for k := len(losses); ; k /= 2 {
		body, ok, err := build(len(records), k)
		if err != nil || ok {
			return len(records), k, body, err
		}

		// lo fits (0: none known), hi does not.
		lo, hi := 0, len(records)
		var best []byte
		for hi-lo > 1 {
			mid := lo + (hi-lo)/2
			body, ok, err := build(mid, k)
			if err != nil {
				return 0, 0, nil, err
			}

			if ok {
				lo, best = mid, body
			} else {
				hi = mid
			}
		}

		if lo > 0 {
			return lo, k, best, nil
		}

		if k == 0 {
			return 0, 0, nil, nil
		}
	}
}

// reject drops the record e of queue p, which cannot fit MaxBody, with a
// rejected_invalid marker when it is a sample, then acknowledges it.
func (bt *Batcher) reject(p model.Priority, e buffer.Entry) error {
	bt.log.Warn("output: dropping a record larger than max_body", "priority", p.String(), "device", e.Record.Device(), "max_body", bt.limits.MaxBody)
	if l, ok := bt.lossOf(p, []buffer.Entry{e}, model.LossRejectedInvalid, bt.clk.Now()); ok {
		if err := bt.addLosses(l); err != nil {
			return err
		}
	}

	if err := bt.buf.Ack(p, e.Next); err != nil {
		return fmt.Errorf("output: acknowledge a record larger than max_body: %w", err)
	}

	return nil
}

// Confirm settles b as delivered: its records are acknowledged in the buffer
// and the inflight batch is cleared, the next source_seq kept.
func (bt *Batcher) Confirm(b *Batch) error {
	if err := bt.checkCurrent(b); err != nil {
		return err
	}

	if err := bt.ack(b); err != nil {
		return err
	}

	if err := bt.inflight.Clear(); err != nil {
		return err
	}

	bt.current = nil

	return nil
}

// Shrink answers a 413: it keeps the first n records of b (critical, then
// bulk), with b's seq and all of b's loss markers (C4), under a new message
// id and a rebuilt body, saves that batch in inflight and returns it. The
// other records go back to the buffer, read again by a later Next under the
// next seq; those the buffer no longer holds (evicted while sealed, cut or
// dropped at recovery) get a durable buffer_full marker from the batch's own copy, so
// nothing vanishes silently. n must be in [1, Len()-1]. On error b stays the
// batch in flight; a retry may count a vanished record twice, never zero
// times.
func (bt *Batcher) Shrink(b *Batch, n int) (*Batch, error) {
	if err := bt.checkCurrent(b); err != nil {
		return nil, err
	}

	if n < 1 || n >= b.Len() {
		return nil, fmt.Errorf("output: shrink a batch of %d records to %d", b.Len(), n)
	}

	crit, bulk := b.Entries[model.Critical], b.Entries[model.Bulk]
	kc := min(n, len(crit))
	kept := map[model.Priority][]buffer.Entry{model.Critical: crit[:kc], model.Bulk: bulk[:n-kc]}
	rest := map[model.Priority][]buffer.Entry{model.Critical: crit[kc:], model.Bulk: bulk[n-kc:]}

	id := bt.ids.New()
	nb := newBatch(id, b.Seq, nil, kept, b.Losses)
	body, err := envelope.Build(id, b.Seq, bt.clk.Now(), nb.Records(), nb.Losses)
	if err != nil {
		return nil, fmt.Errorf("output: build the shrunk envelope: %w", err)
	}

	nb.Body = body

	// Lower the sealed mark first: from now on an eviction counts the
	// records handed back. Those evicted before are counted here. A race
	// between the two counts a record twice, never zero times.
	restore := func() {
		if err := bt.markSealed(b); err != nil {
			bt.log.Error("output: cannot restore the sealed mark", "seq", b.Seq, "error", err)
		}
	}

	if err := bt.markSealed(nb); err != nil {
		restore()

		return nil, err
	}

	var markers []model.Loss
	for _, p := range priorities {
		gone, err := bt.vanished(p, rest[p])
		if err != nil {
			restore()

			return nil, err
		}

		if l, ok := bt.lossOf(p, gone, model.LossBufferFull, bt.clk.Now()); ok {
			markers = append(markers, l)
		}
	}

	if err := bt.addLosses(markers...); err != nil {
		restore()

		return nil, err
	}

	if err := bt.inflight.Save(nb); err != nil {
		restore()

		return nil, err
	}

	bt.current = nb

	return nb, nil
}

// Drop answers a 422, or a 413 on a one-record batch: b can never be
// delivered. Its records leave a loss marker per priority with reason, the
// loss markers it carried become pending again (they never reached the
// backend), its records are acknowledged, and inflight is cleared with b's
// seq as the next source_seq (ruling R24). The markers are made durable
// before the records are acknowledged; when that fails, inflight is left
// unchanged and the error returned, and a retry adds the markers again
// (counted twice, never lost; ruling R41).
func (bt *Batcher) Drop(b *Batch, reason string) error {
	if err := bt.checkCurrent(b); err != nil {
		return err
	}

	now := bt.clk.Now()
	markers := slices.Clone(b.Losses)
	for _, p := range priorities {
		if l, ok := bt.lossOf(p, b.Entries[p], reason, now); ok {
			markers = append(markers, l)
		}
	}

	for i, m := range markers {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("output: drop batch seq %d: marker %d: %w", b.Seq, i, err)
		}
	}

	if err := bt.addLosses(markers...); err != nil {
		return err
	}

	if err := bt.ack(b); err != nil {
		return err
	}

	if err := bt.inflight.ClearRefused(b); err != nil {
		return err
	}

	bt.current = nil

	return nil
}

func (bt *Batcher) checkCurrent(b *Batch) error {
	if b == nil || b != bt.current {
		return errors.New("output: not the batch in flight")
	}

	return nil
}

// ack acknowledges the records of b in the buffer, up to the last one the
// buffer still holds. Records are durable in the buffer only at its next
// group sync, a batch at once: after a crash the buffer may have cut the
// batch's last records, and their positions are no record boundary any more.
func (bt *Batcher) ack(b *Batch) error {
	for _, p := range priorities {
		upTo, ok, err := bt.ackTarget(p, b.Entries[p])
		if err != nil {
			return fmt.Errorf("output: acknowledge batch seq %d: %w", b.Seq, err)
		}

		if !ok {
			continue
		}

		if err := bt.buf.Ack(p, upTo); err != nil {
			return fmt.Errorf("output: acknowledge batch seq %d: %w", b.Seq, err)
		}
	}

	return nil
}

// ackTarget returns the position after the last of entries still in queue
// p, and false when none is left there (all acknowledged, evicted or cut).
func (bt *Batcher) ackTarget(p model.Priority, entries []buffer.Entry) (buffer.Position, bool, error) {
	head := bt.buf.Head(p)
	for i := len(entries) - 1; i >= 0 && entries[i].Pos >= head; i-- {
		ok, err := bt.holds(p, entries[i])
		if err != nil {
			return 0, false, err
		}

		if ok {
			return entries[i].Next, true, nil
		}
	}

	return 0, false, nil
}

// holds reports whether queue p still holds the record of e. Positions are
// never reused, so a record at e.Pos is that record.
func (bt *Batcher) holds(p model.Priority, e buffer.Entry) (bool, error) {
	got, err := bt.buf.Read(p, e.Pos, 1)
	if err != nil {
		return false, fmt.Errorf("read the %s queue at %d: %w", p, uint64(e.Pos), err)
	}

	return len(got) == 1 && got[0].Pos == e.Pos, nil
}

// markSealed sets the sealed mark of both queues to the end of b's records,
// 0 for a queue b has none of.
func (bt *Batcher) markSealed(b *Batch) error {
	for _, p := range priorities {
		if err := bt.buf.MarkSealed(p, b.Positions[p]); err != nil {
			return fmt.Errorf("output: mark batch seq %d sealed: %w", b.Seq, err)
		}
	}

	return nil
}

// vanished returns the entries of queue p the buffer no longer holds:
// evicted while sealed, cut at recovery because they were not synced before
// a crash, or dropped with a corrupt record of an older segment, wherever
// they sit in entries. Entries are consecutive records of the queue,
// positions are never reused and records written after a restart lie beyond
// every earlier position, so the records the buffer holds from the first
// entry on are a subset of entries followed by newer ones: one read from the
// first entry, compared by position, finds every gap.
func (bt *Batcher) vanished(p model.Priority, entries []buffer.Entry) ([]buffer.Entry, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	held, err := bt.buf.Read(p, entries[0].Pos, len(entries))
	if err != nil {
		return nil, fmt.Errorf("output: read the %s queue at %d: %w", p, uint64(entries[0].Pos), err)
	}

	var gone []buffer.Entry
	j := 0
	for _, e := range entries {
		for j < len(held) && held[j].Pos < e.Pos {
			j++
		}

		if j < len(held) && held[j].Pos == e.Pos {
			j++

			continue
		}

		gone = append(gone, e)
	}

	return gone, nil
}

// addLosses adds markers and returns nil only once they are durable, so
// that the caller may then acknowledge the records they stand for. AddLoss
// returning ErrNotDurable means the marker is applied in memory: it is never
// repeated, SyncLosses writes it. When that write fails too the error is
// returned and the caller acknowledges nothing; a retry adds the markers
// again, counted twice, never lost (ruling R41).
func (bt *Batcher) addLosses(markers ...model.Loss) error {
	pending := false
	for _, l := range markers {
		err := bt.buf.AddLoss(l)
		switch {
		case err == nil:
		case errors.Is(err, buffer.ErrNotDurable):
			pending = true
		default:
			return fmt.Errorf("output: add a %s loss marker: %w", l.Reason, err)
		}
	}

	if !pending {
		return nil
	}

	if err := bt.buf.SyncLosses(); err != nil {
		return fmt.Errorf("output: loss markers not durable: %w", err)
	}

	return nil
}

// lossOf returns the marker of the samples among entries of queue p: their
// ts range and count; samples without a ts take now. A device event is not a
// sample: it is not counted, and a lost death is logged with its device. ok
// is false when entries hold no sample: the protocol wants samples >= 1.
func (bt *Batcher) lossOf(p model.Priority, entries []buffer.Entry, reason string, now time.Time) (model.Loss, bool) {
	l := model.Loss{Priority: p, Reason: reason}
	for _, e := range entries {
		if ev := e.Record.Event; ev != nil {
			if ev.Kind == model.Death {
				bt.log.Warn("output: a device death was dropped; the backend will not see it", "priority", p.String(), "device", ev.Device, "reason", reason)
			}

			continue
		}

		l.Samples++
		ts := recordTS(e.Record)
		if ts.IsZero() {
			continue
		}

		if l.FromTS.IsZero() || ts.Before(l.FromTS) {
			l.FromTS = ts
		}

		if ts.After(l.ToTS) {
			l.ToTS = ts
		}
	}

	if l.Samples == 0 {
		return model.Loss{}, false
	}

	if l.FromTS.IsZero() {
		l.FromTS, l.ToTS = now, now
	}

	return l, true
}

func recordTS(r model.Record) time.Time {
	switch {
	case r.Sample != nil:
		return r.Sample.TS
	case r.Event != nil:
		return r.Event.TS
	default:
		return time.Time{}
	}
}
