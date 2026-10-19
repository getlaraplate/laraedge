package buffer

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/swolley/laraplate-edge/internal/model"
)

// Record framing: uint32 payload length | uint32 CRC32C of the payload |
// payload, little endian. The payload is one format-version byte followed by
// the JSON encoding of a model.Record.
const (
	headerSize          = 8
	formatVersion  byte = 1
	segmentExt          = ".seg"
	segmentNameLen      = 20
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

var (
	// errShortFrame means the bytes end before the frame does.
	errShortFrame = errors.New("incomplete record")
	// errCorruptFrame means the frame has a bad length, checksum or payload.
	errCorruptFrame = errors.New("corrupt record")
)

// encodeRecord frames r for a segment.
func encodeRecord(r model.Record) ([]byte, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("encode record: %w", err)
	}

	if uint64(len(body)) >= math.MaxUint32 {
		return nil, fmt.Errorf("encode record: %d bytes do not fit a frame", len(body))
	}

	frame := make([]byte, headerSize+1+len(body))
	frame[headerSize] = formatVersion
	copy(frame[headerSize+1:], body)
	binary.LittleEndian.PutUint32(frame[0:4], uint32(1+len(body)))
	binary.LittleEndian.PutUint32(frame[4:8], crc32.Checksum(frame[headerSize:], castagnoli))

	return frame, nil
}

// parseHeader validates a frame header and returns the payload length. A
// frame is refused when it would not fit in maxFrame bytes (the segment size).
func parseHeader(header []byte, maxFrame int64) (int, error) {
	length := int64(binary.LittleEndian.Uint32(header[0:4]))
	if length == 0 {
		return 0, fmt.Errorf("%w: zero length", errCorruptFrame)
	}

	if length+headerSize > maxFrame {
		return 0, fmt.Errorf("%w: length %d above the limit of %d bytes", errCorruptFrame, length, maxFrame-headerSize)
	}

	return int(length), nil
}

// verifyPayload checks the checksum and the format version of a payload.
func verifyPayload(header, payload []byte) error {
	if crc32.Checksum(payload, castagnoli) != binary.LittleEndian.Uint32(header[4:8]) {
		return fmt.Errorf("%w: checksum mismatch", errCorruptFrame)
	}

	if payload[0] != formatVersion {
		return fmt.Errorf("%w: unknown format version %d", errCorruptFrame, payload[0])
	}

	return nil
}

// decodePayload decodes a verified payload into a valid record.
func decodePayload(payload []byte) (model.Record, error) {
	var r model.Record
	if err := json.Unmarshal(payload[1:], &r); err != nil {
		return model.Record{}, fmt.Errorf("%w: %v", errCorruptFrame, err)
	}

	if err := r.Validate(); err != nil {
		return model.Record{}, fmt.Errorf("%w: %v", errCorruptFrame, err)
	}

	return r, nil
}

// decodeRecord decodes the frame at the start of buf and returns its record
// and size. It never panics on arbitrary input; on error it consumes nothing.
func decodeRecord(buf []byte, maxFrame int64) (model.Record, int, error) {
	if len(buf) < headerSize {
		return model.Record{}, 0, errShortFrame
	}

	length, err := parseHeader(buf[:headerSize], maxFrame)
	if err != nil {
		return model.Record{}, 0, err
	}

	if len(buf)-headerSize < length {
		return model.Record{}, 0, errShortFrame
	}

	payload := buf[headerSize : headerSize+length]
	if err := verifyPayload(buf[:headerSize], payload); err != nil {
		return model.Record{}, 0, err
	}

	r, err := decodePayload(payload)
	if err != nil {
		return model.Record{}, 0, err
	}

	return r, headerSize + length, nil
}

// readFrameAt reads and verifies the frame at off of a segment holding size
// valid bytes.
func readFrameAt(r io.ReaderAt, off, size, maxFrame int64) ([]byte, error) {
	if size-off < headerSize {
		return nil, fmt.Errorf("%w: header past the end of the segment", errCorruptFrame)
	}

	var header [headerSize]byte
	if _, err := r.ReadAt(header[:], off); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}

	length, err := parseHeader(header[:], maxFrame)
	if err != nil {
		return nil, err
	}

	if int64(length) > size-off-headerSize {
		return nil, fmt.Errorf("%w: payload past the end of the segment", errCorruptFrame)
	}

	frame := make([]byte, headerSize+length)
	copy(frame, header[:])
	if _, err := r.ReadAt(frame[headerSize:], off+headerSize); err != nil {
		return nil, fmt.Errorf("read payload: %w", err)
	}

	if err := verifyPayload(frame[:headerSize], frame[headerSize:]); err != nil {
		return nil, err
	}

	return frame, nil
}

