package modbus

import (
	"errors"
	"math"
	"unicode/utf8"

	"github.com/getlaraplate/laraedge/internal/model"
)

// Decode errors have fixed texts: they never carry register values, so they
// are safe in logs. Every error of DecodeRegisters is of health kind decode.
var (
	errDecodeRange  = errors.New("decode: point goes beyond the block reply")
	errDecodeType   = errors.New("decode: unknown point type")
	errDecodeUint64 = errors.New("decode: uint64 value above 9223372036854775807")
	errDecodeFloat  = errors.New("decode: float value is not finite")
	errDecodeString = errors.New("decode: string is not valid UTF-8")
)

// DecodeRegisters decodes the point p from the registers of its block, which
// starts at address blockStart. The point's first register is at index
// Addr.Offset-blockStart of regs, and it spans p.Size registers.
//
// Byte order is two independent swaps. Bytes are swapped inside each register
// for BADC and DCBA, and the registers are taken low word first for CDAB and
// DCBA; empty is ABCD. A 16-bit type is one register, so only the byte swap
// matters. For 32 bits, ABCD is r0 high and r1 low, CDAB is r1 high and r0
// low, BADC is ABCD with the bytes swapped in each register, and DCBA is CDAB
// with the bytes swapped in each register (full little-endian). For 64 bits,
// low word first reverses all four registers (r3 r2 r1 r0). A string ignores
// the byte order: each register holds two characters, high byte first, and
// the text ends at the first NUL.
//
// Integer types give integer values, float types give float values, and
// uint64 above the int64 range is an error, as is a non-finite float, an
// invalid UTF-8 string and a point that does not fit in regs.
func DecodeRegisters(regs []uint16, p PlannedPoint, blockStart uint16) (model.Value, error) {
	first := int(p.Addr.Offset) - int(blockStart)

	width, ok := registerWidth(p)
	if !ok {
		return model.Value{}, errDecodeType
	}
	if first < 0 || first+width > len(regs) {
		return model.Value{}, errDecodeRange
	}
	words := regs[first : first+width]

	if p.Point.Type == "string" {
		return decodeString(words)
	}

	raw := assemble(words, p.Point.ByteOrder)
	switch p.Point.Type {
	case "int16":
		return model.IntValue(int64(int16(raw))), nil
	case "uint16", "uint32", "uint64":
		if raw > math.MaxInt64 {
			return model.Value{}, errDecodeUint64
		}
		return model.IntValue(int64(raw)), nil
	case "int32":
		return model.IntValue(int64(int32(raw))), nil
	case "int64":
		return model.IntValue(int64(raw)), nil
	case "float32":
		return floatValue(float64(math.Float32frombits(uint32(raw))))
	default: // float64, checked by registerWidth
		return floatValue(math.Float64frombits(raw))
	}
}

// DecodeBits decodes the bool point p from the bits of its block, which starts
// at address blockStart. A point outside bits is a programming error and gives
// the zero Value, which fails Validate, instead of a panic.
func DecodeBits(bits []bool, p PlannedPoint, blockStart uint16) model.Value {
	i := int(p.Addr.Offset) - int(blockStart)
	if i < 0 || i >= len(bits) {
		return model.Value{}
	}
	return model.BoolValue(bits[i])
}

// registerWidth is the number of registers the point's type reads: the fixed
// width of a numeric type, the address length of a string.
func registerWidth(p PlannedPoint) (int, bool) {
	switch p.Point.Type {
	case "int16", "uint16":
		return 1, true
	case "int32", "uint32", "float32":
		return 2, true
	case "int64", "uint64", "float64":
		return 4, true
	case "string":
		return int(p.Size), true
	}
	return 0, false
}

// assemble joins the registers of a numeric point into one big-endian number
// after applying the byte order.
func assemble(words []uint16, order string) uint64 {
	swapBytes := order == "BADC" || order == "DCBA"
	lowWordFirst := order == "CDAB" || order == "DCBA"

	var raw uint64
	for i := range words {
		w := words[i]
		if lowWordFirst {
			w = words[len(words)-1-i]
		}
		if swapBytes {
			w = w<<8 | w>>8
		}
		raw = raw<<16 | uint64(w)
	}
	return raw
}

func floatValue(f float64) (model.Value, error) {
	v, err := model.FloatValue(f)
	if err != nil {
		return model.Value{}, errDecodeFloat
	}
	return v, nil
}

func decodeString(words []uint16) (model.Value, error) {
	buf := make([]byte, 0, 2*len(words))
	for _, w := range words {
		buf = append(buf, byte(w>>8), byte(w))
	}
	for i, b := range buf {
		if b == 0 {
			buf = buf[:i]
			break
		}
	}
	if !utf8.Valid(buf) {
		return model.Value{}, errDecodeString
	}
	return model.StringValue(string(buf)), nil
}
