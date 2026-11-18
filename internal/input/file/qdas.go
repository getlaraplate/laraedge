package file

import (
	"strconv"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/model"
)

// Separators of a Q-DAS value line: between the blocks of the characteristics,
// and between the fields of one block.
const (
	qdasBlockSep = "\x0f"
	qdasFieldSep = "\x14"
)

// qdasMaxCharacteristics bounds K0100 and the characteristic indices, so a
// single line cannot make the reader allocate without limit.
const qdasMaxCharacteristics = 100000

// qdasValueKeys are the keys of the positional fields of a value line block.
var qdasValueKeys = [...]string{"K0001", "K0002", "K0004", "K0005", "K0006", "K0007", "K0008", "K0010", "K0011", "K0012"}

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

// qdasFile is a read Q-DAS report: its characteristics and its records.
type qdasFile struct {
	// Count is the number of characteristics.
	Count int
	// Characteristics holds characteristic i (1-based) at [i-1].
	Characteristics []qdasCharacteristic
	Records         []qdasRecord
}

// qdasCharacteristic is the description of one characteristic.
type qdasCharacteristic struct {
	// Number is K2001, Description K2002, Nominal K2101, Lower K2110, Upper
	// K2111 and Unit K2142.
	Number, Description, Nominal, Lower, Upper, Unit string
	// Attributive is K2004 equal to 1.
	Attributive bool
}

// Address is the address a point uses for characteristic index (1-based):
// the trimmed K2001, or #<index> when it has none.
func (c qdasCharacteristic) Address(index int) string {
	if n := strings.TrimSpace(c.Number); n != "" {
		return n
	}

	return "#" + strconv.Itoa(index)
}

// qdasRecord is one measurement of the characteristics.
type qdasRecord struct {
	// Fields holds the fields of characteristic i at [i-1], by key ("K0001");
	// a characteristic without fields has a nil map.
	Fields []map[string]string
	// Shared holds the fields written with index 0, for the whole record.
	Shared map[string]string
	// Malformed is a value line whose block count differs from Count.
	Malformed bool
}

// value returns the record's /0 field key, else that of the lowest
// characteristic index that has one; empty when none has.
func (r qdasRecord) value(key string) string {
	if v := r.Shared[key]; v != "" {
		return v
	}
	for _, fields := range r.Fields {
		if v := fields[key]; v != "" {
			return v
		}
	}

	return ""
}

// timeText returns the K0004 that applies to characteristic index i: its
// own, else the record's /0, else the lowest index one.
func (r qdasRecord) timeText(i int) string {
	if v := r.Fields[i-1]["K0004"]; v != "" {
		return v
	}

	return r.value("K0004")
}

// qdasPending is a record being read; fields are keyed by characteristic index.
type qdasPending struct {
	fields map[int]map[string]string
	shared map[string]string
	// blocks is the block count of a value line, -1 for a coded record.
	blocks int
}

func (r *qdasPending) set(index int, key, value string) {
	if index == 0 {
		if r.shared == nil {
			r.shared = make(map[string]string)
		}
		r.shared[key] = value
		return
	}
	if r.fields[index] == nil {
		r.fields[index] = make(map[string]string)
	}
	r.fields[index][key] = value
}

// qdasReader holds the state of readQDAS between lines.
type qdasReader struct {
	// k0100 is the K0100 value, -1 when absent.
	k0100 int
	// maxChar is the highest K2xxx index, maxCoded the highest K0001 to K0099 one.
	maxChar, maxCoded int
	chars             map[int]*qdasCharacteristic
	records           []*qdasPending
}

// count is the number of characteristics known so far.
func (q *qdasReader) count() int {
	if q.k0100 >= 0 {
		return q.k0100
	}

	return q.maxChar
}

// readQDAS reads Q-DAS transfer text into its characteristics and records.
// Errors wrap errMalformedFile and carry a fixed reason.
func readQDAS(text string) (*qdasFile, error) {
	q := &qdasReader{k0100: -1, chars: make(map[int]*qdasCharacteristic)}
	for line := range strings.Lines(text) {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		var err error
		if line[0] == 'K' {
			err = q.coded(line)
		} else {
			err = q.valueLine(line)
		}
		if err != nil {
			return nil, err
		}
	}

	return q.finish()
}

