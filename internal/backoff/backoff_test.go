package backoff

import (
	"math"
	"testing"
	"time"
)

func constant(v float64) func() float64 { return func() float64 { return v } }

func TestExponentialSequence(t *testing.T) {
	tests := []struct {
		name     string
		min, max time.Duration
		rand     float64
		want     []time.Duration
	}{
		{"lower bound", time.Second, time.Minute, 0, secs(0.5, 1, 2, 4, 8, 16, 30, 30)},
		{"upper bound", time.Second, time.Minute, 1, secs(1, 2, 4, 8, 16, 32, 60, 60)},
		{"cap not a power of two of min", time.Second, 5 * time.Minute, 1, secs(1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300)},
		{"draw below 0 is 0", time.Second, time.Minute, -3, secs(0.5, 1, 2)},
		{"draw above 1 is 1", time.Second, time.Minute, 7, secs(1, 2, 4)},
		{"NaN draw is 0", time.Second, time.Minute, math.NaN(), secs(0.5, 1, 2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New(tt.min, tt.max, constant(tt.rand))
			for i, want := range tt.want {
				if got := b.Next(); got != want {
					t.Fatalf("Next() #%d = %v, want %v", i+1, got, want)
				}
			}
		})
	}
}

func TestExponentialReset(t *testing.T) {
	b := New(time.Second, time.Minute, constant(1))
	for range 5 {
		b.Next()
	}

	b.Reset()

	if got := b.Next(); got != time.Second {
		t.Fatalf("Next() after Reset = %v, want 1s", got)
	}
}

func secs(v ...float64) []time.Duration {
	out := make([]time.Duration, len(v))
	for i, s := range v {
		out[i] = time.Duration(s * float64(time.Second))
	}

	return out
}
