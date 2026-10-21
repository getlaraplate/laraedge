package modbus

import (
	"math"
	"testing"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/model"
)

// pointAt builds an hr point of typ at offset with the byte order and size.
func pointAt(typ, order string, offset, size uint16) PlannedPoint {
	return PlannedPoint{
		Device: "dev",
		Signal: "s",
		Point:  config.Point{Signal: "s", Type: typ, ByteOrder: order},
		Addr:   Address{Area: AreaHoldingRegister, Offset: offset},
		Size:   size,
	}
}

func mustFloat(t *testing.T, f float64) model.Value {
	t.Helper()
	v, err := model.FloatValue(f)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDecodeRegisters(t *testing.T) {
	// 0x12345678 as registers for each order.
	words32 := map[string][]uint16{
		"":     {0x1234, 0x5678},
		"ABCD": {0x1234, 0x5678},
		"CDAB": {0x5678, 0x1234},
		"BADC": {0x3412, 0x7856},
		"DCBA": {0x7856, 0x3412},
	}
	// 0x0102030405060708 as registers for each order.
	words64 := map[string][]uint16{
		"":     {0x0102, 0x0304, 0x0506, 0x0708},
		"ABCD": {0x0102, 0x0304, 0x0506, 0x0708},
		"CDAB": {0x0708, 0x0506, 0x0304, 0x0102},
		"BADC": {0x0201, 0x0403, 0x0605, 0x0807},
		"DCBA": {0x0807, 0x0605, 0x0403, 0x0201},
	}
	// 1.5 = 0x3FC00000 as float32 registers for each order.
	f32 := map[string][]uint16{
		"ABCD": {0x3FC0, 0x0000},
		"CDAB": {0x0000, 0x3FC0},
		"BADC": {0xC03F, 0x0000},
		"DCBA": {0x0000, 0xC03F},
	}
	// -2.5 = 0xC004000000000000 as float64 registers for each order.
	f64 := map[string][]uint16{
		"ABCD": {0xC004, 0x0000, 0x0000, 0x0000},
		"CDAB": {0x0000, 0x0000, 0x0000, 0xC004},
		"BADC": {0x04C0, 0x0000, 0x0000, 0x0000},
		"DCBA": {0x0000, 0x0000, 0x0000, 0x04C0},
	}
	// 0x1234 and its byte-swapped form.
	words16 := map[string]uint16{"": 0x1234, "ABCD": 0x1234, "CDAB": 0x1234, "BADC": 0x3412, "DCBA": 0x3412}

	for order, w := range words16 {
		t.Run("int16 "+order, func(t *testing.T) {
			got, err := DecodeRegisters([]uint16{w}, pointAt("int16", order, 0, 1), 0)
			if err != nil || got != model.IntValue(0x1234) {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
		t.Run("uint16 "+order, func(t *testing.T) {
			got, err := DecodeRegisters([]uint16{w}, pointAt("uint16", order, 0, 1), 0)
			if err != nil || got != model.IntValue(0x1234) {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
	for order, w := range words32 {
		t.Run("uint32 "+order, func(t *testing.T) {
			got, err := DecodeRegisters(w, pointAt("uint32", order, 0, 2), 0)
			if err != nil || got != model.IntValue(0x12345678) {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
		t.Run("int32 "+order, func(t *testing.T) {
			got, err := DecodeRegisters(w, pointAt("int32", order, 0, 2), 0)
			if err != nil || got != model.IntValue(0x12345678) {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
	for order, w := range words64 {
		t.Run("uint64 "+order, func(t *testing.T) {
			got, err := DecodeRegisters(w, pointAt("uint64", order, 0, 4), 0)
			if err != nil || got != model.IntValue(0x0102030405060708) {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
		t.Run("int64 "+order, func(t *testing.T) {
			got, err := DecodeRegisters(w, pointAt("int64", order, 0, 4), 0)
			if err != nil || got != model.IntValue(0x0102030405060708) {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
	for order, w := range f32 {
		t.Run("float32 "+order, func(t *testing.T) {
			got, err := DecodeRegisters(w, pointAt("float32", order, 0, 2), 0)
			if err != nil || got != mustFloat(t, 1.5) {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
	for order, w := range f64 {
		t.Run("float64 "+order, func(t *testing.T) {
			got, err := DecodeRegisters(w, pointAt("float64", order, 0, 4), 0)
			if err != nil || got != mustFloat(t, -2.5) {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

func TestDecodeRegistersNegativeAndLimits(t *testing.T) {
	tests := []struct {
		name string
		regs []uint16
		typ  string
		ord  string
		size uint16
		want model.Value
	}{
		{"int16 -2", []uint16{0xFFFE}, "int16", "", 1, model.IntValue(-2)},
		{"int16 min", []uint16{0x8000}, "int16", "ABCD", 1, model.IntValue(-32768)},
		{"int16 -2 swapped", []uint16{0xFEFF}, "int16", "BADC", 1, model.IntValue(-2)},
		{"uint16 max", []uint16{0xFFFF}, "uint16", "", 1, model.IntValue(65535)},
		{"int32 -2", []uint16{0xFFFF, 0xFFFE}, "int32", "", 2, model.IntValue(-2)},
		{"int32 -2 CDAB", []uint16{0xFFFE, 0xFFFF}, "int32", "CDAB", 2, model.IntValue(-2)},
		{"uint32 max", []uint16{0xFFFF, 0xFFFF}, "uint32", "", 2, model.IntValue(math.MaxUint32)},
		{"int64 -2", []uint16{0xFFFF, 0xFFFF, 0xFFFF, 0xFFFE}, "int64", "", 4, model.IntValue(-2)},
		{"int64 min", []uint16{0x8000, 0, 0, 0}, "int64", "", 4, model.IntValue(math.MinInt64)},
		{"uint64 max int64", []uint16{0x7FFF, 0xFFFF, 0xFFFF, 0xFFFF}, "uint64", "", 4, model.IntValue(math.MaxInt64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeRegisters(tt.regs, pointAt(tt.typ, tt.ord, 0, tt.size), 0)
			if err != nil || got != tt.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestDecodeRegistersErrors(t *testing.T) {
	tests := []struct {
		name string
		regs []uint16
		typ  string
		ord  string
		size uint16
	}{
		{"uint64 above int64", []uint16{0x8000, 0, 0, 0}, "uint64", "", 4},
		{"uint64 max", []uint16{0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF}, "uint64", "", 4},
		{"float32 NaN", []uint16{0x7FC0, 0x0000}, "float32", "", 2},
		{"float32 +Inf", []uint16{0x7F80, 0x0000}, "float32", "", 2},
		{"float32 -Inf CDAB", []uint16{0x0000, 0xFF80}, "float32", "CDAB", 2},
		{"float64 NaN", []uint16{0x7FF8, 0, 0, 0}, "float64", "", 4},
		{"float64 +Inf", []uint16{0x7FF0, 0, 0, 0}, "float64", "", 4},
		{"string invalid utf8", []uint16{0xFFFE}, "string", "", 1},
		{"unknown type", []uint16{0}, "decimal", "", 1},
		{"bool on registers", []uint16{1}, "bool", "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeRegisters(tt.regs, pointAt(tt.typ, tt.ord, 0, tt.size), 0)
			if err == nil {
				t.Fatalf("got %+v, want an error", got)
			}
			if got != (model.Value{}) {
				t.Fatalf("value %+v with an error", got)
			}
		})
	}
}

func TestDecodeRegistersFixedErrorTexts(t *testing.T) {
	// The errors never carry register values.
	_, err := DecodeRegisters([]uint16{0x7FC0, 0x0000}, pointAt("float32", "", 0, 2), 0)
	if err == nil || err.Error() != "decode: float value is not finite" {
		t.Fatalf("got %v", err)
	}
	_, err = DecodeRegisters([]uint16{0x8000, 0, 0, 0}, pointAt("uint64", "", 0, 4), 0)
	if err == nil || err.Error() != "decode: uint64 value above 9223372036854775807" {
		t.Fatalf("got %v", err)
	}
}

func TestDecodeRegistersString(t *testing.T) {
	tests := []struct {
		name string
		regs []uint16
		size uint16
		ord  string
		want string
	}{
		{"fills registers", []uint16{0x4142, 0x4344}, 2, "", "ABCD"},
		{"odd length with NUL", []uint16{0x4142, 0x4300}, 2, "", "ABC"},
		{"stops at first NUL", []uint16{0x4100, 0x4200}, 2, "", "A"},
		{"NUL as high byte", []uint16{0x0041}, 1, "", ""},
		{"empty", []uint16{0, 0}, 2, "", ""},
		{"ignores byte order", []uint16{0x4142, 0x4344}, 2, "DCBA", "ABCD"},
		{"utf8 multibyte", []uint16{0xC3A8, 0x0000}, 2, "", "è"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeRegisters(tt.regs, pointAt("string", tt.ord, 0, tt.size), 0)
			if err != nil || got != model.StringValue(tt.want) {
				t.Fatalf("got %+v, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestDecodeRegistersBlockOffset(t *testing.T) {
	// A block starting at 100; the point sits at 102..103.
	regs := []uint16{0xAAAA, 0xBBBB, 0x3FC0, 0x0000, 0xCCCC}
	got, err := DecodeRegisters(regs, pointAt("float32", "ABCD", 102, 2), 100)
	if err != nil || got != mustFloat(t, 1.5) {
		t.Fatalf("got %+v, %v", got, err)
	}

	// Two points overlapping the same registers with different types.
	got, err = DecodeRegisters(regs, pointAt("uint16", "", 102, 1), 100)
	if err != nil || got != model.IntValue(0x3FC0) {
		t.Fatalf("got %+v, %v", got, err)
	}
	got, err = DecodeRegisters(regs, pointAt("uint32", "", 102, 2), 100)
	if err != nil || got != model.IntValue(0x3FC00000) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestDecodeRegistersBlockEndingAtAddressSpaceEnd(t *testing.T) {
	// A block of 2 registers starting at 65535-1 ends at 65535; indexes stay
	// in int, so nothing wraps.
	regs := []uint16{0x1234, 0x5678}
	got, err := DecodeRegisters(regs, pointAt("uint32", "", 65534, 2), 65534)
	if err != nil || got != model.IntValue(0x12345678) {
		t.Fatalf("got %+v, %v", got, err)
	}
	// A block read from 0 and a point at 65535 is outside a short reply.
	if _, err := DecodeRegisters(regs, pointAt("uint16", "", 65535, 1), 0); err == nil {
		t.Fatal("want an error")
	}
}

func TestDecodeRegistersOutOfRange(t *testing.T) {
	regs := []uint16{1, 2, 3}
	tests := []struct {
		name  string
		point PlannedPoint
		start uint16
	}{
		{"before the block", pointAt("uint16", "", 4, 1), 5},
		{"past the end", pointAt("uint16", "", 3, 1), 0},
		{"straddles the end", pointAt("uint32", "", 2, 2), 0},
		{"string longer than the reply", pointAt("string", "", 0, 4), 0},
		{"empty reply", pointAt("uint16", "", 0, 1), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := regs
			if tt.name == "empty reply" {
				in = nil
			}
			if _, err := DecodeRegisters(in, tt.point, tt.start); err == nil {
				t.Fatal("want an error, not a panic")
			}
		})
	}
}

func TestDecodeBits(t *testing.T) {
	bits := []bool{true, false, false, true}
	at := func(offset uint16) PlannedPoint {
		p := pointAt("bool", "", offset, 1)
		p.Addr.Area = AreaCoil
		return p
	}
	tests := []struct {
		name   string
		offset uint16
		start  uint16
		want   model.Value
	}{
		{"first", 10, 10, model.BoolValue(true)},
		{"second", 11, 10, model.BoolValue(false)},
		{"last", 13, 10, model.BoolValue(true)},
		{"past the end", 14, 10, model.Value{}},
		{"before the block", 9, 10, model.Value{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecodeBits(bits, at(tt.offset), tt.start); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
	if got := DecodeBits(nil, at(0), 0); got != (model.Value{}) {
		t.Fatalf("empty reply: got %+v", got)
	}
}
