package modbus

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
)

// planned builds a PlannedPoint on area at offset spanning size registers or
// bits, polled every interval, with an optional unit override.
func planned(signal string, area Area, offset, size uint16, interval time.Duration, unit *uint8) PlannedPoint {
	return PlannedPoint{
		Device: "dev",
		Signal: signal,
		Point:  config.Point{Signal: signal, Read: config.Read{Mode: config.ReadPoll, Interval: interval}},
		Addr:   Address{Area: area, Offset: offset, Unit: unit},
		Size:   size,
	}
}

func unitPtr(u uint8) *uint8 { return &u }

// shape is a block reduced to what the tests compare.
type shape struct {
	Unit     uint8
	Area     Area
	Interval time.Duration
	Start    uint16
	Count    uint16
	Signals  []string
}

func shapes(blocks []Block) []shape {
	out := make([]shape, 0, len(blocks))
	for _, b := range blocks {
		signals := make([]string, 0, len(b.Points))
		for _, p := range b.Points {
			signals = append(signals, p.Signal)
		}
		out = append(out, shape{b.Unit, b.Area, b.Interval, b.Start, b.Count, signals})
	}
	return out
}

func TestPlan(t *testing.T) {
	const s = time.Second
	tests := []struct {
		name        string
		points      []PlannedPoint
		defaultUnit uint8
		want        []shape
	}{
		{
			name:   "no points",
			points: nil,
			want:   []shape{},
		},
		{
			name: "contiguous points merge",
			points: []PlannedPoint{
				planned("a", AreaHoldingRegister, 10, 1, s, nil),
				planned("b", AreaHoldingRegister, 11, 2, s, nil),
				planned("c", AreaHoldingRegister, 13, 1, s, nil),
			},
			defaultUnit: 1,
			want:        []shape{{1, AreaHoldingRegister, s, 10, 4, []string{"a", "b", "c"}}},
		},
		{
			name: "input order does not matter",
			points: []PlannedPoint{
				planned("c", AreaHoldingRegister, 13, 1, s, nil),
				planned("a", AreaHoldingRegister, 10, 1, s, nil),
				planned("b", AreaHoldingRegister, 11, 2, s, nil),
			},
			defaultUnit: 1,
			want:        []shape{{1, AreaHoldingRegister, s, 10, 4, []string{"a", "b", "c"}}},
		},
		{
			name: "a gap of one register splits",
			points: []PlannedPoint{
				planned("a", AreaHoldingRegister, 10, 1, s, nil),
				planned("b", AreaHoldingRegister, 12, 1, s, nil),
			},
			defaultUnit: 1,
			want: []shape{
				{1, AreaHoldingRegister, s, 10, 1, []string{"a"}},
				{1, AreaHoldingRegister, s, 12, 1, []string{"b"}},
			},
		},
		{
			name: "a gap after a wide point splits",
			points: []PlannedPoint{
				planned("a", AreaInputRegister, 0, 4, s, nil),
				planned("b", AreaInputRegister, 5, 1, s, nil),
			},
			defaultUnit: 1,
			want: []shape{
				{1, AreaInputRegister, s, 0, 4, []string{"a"}},
				{1, AreaInputRegister, s, 5, 1, []string{"b"}},
			},
		},
		{
			name: "overlapping points of different types share one block",
			points: []PlannedPoint{
				planned("as_float", AreaHoldingRegister, 100, 2, s, nil),
				planned("low_word", AreaHoldingRegister, 101, 1, s, nil),
				planned("as_double", AreaHoldingRegister, 100, 4, s, nil),
			},
			defaultUnit: 1,
			want:        []shape{{1, AreaHoldingRegister, s, 100, 4, []string{"as_float", "as_double", "low_word"}}},
		},
		{
			name: "identical addresses share one block",
			points: []PlannedPoint{
				planned("a", AreaHoldingRegister, 7, 1, s, nil),
				planned("b", AreaHoldingRegister, 7, 1, s, nil),
			},
			defaultUnit: 1,
			want:        []shape{{1, AreaHoldingRegister, s, 7, 1, []string{"a", "b"}}},
		},
		{
			name: "different units never merge, override beats default",
			points: []PlannedPoint{
				planned("a", AreaHoldingRegister, 0, 1, s, nil),
				planned("b", AreaHoldingRegister, 1, 1, s, unitPtr(2)),
				planned("c", AreaHoldingRegister, 2, 1, s, unitPtr(1)),
			},
			defaultUnit: 1,
			want: []shape{
				{1, AreaHoldingRegister, s, 0, 1, []string{"a"}},
				{1, AreaHoldingRegister, s, 2, 1, []string{"c"}},
				{2, AreaHoldingRegister, s, 1, 1, []string{"b"}},
			},
		},
		{
			name: "the default unit applies when there is no override",
			points: []PlannedPoint{
				planned("a", AreaHoldingRegister, 0, 1, s, nil),
				planned("b", AreaHoldingRegister, 1, 1, s, unitPtr(9)),
			},
			defaultUnit: 9,
			want:        []shape{{9, AreaHoldingRegister, s, 0, 2, []string{"a", "b"}}},
		},
		{
			name: "different areas never merge",
			points: []PlannedPoint{
				planned("hr", AreaHoldingRegister, 0, 1, s, nil),
				planned("ir", AreaInputRegister, 1, 1, s, nil),
				planned("co", AreaCoil, 2, 1, s, nil),
				planned("di", AreaDiscreteInput, 3, 1, s, nil),
			},
			defaultUnit: 1,
			want: []shape{
				{1, AreaCoil, s, 2, 1, []string{"co"}},
				{1, AreaDiscreteInput, s, 3, 1, []string{"di"}},
				{1, AreaHoldingRegister, s, 0, 1, []string{"hr"}},
				{1, AreaInputRegister, s, 1, 1, []string{"ir"}},
			},
		},
		{
			name: "different intervals never merge",
			points: []PlannedPoint{
				planned("slow", AreaHoldingRegister, 0, 1, 5*s, nil),
				planned("fast", AreaHoldingRegister, 1, 1, s, nil),
			},
			defaultUnit: 1,
			want: []shape{
				{1, AreaHoldingRegister, s, 1, 1, []string{"fast"}},
				{1, AreaHoldingRegister, 5 * s, 0, 1, []string{"slow"}},
			},
		},
		{
			name:        "the last register of the address space",
			points:      []PlannedPoint{planned("last", AreaHoldingRegister, 65535, 1, s, nil)},
			defaultUnit: 1,
			want:        []shape{{1, AreaHoldingRegister, s, 65535, 1, []string{"last"}}},
		},
		{
			name: "points ending at the top of the address space merge",
			points: []PlannedPoint{
				planned("last", AreaHoldingRegister, 65535, 1, s, nil),
				planned("pair", AreaHoldingRegister, 65534, 2, s, nil),
			},
			defaultUnit: 1,
			want:        []shape{{1, AreaHoldingRegister, s, 65534, 2, []string{"pair", "last"}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shapes(Plan(tt.points, tt.defaultUnit))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Plan() =\n %+v\nwant\n %+v", got, tt.want)
			}
		})
	}
}

