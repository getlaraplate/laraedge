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

// FuzzScanSegment feeds arbitrary segment contents to the production readers:
// the recovery scan and readFrameAt. Neither may panic; the scan must stop
// within the data at a frame boundary that readFrameAt agrees with, frame by
// frame, and readFrameAt must refuse whatever the scan stopped at.
func FuzzScanSegment(f *testing.F) {
	var segment []byte
	for _, rec := range seedRecords() {
		frame, err := encodeRecord(rec)
		if err != nil {
			f.Fatal(err)
		}

		segment = append(segment, frame...)
	}

	f.Add(segment, uint64(0))
	f.Add(segment[:len(segment)-3], uint64(100))
	f.Add(append(bytes.Clone(segment), 0xff, 0xff, 0xff, 0x7f, 1, 2, 3, 4), uint64(1<<20))

	middle := bytes.Clone(segment)
	middle[len(middle)/2] ^= 0x10
	f.Add(middle, uint64(0))
	f.Add([]byte{}, uint64(0))

	const limit = 1 << 16

	f.Fuzz(func(t *testing.T, data []byte, ack uint64) {
		size := int64(len(data))
		res, err := scanSegment(bytes.NewReader(data), size, 0, Position(ack), limit)
		if err != nil {
			t.Fatalf("scanSegment() = %v on in-memory data", err)
		}

		if res.size < 0 || res.size > size || res.beforeAck > res.records {
			t.Fatalf("scanSegment() = %+v for %d bytes", res, size)
		}

		r := bytes.NewReader(data)
		var off int64
		var records uint64
		for off < res.size {
			frame, err := readFrameAt(r, off, res.size, limit)
			if err != nil {
				t.Fatalf("readFrameAt(%d) = %v inside the scanned prefix", off, err)
			}

			off += int64(len(frame))
			records++
		}

		if off != res.size || records != res.records {
			t.Fatalf("readFrameAt walk ends at %d with %d records, scan says %d with %d", off, records, res.size, res.records)
		}

		if res.size < size {
			if _, err := readFrameAt(r, res.size, size, limit); err == nil {
				t.Fatalf("readFrameAt(%d) accepts the frame the scan stopped at", res.size)
			}
		}

		for _, at := range []int64{0, size / 2, size - 1, size + 8} {
			_, _ = readFrameAt(r, at, size, limit)
		}
	})
}
