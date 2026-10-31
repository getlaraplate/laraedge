package plugin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge-sdk/sdk"

	"github.com/getlaraplate/laraedge/internal/buffer"
	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/output"
	"github.com/getlaraplate/laraedge/internal/plugin"
)

var _ output.Sender = (*plugin.OutputSender)(nil)

// outputHarness runs a plugin output sender on a clock of its own, apart
// from the host's.
type outputHarness struct {
	*harness
	outClk   *clock.Fake
	resolver *fakeResolver
}

func newOutputHarness(t *testing.T, behaviour string) *outputHarness {
	t.Helper()

	h := &outputHarness{
		harness:  newHarness(t, newInputSpec(t, behaviour)),
		outClk:   clock.NewFake(epoch),
		resolver: &fakeResolver{values: map[string]string{"secret://env/TOKEN": secretValue}},
	}
	h.start()

	return h
}

func (h *outputHarness) sender(t *testing.T, cfg config.Output) *plugin.OutputSender {
	t.Helper()

	s, err := plugin.NewOutputSender(h.host, cfg, h.resolver, h.outClk, slog.New(slog.NewTextHandler(h.log, nil)))
	if err != nil {
		t.Fatalf("NewOutputSender() = %v", err)
	}
	t.Cleanup(s.Close)

	return s
}

func (h *outputHarness) read(name string) string {
	return (&inputHarness{harness: h.harness}).read(name)
}

func (h *outputHarness) waitTrace(want string) {
	h.t.Helper()

	h.waitFor("trace "+want, func() bool { return strings.Contains(h.read("trace"), want) })
}

func (h *outputHarness) restartHost() {
	h.t.Helper()

	(&inputHarness{harness: h.harness}).restartHost()
}

func outputBehaviour(extra string) string {
	return `{"output":true,"trace_file":"trace","batches_file":"batches","config_file":"config.json"` + extra + `}`
}

// testOutput is output o1 of the plugin demo with the config object given.
func testOutput(configJSON string) config.Output {
	raw := fmt.Sprintf(`{"id":"o1","type":"plugin:demo","route":{"include":["*"]},"config":%s}`, configJSON)

	return config.Output{
		ID: "o1", Type: "plugin:demo", Route: config.Route{Include: []string{"*"}},
		Config: json.RawMessage(configJSON), Raw: json.RawMessage(raw),
	}
}

// testBatch holds a birth and a sample of each value kind across both
// priorities, and a loss marker.
func testBatch(id string, seq uint64) *output.Batch {
	sample := func(signal string, v model.Value, q model.Quality, p model.Priority) buffer.Entry {
		return buffer.Entry{Record: model.SampleRecord(model.Sample{Device: "d1", Signal: signal, TS: epoch, Value: v, Quality: q, Priority: p})}
	}
	birth := buffer.Entry{Record: model.EventRecord(model.DeviceEvent{
		Kind: model.Birth, Device: "d1", TS: epoch,
		Signals: []model.SignalInfo{{Signal: "s1", DataType: model.DataTypeNumber, Unit: "mm"}, {Signal: "s2", DataType: model.DataTypeString}},
	})}
	death := buffer.Entry{Record: model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: "d2", TS: epoch})}

	return &output.Batch{
		MessageID: id, Seq: seq, Body: []byte(`{}`),
		Entries: map[model.Priority][]buffer.Entry{
			model.Critical: {birth, sample("s1", model.IntValue(5), model.Good, model.Critical), death, sample("s3", model.BoolValue(true), model.Bad, model.Critical)},
			model.Bulk:     {sample("s1", model.Value{Kind: model.KindFloat, Float: 1.5}, model.Uncertain, model.Bulk), sample("s2", model.StringValue("x"), model.Good, model.Bulk)},
		},
		Losses: []model.Loss{{Priority: model.Bulk, FromTS: epoch.Add(-time.Minute), ToTS: epoch, Samples: 3, Reason: model.LossBufferFull}},
	}
}

