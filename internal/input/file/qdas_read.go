package file

import (
	"strconv"
	"strings"
)

// Separators of a Q-DAS value line: between the blocks of the characteristics,
// and between the fields of one block.
const (
	qdasBlockSep = "\x0f"
	qdasFieldSep = "\x14"
)

// qdasMaxCharacteristics bounds K0100 and the characteristic indices.
const qdasMaxCharacteristics = 100000

// qdasScanLimit is the number of coded fields of a record looked up by a
// linear scan; a record with more gets an index built when it is mapped.
const qdasScanLimit = 8

// qdasValueKeys are the keys of the positional fields of a value line block.
var qdasValueKeys = [...]string{"K0001", "K0002", "K0004", "K0005", "K0006", "K0007", "K0008", "K0010", "K0011", "K0012"}

// qdasFile is a read Q-DAS report: its characteristics and its records. The
// storage grows with the text, never with Count: only the characteristics
// with their own K2xxx lines are stored, and a record keeps its value line
// and its coded fields as read.
type qdasFile struct {
	// Count is the number of characteristics.
	Count int
	// Characteristics holds the own K2xxx values of the characteristics that
	// have some, by index (1-based); read them through Characteristic.
	Characteristics map[int]qdasCharacteristic
	// Default holds the K2xxx values written with index 0.
	Default qdasCharacteristic
	Records []qdasRecord
	// coded holds the coded K0001 to K0099 fields of every record, the fields
	// of one record contiguous.
	coded []qdasCoded
}

// Own-value bits of a qdasCharacteristic, one per K2xxx field.
const (
	qdasOwnNumber uint8 = 1 << iota
	qdasOwnDescription
	qdasOwnNominal
	qdasOwnLower
	qdasOwnUpper
	qdasOwnUnit
	qdasOwnAttributive
)

// qdasCharacteristic is the description of one characteristic.
type qdasCharacteristic struct {
	// Number is K2001, Description K2002, Nominal K2101, Lower K2110, Upper
	// K2111 and Unit K2142.
	Number, Description, Nominal, Lower, Upper, Unit string
	// Attributive is K2004 equal to 1.
	Attributive bool
	// own marks the fields written for this index; the others take Default.
	own uint8
}

// set records the K2xxx field number; numbers the driver does not use are
// ignored.
func (c *qdasCharacteristic) set(number int, value string) {
	switch number {
	case 2001:
		c.Number, c.own = value, c.own|qdasOwnNumber
	case 2002:
		c.Description, c.own = value, c.own|qdasOwnDescription
	case 2004:
		c.Attributive, c.own = value == "1", c.own|qdasOwnAttributive
	case 2101:
		c.Nominal, c.own = value, c.own|qdasOwnNominal
	case 2110:
		c.Lower, c.own = value, c.own|qdasOwnLower
	case 2111:
		c.Upper, c.own = value, c.own|qdasOwnUpper
	case 2142:
		c.Unit, c.own = value, c.own|qdasOwnUnit
	}
}

// Characteristic returns characteristic index (1-based): its own values, and
// the index 0 ones for the fields it has none for, whatever the line order.
func (f *qdasFile) Characteristic(index int) qdasCharacteristic {
	c, d := f.Characteristics[index], f.Default
	if c.own&qdasOwnNumber == 0 {
		c.Number = d.Number
	}
	if c.own&qdasOwnDescription == 0 {
		c.Description = d.Description
	}
	if c.own&qdasOwnAttributive == 0 {
		c.Attributive = d.Attributive
	}
	if c.own&qdasOwnNominal == 0 {
		c.Nominal = d.Nominal
	}
	if c.own&qdasOwnLower == 0 {
		c.Lower = d.Lower
	}
	if c.own&qdasOwnUpper == 0 {
		c.Upper = d.Upper
	}
	if c.own&qdasOwnUnit == 0 {
		c.Unit = d.Unit
	}
	c.own = 0

	return c
}

// Address is the address a point uses for characteristic index (1-based):
// the trimmed K2001, or #<index> when it has none.
func (c qdasCharacteristic) Address(index int) string {
	if n := strings.TrimSpace(c.Number); n != "" {
		return n
	}

	return "#" + strconv.Itoa(index)
}

