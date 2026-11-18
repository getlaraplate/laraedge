package driverkit

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestParseSize(t *testing.T) {
	const limit = 16 << 20
	tests := []struct {
		raw  string
		want int
		ok   bool
	}{
		{`1`, 1, true},
		{`1024`, 1024, true},
		{` 7 `, 7, true},
		{`"64KiB"`, 64 << 10, true},
		{`"16MiB"`, 16 << 20, true},
		{`16777216`, 16 << 20, true},
		{`0`, 0, false},
		{`16777217`, 0, false},
		{`"17MiB"`, 0, false},
		{`"0KiB"`, 0, false},
		{`"64"`, 0, false},
		{`"64kib"`, 0, false},
		{`"-1KiB"`, 0, false},
		{`" 1KiB"`, 0, false},
		{`"KiB"`, 0, false},
		{`-1`, 0, false},
		{`1.5`, 0, false},
		{`true`, 0, false},
		{`""`, 0, false},
		{`"`, 0, false},
		{``, 0, false},
	}
	for _, tt := range tests {
		got, err := ParseSize(json.RawMessage(tt.raw), limit)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("ParseSize(%s) = %d, %v; want %d, ok=%v", tt.raw, got, err, tt.want, tt.ok)
		}
		if err != nil && !errors.Is(err, ErrSize) {
			t.Errorf("ParseSize(%s) error %v is not ErrSize", tt.raw, err)
		}
	}
}

func TestParseSizeLimit(t *testing.T) {
	if _, err := ParseSize(json.RawMessage(`"2KiB"`), 1024); !errors.Is(err, ErrSize) {
		t.Fatalf("got %v", err)
	}
	if n, err := ParseSize(json.RawMessage(`1024`), 1024); err != nil || n != 1024 {
		t.Fatalf("got %d, %v", n, err)
	}
}
