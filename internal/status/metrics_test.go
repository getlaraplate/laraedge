package status

import (
	"bufio"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/supervisor"
)

var (
	metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelName  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	metricType = map[string]bool{"counter": true, "gauge": true, "histogram": true, "summary": true, "untyped": true}
)

// family is one metric family as the validator read it.
type family struct {
	help, typ string
	samples   map[string]float64 // by label set as written
}

// parseExposition validates text against the Prometheus text exposition
// format 0.0.4 and returns its families by name. It checks the grammar of
// HELP, TYPE and sample lines, that HELP and TYPE come once and before the
// samples of their family, that the samples of a family are contiguous, that
// a series is not repeated, and that the text ends with a newline.
func parseExposition(t *testing.T, text string) map[string]*family {
	t.Helper()

	if !strings.HasSuffix(text, "\n") {
		t.Fatal("the exposition does not end with a newline")
	}

	families := map[string]*family{}
	closed := map[string]bool{}
	current := ""
	get := func(name string) *family {
		f := families[name]
		if f == nil {
			f = &family{samples: map[string]float64{}}
			families[name] = f
		}

		return f
	}
	enter := func(n int, name string) {
		if name == current {
			return
		}
		if closed[name] {
			t.Fatalf("line %d: family %s appears again after another family", n, name)
		}
		if current != "" {
			closed[current] = true
		}
		current = name
	}

	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()

		switch {
		case strings.HasPrefix(line, "# HELP "):
			name, help, ok := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			if !ok || !metricName.MatchString(name) || help == "" {
				t.Fatalf("line %d: bad HELP line %q", n, line)
			}
			enter(n, name)
			f := get(name)
			if f.help != "" || f.typ != "" || len(f.samples) > 0 {
				t.Fatalf("line %d: HELP of %s after its TYPE or samples, or twice", n, name)
			}
			f.help = help
		case strings.HasPrefix(line, "# TYPE "):
			name, typ, ok := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			if !ok || !metricName.MatchString(name) || !metricType[typ] {
				t.Fatalf("line %d: bad TYPE line %q", n, line)
			}
			enter(n, name)
			f := get(name)
			if f.typ != "" || len(f.samples) > 0 {
				t.Fatalf("line %d: TYPE of %s after its samples, or twice", n, name)
			}
			f.typ = typ
		case strings.HasPrefix(line, "#") || line == "":
			t.Fatalf("line %d: unexpected line %q", n, line)
		default:
			name, labels, value := parseSample(t, n, line)
			enter(n, name)
			f := get(name)
			if f.typ == "" {
				t.Fatalf("line %d: sample of %s before its TYPE", n, name)
			}
			if _, dup := f.samples[labels]; dup {
				t.Fatalf("line %d: series %s%s repeated", n, name, labels)
			}
			f.samples[labels] = value
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	return families
}

// parseSample parses `name{l="v",...} value` and returns the label set as
// written, "{}" without labels.
func parseSample(t *testing.T, n int, line string) (name, labels string, value float64) {
	t.Helper()

	rest := line
	i := strings.IndexAny(rest, "{ ")
	if i < 0 {
		t.Fatalf("line %d: bad sample %q", n, line)
	}
	name, rest = rest[:i], rest[i:]
	if !metricName.MatchString(name) {
		t.Fatalf("line %d: bad metric name %q", n, name)
	}

	labels = "{}"
	if strings.HasPrefix(rest, "{") {
		end := scanLabels(t, n, rest)
		labels, rest = rest[:end], rest[end:]
	}

	fields := strings.Fields(rest)
	if !strings.HasPrefix(rest, " ") || len(fields) < 1 || len(fields) > 2 {
		t.Fatalf("line %d: bad value part %q", n, rest)
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		t.Fatalf("line %d: bad value %q", n, fields[0])
	}
	if len(fields) == 2 {
		if _, err := strconv.ParseInt(fields[1], 10, 64); err != nil {
			t.Fatalf("line %d: bad timestamp %q", n, fields[1])
		}
	}

	return name, labels, v
}

// scanLabels validates the label set at the start of s and returns its
// length, closing brace included.
func scanLabels(t *testing.T, n int, s string) int {
	t.Helper()

	i := 1
	seen := map[string]bool{}
	for {
		if s[i] == '}' {
			return i + 1
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			t.Fatalf("line %d: label without value in %q", n, s)
		}
		ln := s[i : i+eq]
		if !labelName.MatchString(ln) || seen[ln] {
			t.Fatalf("line %d: bad or repeated label name %q", n, ln)
		}
		seen[ln] = true
		i += eq + 1
		if i >= len(s) || s[i] != '"' {
			t.Fatalf("line %d: label %s value not quoted", n, ln)
		}
		i++
		for {
			if i >= len(s) {
				t.Fatalf("line %d: unterminated label value", n)
			}
			c := s[i]
			if c == '\\' {
				if i+1 >= len(s) || !strings.ContainsRune(`\"n`, rune(s[i+1])) {
					t.Fatalf("line %d: bad escape in label %s", n, ln)
				}
				i += 2

				continue
			}
			if c == '\n' {
				t.Fatalf("line %d: raw newline in label %s", n, ln)
			}
			i++
			if c == '"' {
				break
			}
		}
		if i >= len(s) {
			t.Fatalf("line %d: unterminated label set", n)
		}
		switch s[i] {
		case ',':
			i++
		case '}':
		default:
			t.Fatalf("line %d: unexpected %q after label %s", n, s[i], ln)
		}
	}
}

func TestMetrics(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)

	code, header, body := h.get(t, http.MethodGet, "/metrics", "")

	if code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", code)
	}
	if got := header.Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	families := parseExposition(t, body)

	types := map[string]string{
		"laraplate_edge_config_version":                      "gauge",
		"laraplate_edge_input_up":                            "gauge",
		"laraplate_edge_transform_errors_total":              "counter",
		"laraplate_edge_buffer_records":                      "gauge",
		"laraplate_edge_buffer_bytes":                        "gauge",
		"laraplate_edge_buffer_oldest_unconfirmed_seconds":   "gauge",
		"laraplate_edge_dropped_samples_total":               "counter",
		"laraplate_edge_corrupt_records_total":               "counter",
		"laraplate_edge_last_confirmation_timestamp_seconds": "gauge",
		"laraplate_edge_output_state":                        "gauge",
		"laraplate_edge_clock_offset_seconds":                "gauge",
		"laraplate_edge_last_error_timestamp_seconds":        "gauge",
	}
	for name, typ := range types {
		f := families[name]
		if f == nil {
			t.Errorf("family %s missing", name)

			continue
		}
		if f.typ != typ {
			t.Errorf("%s TYPE = %s, want %s", name, f.typ, typ)
		}
	}
	if len(families) != len(types) {
		t.Errorf("got %d families, want %d", len(families), len(types))
	}

	want := map[string]map[string]float64{
		"laraplate_edge_config_version":         {"{}": 7},
		"laraplate_edge_input_up":               {`{input="line"}`: 1, `{input="probe"}`: 0},
		"laraplate_edge_transform_errors_total": {`{input="line"}`: 3, `{input="probe"}`: 0},
		"laraplate_edge_buffer_records": {
			`{output="main",priority="critical"}`: 3,
			`{output="main",priority="bulk"}`:     5,
		},
		"laraplate_edge_buffer_bytes": {
			`{output="main",priority="critical"}`: 300,
			`{output="main",priority="bulk"}`:     1024,
		},
		"laraplate_edge_buffer_oldest_unconfirmed_seconds": {
			`{output="main",priority="critical"}`: 30,
			`{output="main",priority="bulk"}`:     0,
		},
		"laraplate_edge_dropped_samples_total": {
			`{output="main",reason="buffer_full"}`:      4,
			`{output="main",reason="rejected_invalid"}`: 1,
		},
		"laraplate_edge_corrupt_records_total": {`{output="main"}`: 2},
		"laraplate_edge_last_confirmation_timestamp_seconds": {
			`{output="main"}`:  float64(epoch.Add(-time.Minute).Unix()),
			`{output="spare"}`: 0,
		},
		"laraplate_edge_output_state": {
			`{output="main",state="sending"}`:         0,
			`{output="main",state="backoff"}`:         1,
			`{output="main",state="suspended_auth"}`:  0,
			`{output="main",state="failed"}`:          0,
			`{output="spare",state="sending"}`:        0,
			`{output="spare",state="backoff"}`:        0,
			`{output="spare",state="suspended_auth"}`: 0,
			`{output="spare",state="failed"}`:         1,
		},
		"laraplate_edge_clock_offset_seconds": {`{output="main"}`: 1.5, `{output="spare"}`: 0},
		"laraplate_edge_last_error_timestamp_seconds": {
			`{output="main"}`:  float64(epoch.Add(-10 * time.Second).Unix()),
			`{output="spare"}`: 0,
		},
	}
	for name, series := range want {
		f := families[name]
		if f == nil {
			continue
		}
		if len(f.samples) != len(series) {
			t.Errorf("%s has %d series, want %d: %v", name, len(f.samples), len(series), f.samples)
		}
		for labels, v := range series {
			got, ok := f.samples[labels]
			if !ok {
				t.Errorf("%s%s missing", name, labels)

				continue
			}
			if got != v {
				t.Errorf("%s%s = %v, want %v", name, labels, got, v)
			}
		}
	}
}

