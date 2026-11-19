package serial

import (
	"errors"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/model"
)

// Read error kinds the router reports (spec section 8).
const (
	// KindDecode is a frame that is not valid in the connection's encoding.
	KindDecode = "decode"
	// KindUnmatched is a frame no context rule or point matches.
	KindUnmatched = "unmatched"
	// KindConversion is a captured value that does not convert to the type.
	KindConversion = "conversion"
)

// routePoint is one point with its compiled address.
type routePoint struct {
	device, signal string
	point          config.Point
	address        *regexp.Regexp
	path           string
}

// contextEntry is a context value with the time it was set.
type contextEntry struct {
	value string
	set   time.Time
}

// contextHistory keeps the current entry of a key and the one before it, so a
// frame read before the latest change still sees the value it was read under.
type contextHistory struct {
	cur, prev contextEntry
	hasPrev   bool
}

// pick returns the entry in force at at, ok false when none was set by then.
func (h contextHistory) pick(at time.Time) (contextEntry, bool) {
	if !h.cur.set.After(at) {
		return h.cur, true
	}
	if h.hasPrev && !h.prev.set.After(at) {
		return h.prev, true
	}
	return contextEntry{}, false
}

// router turns frames into samples and context changes (spec section 7). It is
// pure: no clock, no logging, no locks; the caller serialises access.
type router struct {
	conn   Connection
	points []routePoint
	keys   map[string]contextHistory
}

// routed is the outcome of one frame: a sample, a context change, or a Kind
// naming why the frame was dropped (a conversion also carries its bad sample).
type routed struct {
	Sample       *model.Sample
	Kind         string
	ContextKey   string
	ContextValue string
}

// newRouter compiles the points of cfg in document order across devices. The
// error names the input position of the point, never an address.
func newRouter(cfg config.Input, conn Connection) (*router, error) {
	r := &router{conn: conn, keys: make(map[string]contextHistory)}
	for di, dev := range cfg.Devices {
		for pi, p := range dev.Points {
			re, err := CheckPoint(p)
			if err != nil {
				return nil, errors.New("serial input " + strconv.Quote(cfg.ID) + ": devices." + strconv.Itoa(di) + ".points." + strconv.Itoa(pi) + "." + err.Error())
			}
			r.points = append(r.points, routePoint{
				device: dev.Device, signal: p.Signal, point: p, address: re,
				path: "devices." + strconv.Itoa(di) + ".points." + strconv.Itoa(pi),
			})
		}
	}
	return r, nil
}

// duplicateAddresses lists the paths whose address repeats an earlier one.
func (r *router) duplicateAddresses() []string {
	seen := make(map[string]bool, len(r.points))
	var dup []string
	for _, p := range r.points {
		text := p.address.String()
		if seen[text] {
			dup = append(dup, p.path)
		}
		seen[text] = true
	}
	return dup
}

// decode converts a frame to UTF-8 text from the connection's encoding.
func (r *router) decode(frame []byte) (string, bool) {
	switch r.conn.Encoding {
	case "windows-1252":
		out, err := charmap.Windows1252.NewDecoder().Bytes(frame)
		return string(out), err == nil
	case "iso-8859-1":
		out, err := charmap.ISO8859_1.NewDecoder().Bytes(frame)
		return string(out), err == nil
	default:
		return string(frame), utf8.Valid(frame)
	}
}

// route handles one frame. Frames are expected in read order and at is the
// time the frame's last byte was read: a context key applies only to frames
// read at or after the time it was set, so a frame read before a scan but
// routed after it still carries the earlier value.
func (r *router) route(frame []byte, at time.Time) routed {
	text, ok := r.decode(frame)
	if !ok {
		return routed{Kind: KindDecode}
	}
	for _, rule := range r.conn.Context {
		m := rule.Match.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		value := m[rule.Group]
		h, had := r.keys[rule.Key]
		r.keys[rule.Key] = contextHistory{cur: contextEntry{value: value, set: at}, prev: h.cur, hasPrev: had}
		return routed{ContextKey: rule.Key, ContextValue: value}
	}
	for _, p := range r.points {
		m := p.address.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		value := m[0]
		if p.address.NumSubexp() > 0 {
			value = m[1]
		}
		s := model.Sample{Device: p.device, Signal: p.signal, TS: at, Priority: p.point.Priority, Context: r.context(at)}
		if v, ok := driverkit.ConvertText(p.point.Type, value); ok {
			s.Value, s.Quality = v, model.Good
			return routed{Sample: &s}
		}
		s.Value, s.Quality = driverkit.ZeroValue(p.point.Type), model.Bad
		return routed{Sample: &s, Kind: KindConversion}
	}
	return routed{Kind: KindUnmatched}
}

// context returns a copy of the current context as of at, keys older than the
// TTL dropped; nil when nothing is left.
func (r *router) context(at time.Time) *model.Context {
	var c model.Context
	for key, h := range r.keys {
		e, ok := h.pick(at)
		if !ok || e.value == "" {
			continue
		}
		if r.conn.ContextTTL > 0 && at.Sub(e.set) > r.conn.ContextTTL {
			continue
		}
		switch key {
		case "serial":
			c.Serial = e.value
		case "order_ref":
			c.OrderRef = e.value
		case "operation_ref":
			c.OperationRef = e.value
		case "lot":
			c.Lot = e.value
		}
	}
	if c.IsEmpty() {
		return nil
	}
	return &c
}
