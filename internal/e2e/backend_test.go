//go:build e2e && unix

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ingestMode is how the fake backend answers the ingest route.
type ingestMode int

const (
	// modeAccept accepts a new message (202) and answers a known one as a
	// duplicate (200).
	modeAccept ingestMode = iota
	// modeUnavailable answers 503.
	modeUnavailable
	// modeReset closes the connection without an answer.
	modeReset
	// modeHold keeps the request unanswered and accepts nothing.
	modeHold
	// modeAcceptThenHold accepts the message, then keeps the request
	// unanswered: the agent never learns it was accepted.
	modeAcceptThenHold
)

// Outcomes of an ingest attempt.
const (
	outcomeAccepted    = "accepted"
	outcomeDuplicate   = "duplicate"
	outcomeUnavailable = "unavailable"
	outcomeReset       = "reset"
	outcomeHeld        = "held"
)

// envelope is what the fake backend reads of an accepted envelope.
type envelope struct {
	MessageID string `json:"message_id"`
	SourceSeq uint64 `json:"source_seq"`
	Devices   []struct {
		Device  string `json:"device"`
		Type    string `json:"type"`
		Samples []struct {
			Signal string          `json:"signal"`
			TS     time.Time       `json:"ts"`
			Value  json.RawMessage `json:"value"`
		} `json:"samples"`
	} `json:"devices"`
	Losses []json.RawMessage `json:"losses"`
}

type attempt struct {
	messageID string
	seq       uint64
	outcome   string
}

type configGet struct {
	version string
	status  int
}

type report struct {
	Version      int64    `json:"version"`
	Outcome      string   `json:"outcome"`
	Errors       []string `json:"errors"`
	AgentVersion string   `json:"agent_version"`
}

// backend is a fake Laraplate on plain HTTP over loopback: the ingest
// route, validating every body against the backend envelope schema and
// checking that a message_id always comes with the same body and seq, the
// configuration route and the report route.
type backend struct {
	srv     *httptest.Server
	schema  *jsonschema.Schema
	release chan struct{}

	mu         sync.Mutex
	mode       ingestMode
	attempts   []attempt
	held       []string
	bodyOf     map[string][]byte
	seqOf      map[string]uint64
	idOfSeq    map[uint64]string
	accepted   map[uint64]envelope
	acceptedID map[string]bool
	duplicates map[string]int
	violations []string

	docs       map[int64]json.RawMessage
	active     int64
	configGets []configGet
	reports    []report
}

func newBackend(t *testing.T) *backend {
	t.Helper()

	b := &backend{
		schema:     compileSchema(t),
		release:    make(chan struct{}),
		bodyOf:     map[string][]byte{},
		seqOf:      map[string]uint64{},
		idOfSeq:    map[uint64]string{},
		accepted:   map[uint64]envelope{},
		acceptedID: map[string]bool{},
		duplicates: map[string]int{},
		docs:       map[int64]json.RawMessage{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+ingestPath, b.ingest)
	mux.HandleFunc("GET "+configPath, b.config)
	mux.HandleFunc("POST "+reportPath, b.report)
	b.srv = httptest.NewUnstartedServer(b.authorized(mux))
	b.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	b.srv.Start()
	t.Cleanup(func() {
		close(b.release)
		b.srv.Close()
	})

	return b
}

func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	raw, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource("laraplate-machine-1.schema.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile("laraplate-machine-1.schema.json")
	if err != nil {
		t.Fatal(err)
	}

	return schema
}

func (b *backend) url() string {
	return b.srv.URL
}

func (b *backend) violate(format string, args ...any) {
	b.violations = append(b.violations, fmt.Sprintf(format, args...))
}

// authorized answers 401 to a request without the agent token.
func (b *backend) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+agentToken {
			b.mu.Lock()
			b.violate("%s %s without the agent token", r.Method, r.URL.Path)
			b.mu.Unlock()
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Unauthenticated."})

			return
		}
		next.ServeHTTP(w, r)
	})
}

