package serial

import (
	"testing"
	"time"
)

func FuzzFramer(f *testing.F) {
	f.Add([]byte("1.5\r2.5\r\n\x02AB\x03"), uint8(3), uint8(0), uint8(5))
	f.Add([]byte("ABCDEFGH"), uint8(1), uint8(1), uint8(2))
	f.Fuzz(func(t *testing.T, data []byte, mode uint8, split uint8, maxSel uint8) {
		maxFrame := int(maxSel%16) + 4
		var fr Framing
		switch mode % 6 {
		case 0:
			fr = Framing{Terminator: []byte("\r")}
		case 1:
			fr = Framing{Terminator: []byte("\r\n")}
		case 2:
			fr = Framing{Length: maxFrame}
		case 3:
			fr = Framing{Gap: 50 * time.Millisecond}
		case 4:
			fr = Framing{Terminator: []byte("\x03"), Start: 2, HasStart: true}
		default:
			fr = Framing{Length: 3, Start: 2, HasStart: true}
		}
		fm := newFramer(fr, maxFrame)
		step := int(split%7) + 1
		at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
		check := func(frames [][]byte) {
			for _, fb := range frames {
				if len(fb) > maxFrame {
					t.Fatalf("frame of %d bytes exceeds %d", len(fb), maxFrame)
				}
				if len(fb) == 0 {
					t.Fatal("empty frame")
				}
			}
		}
		for i := 0; i < len(data); i += step {
			end := min(i+step, len(data))
			frames, _ := fm.push(data[i:end], at)
			check(frames)
			at = at.Add(time.Duration(int(data[i])%40) * 20 * time.Millisecond)
			frames, _ = fm.tick(at)
			check(frames)
		}
	})
}
