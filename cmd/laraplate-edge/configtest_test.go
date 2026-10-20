package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/getlaraplate/laraplate-edge/internal/config"
	"github.com/getlaraplate/laraplate-edge/internal/transform"
)

// chainDoc returns a document with one simulated input, one device and two
// points: force, a float64 carrying chain (a flow YAML list), and count, a
// uint32 without a chain.
func chainDoc(chain string) string {
	return `schema: laraplate-edge-config/1
version: 1
inputs:
  - id: line
    driver: simulated
    connection: {scenario_file: scenario.yaml}
    devices:
      - device: press-07
        points:
          - {signal: force, address: force, type: float64, read: subscribe, transforms: ` + chain + `}
          - {signal: count, address: count, type: uint32, read: subscribe}
outputs:
  - {id: main, type: laraplate-http, url: 'http://127.0.0.1:8000', token: secret://env/TOKEN}
`
}

const scaleDeadbandSamples = `{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:00.000Z","value":20,"quality":"good"}
{"device":"press-07","signal":"count","ts":"2026-10-19T08:00:00.500Z","value":5,"quality":"good"}
{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:01.000Z","value":21,"quality":"good"}
{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:02.000Z","value":24,"quality":"good"}
{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:03.000Z","value":25,"quality":"good"}
{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:04.000Z","value":25,"quality":"bad"}
`

// scaleDeadbandWant is scaleDeadbandSamples through scale 0.5 and deadband 1:
// 10 passes, 10.5 and 12.5 move less than 1, 12 passes, the bad sample passes
// uncomputed, the count has no chain.
const scaleDeadbandWant = `{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:00.000Z","value":10,"quality":"good"}
{"device":"press-07","signal":"count","ts":"2026-10-19T08:00:00.500Z","value":5,"quality":"good"}
{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:02.000Z","value":12,"quality":"good"}
{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:04.000Z","value":25,"quality":"bad"}
`

func TestConfigTest(t *testing.T) {
	tests := []struct {
		name    string
		chain   string
		samples string
		want    string
	}{
		{
			name:    "scale then deadband, bad sample uncomputed, point without chain untouched",
			chain:   `[{scale: 0.5}, {deadband: 1}]`,
			samples: scaleDeadbandSamples,
			want:    scaleDeadbandWant,
		},
		{
			name:  "a held sample leaves at the end of the file",
			chain: `[{min_interval: 10s}]`,
			samples: `{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:00.000Z","value":1,"quality":"good"}
{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:01.000Z","value":2,"quality":"good"}
{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:02.000Z","value":3,"quality":"good"}
`,
			want: `{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:00.000Z","value":1,"quality":"good"}
{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:02.000Z","value":3,"quality":"good"}
`,
		},
		{
			name:  "a debounced sample leaves when the clock reaches its deadline, before the next sample",
			chain: `[{debounce: 2s}]`,
			samples: `{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:00.000Z","value":1,"quality":"good"}
{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:05.000Z","value":2,"quality":"good"}
`,
			want: `{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:00.000Z","value":1,"quality":"good"}
{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:05.000Z","value":2,"quality":"good"}
`,
		},
		{
			name:    "an empty sample file gives an empty output",
			chain:   `[{scale: 2}]`,
			samples: "",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			doc := filepath.Join(dir, "config.yaml")
			samples := filepath.Join(dir, "samples.jsonl")
			writeFile(t, doc, chainDoc(tt.chain))
			writeFile(t, samples, tt.samples)

			var first, second, stderr bytes.Buffer

			if code := run([]string{"config", "test", doc, "--input", samples}, &first, &stderr); code != exitOK {
				t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
			}

			if first.String() != tt.want {
				t.Errorf("output:\n%s\nwant:\n%s", first.String(), tt.want)
			}

			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}

			if code := run([]string{"config", "test", "--input", samples, doc}, &second, &stderr); code != exitOK {
				t.Fatalf("second run: exit code = %d (stderr: %q)", code, stderr.String())
			}

			if second.String() != first.String() {
				t.Errorf("two runs differ:\n%s\n%s", first.String(), second.String())
			}
		})
	}
}

