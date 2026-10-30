package status

import (
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/buffer"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/output"
	"github.com/getlaraplate/laraedge/internal/plugin"
	"github.com/getlaraplate/laraedge/internal/supervisor"
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

// restartReasons are the restart reasons always exposed for a plugin, at
// zero before any restart.
var restartReasons = []string{plugin.ReasonExit, plugin.ReasonHealth, plugin.ReasonMemory, plugin.ReasonHash}

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

	w.family("transform_errors_total", "counter", "Runtime errors of the input's transform chains since the agent started; each sent its sample bad.")
	for _, in := range st.Inputs {
		w.sample("transform_errors_total", float64(in.TransformErrors), "input", in.ID)
	}

	if reporting := reportingInputs(st.Inputs); len(reporting) > 0 {
		w.family("input_connected", "gauge", "1 when the input's connection to the machine is up, 0 when it is down; only inputs that keep a connection.")
		for _, in := range reporting {
			w.sample("input_connected", boolValue(*in.Connected), "input", in.ID)
		}

		w.family("input_read_errors_total", "counter", "Read errors of the input since it was last started, by kind: timeout, connection, exception, decode; reset to 0 when the input restarts after a configuration change or a rollback.")
		for _, in := range reporting {
			for _, kind := range readErrorKinds(in.ReadErrors) {
				w.sample("input_read_errors_total", float64(in.ReadErrors[kind]), "input", in.ID, "kind", kind)
			}
		}
	}

	if len(st.Plugins) > 0 {
		w.family("plugin_up", "gauge", "1 when the plugin process is up, 0 otherwise; only plugins the configuration uses.")
		for _, p := range st.Plugins {
			w.sample("plugin_up", boolValue(p.State == string(plugin.StateUp)), "plugin", p.ID)
		}

		w.family("plugin_restarts_total", "counter", "Restarts of the plugin process, by reason: exit, health, memory, hash; reset to 0 when a configuration change restarts the plugin.")
		for _, p := range st.Plugins {
			for _, reason := range withKnown(restartReasons, p.RestartsByReason) {
				w.sample("plugin_restarts_total", float64(p.RestartsByReason[reason]), "plugin", p.ID, "reason", reason)
			}
		}
	}

	if len(st.Processors) > 0 {
		w.family("processor_up", "gauge", "1 when the processor's stream to its plugin is open, 0 otherwise; machine data flows either way.")
		for _, p := range st.Processors {
			w.sample("processor_up", boolValue(p.State == supervisor.ProcessorUp), "processor", p.ID)
		}

		w.family("processor_refused_total", "counter", "What the processor's plugin sent outside its declared derived devices and signals, or with another value kind, refused; reset to 0 when the processor restarts.")
		for _, p := range st.Processors {
			w.sample("processor_refused_total", float64(p.Refused), "processor", p.ID)
		}

		w.family("processor_dropped_total", "counter", "Copies of observed records dropped because the processor's queue was full; reset to 0 when the processor restarts.")
		for _, p := range st.Processors {
			w.sample("processor_dropped_total", float64(p.Dropped), "processor", p.ID)
		}
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

	w.family("corrupt_records_total", "counter", "Corrupt records found in the output buffer since it was opened; each dropped the rest of its segment.")
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

	w.family("clock_offset_seconds", "gauge", "Backend clock minus agent clock, estimated from the last response; not for plugin outputs.")
	for _, out := range st.Outputs {
		if !hasClockOffset(out) {
			continue
		}
		w.sample("clock_offset_seconds", out.ClockOffset.Seconds(), "output", out.ID)
	}

	w.family("last_error_timestamp_seconds", "gauge", "Unix time of the last error of the output, 0 when none; the text is in /status.")
	for _, out := range st.Outputs {
		w.sample("last_error_timestamp_seconds", unixSeconds(out.LastErrorAt), "output", out.ID)
	}
}

// reportingInputs are the inputs that report their health.
func reportingInputs(inputs []supervisor.InputStatus) []supervisor.InputStatus {
	var out []supervisor.InputStatus
	for _, in := range inputs {
		if in.Connected != nil {
			out = append(out, in)
		}
	}

	return out
}

// readErrorKinds are the kinds exposed for an input: the known ones, at zero
// before any error so a rate over them never starts from a missing series,
// then any other kind it reported, sorted.
func readErrorKinds(counts map[string]uint64) []string {
	return withKnown(input.ReadErrorKinds, counts)
}

// withKnown returns known, then the other keys of counts, sorted.
func withKnown(known []string, counts map[string]uint64) []string {
	keys := slices.Clone(known)
	for _, key := range slices.Sorted(maps.Keys(counts)) {
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}

	return keys
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