// qdasCoded is one coded K0001 to K0099 field of a record; index 0 is the
// whole record.
type qdasCoded struct {
	index      int
	key, value string
}

// qdasRecord is one measurement of the characteristics: a value line, the
// coded fields after it, or coded fields only.
type qdasRecord struct {
	// Malformed is a value line whose block count differs from Count.
	Malformed bool
	// line is the value line, empty for a coded record.
	line string
	// blocks is the block count of the value line, -1 for a coded record.
	blocks int
	// starts holds the offset of each block in line; nil for one block.
	starts []int
	// codedLo and codedHi delimit the record's fields in qdasFile.coded.
	codedLo, codedHi int
}

// positional returns the trimmed field at position pos of block index
// (1-based) of the value line; empty when the block or the field is absent.
func (r *qdasRecord) positional(index, pos int) string {
	if index > r.blocks {
		return ""
	}
	block := r.line
	if r.starts != nil {
		end := len(r.line)
		if index < len(r.starts) {
			end = r.starts[index] - len(qdasBlockSep)
		}
		block = r.line[r.starts[index-1]:end]
	}
	for range pos {
		var ok bool
		if _, block, ok = strings.Cut(block, qdasFieldSep); !ok {
			return ""
		}
	}
	field, _, _ := strings.Cut(block, qdasFieldSep)

	return strings.TrimSpace(field)
}

// qdasKeyPos is the position of key in a value line block, -1 for a key a
// value line does not carry.
func qdasKeyPos(key string) int {
	for i, k := range qdasValueKeys {
		if k == key {
			return i
		}
	}

	return -1
}

// qdasSlot is a coded field of a record: its characteristic index and key.
type qdasSlot struct {
	index int
	key   string
}

// qdasView reads the fields of one record.
type qdasView struct {
	f *qdasFile
	r *qdasRecord
	// over indexes the coded fields when there are more than qdasScanLimit.
	over map[qdasSlot]string
}

// view returns the reader of record n.
func (f *qdasFile) view(n int) qdasView {
	v := qdasView{f: f, r: &f.Records[n]}
	if entries := v.entries(); len(entries) > qdasScanLimit {
		v.over = make(map[qdasSlot]string, len(entries))
		for _, e := range entries {
			v.over[qdasSlot{e.index, e.key}] = e.value
		}
	}

	return v
}

// Field returns the field key of characteristic index (1-based), or with
// index 0 the record-wide one, of record n.
func (f *qdasFile) Field(n, index int, key string) string {
	v := f.view(n)
	if index == 0 {
		value, _ := v.coded(0, key)
		return value
	}

	return v.field(index, key)
}

func (v qdasView) entries() []qdasCoded {
	return v.f.coded[v.r.codedLo:v.r.codedHi]
}

// coded returns the last coded value of key for index, and whether there is
// one.
func (v qdasView) coded(index int, key string) (string, bool) {
	if v.over != nil {
		value, ok := v.over[qdasSlot{index, key}]
		return value, ok
	}
	entries := v.entries()
	for i := len(entries) - 1; i >= 0; i-- {
		if e := entries[i]; e.index == index && e.key == key {
			return e.value, true
		}
	}

	return "", false
}

// field returns the field key of characteristic index (1-based): a coded
// value written after the value line wins over the positional one.
func (v qdasView) field(index int, key string) string {
	if value, ok := v.coded(index, key); ok {
		return value
	}
	if pos := qdasKeyPos(key); pos >= 0 && index <= v.f.Count {
		return v.r.positional(index, pos)
	}

	return ""
}

// value returns the record's /0 field key, else that of the lowest
// characteristic index that has one; empty when none has.
func (v qdasView) value(key string) string {
	if value, _ := v.coded(0, key); value != "" {
		return value
	}
	best, bestValue := 0, ""
	for _, e := range v.entries() {
		if e.key != key || e.index == 0 || (best > 0 && e.index >= best) {
			continue
		}
		if value := v.field(e.index, key); value != "" {
			best, bestValue = e.index, value
		}
	}
	if pos := qdasKeyPos(key); pos >= 0 {
		limit := min(v.r.blocks, v.f.Count)
		if best > 0 {
			limit = min(limit, best-1)
		}
		for i := 1; i <= limit; i++ {
			if _, overridden := v.coded(i, key); overridden {
				continue
			}
			if value := v.r.positional(i, pos); value != "" {
				return value
			}
		}
	}

	return bestValue
}

