package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlaraplate/laraedge-sdk/handshake"
	pb "github.com/getlaraplate/laraedge-sdk/pluginv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/output"
)

// sendTimeout is the deadline of the Output Send call (spec section 4); a
// Send past it counts as RETRY_AFTER.
const sendTimeout = 30 * time.Second

// OutputSender delivers the batches of one output of type plugin:<id>
// through the Output service of the plugin h runs. It only maps the answer
// of the plugin to an output.Outcome: the output.Loop owns the batch, its
// backoff, split, drop and suspension. The plugin never buffers, so a plugin
// that is down, slow or refusing its configuration is a Retry and the
// buffer keeps everything (decision Q2).
type OutputSender struct {
	h        *Host
	resolver SecretResolver
	cfg      config.Output
	clk      clock.Clock
	log      *slog.Logger

	changes   <-chan uint64
	unwatch   func()
	closeOnce sync.Once
	closed    atomic.Bool
	reload    atomic.Bool

	// cfgGen is the host generation the instance is configured for, 0 when
	// it is not; secretValues are the values resolved for that Configure,
	// redacted from the words of the plugin. Both belong to Send, which the
	// Loop calls from one goroutine.
	cfgGen       uint64
	secretValues []string
}

var _ output.Sender = (*OutputSender)(nil)

// NewOutputSender returns the sender of output cfg through the plugin h
// runs. It only validates: nothing contacts the plugin before the first
// Send, which configures the instance. Secrets of config are resolved with
// resolver at every Configure, and the resolved values go to the plugin
// only. clk times the Configure and Send deadlines; nil means clock.Real().
func NewOutputSender(h *Host, cfg config.Output, resolver SecretResolver, clk clock.Clock, log *slog.Logger) (*OutputSender, error) {
	if h.spec.Manifest.Kinds.Output == nil {
		return nil, fmt.Errorf("plugin output %q: the plugin does not offer the output kind", cfg.ID)
	}
	if len(cfg.Raw) == 0 {
		return nil, fmt.Errorf("plugin output %q: the output has no section to configure the plugin with", cfg.ID)
	}
	if clk == nil {
		clk = clock.Real()
	}
	if log == nil {
		log = h.log.With("output", cfg.ID)
	}

	s := &OutputSender{h: h, resolver: resolver, cfg: cfg, clk: clk, log: log}
	s.changes, s.unwatch = h.Watch()

	return s, nil
}

// Close ends the subscription to the restarts of the plugin; a Send after
// Close is a Retry. It does not release the instance in the plugin
// (decision Q7). Safe to call more than once.
func (s *OutputSender) Close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.unwatch()
	})
}

// ReloadCredentials makes the next Send resolve the secrets of config again
// and configure the instance with them; safe for concurrent use.
func (s *OutputSender) ReloadCredentials() { s.reload.Store(true) }

// Send delivers b once through the plugin, configuring the instance first
// when the plugin restarted, the credentials were reloaded or the previous
// Configure failed.
func (s *OutputSender) Send(ctx context.Context, b *output.Batch) output.Outcome {
	if s.closed.Load() {
		return retry("the output is closed")
	}
	select {
	case <-s.changes:
		s.cfgGen = 0
	default:
	}
	if s.reload.Swap(false) {
		s.cfgGen = 0
	}

	conn, gen := s.h.Conn()
	if conn == nil {
		return retry(fmt.Sprintf("the plugin %q is not running", s.h.spec.ID))
	}
	if outcome, ok := s.ensureConfigured(ctx, conn, gen); !ok {
		return outcome
	}

	req := &pb.SendRequest{InstanceId: s.cfg.ID, Batch: batchToWire(b)}
	if proto.Size(req) > handshake.MaxMessageSize {
		return output.Outcome{Kind: output.Split, Reason: "the batch is over the message limit of the plugin protocol"}
	}

	callCtx, release := deadline(s.clk, ctx, sendTimeout)
	defer release()

	res, err := pb.NewOutputClient(conn).Send(callCtx, req)
	if err != nil {
		return retry(callFailure(ctx, callCtx, "send", err))
	}

	return s.outcome(res)
}

