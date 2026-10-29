// Command plugin is the test plugin of the plugin host tests, built on the
// SDK. The host gives it a minimal environment, so its behaviour comes from
// behaviour.json next to its binary, read at every start:
//
//	crash_at_start   exit 1 before the handshake
//	stderr_lines     write that many lines on stderr before the handshake
//	env_file         write the environment, one KEY=value per line, to this
//	                 file next to the binary
//	ignore_shutdown  serve Info and health without the SDK server, with a
//	                 Shutdown that answers and does nothing
//	ignore_sigterm   ignore SIGTERM
//	pid_file         write the process id to this file next to the binary
//	block_before_handshake  block forever instead of the handshake
//
// Input behaviour, per instance:
//
//	script        events the stream sends at every Subscribe, in order
//	probe         the samples Probe returns
//	refuse_config Configure refuses with a ConfigError at connection.port
//	refuse_echo   the refusal message quotes connection.token
//	config_file   write the configuration of every Configure to this file
//	trace_file    append "configure <id>" and "run <id>" lines to this file
//	browse        offer browse (manifest and service)
//	probe_block   Probe blocks until its context ends
//	busy_runs     the first N Run calls of the process answer ALREADY_EXISTS at once
//	configure_block  Configure blocks until its context ends, then refuses
//
// trace_file also gets "stop <id>" lines.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/getlaraplate/laraedge-sdk/handshake"
	"github.com/getlaraplate/laraedge-sdk/manifest"
	pb "github.com/getlaraplate/laraedge-sdk/pluginv1"
	"github.com/getlaraplate/laraedge-sdk/sdk"
)

const (
	name    = "testplugin"
	version = "0.1.0"
)

type behaviour struct {
	CrashAtStart   bool   `json:"crash_at_start"`
	StderrLines    int    `json:"stderr_lines"`
	EnvFile        string `json:"env_file"`
	IgnoreShutdown bool   `json:"ignore_shutdown"`
	IgnoreSIGTERM  bool   `json:"ignore_sigterm"`
	PIDFile        string `json:"pid_file"`
	Block          bool   `json:"block_before_handshake"`

	Script       []event  `json:"script"`
	Probe        []sample `json:"probe"`
	RefuseConfig bool     `json:"refuse_config"`
	RefuseEcho   bool     `json:"refuse_echo"`
	ConfigFile   string   `json:"config_file"`
	TraceFile    string   `json:"trace_file"`
	Browse       bool     `json:"browse"`
	ProbeBlock   bool     `json:"probe_block"`
	ConfigBlock  bool     `json:"configure_block"`
	BusyRuns     int64    `json:"busy_runs"`
}

// event is one event of the script: exactly one field is set.
type event struct {
	Sample   *sample `json:"sample"`
	Answered string  `json:"answered"`
	Health   *struct {
		Connected bool   `json:"connected"`
		ReadError string `json:"read_error"`
	} `json:"health"`
}

// sample is an integer sample; quality is good unless "bad" is given.
type sample struct {
	Device  string `json:"device"`
	Signal  string `json:"signal"`
	Value   int64  `json:"value"`
	TS      string `json:"ts"`
	Quality string `json:"quality"`
}

func main() {
	b, dir := readBehaviour()
	if b.IgnoreSIGTERM {
		signal.Ignore(syscall.SIGTERM)
	}
	if b.PIDFile != "" {
		if err := os.WriteFile(filepath.Join(dir, b.PIDFile), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			fail(err)
		}
	}
	if b.EnvFile != "" {
		env := strings.Join(os.Environ(), "\n") + "\n"
		if err := os.WriteFile(filepath.Join(dir, b.EnvFile), []byte(env), 0o600); err != nil {
			fail(err)
		}
	}
	for i := range b.StderrLines {
		fmt.Fprintf(os.Stderr, "flood line %d of the test plugin, padded to make the pipe fill up quickly\n", i)
	}
	if b.CrashAtStart {
		fmt.Fprintln(os.Stderr, "crashing at start")
		os.Exit(1)
	}
	if b.Block {
		for {
			time.Sleep(time.Hour)
		}
	}
	if b.IgnoreShutdown {
		serveIgnoringShutdown()

		return
	}
	in := input{b: b, dir: dir, runs: new(atomic.Int64)}
	if b.Browse {
		sdk.ServeInput(name, version, browsingInput{in}, inputKind(b))

		return
	}
	sdk.ServeInput(name, version, in, inputKind(b))
}