// wantBatch is testBatch as the plugin decodes it.
func wantBatch(id string, seq uint64) sdk.Batch {
	sample := func(signal string, v sdk.Value, q sdk.Quality, p sdk.Priority) sdk.Record {
		return sdk.Record{Sample: &sdk.Sample{Device: "d1", Signal: signal, TS: epoch, Value: v, Quality: q, Priority: p}}
	}

	return sdk.Batch{
		MessageID: id, SourceSeq: seq,
		Records: []sdk.Record{
			{Event: &sdk.DeviceEvent{Kind: sdk.EventBirth, Device: "d1", TS: epoch, Signals: []sdk.SignalInfo{
				{Signal: "s1", DataType: sdk.DataTypeNumber, Unit: "mm"}, {Signal: "s2", DataType: sdk.DataTypeString},
			}}},
			sample("s1", sdk.IntValue(5), sdk.QualityGood, sdk.PriorityCritical),
			{Event: &sdk.DeviceEvent{Kind: sdk.EventDeath, Device: "d2", TS: epoch}},
			sample("s3", sdk.BoolValue(true), sdk.QualityBad, sdk.PriorityCritical),
			sample("s1", sdk.Value{Kind: sdk.ValueKindFloat, Float: 1.5}, sdk.QualityUncertain, sdk.PriorityBulk),
			sample("s2", sdk.StringValue("x"), sdk.QualityGood, sdk.PriorityBulk),
		},
		Losses: []sdk.LossMarker{{Priority: sdk.PriorityBulk, From: epoch.Add(-time.Minute), To: epoch, Count: 3, Reason: model.LossBufferFull}},
	}
}

func send(s *plugin.OutputSender, b *output.Batch) output.Outcome {
	return s.Send(context.Background(), b)
}

func TestOutputSendDeliversTheBatchWithResolvedSecrets(t *testing.T) {
	h := newOutputHarness(t, outputBehaviour(""))
	s := h.sender(t, testOutput(`{"path":"p","token":"secret://env/TOKEN"}`))

	if got := send(s, testBatch("m-1", 41)); got.Kind != output.Confirmed {
		t.Fatalf("Send() = %+v, want Confirmed", got)
	}

	want, err := json.Marshal(wantBatch("m-1", 41))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(h.read("batches")); got != string(want) {
		t.Fatalf("the plugin received\n%s\nwant\n%s", got, want)
	}
	cfg := h.read("config.json")
	if !strings.Contains(cfg, `"token":"`+secretValue+`"`) || strings.Contains(cfg, "secret://") {
		t.Fatalf("the plugin was configured with %s, want the token resolved", cfg)
	}
	var section struct {
		ID     string          `json:"id"`
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal([]byte(cfg), &section); err != nil || section.ID != "o1" || len(section.Config) == 0 {
		t.Fatalf("the plugin was configured with %s (%v), want the section of output o1", cfg, err)
	}
	if got := h.read("trace"); got != "configure o1\nsend o1 m-1\n" {
		t.Fatalf("trace = %q, want one Configure before the Send", got)
	}

	send(s, testBatch("m-2", 42))
	if got := h.read("trace"); strings.Count(got, "configure o1") != 1 {
		t.Fatalf("trace = %q, want the instance configured once", got)
	}
	if strings.Contains(h.log.String(), secretValue) {
		t.Fatalf("the log holds the secret value:\n%s", h.log.String())
	}
}

func TestOutputOutcomesMapToTheLoop(t *testing.T) {
	tests := []struct {
		name   string
		extra  string
		want   output.OutcomeKind
		after  time.Duration
		reason string
	}{
		{"accepted", `,"outcome":"accepted"`, output.Confirmed, 0, ""},
		{"duplicate", `,"outcome":"duplicate"`, output.Confirmed, 0, ""},
		{"retry after", `,"outcome":"retry_after","retry_after":"7s"`, output.Retry, 7 * time.Second, ""},
		{"retry without a wait", `,"outcome":"retry_after"`, output.Retry, 0, ""},
		{"split", `,"outcome":"split"`, output.Split, 0, ""},
		{"reject invalid", `,"outcome":"reject_invalid","reason":"unknown device"`, output.Invalid, 0, "unknown device"},
		{"suspend auth", `,"outcome":"suspend_auth"`, output.Suspend, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newOutputHarness(t, outputBehaviour(tc.extra))
			s := h.sender(t, testOutput(`{}`))

			got := send(s, testBatch("m-1", 1))
			if got.Kind != tc.want || got.RetryAfter != tc.after {
				t.Fatalf("Send() = %+v, want kind %v after %v", got, tc.want, tc.after)
			}
			if tc.reason != "" && !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("reason = %q, want it to hold %q", got.Reason, tc.reason)
			}
			if got.Kind != output.Confirmed && got.Reason == "" {
				t.Fatal("an outcome other than Confirmed has no reason")
			}
		})
	}
}