// outcome maps the answer of the plugin (spec section 4).
func (s *OutputSender) outcome(res *pb.SendResult) output.Outcome {
	switch res.GetOutcome() {
	case pb.Outcome_OUTCOME_ACCEPTED, pb.Outcome_OUTCOME_DUPLICATE:
		return output.Outcome{Kind: output.Confirmed}
	case pb.Outcome_OUTCOME_RETRY_AFTER:
		var wait time.Duration
		if d := res.GetRetryAfter(); d != nil && d.CheckValid() == nil && d.AsDuration() > 0 {
			wait = d.AsDuration()
		}

		return output.Outcome{Kind: output.Retry, RetryAfter: wait, Reason: "the plugin asked to retry later"}
	case pb.Outcome_OUTCOME_SPLIT:
		return output.Outcome{Kind: output.Split, Reason: "the plugin found the batch too large"}
	case pb.Outcome_OUTCOME_REJECT_INVALID:
		reason := "the plugin rejected the batch as invalid"
		if why := redact(res.GetReason(), s.secretValues); why != "" {
			reason += ": " + why
		}
		s.log.Warn("plugin output rejected a batch as invalid", "reason", reason)

		return output.Outcome{Kind: output.Invalid, Reason: reason}
	case pb.Outcome_OUTCOME_SUSPEND_AUTH:
		return output.Outcome{Kind: output.Suspend, Reason: "the plugin refused the credentials"}
	default:
		return retry(fmt.Sprintf("the plugin answered the unknown outcome %d", int32(res.GetOutcome())))
	}
}

// ensureConfigured configures the instance unless it is configured for the
// running generation of the plugin. ok false gives the outcome of the
// attempt: Suspend when a secret cannot be resolved (ReloadCredentials is
// the fix), Retry otherwise, a refusal included (decision H6 of plan 2
// applied to outputs: the output stays in backoff and the paths are
// logged).
func (s *OutputSender) ensureConfigured(ctx context.Context, conn *grpc.ClientConn, gen uint64) (output.Outcome, bool) {
	if s.cfgGen == gen {
		return output.Outcome{}, true
	}
	s.cfgGen = 0

	payload, values, problems, err := s.payload(ctx)
	switch {
	case len(problems) > 0:
		messages := strings.Join((&ConfigRefusedError{Input: s.cfg.ID, Problems: problems}).Messages(s.prefix()), "; ")
		s.log.Warn("plugin output secrets cannot be resolved, the output is suspended", "problems", messages)

		return output.Outcome{Kind: output.Suspend, Reason: messages}, false
	case err != nil:
		return retry(err.Error()), false
	}

	callCtx, release := deadline(s.clk, ctx, configureTimeout)
	defer release()

	resp, err := pb.NewOutputClient(conn).Configure(callCtx, &pb.ConfigureRequest{InstanceId: s.cfg.ID, Config: payload})
	if err != nil {
		return retry(callFailure(ctx, callCtx, "configure", err)), false
	}
	if len(resp.GetErrors()) > 0 {
		refused := &ConfigRefusedError{Input: s.cfg.ID}
		for _, e := range resp.GetErrors() {
			refused.Problems = append(refused.Problems, ConfigProblem{
				Path:    redact(e.GetPath(), values),
				Message: redact(e.GetMessage(), values),
			})
		}
		messages := strings.Join(refused.Messages(s.prefix()), "; ")
		s.log.Warn("plugin refused the configuration of the output, retrying", "problems", messages)

		return retry("the plugin refused the configuration: " + messages), false
	}

	s.cfgGen, s.secretValues = gen, values

	return output.Outcome{}, true
}

// prefix is the document path of the output in messages.
func (s *OutputSender) prefix() string { return "outputs." + s.cfg.ID }

// payload returns the output section with every secret reference of config
// resolved now, the resolved values (for redaction), and the problems of
// the secrets that cannot be resolved.
func (s *OutputSender) payload(ctx context.Context) ([]byte, []string, []ConfigProblem, error) {
	var section map[string]any
	dec := json.NewDecoder(bytes.NewReader(s.cfg.Raw))
	dec.UseNumber()
	if err := dec.Decode(&section); err != nil || section == nil {
		return nil, nil, nil, errors.New("the output section is not a JSON object")
	}

	var (
		values   []string
		problems []ConfigProblem
	)
	if cfg, ok := section["config"]; ok {
		section["config"] = resolveSecrets(ctx, s.resolver, cfg, "config", &values, &problems)
	}
	if len(problems) > 0 {
		return nil, nil, problems, nil
	}

	out, err := json.Marshal(section)
	if err != nil {
		return nil, nil, nil, errors.New("the output section cannot be encoded")
	}

	return out, values, nil, nil
}

