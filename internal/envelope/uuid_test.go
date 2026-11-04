package envelope_test

import (
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/envelope"
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

func TestIDGeneratorAheadOfAFrozenClockStaysMonotonic(t *testing.T) {
	// 5000 ids overflow the counter, so the generator's timestamp runs ahead
	// of the frozen clock; the ids that follow must keep increasing.
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

// idCounter returns the 12-bit rand_a counter of an id.
func idCounter(b [16]byte) uint16 { return uint16(b[6]&0x0f)<<8 | uint16(b[7]) }

func TestIDGeneratorCounterLayoutAndMillisecondRoll(t *testing.T) {
	gen := envelope.NewIDGenerator(clock.NewFake(testStart))
	startMS := testStart.UnixMilli()

	first := idParts(t, gen.New())
	if got := embeddedMillis(first); got != startMS {
		t.Fatalf("embedded ms = %d, want %d", got, startMS)
	}

	if c := idCounter(first); c >= 1<<11 {
		t.Fatalf("seeded counter %#x has its top bit set", c)
	}

	prev := first
	for idCounter(prev) != 1<<12-1 {
		b := idParts(t, gen.New())
		if got := embeddedMillis(b); got != startMS {
			t.Fatalf("embedded ms moved to %d before the counter overflowed", got)
		}

		if idCounter(b) != idCounter(prev)+1 {
			t.Fatalf("counter %#x after %#x, want +1", idCounter(b), idCounter(prev))
		}
		prev = b
	}

	rolled := idParts(t, gen.New())
	if got := embeddedMillis(rolled); got != startMS+1 {
		t.Fatalf("embedded ms after overflow = %d, want %d", got, startMS+1)
	}

	if c := idCounter(rolled); c >= 1<<11 {
		t.Fatalf("counter reseeded to %#x with its top bit set", c)
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
