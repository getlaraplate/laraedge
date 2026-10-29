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
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

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
	sdk.ServeInput(name, version, input{}, inputKind())
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

func inputKind() manifest.InputKind {
	return manifest.InputKind{
		ConnectionSchema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}`),
		AddressSchema:    json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string"}`),
		ReadModes:        []string{"poll"},
		Types:            []string{"int32"},
	}
}

// input is an Input that reads nothing.
type input struct{}

func (input) Configure(context.Context, string, json.RawMessage) []sdk.ConfigError { return nil }

func (input) Run(ctx context.Context, _ string, _ sdk.InputEmitter) error {
	<-ctx.Done()

	return nil
}

func (input) Probe(context.Context, string) ([]sdk.Sample, error) { return nil, nil }

func (input) Stop(context.Context, string) error { return nil }

// serveIgnoringShutdown serves Info and health like the SDK, but Shutdown
// leaves the process running.
func serveIgnoringShutdown() {
	kind := inputKind()
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