// recordTS returns the device timestamp of a record.
func recordTS(r model.Record) time.Time {
	switch {
	case r.Sample != nil:
		return r.Sample.TS
	case r.Event != nil:
		return r.Event.TS
	default:
		return time.Time{}
	}
}

// payloadTS extracts only the timestamp of a verified payload, cheaper than
// decoding the whole record. It returns the zero time when it cannot.
func payloadTS(payload []byte) time.Time {
	var v struct {
		Sample *struct{ TS time.Time }
		Event  *struct{ TS time.Time }
	}

	if err := json.Unmarshal(payload[1:], &v); err != nil {
		return time.Time{}
	}

	switch {
	case v.Sample != nil:
		return v.Sample.TS
	case v.Event != nil:
		return v.Event.TS
	default:
		return time.Time{}
	}
}

// segment is one file of a queue, named by the position of its first byte.
// size counts the valid bytes; only the active (last) segment keeps file
// open, for writing.
type segment struct {
	base  Position
	size  int64
	file  *os.File
	dirty bool
}

// end returns the position just after the segment's last valid byte.
func (s *segment) end() Position { return s.base + Position(s.size) }

// close syncs unsynced writes and closes the file of the active segment.
func (s *segment) close() error {
	if s.file == nil {
		return nil
	}

	var syncErr error
	if s.dirty {
		syncErr = s.file.Sync()
	}

	closeErr := s.file.Close()
	s.file, s.dirty = nil, false

	return errors.Join(syncErr, closeErr)
}

func segmentName(base Position) string {
	return fmt.Sprintf("%0*d%s", segmentNameLen, uint64(base), segmentExt)
}

func parseSegmentName(name string) (Position, bool) {
	digits, ok := strings.CutSuffix(name, segmentExt)
	if !ok || len(digits) != segmentNameLen {
		return 0, false
	}

	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
	}

	base, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, false
	}

	return Position(base), true
}

// createSegment creates an empty segment file at base and makes its directory
// entry durable before anything is written to it.
func createSegment(dir string, base Position) (*segment, error) {
	path := filepath.Join(dir, segmentName(base))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create segment: %w", err)
	}

	if err := syncDir(dir); err != nil {
		_ = f.Close()
		_ = os.Remove(path)

		return nil, fmt.Errorf("create segment %s: %w", path, err)
	}

	return &segment{base: base, file: f}, nil
}

// scanResult is what a recovery scan learns about one segment file.
type scanResult struct {
	size      int64  // valid bytes: up to the first bad or incomplete frame
	records   uint64 // valid records
	beforeAck uint64 // valid records starting before the confirmed position
}

// scanSegment reads a segment sequentially, verifying each frame's length,
// checksum and format version, and stops at the first bad or incomplete one.
// Only I/O failures are errors; a bad frame shows as size below fileSize.
func scanSegment(r io.Reader, fileSize int64, base, ack Position, maxFrame int64) (scanResult, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var res scanResult
	var header [headerSize]byte
	var payload []byte

	for fileSize-res.size >= headerSize {
		if _, err := io.ReadFull(br, header[:]); err != nil {
			return res, fmt.Errorf("read header at offset %d: %w", res.size, err)
		}

		length, err := parseHeader(header[:], maxFrame)
		if err != nil || int64(length) > fileSize-res.size-headerSize {
			break
		}

		if cap(payload) < length {
			payload = make([]byte, length)
		}

		payload = payload[:length]
		if _, err := io.ReadFull(br, payload); err != nil {
			return res, fmt.Errorf("read payload at offset %d: %w", res.size, err)
		}

		if verifyPayload(header[:], payload) != nil {
			break
		}

		res.records++
		if base+Position(res.size) < ack {
			res.beforeAck++
		}

		res.size += headerSize + int64(length)
	}

	return res, nil
}
