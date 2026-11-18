package driverkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// ErrSize is the error ParseSize returns for any size it refuses. It carries
// no text of the document: the caller names the field and the accepted shape.
var ErrSize = errors.New("invalid size")

// ParseSize decodes a size setting: an integer number of bytes or a string
// with the suffix KiB or MiB, from 1 byte to limit.
func ParseSize(raw json.RawMessage, limit int) (int, error) {
	trimmed := bytes.TrimSpace(raw)
	var n uint64
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return 0, ErrSize
		}
		unit := uint64(0)
		switch {
		case strings.HasSuffix(text, "KiB"):
			unit, text = 1<<10, strings.TrimSuffix(text, "KiB")
		case strings.HasSuffix(text, "MiB"):
			unit, text = 1<<20, strings.TrimSuffix(text, "MiB")
		default:
			return 0, ErrSize
		}
		count, ok := digits(text, uint64(limit))
		if !ok {
			return 0, ErrSize
		}
		n = count * unit
	} else {
		count, err := strconv.ParseUint(string(trimmed), 10, 64)
		if err != nil {
			return 0, ErrSize
		}
		n = count
	}
	if n < 1 || n > uint64(limit) {
		return 0, ErrSize
	}

	return int(n), nil
}

// digits parses a non-empty run of ASCII digits no greater than limit. A sign,
// a space or a prefix is refused.
func digits(s string, limit uint64) (uint64, bool) {
	if s == "" || len(s) > 10 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n > limit {
		return 0, false
	}

	return n, true
}
