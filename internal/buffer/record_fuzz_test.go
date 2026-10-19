package buffer

import (
	"bytes"
	"testing"
)

// FuzzDecode feeds arbitrary bytes to the record decoder. It must never
// panic, must never accept a frame longer than the limit, and every frame it
// accepts must hold a valid record that encodes back to a decodable frame.
func FuzzDecode(f *testing.F) {
	for _, rec := range seedRecords() {
		frame, err := encodeRecord(rec)
		if err != nil {
			f.Fatal(err)
		}

		f.Add(frame)
		f.Add(frame[:len(frame)-1])
		f.Add(frame[:headerSize])

		bad := bytes.Clone(frame)
		bad[len(bad)-1] ^= 0xff
		f.Add(bad)
	}

	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0})
	f.Add([]byte{1, 0, 0, 0, 0, 0, 0, 0, formatVersion})

	const limit = 1 << 16

	f.Fuzz(func(t *testing.T, data []byte) {
		rec, n, err := decodeRecord(data, limit)
		if err != nil {
			if n != 0 {
				t.Fatalf("decodeRecord() consumed %d bytes with error %v", n, err)
			}

			return
		}

		if n < headerSize || n > len(data) || n > limit {
			t.Fatalf("decodeRecord() consumed %d of %d bytes (limit %d)", n, len(data), limit)
		}

		if err := rec.Validate(); err != nil {
			t.Fatalf("decodeRecord() accepted an invalid record: %v", err)
		}

		frame, err := encodeRecord(rec)
		if err != nil {
			t.Fatalf("encodeRecord() of a decoded record = %v", err)
		}

		again, m, err := decodeRecord(frame, int64(len(frame)))
		if err != nil || m != len(frame) {
			t.Fatalf("re-encoded frame does not decode: %d, %v", m, err)
		}

		reencoded, err := encodeRecord(again)
		if err != nil || !bytes.Equal(reencoded, frame) {
			t.Fatalf("encoding is not stable after a round trip")
		}
	})
}