func TestOutputSendPast30SecondsIsARetry(t *testing.T) {
	h := newOutputHarness(t, outputBehaviour(`,"send_block":true`))
	s := h.sender(t, testOutput(`{}`))

	result := make(chan output.Outcome, 1)
	go func() { result <- send(s, testBatch("m-1", 1)) }()

	h.waitTrace("send o1 m-1")
	h.outClk.Advance(30*time.Second - time.Millisecond)
	select {
	case got := <-result:
		t.Fatalf("Send returned %+v before 30 s", got)
	default:
	}
	h.outClk.Advance(time.Millisecond)
	select {
	case got := <-result:
		if got.Kind != output.Retry || !strings.Contains(got.Reason, "did not answer in time") {
			t.Fatalf("Send() = %+v, want a Retry naming the timeout", got)
		}
	case <-time.After(guard):
		t.Fatal("Send did not give up")
	}
}

func TestOutputConfigurePast10SecondsIsARetry(t *testing.T) {
	h := newOutputHarness(t, outputBehaviour(`,"configure_block":true`))
	s := h.sender(t, testOutput(`{}`))

	result := make(chan output.Outcome, 1)
	go func() { result <- send(s, testBatch("m-1", 1)) }()

	h.waitTrace("configure o1")
	h.outClk.Advance(10 * time.Second)
	select {
	case got := <-result:
		if got.Kind != output.Retry || !strings.Contains(got.Reason, "configure: the plugin did not answer in time") {
			t.Fatalf("Send() = %+v, want a Retry naming the Configure timeout", got)
		}
	case <-time.After(guard):
		t.Fatal("Send did not give up")
	}
	if strings.Contains(h.read("trace"), "send o1") {
		t.Fatal("a batch was sent to an instance that is not configured")
	}
}

// TestOutputPluginDownRetriesThenConfiguresAgainAfterTheRestart is the
// crash loop of the review focus: while the plugin is down every Send is a
// Retry, and after the restart the instance is configured again before the
// batch is resent under its message_id, which the plugin answers DUPLICATE.
func TestOutputPluginDownRetriesThenConfiguresAgainAfterTheRestart(t *testing.T) {
	behaviour := outputBehaviour(`,"dedupe":true`)
	h := newOutputHarness(t, behaviour)
	s := h.sender(t, testOutput(`{}`))
	if got := send(s, testBatch("m-1", 1)); got.Kind != output.Confirmed {
		t.Fatalf("first Send() = %+v", got)
	}

	setBehaviour(t, h.spec.Path, `{"crash_at_start":true}`)
	h.kill()
	h.waitFor("the plugin down", func() bool { return h.host.Status().State != plugin.StateUp })
	for range 3 {
		got := send(s, testBatch("m-1", 1))
		if got.Kind != output.Retry || !strings.Contains(got.Reason, "not running") {
			t.Fatalf("Send() with the plugin down = %+v, want a Retry naming the plugin down", got)
		}
	}

	setBehaviour(t, h.spec.Path, behaviour)
	h.restartHost()
	if got := send(s, testBatch("m-1", 1)); got.Kind != output.Confirmed {
		t.Fatalf("Send() after the restart = %+v, want the duplicate confirmed", got)
	}
	if got := send(s, testBatch("m-2", 2)); got.Kind != output.Confirmed {
		t.Fatalf("Send() of the next batch = %+v", got)
	}
	if got := h.read("trace"); got != "configure o1\nsend o1 m-1\nconfigure o1\nsend o1 m-1\nsend o1 m-2\n" {
		t.Fatalf("trace = %q, want a Configure before the first Send of each run of the plugin", got)
	}
	if got := strings.Count(h.read("batches"), `"MessageID"`); got != 2 {
		t.Fatalf("the plugin stored %d batches, want m-1 and m-2 once each", got)
	}
}

