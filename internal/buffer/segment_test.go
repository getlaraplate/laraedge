package buffer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"reflect"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/model"
)

// seedRecords returns one record of every shape the buffer stores.
func seedRecords() []model.Record {
	ts := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	temp, _ := model.FloatValue(21.5)

	return []model.Record{
		model.SampleRecord(model.Sample{Device: "press-07", Signal: "temp", TS: ts, Value: temp, Quality: model.Good, Priority: model.Bulk}),
		model.SampleRecord(model.Sample{Device: "press-07", Signal: "count", TS: ts, Value: model.IntValue(-42), Quality: model.Uncertain, Priority: model.Critical}),
		model.SampleRecord(model.Sample{Device: "press-07", Signal: "door", TS: ts, Value: model.BoolValue(true), Quality: model.Bad, Priority: model.Bulk}),
		model.SampleRecord(model.Sample{Device: "press-07", Signal: "state", TS: ts, Value: model.StringValue("RUN"), Quality: model.Good, Priority: model.Critical}),
		model.EventRecord(model.DeviceEvent{Kind: model.Birth, Device: "press-07", TS: ts, Signals: []model.SignalInfo{
			{Signal: "temp", DataType: model.DataTypeNumber, Unit: "C"},
			{Signal: "state", DataType: model.DataTypeString},
		}}),
		model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: "press-07", TS: ts}),
	}
}

func TestRecordRoundTrip(t *testing.T) {
	for _, rec := range seedRecords() {
		t.Run(rec.Device()+"/"+rec.Priority().String(), func(t *testing.T) {
			frame, err := encodeRecord(rec)
			if err != nil {
				t.Fatalf("encodeRecord() = %v", err)
			}

			if got := binary.LittleEndian.Uint32(frame[0:4]); int(got) != len(frame)-headerSize {
				t.Fatalf("length field = %d, want %d", got, len(frame)-headerSize)
			}

			if got, want := binary.LittleEndian.Uint32(frame[4:8]), crc32.Checksum(frame[headerSize:], crc32.MakeTable(crc32.Castagnoli)); got != want {
				t.Fatalf("checksum field = %#x, want CRC32C %#x", got, want)
			}

			if frame[headerSize] != formatVersion {
				t.Fatalf("format version byte = %d, want %d", frame[headerSize], formatVersion)
			}

			got, n, err := decodeRecord(frame, 1<<20)
			if err != nil {
				t.Fatalf("decodeRecord() = %v", err)
			}

			if n != len(frame) {
				t.Fatalf("decodeRecord() consumed %d bytes, want %d", n, len(frame))
			}

			if !reflect.DeepEqual(got, rec) {
				t.Fatalf("decodeRecord() = %+v, want %+v", got, rec)
			}
		})
	}
}

func TestDecodeRecordRejects(t *testing.T) {
	valid, err := encodeRecord(seedRecords()[0])
	if err != nil {
		t.Fatal(err)
	}

	withPayload := func(payload []byte) []byte {
		frame := make([]byte, headerSize+len(payload))
		binary.LittleEndian.PutUint32(frame[0:4], uint32(len(payload)))
		binary.LittleEndian.PutUint32(frame[4:8], crc32.Checksum(payload, crc32.MakeTable(crc32.Castagnoli)))
		copy(frame[headerSize:], payload)

		return frame
	}

	flipped := bytes.Clone(valid)
	flipped[len(flipped)-2] ^= 0x01

	huge := bytes.Clone(valid)
	binary.LittleEndian.PutUint32(huge[0:4], 1<<30)

	tests := []struct {
		name    string
		frame   []byte
		maxSize int64
		want    error
	}{
		{"empty", nil, 1 << 20, errShortFrame},
		{"half a header", valid[:4], 1 << 20, errShortFrame},
		{"header only", valid[:headerSize], 1 << 20, errShortFrame},
		{"truncated payload", valid[:len(valid)-1], 1 << 20, errShortFrame},
		{"zero length", withPayload(nil), 1 << 20, errCorruptFrame},
		{"length above the limit", huge, 1 << 20, errCorruptFrame},
		{"frame larger than the segment size", valid, int64(len(valid) - 1), errCorruptFrame},
		{"bad checksum", flipped, 1 << 20, errCorruptFrame},
		{"unknown format version", withPayload(append([]byte{2}, valid[headerSize+1:]...)), 1 << 20, errCorruptFrame},
		{"payload not JSON", withPayload([]byte{formatVersion, '{', 'x'}), 1 << 20, errCorruptFrame},
		{"invalid record", withPayload([]byte{formatVersion, '{', '}'}), 1 << 20, errCorruptFrame},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, n, err := decodeRecord(tc.frame, tc.maxSize)
			if !errors.Is(err, tc.want) {
				t.Fatalf("decodeRecord() error = %v, want %v", err, tc.want)
			}

			if n != 0 {
				t.Fatalf("decodeRecord() consumed %d bytes on error", n)
			}
		})
	}
}

func TestDecodeRecordReadsOnlyTheFirstFrame(t *testing.T) {
	first, _ := encodeRecord(seedRecords()[0])
	second, _ := encodeRecord(seedRecords()[1])

	got, n, err := decodeRecord(append(bytes.Clone(first), second...), 1<<20)
	if err != nil {
		t.Fatalf("decodeRecord() = %v", err)
	}

	if n != len(first) || !reflect.DeepEqual(got, seedRecords()[0]) {
		t.Fatalf("decodeRecord() = %+v, %d; want the first record, %d", got, n, len(first))
	}
}

func TestSegmentNames(t *testing.T) {
	tests := []struct {
		name   string
		base   Position
		ok     bool
		parsed string
	}{
		{"zero", 0, true, "00000000000000000000.seg"},
		{"large", 18446744073709551615, true, "18446744073709551615.seg"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := segmentName(tc.base); got != tc.parsed {
				t.Fatalf("segmentName(%d) = %q, want %q", tc.base, got, tc.parsed)
			}

			base, ok := parseSegmentName(tc.parsed)
			if !ok || base != tc.base {
				t.Fatalf("parseSegmentName(%q) = %d, %v", tc.parsed, base, ok)
			}
		})
	}

	for _, name := range []string{"", "acked", "0000000000000000000.seg", "000000000000000000001.seg", "0000000000000000000a.seg", "00000000000000000000.tmp", "99999999999999999999.seg"} {
		if _, ok := parseSegmentName(name); ok {
			t.Fatalf("parseSegmentName(%q) accepted", name)
		}
	}
}