// TestMetricsTransformErrorsFollowInputUp: the transform error counter comes
// right after input_up, in the family order of spec 6.8.
func TestMetricsTransformErrorsFollowInputUp(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)

	_, _, body := h.get(t, http.MethodGet, "/metrics", "")

	var order []string
	for line := range strings.Lines(body) {
		if name, ok := strings.CutPrefix(line, "# HELP "); ok {
			name, _, _ = strings.Cut(name, " ")
			order = append(order, name)
		}
	}
	i := slices.Index(order, "laraplate_edge_input_up")
	if i < 0 || i+1 >= len(order) || order[i+1] != "laraplate_edge_transform_errors_total" {
		t.Fatalf("families %q, want laraplate_edge_transform_errors_total right after laraplate_edge_input_up", order)
	}
}

func TestMetricsBeforeAnyConfiguration(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)
	h.src.set(supervisor.Status{})

	_, _, body := h.get(t, http.MethodGet, "/metrics", "")

	families := parseExposition(t, body)
	if got := families["laraplate_edge_config_version"].samples["{}"]; got != 0 {
		t.Fatalf("config_version = %v, want 0", got)
	}
	if n := len(families["laraplate_edge_input_up"].samples); n != 0 {
		t.Fatalf("input_up has %d series, want none", n)
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	var b strings.Builder
	w := &expositionWriter{b: &b}
	w.family("laraplate_edge_test", "gauge", "A test family.")
	w.sample("laraplate_edge_test", 1, "output", "a\"b\\c\nd")

	families := parseExposition(t, b.String())
	if _, ok := families["laraplate_edge_test"].samples[`{output="a\"b\\c\nd"}`]; !ok {
		t.Fatalf("exposition = %q, want the label value escaped", b.String())
	}
}