// parseCodedLine splits a coded line: K and four digits, an optional / and a
// decimal index, then one space or the end of the line. The key number is
// returned as an int and the value trimmed.
func parseCodedLine(line string) (key string, number, index int, value string, ok bool) {
	if len(line) < 5 || !IsKField(line[:5]) {
		return "", 0, 0, "", false
	}
	key, rest := line[:5], line[5:]
	number, _ = strconv.Atoi(key[1:])
	index = 1
	if after, found := strings.CutPrefix(rest, "/"); found {
		end := 0
		for end < len(after) && after[end] >= '0' && after[end] <= '9' {
			end++
		}
		n, err := strconv.Atoi(after[:end])
		if end == 0 || err != nil {
			return "", 0, 0, "", false
		}
		index, rest = n, after[end:]
	}
	if rest != "" && rest[0] != ' ' {
		return "", 0, 0, "", false
	}

	return key, number, index, strings.TrimSpace(rest), true
}

func (q *qdasReader) coded(line string) error {
	key, number, index, value, ok := parseCodedLine(line)
	if !ok {
		return refuse("a line starting with K is not a K-field")
	}
	if index > qdasMaxCharacteristics {
		return refuse("a K-field index is above the supported number of characteristics")
	}
	switch {
	case number == 1:
		return q.codedValue(index, value)
	case number >= 2 && number <= 99:
		q.codedField(key, index, value)
	case number == 100:
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > qdasMaxCharacteristics {
			return refuse("K0100 is not a supported number of characteristics")
		}
		q.k0100 = n
	case number >= 1000 && number <= 1999:
		if index > 1 {
			return refuse("more than one part type")
		}
	case number >= 2000 && number <= 2999:
		return q.description(number, index, value)
	}

	return nil
}

// codedValue reads K0001/index: it adds to the last coded record unless that
// one already has a value for index, else it starts a record.
func (q *qdasReader) codedValue(index int, value string) error {
	if index == 0 {
		return refuse("a value with index 0")
	}
	if q.count() == 0 {
		return refuse("a value before any characteristic")
	}
	q.maxCoded = max(q.maxCoded, index)
	var last *qdasPending
	if n := len(q.records); n > 0 {
		last = q.records[n-1]
	}
	if last == nil || last.blocks >= 0 {
		last = q.newRecord(-1)
	} else if _, has := last.fields[index]["K0001"]; has {
		last = q.newRecord(-1)
	}
	last.set(index, "K0001", value)

	return nil
}

// codedField reads K0002 to K0099: it sets the field of the last record, for
// characteristic index or, with index 0, for the whole record. Before any
// record it is ignored.
func (q *qdasReader) codedField(key string, index int, value string) {
	if len(q.records) == 0 {
		return
	}
	q.maxCoded = max(q.maxCoded, index)
	q.records[len(q.records)-1].set(index, key, value)
}

func (q *qdasReader) description(number, index int, value string) error {
	if index == 0 {
		return refuse("a characteristic with index 0")
	}
	q.maxChar = max(q.maxChar, index)
	c := q.chars[index]
	if c == nil {
		c = &qdasCharacteristic{}
		q.chars[index] = c
	}
	switch number {
	case 2001:
		c.Number = value
	case 2002:
		c.Description = value
	case 2004:
		c.Attributive = value == "1"
	case 2101:
		c.Nominal = value
	case 2110:
		c.Lower = value
	case 2111:
		c.Upper = value
	case 2142:
		c.Unit = value
	}

	return nil
}

// valueLine reads one value line as a record: one block per characteristic,
// the positional fields of each block under their keys, trimmed.
func (q *qdasReader) valueLine(line string) error {
	if q.count() == 0 {
		return refuse("a value line before any characteristic")
	}
	blocks := strings.Split(line, qdasBlockSep)
	r := q.newRecord(len(blocks))
	for i, block := range blocks {
		fields := strings.SplitN(block, qdasFieldSep, len(qdasValueKeys)+1)
		m := make(map[string]string, len(qdasValueKeys))
		for k, key := range qdasValueKeys {
			if k < len(fields) {
				m[key] = strings.TrimSpace(fields[k])
			} else {
				m[key] = ""
			}
		}
		r.fields[i+1] = m
	}

	return nil
}