func TestOutputRefusedConfigureRetriesAndLogsThePaths(t *testing.T) {
	h := newOutputHarness(t, outputBehaviour(`,"refuse_config":true,"refuse_echo":true`))
	s := h.sender(t, testOutput(`{"token":"secret://env/TOKEN"}`))

	for i := range 2 {
		got := send(s, testBatch("m-1", 1))
		if got.Kind != output.Retry || !strings.Contains(got.Reason, "refused the configuration") {
			t.Fatalf("Send() %d = %+v, want a Retry naming the refusal", i, got)
		}
		if strings.Contains(got.Reason, secretValue) {
			t.Fatalf("the reason holds the secret: %q", got.Reason)
		}
	}
	if got := h.read("trace"); got != "configure o1\nconfigure o1\n" {
		t.Fatalf("trace = %q, want a Configure per attempt and no Send", got)
	}
	if !strings.Contains(h.log.String(), "outputs.o1.config.port: bad value ***") {
		t.Fatalf("the log does not name the path:\n%s", h.log.String())
	}
	if strings.Contains(h.log.String(), secretValue) {
		t.Fatalf("the log holds the secret value:\n%s", h.log.String())
	}
}

func TestOutputUnresolvableSecretSuspendsWithoutContactingThePlugin(t *testing.T) {
	h := newOutputHarness(t, outputBehaviour(""))
	s := h.sender(t, testOutput(`{"auth":{"api_key":"secret://env/MISSING"}}`))

	got := send(s, testBatch("m-1", 1))
	if got.Kind != output.Suspend || !strings.Contains(got.Reason, "outputs.o1.config.auth.api_key: the secret cannot be resolved") {
		t.Fatalf("Send() = %+v, want Suspend naming the path", got)
	}
	if strings.Contains(got.Reason, secretValue) || strings.Contains(h.log.String(), secretValue) {
		t.Fatalf("the resolver's words leaked: %q\n%s", got.Reason, h.log.String())
	}
	if strings.Contains(h.read("trace"), "configure") {
		t.Fatal("the plugin was configured with an unresolved secret")
	}
}

func TestOutputReloadCredentialsConfiguresAgainWithNewSecrets(t *testing.T) {
	h := newOutputHarness(t, outputBehaviour(""))
	s := h.sender(t, testOutput(`{"token":"secret://env/TOKEN"}`))
	if got := send(s, testBatch("m-1", 1)); got.Kind != output.Confirmed {
		t.Fatalf("Send() = %+v", got)
	}

	const rotated = "r0tat3d-value-0043"
	h.resolver.mu.Lock()
	h.resolver.values["secret://env/TOKEN"] = rotated
	h.resolver.mu.Unlock()
	s.ReloadCredentials()

	if got := send(s, testBatch("m-2", 2)); got.Kind != output.Confirmed {
		t.Fatalf("Send() after the reload = %+v", got)
	}
	if got := h.read("trace"); got != "configure o1\nsend o1 m-1\nconfigure o1\nsend o1 m-2\n" {
		t.Fatalf("trace = %q, want a Configure before the Send after the reload", got)
	}
	if cfg := h.read("config.json"); !strings.Contains(cfg, rotated) {
		t.Fatalf("the plugin was configured with %s, want the rotated value", cfg)
	}
	if n := h.resolver.count(); n != 2 {
		t.Fatalf("resolver calls = %d, want one per Configure", n)
	}
	if log := h.log.String(); strings.Contains(log, secretValue) || strings.Contains(log, rotated) {
		t.Fatalf("the log holds a secret value:\n%s", log)
	}
}

func TestOutputBatchOverTheMessageLimitIsSplit(t *testing.T) {
	h := newOutputHarness(t, outputBehaviour(""))
	s := h.sender(t, testOutput(`{}`))

	big := &output.Batch{
		MessageID: "m-big", Seq: 1, Body: []byte(`{}`),
		Entries: map[model.Priority][]buffer.Entry{model.Bulk: {{Record: model.SampleRecord(model.Sample{
			Device: "d1", Signal: "s1", TS: epoch, Value: model.StringValue(strings.Repeat("x", 17<<20)), Quality: model.Good, Priority: model.Bulk,
		})}}},
	}
	if got := send(s, big); got.Kind != output.Split {
		t.Fatalf("Send() = %+v, want Split", got)
	}
	if strings.Contains(h.read("trace"), "send o1") {
		t.Fatal("a batch over the message limit reached the plugin")
	}
}