func TestConfigTestFailures(t *testing.T) {
	const secret = "s3cr3t-reading"

	good := `{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:00.000Z","value":1,"quality":"good"}` + "\n"

	tests := []struct {
		name       string
		doc        string
		samples    string
		args       func(doc, samples string) []string
		wantCode   int
		wantStderr []string
	}{
		{
			name:       "a malformed line",
			doc:        chainDoc(`[{scale: 2}]`),
			samples:    good + `{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:01.000Z","value":"` + secret + `","quality":"good"}` + "\n",
			wantCode:   exitFailure,
			wantStderr: []string{"samples.jsonl", "line 2"},
		},
		{
			name:       "an unknown signal",
			doc:        chainDoc(`[{scale: 2}]`),
			samples:    good + `{"device":"press-07","signal":"` + secret + `","ts":"2026-10-19T08:00:01.000Z","value":1,"quality":"good"}` + "\n",
			wantCode:   exitFailure,
			wantStderr: []string{"line 2", "unknown device or signal"},
		},
		{
			name:       "a chain that does not type-check",
			doc:        chainDoc(`[{bit: 3}]`),
			samples:    good,
			wantCode:   exitFailure,
			wantStderr: []string{"config.yaml:", "step 1 (bit)"},
		},
		{
			name:       "a document the build does not provide",
			doc:        strings.Replace(chainDoc(`[{scale: 2}]`), "driver: simulated", "driver: modbus", 1),
			samples:    good,
			wantCode:   exitFailure,
			wantStderr: []string{"config.yaml:", "not available in this build"},
		},
		{
			name: "a missing sample file",
			doc:  chainDoc(`[{scale: 2}]`),
			args: func(doc, samples string) []string {
				return []string{"config", "test", doc, "--input", samples + ".missing"}
			},
			wantCode:   exitFailure,
			wantStderr: []string{"samples.jsonl.missing"},
		},
		{
			name:    "a missing document",
			doc:     chainDoc(`[{scale: 2}]`),
			samples: good,
			args: func(doc, samples string) []string {
				return []string{"config", "test", doc + ".missing", "--input", samples}
			},
			wantCode:   exitFailure,
			wantStderr: []string{"config.yaml.missing"},
		},
		{
			name:       "no --input",
			doc:        chainDoc(`[{scale: 2}]`),
			samples:    good,
			args:       func(doc, _ string) []string { return []string{"config", "test", doc} },
			wantCode:   exitUsage,
			wantStderr: []string{"--input"},
		},
		{
			name:       "no document",
			doc:        chainDoc(`[{scale: 2}]`),
			samples:    good,
			args:       func(_, samples string) []string { return []string{"config", "test", "--input", samples} },
			wantCode:   exitUsage,
			wantStderr: []string{"usage:"},
		},
		{
			name:       "two documents",
			doc:        chainDoc(`[{scale: 2}]`),
			samples:    good,
			args:       func(doc, samples string) []string { return []string{"config", "test", doc, doc, "--input", samples} },
			wantCode:   exitUsage,
			wantStderr: []string{"usage:"},
		},
		{
			name:    "an unknown flag",
			doc:     chainDoc(`[{scale: 2}]`),
			samples: good,
			args: func(doc, samples string) []string {
				return []string{"config", "test", doc, "--input", samples, "--nope"}
			},
			wantCode:   exitUsage,
			wantStderr: []string{"nope"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			doc := filepath.Join(dir, "config.yaml")
			samples := filepath.Join(dir, "samples.jsonl")
			writeFile(t, doc, tt.doc)
			writeFile(t, samples, tt.samples)

			args := []string{"config", "test", doc, "--input", samples}
			if tt.args != nil {
				args = tt.args(doc, samples)
			}

			var stdout, stderr bytes.Buffer

			code := run(args, &stdout, &stderr)

			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %q)", code, tt.wantCode, stderr.String())
			}

			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty on failure", stdout.String())
			}

			for _, want := range tt.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
			}

			if strings.Contains(stderr.String(), secret) {
				t.Errorf("stderr echoes a value: %q", stderr.String())
			}
		})
	}
}

