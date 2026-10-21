package modbus

import (
	"cmp"
	"slices"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
)

// PlannedPoint is one point of an input, ready to be planned: its device and
// signal, the configured point, its parsed address and its size.
type PlannedPoint struct {
	// Device is the name of the device the point belongs to.
	Device string
	// Signal is the point's signal name.
	Signal string
	// Point is the configured point.
	Point config.Point
	// Addr is the parsed address of the point.
	Addr Address
	// Size is the point's size in registers (bits for a bit area), as Size
	// returns it. Plan trusts it: the points have passed CheckPoint.
	Size uint16
}

// Block is one read request: a run of addresses of one unit and area that is
// read together at one interval. The points keep their address order, so a
// point's registers or bits start at Addr.Offset-Start in the block's reply.
type Block struct {
	// Unit is the unit the request is sent to.
	Unit uint8
	// Area is the data area, which fixes the function code.
	Area Area
	// Interval is the polling interval of the block's points.
	Interval time.Duration
	// Start is the first register or bit read.
	Start uint16
	// Count is the number of registers or bits read, at most MaxBlockRegisters
	// for hr and ir and MaxBlockBits for co and di.
	Count uint16
	// Points are the points decoded from the reply, ordered by offset.
	Points []PlannedPoint
}

// groupKey is what two points must share to be read by the same request.
type groupKey struct {
	unit     uint8
	area     Area
	interval time.Duration
}

// blockCap is the largest block of an area.
func blockCap(a Area) int {
	if a.IsRegister() {
		return MaxBlockRegisters
	}
	return MaxBlockBits
}

// Plan groups the points of an input into read requests. Points of one unit
// (the address override, else defaultUnit), area and interval are sorted by
// offset and merged into a block while they touch or overlap the block and the
// merged block stays within the area's cap; a gap of one address, or a point
// that would cross the cap, starts a new block. A point is never split, and
// points that overlap with different types share a block, each decoded from
// the same reply. The blocks are ordered by unit, area, interval and start, so
// the plan does not depend on the order of points. The input is not modified.
func Plan(points []PlannedPoint, defaultUnit uint8) []Block {
	groups := make(map[groupKey][]PlannedPoint)
	for _, p := range points {
		key := groupKey{unit: defaultUnit, area: p.Addr.Area, interval: p.Point.Read.Interval}
		if p.Addr.Unit != nil {
			key.unit = *p.Addr.Unit
		}
		groups[key] = append(groups[key], p)
	}

	keys := make([]groupKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b groupKey) int {
		return cmp.Or(
			cmp.Compare(a.unit, b.unit),
			cmp.Compare(a.area, b.area),
			cmp.Compare(a.interval, b.interval),
		)
	})

	blocks := make([]Block, 0, len(keys))
	for _, key := range keys {
		group := groups[key]
		slices.SortStableFunc(group, func(a, b PlannedPoint) int {
			return cmp.Compare(a.Addr.Offset, b.Addr.Offset)
		})
		blocks = append(blocks, mergeGroup(key, group)...)
	}
	return blocks
}

// mergeGroup merges points of one group, already sorted by offset, into blocks.
func mergeGroup(key groupKey, group []PlannedPoint) []Block {
	limit := blockCap(key.area)
	var blocks []Block
	var current *Block
	end := 0 // one past the last address the current block reads

	for _, p := range group {
		start := int(p.Addr.Offset)
		pointEnd := start + int(p.Size)
		if current != nil && start <= end && max(end, pointEnd)-int(current.Start) <= limit {
			current.Points = append(current.Points, p)
			end = max(end, pointEnd)
			current.Count = uint16(end - int(current.Start))
			continue
		}
		blocks = append(blocks, Block{
			Unit:     key.unit,
			Area:     key.area,
			Interval: key.interval,
			Start:    p.Addr.Offset,
			Count:    p.Size,
			Points:   []PlannedPoint{p},
		})
		current = &blocks[len(blocks)-1]
		end = pointEnd
	}
	return blocks
}