func retry(reason string) output.Outcome {
	return output.Outcome{Kind: output.Retry, Reason: reason}
}

// callFailure turns an RPC failure into fixed text: a plugin's status
// message is its own words.
func callFailure(ctx, callCtx context.Context, what string, err error) string {
	if ctx.Err() != nil {
		return what + ": " + ctx.Err().Error()
	}
	if errors.Is(context.Cause(callCtx), context.DeadlineExceeded) {
		return what + ": the plugin did not answer in time"
	}

	return fmt.Sprintf("%s: the plugin answered %s", what, status.Code(err))
}

// batchToWire converts a sealed batch: its records in envelope order
// (critical first) and its loss markers.
func batchToWire(b *output.Batch) *pb.Batch {
	records := b.Records()
	out := &pb.Batch{MessageId: b.MessageID, SourceSeq: b.Seq, Records: make([]*pb.Record, 0, len(records))}
	for _, r := range records {
		out.Records = append(out.Records, recordToWire(r))
	}
	for _, l := range b.Losses {
		count := uint64(0)
		if l.Samples > 0 {
			count = uint64(l.Samples)
		}
		out.Losses = append(out.Losses, &pb.LossMarker{
			Priority: priorityToWire(l.Priority),
			From:     timestamppb.New(l.FromTS),
			To:       timestamppb.New(l.ToTS),
			Count:    count,
			Reason:   l.Reason,
		})
	}

	return out
}

func recordToWire(r model.Record) *pb.Record {
	if r.Event != nil {
		e := r.Event
		wire := &pb.DeviceEvent{Device: e.Device, Ts: timestamppb.New(e.TS)}
		switch e.Kind {
		case model.Birth:
			wire.Kind = pb.EventKind_EVENT_KIND_BIRTH
		case model.Death:
			wire.Kind = pb.EventKind_EVENT_KIND_DEATH
		}
		for _, si := range e.Signals {
			wire.Signals = append(wire.Signals, &pb.SignalInfo{Signal: si.Signal, DataType: dataTypeToWire(si.DataType), Unit: si.Unit})
		}

		return &pb.Record{Item: &pb.Record_DeviceEvent{DeviceEvent: wire}}
	}

	s := r.Sample
	if s == nil {
		return &pb.Record{}
	}
	wire := &pb.Sample{
		Device: s.Device, Signal: s.Signal, Ts: timestamppb.New(s.TS),
		Value: valueToWire(s.Value), Priority: priorityToWire(s.Priority),
	}
	switch s.Quality {
	case model.Good:
		wire.Quality = pb.Quality_QUALITY_GOOD
	case model.Uncertain:
		wire.Quality = pb.Quality_QUALITY_UNCERTAIN
	case model.Bad:
		wire.Quality = pb.Quality_QUALITY_BAD
	}

	return &pb.Record{Item: &pb.Record_Sample{Sample: wire}}
}

func valueToWire(v model.Value) *pb.Value {
	switch v.Kind {
	case model.KindInt:
		return &pb.Value{Kind: &pb.Value_IntValue{IntValue: v.Int}}
	case model.KindBool:
		return &pb.Value{Kind: &pb.Value_BoolValue{BoolValue: v.Bool}}
	case model.KindString:
		return &pb.Value{Kind: &pb.Value_StringValue{StringValue: v.Str}}
	default:
		return &pb.Value{Kind: &pb.Value_DoubleValue{DoubleValue: v.Float}}
	}
}

func priorityToWire(p model.Priority) pb.Priority {
	switch p {
	case model.Critical:
		return pb.Priority_PRIORITY_CRITICAL
	case model.Bulk:
		return pb.Priority_PRIORITY_BULK
	default:
		return pb.Priority_PRIORITY_UNSPECIFIED
	}
}

func dataTypeToWire(t string) pb.DataType {
	switch t {
	case model.DataTypeNumber:
		return pb.DataType_DATA_TYPE_NUMBER
	case model.DataTypeBoolean:
		return pb.DataType_DATA_TYPE_BOOLEAN
	case model.DataTypeString:
		return pb.DataType_DATA_TYPE_STRING
	default:
		return pb.DataType_DATA_TYPE_UNSPECIFIED
	}
}
