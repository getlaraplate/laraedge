package file

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"

	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/model"
)

// Read error kinds of the file driver (spec section 4).
const (
	// KindListing is a directory that could not be listed.
	KindListing = "listing"
	// KindFileTooLarge is a file above max_file_size.
	KindFileTooLarge = "file_too_large"
	// KindMalformedFile is a file refused as a whole.
	KindMalformedFile = "malformed_file"
	// KindMalformedRow is a row with a different field count than the header,
	// or one the CSV reader rejects.
	KindMalformedRow = "malformed_row"
	// KindMissingField is a point whose column or key is not in the file.
	KindMissingField = "missing_field"
	// KindConversion is a value that does not convert losslessly to the type.
	KindConversion = "conversion"
	// KindBadTimestamp is a timestamp that is missing or unreadable.
	KindBadTimestamp = "bad_timestamp"
	// KindMoveFailed is a file that could not be moved after it was read.
	KindMoveFailed = "move_failed"
)

// errMalformedFile marks a file refused as a whole; the message after the
// colon is a fixed reason, never a cell or a name from the file.
var errMalformedFile = errors.New("malformed file")

func refuse(reason string) error {
	return fmt.Errorf("%w: %s", errMalformedFile, reason)
}

// timestampCeiling is the first instant after the latest timestamp the
// envelope can write (a four-digit year).
var timestampCeiling = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)

// row is one data row; bad is a row the reader rejected or whose field count
// differs from the header's.
type row struct {
	fields []string
	bad    bool
}

// csvFile is a decoded file: its header and its data rows.
type csvFile struct {
	header []string
	// index maps a trimmed header name to its column positions.
	index map[string][]int
	rows  []row
}

// parseCSV turns the bytes of one CSV file into samples. A non-nil error
// refuses the whole file (errMalformedFile) and comes with no samples. kinds
// holds one read error kind per counted failure. modTime is the sample time
// when the connection has no timestamp column.
func parseCSV(p *plan, data []byte, modTime time.Time) ([]model.Sample, []string, error) {
	text, err := decodeText(data, p.conn.Encoding)
	if err != nil {
		return nil, nil, err
	}
	f, err := readCSV(text, p.conn)
	if err != nil {
		return nil, nil, err
	}
	var (
		samples []model.Sample
		kinds   []string
	)
	if p.conn.CSV.Layout == LayoutLong {
		samples, kinds, err = parseLong(p, f, modTime.UTC())
	} else {
		samples, kinds, err = parseWide(p, f, modTime.UTC())
	}
	if err != nil {
		return nil, nil, err
	}

	return samples, kinds, nil
}

// decodeText returns the file as UTF-8 text, a UTF-8 byte order mark skipped.
func decodeText(data []byte, encoding string) (string, error) {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	switch encoding {
	case "windows-1252":
		data, _ = charmap.Windows1252.NewDecoder().Bytes(data)
	case "iso-8859-1":
		data, _ = charmap.ISO8859_1.NewDecoder().Bytes(data)
	}
	if !utf8.Valid(data) {
		return "", refuse("text is not valid in the configured encoding")
	}

	return string(data), nil
}

// readCSV splits the text into the header and the data rows.
func readCSV(text string, conn Connection) (*csvFile, error) {
	headerRow := max(conn.CSV.HeaderRow, 1)
	// header_row counts physical lines: the lines before it are dropped as
	// text, so a blank or malformed preamble line cannot shift or swallow the
	// header.
	for range headerRow - 1 {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			return nil, refuse("no header row")
		}
		text = text[i+1:]
	}
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = conn.CSV.Delimiter
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	f := &csvFile{index: make(map[string][]int)}
	for n := 1; ; n++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			if n == 1 {
				return nil, refuse("no header row")
			}
			break
		}
		switch {
		case n == 1:
			if err != nil {
				return nil, refuse("unreadable header row")
			}
			f.header = make([]string, len(rec))
			for i, name := range rec {
				name = strings.TrimSpace(name)
				f.header[i] = name
				f.index[name] = append(f.index[name], i)
			}
		default:
			fields := append([]string(nil), rec...)
			f.rows = append(f.rows, row{fields: fields, bad: err != nil || len(fields) != len(f.header)})
		}
	}

	return f, nil
}