func TestConfigValidateReportsAChainTypeError(t *testing.T) {
	doc := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, doc, chainDoc(`[{scale: 2}, {bit: 3}]`))

	var stdout, stderr bytes.Buffer

	if code := run([]string{"config", "validate", doc}, &stdout, &stderr); code != exitFailure {
		t.Fatalf("exit code = %d, want 1 (stderr: %q)", code, stderr.String())
	}

	for _, want := range []string{doc + ": inputs.0.devices.0.points.0.transforms:", "step 2 (bit)"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
		}
	}

	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}

	writeFile(t, doc, chainDoc(`[{scale: 2}, {round: 1}]`))

	stdout.Reset()
	stderr.Reset()

	if code := run([]string{"config", "validate", doc}, &stdout, &stderr); code != exitOK {
		t.Fatalf("a valid chain: exit code = %d (stderr: %q)", code, stderr.String())
	}
}

func parseChainDoc(t *testing.T, chain string) config.Document {
	t.Helper()

	doc, msgs, err := config.Parse([]byte(chainDoc(chain)))
	if err != nil || len(msgs) > 0 {
		t.Fatalf("Parse: %v %v", err, msgs)
	}

	return doc
}

func TestReplayStopsAtTheFlushBound(t *testing.T) {
	doc := parseChainDoc(t, `[{heartbeat: 1s}]`)
	in, err := transform.ReadSamples(strings.NewReader(
		`{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:00.000Z","value":1,"quality":"good"}`+"\n"), pointKinds(doc))
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = replay(doc, in, 10)
	if !errors.Is(err, errFlushBound) {
		t.Fatalf("err = %v, want errFlushBound", err)
	}

	// The same chain settles when the bound is large enough for a bounded
	// chain: min_interval has one pending sample, one fire.
	bounded := parseChainDoc(t, `[{min_interval: 5s}]`)
	two := `{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:00.000Z","value":1,"quality":"good"}` + "\n" +
		`{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:01.000Z","value":2,"quality":"good"}` + "\n"

	samples, err := transform.ReadSamples(strings.NewReader(two), pointKinds(bounded))
	if err != nil {
		t.Fatal(err)
	}

	out, _, err := replay(bounded, samples, 1)
	if err != nil || len(out) != 2 {
		t.Fatalf("replay = %d samples, %v; want 2, nil", len(out), err)
	}
}

// TestReplayAppliesAnEarlierSampleAtTheCurrentTime checks ruling R4: a sample
// older than the virtual clock does not move the clock back; the chain sees
// it with its own ts and the held sample still leaves at the end.
func TestReplayAppliesAnEarlierSampleAtTheCurrentTime(t *testing.T) {
	doc := parseChainDoc(t, `[{min_interval: 1s}]`)
	in := `{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:10.000Z","value":1,"quality":"good"}` + "\n" +
		`{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:05.000Z","value":2,"quality":"good"}` + "\n"

	samples, err := transform.ReadSamples(strings.NewReader(in), pointKinds(doc))
	if err != nil {
		t.Fatal(err)
	}

	out, _, err := replay(doc, samples, 100)
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, s := range out {
		got = append(got, s.TS.Format("15:04:05")+"="+strconv.FormatFloat(s.Value.Float, 'g', -1, 64))
	}

	if want := "08:00:10=1,08:00:05=2"; strings.Join(got, ",") != want {
		t.Errorf("replay = %v, want %s", got, want)
	}
}

func TestConfigTestDoesNotTouchTheFilesystem(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "config.yaml")
	samples := filepath.Join(dir, "samples.jsonl")
	writeFile(t, doc, chainDoc(`[{scale: 0.5}, {deadband: 1}]`))
	writeFile(t, samples, scaleDeadbandSamples)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "test", doc, "--input", samples}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d (stderr: %q)", code, stderr.String())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 2 {
		t.Errorf("directory holds %d entries, want the 2 inputs only", len(entries))
	}
}
