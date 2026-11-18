package file

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits of the number of files browse lists.
const (
	DefaultBrowseLimit = 20
	MaxBrowseLimit     = 1000
)

// States of a file in a browse listing.
const (
	// StateWaiting is a file changed less than stable_for ago.
	StateWaiting = "waiting"
	// StateReady is a file unchanged for stable_for and not read yet.
	StateReady = "ready"
	// StateSeen is a file whose hash and modification time are in the ledger.
	StateSeen = "seen"
	// StateIncomplete is a .dfx whose .dfd is not in the directory (qdas).
	StateIncomplete = "incomplete"
)

// previewMax is the longest preview, in characters of the value.
const previewMax = 40

// BrowseFile is one file of the watched directory.
type BrowseFile struct {
	Name  string
	Size  int64
	Age   time.Duration
	State string
}

// BrowseAddress is an address a point can use, with the value of the first
// data row.
type BrowseAddress struct {
	Address string
	Preview string
	// Detail describes the characteristic of a Q-DAS address; empty for CSV.
	Detail string
}

// Browse is what browse shows: the newest files, and the addresses of the
// newest one that can be read.
type Browse struct {
	Files     []BrowseFile
	Source    string
	Addresses []BrowseAddress
	// Context holds the K0005 to K0099 fields of the first record of a Q-DAS
	// report, with their value as preview.
	Context []BrowseAddress
}

// Browse lists the limit newest files that match, with their state, then the
// addresses of the newest readable one: the header names under the wide
// layout, the keys under the long one, each with the value of the first data
// row as preview (quoted, cut to 40 characters). A file is judged by its
// modification time, as browse runs once: unchanged for stable_for is ready.
// At most limit files are read, for their hash. It writes nothing; the ledger
// is read as it is.
func (in *Input) Browse(limit int) (Browse, error) {
	var out Browse

	files, err := in.deps.list(in.conn.Directory)
	if err != nil {
		return out, fmt.Errorf("file input %q: the directory cannot be listed: %w", in.cfg.ID, err)
	}

	matching := in.matching(files)
	shown := matching
	if len(shown) > limit {
		shown = shown[:limit]
	}

	led := peekLedger(in.opts.DataDir, in.cfg.ID)
	now := in.clk.Now()

	// Every file counts as ready for the pairing: browse shows the state of a
	// file by its own age, not by the rules of the scanner.
	reports, incomplete := reportsOf(in.conn, matching, matching)
	reportOf := map[string]fileReport{}
	for _, r := range reports {
		for _, f := range r.files() {
			reportOf[f.Name] = r
		}
	}
	isIncomplete := map[string]bool{}
	for _, f := range incomplete {
		isIncomplete[f.Name] = true
	}

	// A report is read once, also when both files of a pair are shown: only
	// its key is kept, and its addresses are taken at once while none were
	// found yet, so no report's bytes outlive its turn.
	type read struct {
		key ledgerKey
		ok  bool
	}
	cache := map[string]read{}
	load := func(r fileReport) read {
		if c, ok := cache[r.Name()]; ok {
			return c
		}
		data, key, tooLarge, err := readReport(in.deps, in.conn.Directory, r, in.conn.MaxFileSize)
		c := read{key: key, ok: err == nil && !tooLarge}
		cache[r.Name()] = c
		if c.ok && out.Source == "" {
			if addrs, ctx, ok := in.addressesOf(data); ok {
				out.Source, out.Addresses, out.Context = reportSource(r), addrs, ctx
			}
		}

		return c
	}

	for _, f := range shown {
		age := max(now.Sub(f.ModTime), 0)
		bf := BrowseFile{Name: f.Name, Size: f.Size, Age: age, State: StateWaiting}
		if age >= in.conn.StableFor {
			bf.State = StateReady
		}
		if isIncomplete[f.Name] {
			bf.State = StateIncomplete
		}

		if r, inReport := reportOf[f.Name]; inReport {
			if c := load(r); c.ok && led.Has(c.key) {
				bf.State = StateSeen
			}
		}
		out.Files = append(out.Files, bf)
	}

	return out, nil
}