// column returns the first position of name in the header.
func (f *csvFile) column(name string) (int, bool) {
	if name == "" {
		return -1, false
	}
	idx := f.index[name]
	if len(idx) == 0 {
		return -1, false
	}

	return idx[0], true
}

// columns holds the header positions of the timestamp and context columns;
// -1 is a column that is not mapped, or not in the file for the timestamp.
type columns struct {
	ts  int
	ctx [4]int
}

// mappedColumns locates the timestamp and context columns. A mapped context
// column that is not in the header refuses the file.
func mappedColumns(p *plan, f *csvFile) (columns, error) {
	c := columns{ts: -1, ctx: [4]int{-1, -1, -1, -1}}
	if spec := p.conn.Timestamp; spec != nil {
		i, ok := f.column(strings.TrimSpace(spec.Column))
		if !ok {
			return c, refuse("the timestamp column is not in the header")
		}
		c.ts = i
	}
	names := [4]string{p.conn.Context.Serial, p.conn.Context.OrderRef, p.conn.Context.OperationRef, p.conn.Context.Lot}
	for k, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		i, ok := f.column(name)
		if !ok {
			return c, refuse("a context column is not in the header")
		}
		c.ctx[k] = i
	}

	return c, nil
}

// cell returns the trimmed cell at i, empty when the row is shorter.
func cell(fields []string, i int) string {
	if i < 0 || i >= len(fields) {
		return ""
	}

	return strings.TrimSpace(fields[i])
}

// contextOf reads the context values of a row; nil when all are empty.
func (c columns) contextOf(fields []string) *model.Context {
	ctx := model.Context{
		Serial:       cell(fields, c.ctx[0]),
		OrderRef:     cell(fields, c.ctx[1]),
		OperationRef: cell(fields, c.ctx[2]),
		Lot:          cell(fields, c.ctx[3]),
	}
	if ctx == (model.Context{}) {
		return nil
	}

	return &ctx
}

// timestampOf reads the timestamp of a row in UTC.
func (p *plan) timestampOf(c columns, fields []string) (time.Time, bool) {
	text := cell(fields, c.ts)
	if text == "" {
		return time.Time{}, false
	}
	t, ok := ParseTimestamp(p.conn.Timestamp, text, p.conn.Location)
	if !ok || t.Unix() < 0 || !t.Before(timestampCeiling) {
		return time.Time{}, false
	}

	return t.UTC(), true
}

// sampleOf builds the sample of a point from a cell. bad forces a bad sample
// without a count; otherwise a value that does not convert is bad and its
// kind KindConversion is returned.
func (p *plan) sampleOf(pp PlannedPoint, ts time.Time, text string, bad bool, ctx *model.Context) (model.Sample, string) {
	s := model.Sample{Device: pp.Device, Signal: pp.Signal, TS: ts, Priority: pp.Point.Priority}
	if ctx != nil {
		c := *ctx
		s.Context = &c
	}
	if !bad && !p.thousandsDot(pp.Point.Type, text) {
		if v, ok := driverkit.ConvertText(pp.Point.Type, p.decimalText(pp.Point.Type, text)); ok {
			s.Value, s.Quality = v, model.Good
			return s, ""
		}
	}
	s.Value, s.Quality = driverkit.ZeroValue(pp.Point.Type), model.Bad
	if bad {
		return s, ""
	}

	return s, KindConversion
}

// thousandsDot reports a numeric text with a dot under decimal comma: a
// European thousands separator, which is not accepted.
func (p *plan) thousandsDot(typ, text string) bool {
	return p.conn.CSV.Decimal == ',' && typ != "string" && typ != "bool" && strings.Contains(text, ".")
}

// decimalText applies the decimal separator to the text of a numeric point.
func (p *plan) decimalText(typ, text string) string {
	if p.conn.CSV.Decimal == ',' && typ != "string" && typ != "bool" {
		return strings.ReplaceAll(text, ",", ".")
	}

	return text
}