func (q *qdasReader) newRecord(blocks int) *qdasPending {
	r := &qdasPending{fields: make(map[int]map[string]string), blocks: blocks}
	q.records = append(q.records, r)

	return r
}

// finish checks the indices against the count and builds the file.
func (q *qdasReader) finish() (*qdasFile, error) {
	count := q.count()
	if q.maxChar > count || q.maxCoded > count {
		return nil, refuse("a K-field index is above the number of characteristics")
	}
	f := &qdasFile{Count: count, Characteristics: make([]qdasCharacteristic, count)}
	for i, c := range q.chars {
		f.Characteristics[i-1] = *c
	}
	f.Records = make([]qdasRecord, len(q.records))
	for n, pending := range q.records {
		r := qdasRecord{
			Fields:    make([]map[string]string, count),
			Shared:    pending.shared,
			Malformed: pending.blocks >= 0 && pending.blocks != count,
		}
		for i := range r.Fields {
			r.Fields[i] = pending.fields[i+1]
		}
		f.Records[n] = r
	}

	return f, nil
}

// parseQDAS turns the bytes of one Q-DAS report into samples, with the
// contract of parseCSV. A point addresses a characteristic by its K2001, or
// by #<index> when it has none. modTime is the sample time of a file whose
// only record has no K0004.
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
	if len(f.Records) > 1 {
		for _, r := range f.Records {
			if !r.Malformed && r.value("K0004") == "" {
				return nil, nil, refuse("several records and one without K0004")
			}
		}
	}

	var samples []model.Sample
	for _, r := range f.Records {
		samples, kinds = p.qdasRecordSamples(samples, kinds, targets, r, modTime.UTC())
	}

	return samples, kinds, nil
}

// qdasTargets maps each address a point uses to its characteristic index.
// An address no characteristic has, or one of an attributive characteristic,
// counts one KindMissingField per file; an address two characteristics share
// refuses the file.
func qdasTargets(p *plan, f *qdasFile) (map[string]int, []string, error) {
	byAddr := make(map[string][]int, f.Count)
	for i, c := range f.Characteristics {
		addr := c.Address(i + 1)
		byAddr[addr] = append(byAddr[addr], i+1)
	}
	var kinds []string
	targets := make(map[string]int, len(p.addrs))
	for _, addr := range p.addrs {
		idx := byAddr[addr]
		switch {
		case len(idx) > 1:
			return nil, nil, refuse("an addressed characteristic number appears twice")
		case len(idx) == 0, f.Characteristics[idx[0]-1].Attributive:
			kinds = appendOnce(kinds, KindMissingField)
		default:
			targets[addr] = idx[0]
		}
	}

	return targets, kinds, nil
}

// qdasRecordSamples appends the samples of one record. A malformed record
// gives bad samples, no context and one KindMalformedRow; a K0004 that does
// not read gives bad samples and one KindBadTimestamp for the record.
func (p *plan) qdasRecordSamples(samples []model.Sample, kinds []string, targets map[string]int, r qdasRecord, modTime time.Time) ([]model.Sample, []string) {
	var ctx *model.Context
	if r.Malformed {
		kinds = append(kinds, KindMalformedRow)
	} else {
		ctx = p.qdasContext(r)
	}
	badTime := false
	for _, addr := range p.addrs {
		i, ok := targets[addr]
		if !ok {
			continue
		}
		fields := r.Fields[i-1]
		text := fields["K0001"]
		if !r.Malformed {
			if attr := fields["K0002"]; text == "" || attr == "255" || attr == "256" {
				continue
			}
		}
		ts, bad := modTime, r.Malformed
		if tsText := r.timeText(i); tsText != "" {
			if t, ok := p.qdasTime(tsText); ok {
				ts = t
			} else if !r.Malformed {
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
func (p *plan) qdasContext(r qdasRecord) *model.Context {
	value := func(key string) string {
		if key = strings.TrimSpace(key); key == "" {
			return ""
		}
		return r.value(key)
	}

	return contextOrNil(model.Context{
		Serial:       value(p.conn.Context.Serial),
		OrderRef:     value(p.conn.Context.OrderRef),
		OperationRef: value(p.conn.Context.OperationRef),
		Lot:          value(p.conn.Context.Lot),
	})
}
