package file

import (
	"strings"

	"github.com/getlaraplate/laraedge/internal/config"
)

// PlannedPoint is one point of an input with the device it belongs to.
type PlannedPoint struct {
	Device string
	Signal string
	Point  config.Point
}

// plan is everything a parser needs for one input: the connection and the
// points grouped by their trimmed address (a column name or a key). Several
// points may share an address; each gets its own sample.
type plan struct {
	conn Connection
	// addrs lists the addresses in order of first appearance.
	addrs  []string
	points map[string][]PlannedPoint
}

// newPlan groups the points of all devices of cfg by trimmed address.
func newPlan(cfg config.Input, conn Connection) *plan {
	p := &plan{conn: conn, points: make(map[string][]PlannedPoint)}
	for _, dev := range cfg.Devices {
		for _, pt := range dev.Points {
			addr := strings.TrimSpace(pt.Address)
			if _, ok := p.points[addr]; !ok {
				p.addrs = append(p.addrs, addr)
			}
			p.points[addr] = append(p.points[addr], PlannedPoint{Device: dev.Device, Signal: pt.Signal, Point: pt})
		}
	}

	return p
}
