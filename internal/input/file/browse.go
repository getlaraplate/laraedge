package file

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
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
}

// Browse is what browse shows: the newest files, and the addresses of the
// newest one that can be read.
type Browse struct {
	Files     []BrowseFile
	Source    string
	Addresses []BrowseAddress
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
	if len(matching) > limit {
		matching = matching[:limit]
	}

	led := peekLedger(in.opts.DataDir, in.cfg.ID)
	now := in.clk.Now()
	for _, f := range matching {
		age := max(now.Sub(f.ModTime), 0)
		bf := BrowseFile{Name: f.Name, Size: f.Size, Age: age, State: StateWaiting}
		if age >= in.conn.StableFor {
			bf.State = StateReady
		}

		var data []byte
		if f.Size <= int64(in.conn.MaxFileSize) {
			data, err = in.deps.readFile(filepath.Join(in.conn.Directory, f.Name))
			if err != nil || len(data) > in.conn.MaxFileSize {
				data = nil
			}
		}
		if data != nil && led.Has(ledgerKey{Hash: sha256.Sum256(data), ModTime: f.ModTime}) {
			bf.State = StateSeen
		}
		out.Files = append(out.Files, bf)

		if out.Source == "" && data != nil {
			if addrs, ok := in.addressesOf(data); ok {
				out.Source, out.Addresses = f.Name, addrs
			}
		}
	}

	return out, nil
}

// addressesOf returns the addresses of a file; ok is false when the file
// cannot be read as a CSV with the layout of the connection.
func (in *Input) addressesOf(data []byte) ([]BrowseAddress, bool) {
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