func TestOutputSendAfterCloseIsARetry(t *testing.T) {
	h := newOutputHarness(t, outputBehaviour(""))
	s := h.sender(t, testOutput(`{}`))

	s.Close()
	s.Close()
	if got := send(s, testBatch("m-1", 1)); got.Kind != output.Retry {
		t.Fatalf("Send() after Close = %+v, want Retry", got)
	}
	if strings.Contains(h.read("trace"), "send o1") {
		t.Fatal("a closed sender reached the plugin")
	}
}

func TestNewOutputSenderRefusesAPluginWithoutTheOutputKind(t *testing.T) {
	spec := newSpec(t, "")
	host := plugin.NewHost(spec, plugin.HostOptions{DataDir: shortTempDir(t)})

	if _, err := plugin.NewOutputSender(host, testOutput(`{}`), nil, clock.NewFake(epoch), nil); err == nil {
		t.Fatal("the sender accepted a plugin without the output kind")
	}
}

// TestOutputConfigureAtApply is ruling R6: the supervisor configures a
// plugin output synchronously when it applies a document, so a refusal or
// an unresolvable secret rolls the document back. The error names paths
// under outputs.<id>, never values; a configured instance is not configured
// again by the first Send.
func TestOutputConfigureAtApply(t *testing.T) {
	tests := []struct {
		name      string
		behaviour string
		config    string
		want      []string // Messages("outputs.3"), nil for success
	}{
		{"accepted", outputBehaviour(""), `{"token":"secret://env/TOKEN"}`, nil},
		{
			"refused", outputBehaviour(`,"refuse_config":true,"refuse_echo":true`), `{"token":"secret://env/TOKEN"}`,
			[]string{"outputs.3.config.port: bad value ***"},
		},
		{
			"unresolvable secret", outputBehaviour(""), `{"auth":{"api_key":"secret://env/MISSING"}}`,
			[]string{"outputs.3.config.auth.api_key: the secret cannot be resolved"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newOutputHarness(t, tc.behaviour)
			s := h.sender(t, testOutput(tc.config))

			err := s.Configure(context.Background())

			if tc.want == nil {
				if err != nil {
					t.Fatalf("Configure() = %v, want nil", err)
				}
				if got := send(s, testBatch("m-1", 1)); got.Kind != output.Confirmed {
					t.Fatalf("Send() = %+v", got)
				}
				if got := h.read("trace"); got != "configure o1\nsend o1 m-1\n" {
					t.Fatalf("trace = %q, want the Send to reuse the Configure", got)
				}

				return
			}
			var refused *plugin.ConfigRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Configure() = %v, want a *ConfigRefusedError", err)
			}
			if got := refused.Messages("outputs.3"); !slices.Equal(got, tc.want) {
				t.Fatalf("Messages() = %q, want %q", got, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "outputs.o1.") || strings.Contains(err.Error(), secretValue) {
				t.Fatalf("Error() = %q, want paths under outputs.o1 and no value", err.Error())
			}
		})
	}
}

// TestOutputConfigureAtApplyPluginProblems: a plugin that is down or does
// not answer within 10 s is an error that is not a refusal: the Loop
// configures the instance at its next Send.
func TestOutputConfigureAtApplyPluginProblems(t *testing.T) {
	t.Run("past 10 seconds", func(t *testing.T) {
		h := newOutputHarness(t, outputBehaviour(`,"configure_block":true`))
		s := h.sender(t, testOutput(`{}`))

		result := make(chan error, 1)
		go func() { result <- s.Configure(context.Background()) }()
		h.waitTrace("configure o1")
		h.outClk.Advance(10 * time.Second)

		var err error
		select {
		case err = <-result:
		case <-time.After(guard):
			t.Fatal("Configure did not give up")
		}
		var refused *plugin.ConfigRefusedError
		if err == nil || errors.As(err, &refused) || !strings.Contains(err.Error(), "did not answer in time") {
			t.Fatalf("Configure() = %v, want a timeout that is not a refusal", err)
		}
	})
	t.Run("plugin down", func(t *testing.T) {
		h := newOutputHarness(t, outputBehaviour(""))
		s := h.sender(t, testOutput(`{}`))
		h.stop()

		err := s.Configure(context.Background())
		var refused *plugin.ConfigRefusedError
		if err == nil || errors.As(err, &refused) || !strings.Contains(err.Error(), "not running") {
			t.Fatalf("Configure() = %v, want the plugin down, not a refusal", err)
		}
	})
}
