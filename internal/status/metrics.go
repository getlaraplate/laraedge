package status

import (
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/swolley/laraplate-edge/internal/buffer"
	"github.com/swolley/laraplate-edge/internal/model"
	"github.com/swolley/laraplate-edge/internal/output"
	"github.com/swolley/laraplate-edge/internal/supervisor"
)

// metricsContentType is the Prometheus text exposition format 0.0.4.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// prefix starts every metric name.
const prefix = "laraplate_edge_"

// outputStates are the values of the state label of output_state: the
// sending states of spec 6.8 and the failed state of the supervisor.
var outputStates = []string{
	output.Sending.String(),
	output.Backoff.String(),
	output.SuspendedAuth.String(),
	supervisor.StateFailed,
}

// lossReasons are the drop reasons always exposed, at zero before any drop,
// so a rate over them never starts from a missing series.
var lossReasons = []string{model.LossBufferFull, model.LossRejectedInvalid}

// metrics writes the exposition of the current state (spec 6.8). The buffer
// series (records, bytes, oldest age, drops, corrupt records) are written
// for the outputs that run, which have an open buffer; the others for every
// output of the applied document.
func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	st := s.source.Status()

	var b strings.Builder
	writeMetrics(&expositionWriter{b: &b}, st, s.clk.Now())

	w.Header().Set("Content-Type", metricsContentType)
	_, _ = w.Write([]byte(b.String()))
}

func writeMetrics(w *expositionWriter, st supervisor.Status, now time.Time) {
	running := make([]supervisor.OutputStatus, 0, len(st.Outputs))
	for _, out := range st.Outputs {
		if out.State != supervisor.StateFailed {
			running = append(running, out)
		}
	}

	w.family("config_version", "gauge", "Version of the applied configuration document, 0 when none is applied.")
	w.sample("config_version", float64(st.Version))

	w.family("input_up", "gauge", "1 when the input runs, 0 when it failed.")
	for _, in := range st.Inputs {
		w.sample("input_up", boolValue(in.State == supervisor.InputRunning), "input", in.ID)
	}

	queues := func(name, help string, value func(buffer.QueueStats) float64) {
		w.family(name, "gauge", help)
		for _, out := range running {
			w.sample(name, value(out.Buffer.Critical), "output", out.ID, "priority", model.Critical.String())
			w.sample(name, value(out.Buffer.Bulk), "output", out.ID, "priority", model.Bulk.String())
		}
	}
	queues("buffer_records", "Records of the output buffer not yet confirmed, by priority.",
		func(q buffer.QueueStats) float64 { return float64(q.Records) })
	queues("buffer_bytes", "Bytes of the output buffer on disk, by priority.",
		func(q buffer.QueueStats) float64 { return float64(q.Bytes) })
	queues("buffer_oldest_unconfirmed_seconds", "Age of the oldest unconfirmed record, by priority, 0 when there is none.",
		func(q buffer.QueueStats) float64 { return oldestAge(q, now).Seconds() })

	w.family("dropped_samples_total", "counter", "Samples dropped with a loss marker since the output buffer was opened, by reason.")
	for _, out := range running {
		reasons := slices.Clone(lossReasons)
		for _, reason := range slices.Sorted(maps.Keys(out.Buffer.Dropped)) {
			if !slices.Contains(reasons, reason) {
				reasons = append(reasons, reason)
			}
		}
		for _, reason := range reasons {
			w.sample("dropped_samples_total", float64(out.Buffer.Dropped[reason]), "output", out.ID, "reason", reason)
		}
	}

	w.family("corrupt_records_total", "counter", "Corrupt records found in the output buffer when it was opened.")
	for _, out := range running {
		w.sample("corrupt_records_total", float64(out.Buffer.CorruptRecords), "output", out.ID)
	}

	w.family("last_confirmation_timestamp_seconds", "gauge", "Unix time of the last confirmed batch, 0 before any.")
	for _, out := range st.Outputs {
		w.sample("last_confirmation_timestamp_seconds", unixSeconds(out.LastConfirmation), "output", out.ID)
	}

	w.family("output_state", "gauge", "1 for the current state of the output, 0 for the others.")
	for _, out := range st.Outputs {
		for _, state := range outputStates {
			w.sample("output_state", boolValue(out.State == state), "output", out.ID, "state", state)
		}
	}

	w.family("clock_offset_seconds", "gauge", "Backend clock minus agent clock, estimated from the last response.")
	for _, out := range st.Outputs {
		w.sample("clock_offset_seconds", out.ClockOffset.Seconds(), "output", out.ID)
	}

	w.family("last_error_timestamp_seconds", "gauge", "Unix time of the last error of the output, 0 when none; the text is in /status.")
	for _, out := range st.Outputs {
		w.sample("last_error_timestamp_seconds", unixSeconds(out.LastErrorAt), "output", out.ID)
	}
}

// expositionWriter writes the Prometheus text format: a HELP and a TYPE line
// per family, then its samples.
type expositionWriter struct {
	b *strings.Builder
}

// family starts the family prefix+name.
func (w *expositionWriter) family(name, typ, help string) {
	name = fullName(name)
	w.b.WriteString("# HELP " + name + " " + escapeHelp(help) + "\n")
	w.b.WriteString("# TYPE " + name + " " + typ + "\n")
}

// sample writes one sample of the family prefix+name; labels are name,
// value pairs.
func (w *expositionWriter) sample(name string, value float64, labels ...string) {
	w.b.WriteString(fullName(name))
	if len(labels) > 0 {
		w.b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.b.WriteByte(',')
			}
			w.b.WriteString(labels[i] + `="` + escapeLabel(labels[i+1]) + `"`)
		}
		w.b.WriteByte('}')
	}
	w.b.WriteString(" " + formatValue(value) + "\n")
}

func fullName(name string) string {
	if strings.HasPrefix(name, prefix) {
		return name
	}

	return prefix + name
}

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }

func escapeHelp(s string) string { return helpEscaper.Replace(s) }

// formatValue writes v in the shortest decimal form, without an exponent.
func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}

	return 0
}

// unixSeconds is t as Unix seconds with milliseconds, 0 for the zero time.
func unixSeconds(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}

	return float64(t.UnixMilli()) / 1000
}
