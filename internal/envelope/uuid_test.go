package envelope_test

import (
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/envelope"
)

var testStart = time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)

// idParts decodes a UUID string into its raw 16 bytes.
func idParts(t *testing.T, id string) [16]byte {
	t.Helper()

	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("malformed UUID %q", id)
	}

	if id != strings.ToLower(id) {
		t.Fatalf("UUID %q is not lowercase", id)
	}

	raw, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil {
		t.Fatalf("UUID %q is not hex: %v", id, err)
	}

	var out [16]byte
	copy(out[:], raw)

	return out
}

func embeddedMillis(b [16]byte) int64 {
	var ms int64
	for _, v := range b[:6] {
		ms = ms<<8 | int64(v)
	}

	return ms
}

func TestIDGeneratorVersionAndVariant(t *testing.T) {
	gen := envelope.NewIDGenerator(clock.NewFake(testStart))

	for range 100 {
		b := idParts(t, gen.New())

		if b[6]>>4 != 0x7 {
			t.Fatalf("version nibble = %x, want 7", b[6]>>4)
		}

		if b[8]>>6 != 0b10 {
			t.Fatalf("variant bits = %b, want 10", b[8]>>6)
		}
	}
}

func TestIDGeneratorStrictlyIncreasingWithFrozenClock(t *testing.T) {
	fake := clock.NewFake(testStart)
	gen := envelope.NewIDGenerator(fake)
	startMS := testStart.UnixMilli()

	const n = 10000
	seen := make(map[string]struct{}, n)
	prev := ""

	for i := range n {
		id := gen.New()

		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q at %d", id, i)
		}
		seen[id] = struct{}{}

		if id <= prev {
			t.Fatalf("id %d (%q) is not greater than previous (%q)", i, id, prev)
		}
		prev = id

		if got := embeddedMillis(idParts(t, id)); got < startMS {
			t.Fatalf("embedded timestamp %d earlier than clock %d", got, startMS)
		}
	}

	// 10 000 ids cannot fit one 12-bit counter, so the timestamp must have
	// been advanced past the frozen clock.
	if got := embeddedMillis(idParts(t, prev)); got <= startMS {
		t.Fatalf("timestamp never advanced on counter overflow: %d <= %d", got, startMS)
	}
}

func TestIDGeneratorFollowsAdvancingClock(t *testing.T) {
	fake := clock.NewFake(testStart)
	gen := envelope.NewIDGenerator(fake)

	first := gen.New()

	if got, want := embeddedMillis(idParts(t, first)), testStart.UnixMilli(); got != want {
		t.Fatalf("embedded ms = %d, want %d", got, want)
	}

	fake.Advance(time.Hour)
	second := gen.New()

	if got, want := embeddedMillis(idParts(t, second)), testStart.Add(time.Hour).UnixMilli(); got != want {
		t.Fatalf("embedded ms = %d, want %d", got, want)
	}

	if second <= first {
		t.Fatalf("ids not time ordered: %q then %q", first, second)
	}
}

func TestIDGeneratorClockGoingBackwardsStaysMonotonic(t *testing.T) {
	// The fake clock cannot go back; a generator that already advanced its
	// timestamp past the clock on counter overflow behaves the same way.
	fake := clock.NewFake(testStart)
	gen := envelope.NewIDGenerator(fake)

	prev := ""
	for range 5000 {
		prev = gen.New()
	}

	for range 10 {
		next := gen.New()
		if next <= prev {
			t.Fatalf("%q not greater than %q", next, prev)
		}
		prev = next
	}
}

func TestIDGeneratorConcurrentUniqueAndIncreasing(t *testing.T) {
	gen := envelope.NewIDGenerator(clock.NewFake(testStart))

	const workers, perWorker = 8, 500

	var wg sync.WaitGroup
	results := make([][]string, workers)

	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			ids := make([]string, 0, perWorker)
			for range perWorker {
				ids = append(ids, gen.New())
			}
			results[w] = ids
		}()
	}

	wg.Wait()

	seen := make(map[string]struct{}, workers*perWorker)
	for _, ids := range results {
		prev := ""
		for _, id := range ids {
			if _, dup := seen[id]; dup {
				t.Fatalf("duplicate id %q", id)
			}
			seen[id] = struct{}{}

			if id <= prev {
				t.Fatalf("ids of one goroutine not increasing: %q after %q", id, prev)
			}
			prev = id
		}
	}
}
