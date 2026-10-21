package envelope

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"sync"

	"github.com/getlaraplate/laraedge/internal/clock"
)

// counterBits is the width of the rand_a field used as a sequence counter.
const counterBits = 12

// IDGenerator produces UUIDv7 message ids (RFC 9562). Ids from one generator
// are strictly increasing, also while the clock stands still or goes back: the
// 12-bit rand_a field is a counter seeded randomly for each new millisecond
// (RFC 9562, method 1), and when it overflows the embedded timestamp moves
// forward by one millisecond. It is safe for concurrent use.
type IDGenerator struct {
	clock clock.Clock

	mu      sync.Mutex
	lastMS  int64
	counter uint16
}

// NewIDGenerator returns a generator reading time from c.
func NewIDGenerator(c clock.Clock) *IDGenerator {
	return &IDGenerator{clock: c}
}

// New returns a new UUIDv7 in its canonical lowercase text form.
func (g *IDGenerator) New() string {
	ms, counter := g.next()

	var b [16]byte

	// 48-bit big-endian Unix millisecond timestamp.
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)

	// rand_b: 62 random bits after the variant.
	// crypto/rand.Read does not return an error since Go 1.24: it aborts the
	// process if the system source fails.
	_, _ = rand.Read(b[8:])

	b[6] = 0x70 | byte(counter>>8)
	b[7] = byte(counter)
	b[8] = 0x80 | b[8]&0x3f

	var out [36]byte

	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])

	return string(out[:])
}

// next returns the timestamp and counter of the next id.
func (g *IDGenerator) next() (int64, uint16) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.clock.Now().UnixMilli()

	switch {
	case now > g.lastMS:
		g.lastMS = now
		g.counter = seedCounter()
	case g.counter == 1<<counterBits-1:
		g.lastMS++
		g.counter = seedCounter()
	default:
		g.counter++
	}

	return g.lastMS, g.counter
}

// seedCounter returns a random starting counter with the top bit clear, so
// that at least half of the counter range is left for the millisecond.
func seedCounter() uint16 {
	var r [2]byte

	_, _ = rand.Read(r[:])

	return binary.BigEndian.Uint16(r[:]) & (1<<(counterBits-1) - 1)
}