func parseWide(p *plan, f *csvFile, modTime time.Time) ([]model.Sample, []string, error) {
	var kinds []string
	for _, addr := range p.addrs {
		switch len(f.index[addr]) {
		case 0:
			if !slices.Contains(kinds, KindMissingField) {
				kinds = append(kinds, KindMissingField)
			}
		case 1:
		default:
			return nil, nil, refuse("an addressed column name appears twice in the header")
		}
	}
	if p.conn.Timestamp == nil && len(f.rows) > 1 {
		return nil, nil, refuse("several rows and no timestamp column")
	}
	cols, err := mappedColumns(p, f)
	if err != nil {
		return nil, nil, err
	}

	var samples []model.Sample
	for _, r := range f.rows {
		ts, ctx, rowBad := modTime, (*model.Context)(nil), r.bad
		switch {
		case r.bad:
			// The cells cannot be trusted: no context, and the time is read
			// only when it is there.
			kinds = append(kinds, KindMalformedRow)
			if p.conn.Timestamp != nil {
				if t, ok := p.timestampOf(cols, r.fields); ok {
					ts = t
				}
			}
		case p.conn.Timestamp != nil:
			t, ok := p.timestampOf(cols, r.fields)
			if ok {
				ts = t
			} else {
				kinds = append(kinds, KindBadTimestamp)
				rowBad = true
			}
			ctx = cols.contextOf(r.fields)
		default:
			ctx = cols.contextOf(r.fields)
		}
		for _, addr := range p.addrs {
			idx := f.index[addr]
			if len(idx) == 0 {
				continue
			}
			text := cell(r.fields, idx[0])
			if text == "" && !r.bad {
				continue
			}
			for _, pp := range p.points[addr] {
				s, kind := p.sampleOf(pp, ts, rawCell(r.fields, idx[0]), rowBad, ctx)
				if kind != "" {
					kinds = append(kinds, kind)
				}
				samples = append(samples, s)
			}
		}
	}

	return samples, kinds, nil
}

// rawCell returns the cell at i untrimmed, empty when the row is shorter.
func rawCell(fields []string, i int) string {
	if i < 0 || i >= len(fields) {
		return ""
	}

	return fields[i]
}

// longMatch is a row of a long file whose key a point addresses.
type longMatch struct {
	addr string
	text string
	bad  bool
}

func parseLong(p *plan, f *csvFile, modTime time.Time) ([]model.Sample, []string, error) {
	keyIdx, ok := f.column(strings.TrimSpace(p.conn.CSV.KeyColumn))
	if !ok {
		return nil, nil, refuse("the key column is not in the header")
	}
	valIdx, ok := f.column(strings.TrimSpace(p.conn.CSV.ValueColumn))
	if !ok {
		return nil, nil, refuse("the value column is not in the header")
	}
	cols, err := mappedColumns(p, f)
	if err != nil {
		return nil, nil, err
	}

	var (
		kinds   []string
		matches []longMatch
		seen    = make(map[string]bool)
		ctx     model.Context
		ts      time.Time
		tsFound bool
		tsOK    bool
	)
	ctxFields := [4]*string{&ctx.Serial, &ctx.OrderRef, &ctx.OperationRef, &ctx.Lot}
	for _, r := range f.rows {
		key := cell(r.fields, keyIdx)
		if _, addressed := p.points[key]; addressed {
			if seen[key] {
				return nil, nil, refuse("an addressed key appears twice")
			}
			seen[key] = true
			matches = append(matches, longMatch{addr: key, text: rawCell(r.fields, valIdx), bad: r.bad})
		}
		if r.bad {
			kinds = append(kinds, KindMalformedRow)
			continue
		}
		if p.conn.Timestamp != nil && !tsFound && cell(r.fields, cols.ts) != "" {
			tsFound = true
			ts, tsOK = p.timestampOf(cols, r.fields)
		}
		for k, dst := range ctxFields {
			if *dst == "" {
				*dst = cell(r.fields, cols.ctx[k])
			}
		}
	}

	rowBad := false
	switch {
	case p.conn.Timestamp == nil:
		ts = modTime
	case !tsFound || !tsOK:
		kinds = append(kinds, KindBadTimestamp)
		ts, rowBad = modTime, true
	}
	var rowCtx *model.Context
	if ctx != (model.Context{}) {
		rowCtx = &ctx
	}

	for _, addr := range p.addrs {
		if !seen[addr] {
			kinds = append(kinds, KindMissingField)
			break
		}
	}
	var samples []model.Sample
	for _, m := range matches {
		if strings.TrimSpace(m.text) == "" && !m.bad {
			continue
		}
		for _, pp := range p.points[m.addr] {
			s, kind := p.sampleOf(pp, ts, m.text, rowBad || m.bad, rowCtx)
			if kind != "" {
				kinds = append(kinds, kind)
			}
			samples = append(samples, s)
		}
	}

	return samples, kinds, nil
}