// run builds n one-size points from first, contiguous.
func run(area Area, first uint16, n int) []PlannedPoint {
	points := make([]PlannedPoint, 0, n)
	for i := range n {
		points = append(points, planned(fmt.Sprintf("p%d", i), area, first+uint16(i), 1, time.Second, nil))
	}
	return points
}

func TestPlanCaps(t *testing.T) {
	tests := []struct {
		name       string
		area       Area
		points     int
		wantCounts []uint16
	}{
		{"registers exactly at the cap", AreaHoldingRegister, MaxBlockRegisters, []uint16{125}},
		{"registers one over the cap", AreaHoldingRegister, MaxBlockRegisters + 1, []uint16{125, 1}},
		{"input registers split at the cap", AreaInputRegister, 2 * MaxBlockRegisters, []uint16{125, 125}},
		{"bits exactly at the cap", AreaCoil, MaxBlockBits, []uint16{2000}},
		{"bits one over the cap", AreaDiscreteInput, MaxBlockBits + 1, []uint16{2000, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks := Plan(run(tt.area, 0, tt.points), 1)
			var counts []uint16
			next := uint16(0)
			total := 0
			for _, b := range blocks {
				counts = append(counts, b.Count)
				if b.Start != next {
					t.Errorf("block starts at %d, want %d", b.Start, next)
				}
				next = b.Start + b.Count
				total += len(b.Points)
			}
			if !reflect.DeepEqual(counts, tt.wantCounts) {
				t.Errorf("counts = %v, want %v", counts, tt.wantCounts)
			}
			if total != tt.points {
				t.Errorf("points in blocks = %d, want %d", total, tt.points)
			}
		})
	}
}