func (b *backend) ingest(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		return
	}

	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err == nil {
		err = b.schema.Validate(inst)
	}
	if err != nil {
		b.mu.Lock()
		b.violate("an envelope fails the backend schema: %v\n%s", err, body)
		b.mu.Unlock()
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "The envelope is invalid.", "errors": map[string]any{"body": []string{err.Error()}}})

		return
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		b.mu.Lock()
		b.violate("an envelope does not decode: %v", err)
		b.mu.Unlock()
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "The envelope is invalid."})

		return
	}
	id, seq := env.MessageID, env.SourceSeq

	b.mu.Lock()
	if first, ok := b.bodyOf[id]; ok && !bytes.Equal(first, body) {
		b.violate("message %s resent with a different body", id)
	} else if !ok {
		b.bodyOf[id] = body
	}
	if s, ok := b.seqOf[id]; ok && s != seq {
		b.violate("message %s sent with source_seq %d, then %d", id, s, seq)
	}
	b.seqOf[id] = seq
	if other, ok := b.idOfSeq[seq]; ok && other != id {
		b.violate("source_seq %d sent as message %s, then as message %s", seq, other, id)
	}
	b.idOfSeq[seq] = id

	record := func(outcome string) {
		b.attempts = append(b.attempts, attempt{messageID: id, seq: seq, outcome: outcome})
	}
	switch b.mode {
	case modeUnavailable:
		record(outcomeUnavailable)
		b.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"message": "Service Unavailable"})

		return
	case modeReset:
		record(outcomeReset)
		b.mu.Unlock()
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}

		return
	case modeHold:
		record(outcomeHeld)
		b.held = append(b.held, id)
		b.mu.Unlock()
		b.hold(r)

		return
	}

	if b.acceptedID[id] {
		record(outcomeDuplicate)
		b.duplicates[id]++
		b.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "duplicate", "duplicate": true, "message_id": id})

		return
	}
	record(outcomeAccepted)
	b.accepted[seq] = env
	b.acceptedID[id] = true
	if b.mode == modeAcceptThenHold {
		b.held = append(b.held, id)
		b.mu.Unlock()
		b.hold(r)

		return
	}
	b.mu.Unlock()
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "message_id": id})
}

// hold waits until the client gives the request up or the backend closes.
func (b *backend) hold(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-b.release:
	}
}

// config serves the active document: 304 when ?version= is the active
// version, 404 when there is none (MachineAgentConfigController).
func (b *backend) config(w http.ResponseWriter, r *http.Request) {
	version := r.URL.Query().Get("version")

	b.mu.Lock()
	active, doc := b.active, b.docs[b.active]
	status := http.StatusOK
	switch {
	case active == 0:
		status = http.StatusNotFound
	case version == strconv.FormatInt(active, 10):
		status = http.StatusNotModified
	}
	b.configGets = append(b.configGets, configGet{version: version, status: status})
	b.mu.Unlock()

	switch status {
	case http.StatusNotFound:
		writeJSON(w, status, map[string]any{"message": "No active configuration."})
	case http.StatusNotModified:
		w.WriteHeader(status)
	default:
		writeJSON(w, status, map[string]any{"version": active, "document": doc})
	}
}

// report records a configuration report, validated with the backend rules
// (MachineAgentConfigReportController): 201, or 422 with no record.
func (b *backend) report(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return
	}

	var fields map[string]json.RawMessage
	var rep report
	problem := ""
	switch {
	case json.Unmarshal(body, &fields) != nil:
		problem = "not a JSON object"
	case json.Unmarshal(fields["version"], &rep.Version) != nil || rep.Version < 1:
		problem = "version is not a positive integer"
	case json.Unmarshal(fields["outcome"], &rep.Outcome) != nil || !slices.Contains([]string{"applied", "rejected", "rolled_back"}, rep.Outcome):
		problem = "outcome is not applied, rejected or rolled_back"
	case fields["errors"] != nil && json.Unmarshal(fields["errors"], &rep.Errors) != nil:
		problem = "errors is not null or a list of strings"
	case len(rep.Errors) > 100:
		problem = "more than 100 errors"
	case json.Unmarshal(fields["agent_version"], &rep.AgentVersion) != nil || rep.AgentVersion == "" || len([]rune(rep.AgentVersion)) > 64:
		problem = "agent_version is not a string of 1 to 64 characters"
	}
	for _, e := range rep.Errors {
		if len([]rune(e)) > 1000 {
			problem = "an error longer than 1000 characters"
		}
	}

	b.mu.Lock()
	if problem == "" {
		if _, published := b.docs[rep.Version]; !published {
			problem = "version is not published"
		}
	}
	if problem != "" {
		b.violate("an invalid configuration report: %s: %s", problem, body)
		b.mu.Unlock()
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "The report is invalid."})

		return
	}
	b.reports = append(b.reports, rep)
	id := len(b.reports)
	b.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"status": "recorded", "id": id})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// publish makes doc the active version.
func (b *backend) publish(version int64, doc json.RawMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.docs[version] = doc
	b.active = version
}

func (b *backend) setMode(m ingestMode) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.mode = m
}

func (b *backend) acceptedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.accepted)
}

func (b *backend) attemptsWith(outcome string) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := 0
	for _, a := range b.attempts {
		if a.outcome == outcome {
			n++
		}
	}

	return n
}

// refusedIDs returns the messages refused so far and not accepted.
func (b *backend) refusedIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	var ids []string
	for _, a := range b.attempts {
		if (a.outcome == outcomeUnavailable || a.outcome == outcomeReset) && !b.acceptedID[a.messageID] && !slices.Contains(ids, a.messageID) {
			ids = append(ids, a.messageID)
		}
	}

	return ids
}

func (b *backend) maxAttemptedSeq() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	var top uint64
	for _, a := range b.attempts {
		top = max(top, a.seq)
	}

	return top
}