// addressesOf returns the addresses of a report, and under qdas its context
// fields; ok is false when it cannot be read with the layout of the
// connection.
func (in *Input) addressesOf(data []byte) (addrs, context []BrowseAddress, ok bool) {
	if in.conn.Format == FormatQDAS {
		return in.qdasAddressesOf(data)
	}
	addrs, ok = in.csvAddressesOf(data)

	return addrs, nil, ok
}

// qdasAddressesOf lists the characteristics of a Q-DAS report in index order
// and the context fields of its first record.
func (in *Input) qdasAddressesOf(data []byte) (addrs, context []BrowseAddress, ok bool) {
	text, err := decodeText(data, in.conn.Encoding)
	if err != nil {
		return nil, nil, false
	}
	f, err := readQDAS(text)
	if err != nil {
		return nil, nil, false
	}

	var v qdasView
	if len(f.Records) > 0 {
		v = f.view(0)
	}
	addrs = make([]BrowseAddress, 0, f.Count)
	for i := 1; i <= f.Count; i++ {
		c := f.Characteristic(i)
		a := BrowseAddress{Address: c.Address(i), Detail: qdasDetail(c)}
		if len(f.Records) > 0 {
			a.Preview = preview(v.field(i, "K0001"))
		}
		addrs = append(addrs, a)
	}
	if len(f.Records) > 0 {
		for _, key := range v.contextKeys() {
			context = append(context, BrowseAddress{Address: key, Preview: preview(v.value(key))})
		}
	}

	return addrs, context, true
}

// qdasDetail describes a characteristic: its description, nominal, limits and
// unit, the parts it lacks left out, and a note for an attributive one.
func qdasDetail(c qdasCharacteristic) string {
	var parts []string
	if d := strings.TrimSpace(c.Description); d != "" {
		parts = append(parts, d)
	}
	if n := strings.TrimSpace(c.Nominal); n != "" {
		parts = append(parts, "nominal "+n)
	}
	lower, upper := strings.TrimSpace(c.Lower), strings.TrimSpace(c.Upper)
	if lower != "" || upper != "" {
		parts = append(parts, "limits "+lower+".."+upper)
	}
	if u := strings.TrimSpace(c.Unit); u != "" {
		parts = append(parts, u)
	}
	if c.Attributive {
		parts = append(parts, "attributive, not read")
	}

	return strings.Join(parts, "  ")
}

// csvAddressesOf returns the addresses of a CSV file; ok is false when the
// file cannot be read with the layout of the connection.
func (in *Input) csvAddressesOf(data []byte) ([]BrowseAddress, bool) {
	text, err := decodeText(data, in.conn.Encoding)
	if err != nil {
		return nil, false
	}
	f, err := readCSV(text, in.conn)
	if err != nil {
		return nil, false
	}

	var addrs []BrowseAddress
	seen := map[string]bool{}
	add := func(name string, value string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		addrs = append(addrs, BrowseAddress{Address: name, Preview: preview(value)})
	}

	if in.conn.CSV.Layout == LayoutLong {
		keyIdx, ok := f.column(strings.TrimSpace(in.conn.CSV.KeyColumn))
		if !ok {
			return nil, false
		}
		valIdx, ok := f.column(strings.TrimSpace(in.conn.CSV.ValueColumn))
		if !ok {
			return nil, false
		}
		for _, r := range f.rows {
			add(cell(r.fields, keyIdx), rawCell(r.fields, valIdx))
		}

		return addrs, true
	}

	for i, name := range f.header {
		value := ""
		if len(f.rows) > 0 {
			value = rawCell(f.rows[0].fields, i)
		}
		add(name, value)
	}

	return addrs, true
}

// preview quotes the value, cut to previewMax characters first, so a control
// character from a machine never reaches the terminal.
func preview(s string) string {
	if utf8.RuneCountInString(s) > previewMax {
		s = string([]rune(s)[:previewMax])
	}

	return strconv.Quote(s)
}