func TestFormatValue(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{in: 0, want: "0"},
		{in: 3, want: "3"},
		{in: 1.5, want: "1.5"},
		{in: -0.25, want: "-0.25"},
		{in: 1760860800.125, want: "1760860800.125"},
	}

	for _, tt := range tests {
		if got := formatValue(tt.in); got != tt.want {
			t.Errorf("formatValue(%v) = %q, want %q", tt.in, got, tt.want)
		}
		if _, err := strconv.ParseFloat(formatValue(tt.in), 64); err != nil {
			t.Errorf("formatValue(%v) does not parse: %v", tt.in, err)
		}
	}
}

// healthStatus is a status with a connected modbus input, a disconnected one
// that counted errors, a failed one and a simulated input, which does not
// report.
func healthStatus() supervisor.Status {
	up, down := true, false

	return supervisor.Status{
		Applied: true,
		Version: 1,
		Inputs: []supervisor.InputStatus{
			{ID: "cell", Driver: "modbus", State: supervisor.InputRunning, Connected: &up, ReadErrors: map[string]uint64{}},
			{ID: "oven", Driver: "modbus", State: supervisor.InputRunning, Connected: &down, ReadErrors: map[string]uint64{"timeout": 4, "decode": 1}},
			{ID: "line", Driver: "simulated", State: supervisor.InputRunning},
			{ID: "broken", Driver: "modbus", State: supervisor.StateFailed, LastError: "start: bad connection"},
		},
	}
}

