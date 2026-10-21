package driverkit

import (
	"maps"
	"sync"
	"testing"

	"github.com/getlaraplate/laraedge/internal/input"
)

var _ interface{ Snapshot() input.Health } = (*Health)(nil)

func TestHealthZeroValue(t *testing.T) {
	var h Health

	got := h.Snapshot()

	if got.Connected {
		t.Error("a new Health is connected")
	}
	if got.ReadErrors == nil || len(got.ReadErrors) != 0 {
		t.Errorf("ReadErrors = %#v, want an empty map", got.ReadErrors)
	}
}

func TestHealthRecordsConnectionAndErrors(t *testing.T) {
	var h Health

	h.SetConnected(true)
	h.CountError(input.ReadErrorTimeout)
	h.CountError(input.ReadErrorTimeout)
	h.CountError(input.ReadErrorDecode)

	got := h.Snapshot()
	if !got.Connected {
		t.Error("Connected = false after SetConnected(true)")
	}
	want := map[string]uint64{"timeout": 2, "decode": 1}
	if !maps.Equal(got.ReadErrors, want) {
		t.Errorf("ReadErrors = %v, want %v", got.ReadErrors, want)
	}

	h.SetConnected(false)
	if h.Snapshot().Connected {
		t.Error("Connected = true after SetConnected(false)")
	}
}

func TestHealthSnapshotIsACopy(t *testing.T) {
	var h Health
	h.CountError("connection")

	snap := h.Snapshot()
	snap.ReadErrors["connection"] = 99
	h.CountError("connection")

	if got := h.Snapshot().ReadErrors["connection"]; got != 2 {
		t.Fatalf("connection = %d, want 2", got)
	}
	if snap.ReadErrors["connection"] != 99 {
		t.Error("a later count changed an earlier snapshot")
	}
}

func TestHealthIsSafeForConcurrentUse(t *testing.T) {
	var h Health
	var wg sync.WaitGroup

	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 100 {
				h.CountError("timeout")
				h.SetConnected(true)
			}
		}()
		go func() {
			defer wg.Done()
			for range 100 {
				_ = h.Snapshot()
			}
		}()
	}
	wg.Wait()

	if got := h.Snapshot().ReadErrors["timeout"]; got != 800 {
		t.Fatalf("timeout = %d, want 800", got)
	}
}