func TestPlanWidePointMovesWholeToNextBlock(t *testing.T) {
	// 123 single registers (0..122), then a 4-register float64 at 123..126
	// that would end at 127 and cross the 125 cap: it moves whole to the next
	// block, which starts at its own offset.
	points := run(AreaHoldingRegister, 0, 123)
	points = append(points, planned("wide", AreaHoldingRegister, 123, 4, time.Second, nil))
	points = append(points, planned("after", AreaHoldingRegister, 127, 1, time.Second, nil))

	got := Plan(points, 1)
	if len(got) != 2 {
		t.Fatalf("blocks = %d, want 2: %+v", len(got), shapes(got))
	}
	if got[0].Start != 0 || got[0].Count != 123 || len(got[0].Points) != 123 {
		t.Errorf("first block = start %d count %d points %d, want 0, 123, 123", got[0].Start, got[0].Count, len(got[0].Points))
	}
	second := shape{1, AreaHoldingRegister, time.Second, 123, 5, []string{"wide", "after"}}
	if g := shapes(got)[1]; !reflect.DeepEqual(g, second) {
		t.Errorf("second block = %+v, want %+v", g, second)
	}
}

func TestPlanOverlapAtTheCapStartsNewBlock(t *testing.T) {
	// A point overlapping the block's last register but ending past the cap
	// is not split: it starts a new block.
	points := []PlannedPoint{
		planned("fill", AreaHoldingRegister, 0, 125, time.Second, nil),
		planned("overlap", AreaHoldingRegister, 124, 2, time.Second, nil),
	}
	got := shapes(Plan(points, 1))
	want := []shape{
		{1, AreaHoldingRegister, time.Second, 0, 125, []string{"fill"}},
		{1, AreaHoldingRegister, time.Second, 124, 2, []string{"overlap"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Plan() =\n %+v\nwant\n %+v", got, want)
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	points := []PlannedPoint{
		planned("a", AreaInputRegister, 5, 1, 2*time.Second, unitPtr(3)),
		planned("b", AreaHoldingRegister, 0, 2, time.Second, nil),
		planned("c", AreaCoil, 9, 1, time.Second, unitPtr(3)),
		planned("d", AreaHoldingRegister, 20, 1, time.Second, nil),
		planned("e", AreaHoldingRegister, 1, 1, time.Second, nil),
	}
	want := shapes(Plan(points, 1))
	for i := range 20 {
		rotated := append(append([]PlannedPoint{}, points[i%len(points):]...), points[:i%len(points)]...)
		if got := shapes(Plan(rotated, 1)); !reflect.DeepEqual(got, want) {
			t.Fatalf("rotation %d: Plan() = %+v, want %+v", i, got, want)
		}
	}
}

func TestPlanKeepsPointValues(t *testing.T) {
	in := planned("a", AreaHoldingRegister, 4, 2, time.Second, unitPtr(7))
	in.Device = "press-1"
	got := Plan([]PlannedPoint{in}, 1)
	if len(got) != 1 || len(got[0].Points) != 1 || !reflect.DeepEqual(got[0].Points[0], in) {
		t.Errorf("Plan() = %+v, want the point carried unchanged", got)
	}
}

func TestPlanDoesNotModifyItsInput(t *testing.T) {
	points := []PlannedPoint{
		planned("b", AreaHoldingRegister, 5, 1, time.Second, nil),
		planned("a", AreaHoldingRegister, 1, 1, time.Second, nil),
	}
	Plan(points, 1)
	if points[0].Signal != "b" || points[1].Signal != "a" {
		t.Errorf("input reordered: %q, %q", points[0].Signal, points[1].Signal)
	}
}
