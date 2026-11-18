package file

import (
	"strconv"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/model"
)

// qdasTimeLayouts are the K0004 layouts, tried in order.
var qdasTimeLayouts = mustTimeLayouts("DD.MM.YYYY/hh:mm:ss", "DD.MM.YYYY/hh:mm")

func mustTimeLayouts(specs ...string) []TimeLayout {
	layouts := make([]TimeLayout, len(specs))
	for i, spec := range specs {
		l, err := ParseTimeLayout(spec)
		if err != nil {
			panic("file: invalid built-in time layout")
		}
		layouts[i] = l
	}

	return layouts
}

// parseQDAS turns the bytes of one Q-DAS report into samples, with the
// contract of parseCSV. A point addresses a characteristic by its K2001, or
// by #<index> when it has none. modTime is the sample time of a file whose
// only record has no K0004. The work per record follows the points, not the
// number of characteristics.
func parseQDAS(p *plan, data []byte, modTime time.Time) ([]model.Sample, []string, error) {
	text, err := decodeText(data, p.conn.Encoding)
	if err != nil {
		return nil, nil, err
	}
	f, err := readQDAS(text)
	if err != nil {
		return nil, nil, err
	}
	if len(f.Records) == 0 {
		return nil, nil, nil
	}
	targets, kinds, err := qdasTargets(p, f)
	if err != nil {
		return nil, nil, err
	}

	var samples []model.Sample
	for n := range f.Records {
		v := f.view(n)
		recordTime := v.value("K0004")
		if len(f.Records) > 1 && !v.r.Malformed && recordTime == "" {
			return nil, nil, refuse("several records and one without K0004")
		}
		samples, kinds = p.qdasRecordSamples(samples, kinds, targets, v, recordTime, modTime.UTC())
	}

	return samples, kinds, nil
}

// qdasTargets maps each address a point uses to its characteristic index.
// An address no characteristic has, or one of an attributive characteristic,
// counts one KindMissingField per file; an address two characteristics share
// refuses the file. Only the characteristics with their own K2xxx lines are
// listed; the others all have the Default number, or #<index> without one.
func qdasTargets(p *plan, f *qdasFile) (map[string]int, []string, error) {
	byAddr := make(map[string][]int, len(f.Characteristics))
	for i := range f.Characteristics {
		addr := f.Characteristic(i).Address(i)
		byAddr[addr] = append(byAddr[addr], i)
	}
	others := f.Count - len(f.Characteristics)
	defaultAddr := strings.TrimSpace(f.Default.Number)
	var kinds []string
	targets := make(map[string]int, len(p.addrs))
	for _, addr := range p.addrs {
		idx := byAddr[addr]
		matches, index := len(idx), 0
		if matches > 0 {
			index = idx[0]
		}
		switch {
		case others == 0:
		case defaultAddr != "" && addr == defaultAddr:
			if matches += others; matches == 1 {
				index = f.firstWithoutOwn()
			}
		case defaultAddr == "":
			if i, ok := f.hashIndex(addr); ok {
				matches, index = matches+1, i
			}
		}
		switch {
		case matches > 1:
			return nil, nil, refuse("an addressed characteristic number appears twice")
		case matches == 0, f.Characteristic(index).Attributive:
			kinds = appendOnce(kinds, KindMissingField)
		default:
			targets[addr] = index
		}
	}

	return targets, kinds, nil
}

// hashIndex returns the index n of an address #<n> that names a
// characteristic without its own K2xxx lines.
func (f *qdasFile) hashIndex(addr string) (int, bool) {
	digits, ok := strings.CutPrefix(addr, "#")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 || n > f.Count || strconv.Itoa(n) != digits {
		return 0, false
	}
	if _, own := f.Characteristics[n]; own {
		return 0, false
	}

	return n, true
}

// firstWithoutOwn returns the lowest index without its own K2xxx lines; called
// when exactly one such index exists, so Count is the stored ones plus one.
func (f *qdasFile) firstWithoutOwn() int {
	for i := 1; i <= f.Count; i++ {
		if _, own := f.Characteristics[i]; !own {
			return i
		}
	}

	return 0
}

// qdasRecordSamples appends the samples of one record. A malformed record
// gives bad samples, no context and one KindMalformedRow; a K0004 that does
// not read gives bad samples and one KindBadTimestamp for the record.
func (p *plan) qdasRecordSamples(samples []model.Sample, kinds []string, targets map[string]int, v qdasView, recordTime string, modTime time.Time) ([]model.Sample, []string) {
	malformed := v.r.Malformed
	var ctx *model.Context
	if malformed {
		kinds = append(kinds, KindMalformedRow)
	} else {
		ctx = p.qdasContext(v)
	}
	badTime := false
	for _, addr := range p.addrs {
		i, ok := targets[addr]
		if !ok {
			continue
		}
		text := v.field(i, "K0001")
		if !malformed {
			if attr := v.field(i, "K0002"); text == "" || attr == "255" || attr == "256" {
				continue
			}
		}
		ts, bad := modTime, malformed
		if tsText := v.timeText(i, recordTime); tsText != "" {
			if t, ok := p.qdasTime(tsText); ok {
				ts = t
			} else if !malformed {
				bad, badTime = true, true
			}
		}
		samples, kinds = p.appendSamples(samples, kinds, addr, ts, text, bad, ctx)
	}
	if badTime {
		kinds = append(kinds, KindBadTimestamp)
	}

	return samples, kinds
}

// qdasTime reads a K0004 in the connection's location, in UTC.
func (p *plan) qdasTime(text string) (time.Time, bool) {
	for _, layout := range qdasTimeLayouts {
		if t, ok := layout.Parse(text, p.conn.Location); ok && inEnvelope(t) {
			return t, true
		}
	}

	return time.Time{}, false
}

// qdasContext reads the mapped context keys of a record; nil when all are
// empty.
func (p *plan) qdasContext(v qdasView) *model.Context {
	value := func(key string) string {
		if key = strings.TrimSpace(key); key == "" {
			return ""
		}
		return v.value(key)
	}

	return contextOrNil(model.Context{
		Serial:       value(p.conn.Context.Serial),
		OrderRef:     value(p.conn.Context.OrderRef),
		OperationRef: value(p.conn.Context.OperationRef),
		Lot:          value(p.conn.Context.Lot),
	})
}
