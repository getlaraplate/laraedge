package file

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	edgeinput "github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
)

// fileInput is a file input on dir with the given points on device cmm.
func fileInput(t *testing.T, dir, format string, points ...config.Point) config.Input {
	t.Helper()

	raw, err := json.Marshal(map[string]any{"directory": dir, "format": format})
	if err != nil {
		t.Fatal(err)
	}

	return config.Input{ID: "cmm-line", Driver: config.DriverFile, Connection: raw, Devices: []config.Device{{Device: "cmm", Points: points}}}
}

func TestNewRefusesNamingTheInputAndTheField(t *testing.T) {
	dir := t.TempDir()
	poll := eventPoint("bore", "Bore", "float64")
	poll.Read = config.Read{Mode: config.ReadPoll}
	blank := eventPoint("bore", " ", "float64")

	for name, tt := range map[string]struct {
		cfg  config.Input
		want string
	}{
		"poll point":  {fileInput(t, dir, FormatCSV, eventPoint("ok", "Ok", "float64"), poll), `file input "cmm-line": devices.0.points.1: read: want event`},
		"blank point": {fileInput(t, dir, FormatCSV, blank), `file input "cmm-line": devices.0.points.0: address: required`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(tt.cfg, clock.Real(), Options{DataDir: t.TempDir()})
			if err == nil || err.Error() != tt.want {
				t.Fatalf("New = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestFactoryBuildsWithoutTouchingTheDisk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	dataDir := filepath.Join(t.TempDir(), "data")

	in, err := Factory(Options{DataDir: dataDir})(fileInput(t, dir, FormatCSV, eventPoint("bore", "Bore", "float64")), clock.Real())
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	if _, ok := in.(edgeinput.HealthReporter); !ok {
		t.Fatal("the file input reports no health")
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, dataDir} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s exists after the factory: %v", p, err)
		}
	}
}

func TestStartNeedsTheDataDir(t *testing.T) {
	in, err := New(fileInput(t, t.TempDir(), FormatCSV, eventPoint("bore", "Bore", "float64")), clock.NewFake(runEpoch), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close() })

	err = in.Start(t.Context(), func(model.Record) {})
	if err == nil || !strings.Contains(err.Error(), "data_dir") {
		t.Fatalf("Start without data_dir = %v, want an error naming data_dir", err)
	}
}

func TestStartTwiceAndAfterCloseAreRefused(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	h.start()
	if err := h.in.Start(t.Context(), h.emit); err == nil {
		t.Fatal("a second Start was accepted")
	}
	h.close()
	if err := h.in.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
	if err := h.in.Start(t.Context(), h.emit); err == nil {
		t.Fatal("Start after Close was accepted")
	}
}

func TestCloseOnAnInputNeverStarted(t *testing.T) {
	in, err := New(fileInput(t, t.TempDir(), FormatCSV, eventPoint("bore", "Bore", "float64")), clock.Real(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