func readBehaviour() (behaviour, string) {
	exe, err := os.Executable()
	if err != nil {
		fail(err)
	}
	dir := filepath.Dir(exe)
	var b behaviour
	data, err := os.ReadFile(filepath.Join(dir, "behaviour.json"))
	if errors.Is(err, os.ErrNotExist) {
		return b, dir
	}
	if err != nil {
		fail(err)
	}
	if err := json.Unmarshal(data, &b); err != nil {
		fail(err)
	}

	return b, dir
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func inputKind(b behaviour) manifest.InputKind {
	return manifest.InputKind{
		ConnectionSchema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}`),
		AddressSchema:    json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string"}`),
		ReadModes:        []string{"poll"},
		Types:            []string{"int32"},
		Browse:           b.Browse,
	}
}

// input is an Input driven by the behaviour file.
type input struct {
	b    behaviour
	dir  string
	runs *atomic.Int64
}

// browsingInput is an input that also browses.
type browsingInput struct{ input }

func (browsingInput) Browse(_ context.Context, _, node string, depth int) ([]sdk.Node, error) {
	return []sdk.Node{{Address: fmt.Sprintf("%s/%d", node, depth), Name: "n1", Type: "int32", Access: "read"}}, nil
}

func (in input) trace(line string) {
	if in.b.TraceFile == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(in.dir, in.b.TraceFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fail(err)
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

func (in input) Configure(ctx context.Context, id string, cfg json.RawMessage) []sdk.ConfigError {
	in.trace("configure " + id)
	if in.b.ConfigBlock {
		<-ctx.Done()

		return []sdk.ConfigError{{Message: "interrupted"}}
	}
	if in.b.ConfigFile != "" {
		if err := os.WriteFile(filepath.Join(in.dir, in.b.ConfigFile), cfg, 0o600); err != nil {
			fail(err)
		}
	}
	if !in.b.RefuseConfig {
		return nil
	}
	message := "must be an integer"
	if in.b.RefuseEcho {
		var c struct {
			Connection struct {
				Token string `json:"token"`
			} `json:"connection"`
		}
		_ = json.Unmarshal(cfg, &c)
		message = "bad value " + c.Connection.Token
	}

	return []sdk.ConfigError{{Path: "connection.port", Message: message}}
}

func (in input) Run(ctx context.Context, id string, out sdk.InputEmitter) error {
	if in.b.BusyRuns > 0 && in.runs.Add(1) <= in.b.BusyRuns {
		return status.Error(codes.AlreadyExists, "busy")
	}
	in.trace("run " + id)
	for _, e := range in.b.Script {
		var err error
		switch {
		case e.Sample != nil:
			err = out.Sample(e.Sample.sdk())
		case e.Answered != "":
			err = out.Answered(e.Answered)
		case e.Health != nil:
			err = out.Health(e.Health.Connected, sdk.ReadError(e.Health.ReadError))
		}
		if err != nil {
			return err
		}
	}
	<-ctx.Done()

	return nil
}

func (s sample) sdk() sdk.Sample {
	ts, err := time.Parse(time.RFC3339, s.TS)
	if err != nil {
		fail(err)
	}
	quality := sdk.QualityGood
	if s.Quality == "bad" {
		quality = sdk.QualityBad
	}

	return sdk.Sample{Device: s.Device, Signal: s.Signal, TS: ts, Value: sdk.IntValue(s.Value), Quality: quality, Priority: sdk.PriorityCritical}
}

func (in input) Probe(ctx context.Context, id string) ([]sdk.Sample, error) {
	in.trace("probe " + id)
	if in.b.ProbeBlock {
		<-ctx.Done()

		return nil, ctx.Err()
	}
	out := make([]sdk.Sample, 0, len(in.b.Probe))
	for _, s := range in.b.Probe {
		out = append(out, s.sdk())
	}

	return out, nil
}

func (in input) Stop(_ context.Context, id string) error {
	in.trace("stop " + id)

	return nil
}

// serveIgnoringShutdown serves Info and health like the SDK, but Shutdown
// leaves the process running.
func serveIgnoringShutdown() {
	kind := inputKind(behaviour{})
	m := manifest.Manifest{Name: name, Version: version, ProtocolVersions: []string{"v1"}, Kinds: manifest.Kinds{Input: &kind}}
	raw, err := json.Marshal(m)
	if err != nil {
		fail(err)
	}
	canonical, err := manifest.Canonical(raw)
	if err != nil {
		fail(err)
	}
	lis, _, err := handshake.Listen([]string{"v1"})
	if err != nil {
		fail(err)
	}
	server := grpc.NewServer()
	pb.RegisterPluginServer(server, &stubborn{manifest: canonical})
	healthpb.RegisterHealthServer(server, health.NewServer())
	if err := server.Serve(lis); err != nil {
		fail(err)
	}
}

type stubborn struct {
	pb.UnimplementedPluginServer
	manifest []byte
}

func (s *stubborn) Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{Name: name, Version: version, ProtocolVersions: []string{"v1"}, Manifest: s.manifest}, nil
}

func (s *stubborn) Shutdown(context.Context, *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	return &pb.ShutdownResponse{}, nil
}