// timeText returns the K0004 that applies to characteristic index: its own,
// else recordTime, the record's value of K0004.
func (v qdasView) timeText(index int, recordTime string) string {
	if value := v.field(index, "K0004"); value != "" {
		return value
	}

	return recordTime
}

// qdasReader holds the state of readQDAS between lines.
type qdasReader struct {
	f *qdasFile
	// k0100 is the K0100 value, -1 when absent.
	k0100 int
	// maxChar is the highest K2xxx index, maxCoded the highest K0001 to K0099 one.
	maxChar, maxCoded int
	// valued holds the indices with a K0001 in the last coded record.
	valued map[int]struct{}
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
	q := &qdasReader{
		f:      &qdasFile{Characteristics: make(map[int]qdasCharacteristic)},
		k0100:  -1,
		valued: make(map[int]struct{}),
	}
	for line := range strings.Lines(text) {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		var err error
		if line[0] == 'K' {
			err = q.codedLine(line)
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

func (q *qdasReader) codedLine(line string) error {
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
		q.description(number, index, value)
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
	records := q.f.Records
	_, valued := q.valued[index]
	if len(records) == 0 || records[len(records)-1].blocks >= 0 || valued {
		clear(q.valued)
		q.f.Records = append(q.f.Records, qdasRecord{blocks: -1, codedLo: len(q.f.coded), codedHi: len(q.f.coded)})
	}
	q.valued[index] = struct{}{}
	q.appendCoded(index, "K0001", value)

	return nil
}

// codedField reads K0002 to K0099: it sets the field of the last record, for
// characteristic index or, with index 0, for the whole record. Before any
// record it is ignored, its index still checked against the count.
func (q *qdasReader) codedField(key string, index int, value string) {
	q.maxCoded = max(q.maxCoded, index)
	if len(q.f.Records) == 0 {
		return
	}
	q.appendCoded(index, key, value)
}

// appendCoded adds a coded field to the last record.
func (q *qdasReader) appendCoded(index int, key, value string) {
	q.f.coded = append(q.f.coded, qdasCoded{index: index, key: key, value: value})
	q.f.Records[len(q.f.Records)-1].codedHi = len(q.f.coded)
}

// description reads a K2xxx line. With index 0 it applies to every
// characteristic without its own value for that key.
func (q *qdasReader) description(number, index int, value string) {
	if index == 0 {
		q.f.Default.set(number, value)
		return
	}
	q.maxChar = max(q.maxChar, index)
	c := q.f.Characteristics[index]
	c.set(number, value)
	q.f.Characteristics[index] = c
}

// valueLine reads one value line as a record, one block per characteristic.
// The line is kept as read with the offset of each block; the fields are
// read when the record is mapped.
func (q *qdasReader) valueLine(line string) error {
	if q.count() == 0 {
		return refuse("a value line before any characteristic")
	}
	r := qdasRecord{line: line, blocks: strings.Count(line, qdasBlockSep) + 1, codedLo: len(q.f.coded), codedHi: len(q.f.coded)}
	if r.blocks > 1 {
		r.starts = make([]int, 1, r.blocks)
		for i := 0; ; {
			j := strings.Index(line[i:], qdasBlockSep)
			if j < 0 {
				break
			}
			i += j + len(qdasBlockSep)
			r.starts = append(r.starts, i)
		}
	}
	q.f.Records = append(q.f.Records, r)

	return nil
}

// finish checks the indices against the count and marks the value lines
// whose block count differs from it.
func (q *qdasReader) finish() (*qdasFile, error) {
	f := q.f
	f.Count = q.count()
	if q.maxChar > f.Count || q.maxCoded > f.Count {
		return nil, refuse("a K-field index is above the number of characteristics")
	}
	f.Default.own = 0
	for n := range f.Records {
		r := &f.Records[n]
		r.Malformed = r.blocks >= 0 && r.blocks != f.Count
	}

	return f, nil
}