func (b *backend) allAccepted(ids []string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, id := range ids {
		if !b.acceptedID[id] {
			return false
		}
	}

	return true
}

func (b *backend) heldIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return slices.Clone(b.held)
}

func (b *backend) duplicatesOf(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.duplicates[id]
}

func (b *backend) maxAttemptsOfOneMessage() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	count := map[string]int{}
	top := 0
	for _, a := range b.attempts {
		count[a.messageID]++
		top = max(top, count[a.messageID])
	}

	return top
}

func (b *backend) reported(version int64, outcome string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return slices.ContainsFunc(b.reports, func(r report) bool { return r.Version == version && r.Outcome == outcome })
}

func (b *backend) configGetsWith(version string, status int) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := 0
	for _, g := range b.configGets {
		if g.version == version && g.status == status {
			n++
		}
	}

	return n
}

// waitFor polls cond until it holds; it fails at once when the agent exits
// and after guard otherwise.
func (b *backend) waitFor(t *testing.T, a *agent, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(guard)
	for !cond() {
		if a.done() {
			t.Fatalf("the agent exited (%v) while waiting for %s\nstderr:\n%s\nlogs:\n%s", a.exitErr, what, a.stderr.String(), a.stdout.String())
		}
		if time.Now().After(deadline) {
			b.mu.Lock()
			violations := slices.Clone(b.violations)
			b.mu.Unlock()
			t.Fatalf("waited %s for %s; violations: %q\nlogs:\n%s", guard, what, violations, a.stdout.String())
		}
		time.Sleep(pollEvery)
	}
}

// checkReports wants exactly the given number of applied reports per
// version, each with the agent version and no error.
func (b *backend) checkReports(t *testing.T, want map[int64]int) {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()

	got := map[int64]int{}
	for _, r := range b.reports {
		got[r.Version]++
		if r.Outcome != "applied" || r.AgentVersion != agentVersion || len(r.Errors) != 0 {
			t.Errorf("report %+v, want applied by %s without errors", r, agentVersion)
		}
	}
	for v, n := range want {
		if got[v] != n {
			t.Errorf("reports of version %d = %d, want %d", v, got[v], n)
		}
	}
}

// checkDelivery checks what the backend accepted from the runs of one
// data_dir: no violation (every body valid against the schema, a message_id
// always with the same body and seq, a seq never reused), every source_seq
// from 0 to the highest accepted once, no loss marker, the births of every
// run, and every counter of every device of every run arriving as 0, 1, 2,
// ... with no value lost or repeated. A sample belongs to the run whose start
// precedes its ts.
func (b *backend) checkDelivery(t *testing.T, runs []*agent) {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()

	for _, v := range b.violations {
		t.Error(v)
	}

	n := len(b.accepted)
	if n == 0 {
		t.Fatal("the backend accepted nothing")
	}
	starts := make([]time.Time, len(runs))
	for i, a := range runs {
		starts[i] = a.started.Truncate(time.Millisecond)
	}

	type key struct {
		run            int
		device, signal string
	}
	counters := map[key][]int64{}
	births := 0
	for seq := range uint64(n) {
		env, ok := b.accepted[seq]
		if !ok {
			t.Errorf("source_seq %d was never accepted, yet %d batches were: a gap", seq, n)

			continue
		}
		if len(env.Losses) > 0 {
			t.Errorf("batch %d carries loss markers: %s", seq, env.Losses)
		}
		for _, dev := range env.Devices {
			if dev.Type == "birth" {
				births++
			}
			for _, s := range dev.Samples {
				if s.Signal != "good_count" && s.Signal != "scrap_count" {
					continue
				}
				v, err := strconv.ParseInt(string(s.Value), 10, 64)
				if err != nil {
					t.Errorf("batch %d: %s %s = %s, want an integer", seq, dev.Device, s.Signal, s.Value)

					continue
				}
				run := 0
				for i, start := range starts {
					if !s.TS.Before(start) {
						run = i
					}
				}
				k := key{run, dev.Device, s.Signal}
				counters[k] = append(counters[k], v)
			}
		}
	}

	parts := 0
	for _, values := range counters {
		parts += max(len(values)-1, 0)
	}
	if parts == 0 {
		t.Errorf("no part completed: the counter check proves nothing")
	}
	if births < len(runs)*len(devices) {
		t.Errorf("births = %d, want at least %d (each device at each start)", births, len(runs)*len(devices))
	}
	for run := range runs {
		for _, d := range devices {
			for _, signal := range []string{"good_count", "scrap_count"} {
				values := counters[key{run, d, signal}]
				if len(values) == 0 {
					t.Errorf("run %d: no %s of %s, want at least its initial 0", run+1, signal, d)
				}
				for i, v := range values {
					if v != int64(i) {
						t.Errorf("run %d: %s of %s arrived as %v, want 0, 1, 2, ...: a sample lost or repeated", run+1, signal, d, values)

						break
					}
				}
			}
		}
	}
}