func TestMetricsExposeHealthOfReportingInputsOnly(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)
	h.src.set(healthStatus())

	_, _, body := h.get(t, http.MethodGet, "/metrics", "")
	families := parseExposition(t, body)

	connected := families["laraplate_edge_input_connected"]
	if connected == nil || connected.typ != "gauge" {
		t.Fatalf("input_connected = %+v, want a gauge", connected)
	}
	wantConnected := map[string]float64{`{input="cell"}`: 1, `{input="oven"}`: 0}
	if !maps.Equal(connected.samples, wantConnected) {
		t.Errorf("input_connected = %v, want %v", connected.samples, wantConnected)
	}

	errs := families["laraplate_edge_input_read_errors_total"]
	if errs == nil || errs.typ != "counter" {
		t.Fatalf("input_read_errors_total = %+v, want a counter", errs)
	}
	wantErrs := map[string]float64{
		`{input="cell",kind="timeout"}`:    0,
		`{input="cell",kind="connection"}`: 0,
		`{input="cell",kind="exception"}`:  0,
		`{input="cell",kind="decode"}`:     0,
		`{input="oven",kind="timeout"}`:    4,
		`{input="oven",kind="connection"}`: 0,
		`{input="oven",kind="exception"}`:  0,
		`{input="oven",kind="decode"}`:     1,
	}
	if !maps.Equal(errs.samples, wantErrs) {
		t.Errorf("input_read_errors_total = %v, want %v", errs.samples, wantErrs)
	}
}

func TestMetricsHaveNoHealthFamiliesWithoutAReportingInput(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)

	_, _, body := h.get(t, http.MethodGet, "/metrics", "")

	for _, name := range []string{"input_connected", "input_read_errors_total"} {
		if strings.Contains(body, "laraplate_edge_"+name) {
			t.Errorf("/metrics mentions %s although no input reports its health", name)
		}
	}
}

func TestMetricsHealthFamiliesFollowTransformErrors(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)
	h.src.set(healthStatus())

	_, _, body := h.get(t, http.MethodGet, "/metrics", "")

	var order []string
	for line := range strings.Lines(body) {
		if name, ok := strings.CutPrefix(line, "# HELP "); ok {
			name, _, _ = strings.Cut(name, " ")
			order = append(order, name)
		}
	}
	i := slices.Index(order, "laraplate_edge_transform_errors_total")
	if i < 0 || i+2 >= len(order) || order[i+1] != "laraplate_edge_input_connected" || order[i+2] != "laraplate_edge_input_read_errors_total" {
		t.Fatalf("families %q, want the health families right after laraplate_edge_transform_errors_total", order)
	}
}

func TestMetricsExposePlugins(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)
	h.src.set(pluginStatus())

	_, _, body := h.get(t, http.MethodGet, "/metrics", "")
	families := parseExposition(t, body)

	up := families["laraplate_edge_plugin_up"]
	if up == nil || up.typ != "gauge" {
		t.Fatalf("plugin_up = %+v, want a gauge", up)
	}
	if want := map[string]float64{`{plugin="demo"}`: 1, `{plugin="gone"}`: 0}; !maps.Equal(up.samples, want) {
		t.Errorf("plugin_up = %v, want %v", up.samples, want)
	}

	restarts := families["laraplate_edge_plugin_restarts_total"]
	if restarts == nil || restarts.typ != "counter" {
		t.Fatalf("plugin_restarts_total = %+v, want a counter", restarts)
	}
	want := map[string]float64{
		`{plugin="demo",reason="exit"}`:   1,
		`{plugin="demo",reason="health"}`: 0,
		`{plugin="demo",reason="memory"}`: 1,
		`{plugin="demo",reason="hash"}`:   0,
		`{plugin="gone",reason="exit"}`:   0,
		`{plugin="gone",reason="health"}`: 0,
		`{plugin="gone",reason="memory"}`: 0,
		`{plugin="gone",reason="hash"}`:   0,
	}
	if !maps.Equal(restarts.samples, want) {
		t.Errorf("plugin_restarts_total = %v, want %v", restarts.samples, want)
	}
}

func TestMetricsHaveNoPluginFamiliesWithoutPlugins(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)

	_, _, body := h.get(t, http.MethodGet, "/metrics", "")

	for _, name := range []string{"plugin_up", "plugin_restarts_total"} {
		if strings.Contains(body, "laraplate_edge_"+name) {
			t.Errorf("/metrics mentions %s although the configuration uses no plugin", name)
		}
	}
}
