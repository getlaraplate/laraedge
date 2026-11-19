package file

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/model"
)

// Extensions of the Q-DAS files (decision Q8): a .dfq holds description and
// values, a .dfd the description and a .dfx the values of a pair.
const (
	extDFQ = ".dfq"
	extDFD = ".dfd"
	extDFX = ".dfx"
)

// fileReport is what the scanner reads, records and moves as one: a single
// file, or a .dfx with the .dfd of the same base name (decision Q8). It is
// the report of the plan, named so as not to clash with the test report of
// the plan 1 tests.
type fileReport struct {
	Primary fileInfo
	// Companion is the .dfd of a pair, nil for a single file.
	Companion *fileInfo
}

// Name returns the name of the primary file: the report's name in the logs,
// the ledger and the scanner's state.
func (r fileReport) Name() string {
	return r.Primary.Name
}

// files lists the files of the report, the companion first: the order they
// are read and moved in.
func (r fileReport) files() []fileInfo {
	if r.Companion == nil {
		return []fileInfo{r.Primary}
	}

	return []fileInfo{*r.Companion, r.Primary}
}

// extension returns the extension of name in lower case, with its dot.
func extension(name string) string {
	return strings.ToLower(filepath.Ext(name))
}

// stem returns name without its extension.
func stem(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// isQDASName reports whether name has one of the Q-DAS extensions, in any
// case.
func isQDASName(name string) bool {
	switch extension(name) {
	case extDFQ, extDFD, extDFX:
		return true
	}

	return false
}

// reportsOf groups the ready files into reports, in the order of ready
// (oldest primary modification time first, then by name). Under csv every
// ready file is a report. Under qdas a .dfq is a report alone; a .dfx is one
// with the ready .dfd of the same stem (the extension compared ignoring
// case, the stem exactly), and waits while that .dfd is listed but not ready
// yet; a .dfx whose .dfd is not listed, or does not match the pattern, is
// incomplete (decision Q9). A .dfd is never a report alone. When two .dfd
// names differ only in the case of their extension, the name first in byte
// order is taken.
func reportsOf(conn Connection, ready []fileInfo, listed []fileInfo) (reports []fileReport, incomplete []fileInfo) {
	if conn.Format != FormatQDAS {
		for _, f := range ready {
			reports = append(reports, fileReport{Primary: f})
		}

		return reports, nil
	}

	readyDFD := map[string]fileInfo{}
	for _, f := range ready {
		if extension(f.Name) != extDFD {
			continue
		}
		if prev, ok := readyDFD[stem(f.Name)]; ok && prev.Name < f.Name {
			continue
		}
		readyDFD[stem(f.Name)] = f
	}
	listedDFD := map[string]bool{}
	for _, f := range listed {
		if extension(f.Name) == extDFD && considered(conn, f.Name) {
			listedDFD[stem(f.Name)] = true
		}
	}

	for _, f := range ready {
		switch extension(f.Name) {
		case extDFQ:
			reports = append(reports, fileReport{Primary: f})
		case extDFX:
			if dfd, ok := readyDFD[stem(f.Name)]; ok {
				reports = append(reports, fileReport{Primary: f, Companion: &dfd})
			} else if !listedDFD[stem(f.Name)] {
				incomplete = append(incomplete, f)
			}
		}
	}

	return reports, incomplete
}

// utf8BOM is the UTF-8 byte order mark.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// readReport reads the files of r in dir. tooLarge is set, with nothing else,
// when a file is above maxSize in the listing or once read. A single file is
// its own bytes, keyed by their SHA-256. A pair is read as the .dfd, a line
// end, then the .dfx without a leading UTF-8 byte order mark, and keyed by
// the SHA-256 of the 8-byte big-endian length of the .dfd, the .dfd and the
// .dfx as stored (decision Q8). The key's time is the primary's modification
// time. err is the error of a file that cannot be read.
func readReport(d deps, dir string, r fileReport, maxSize int) (data []byte, key ledgerKey, tooLarge bool, err error) {
	for _, f := range r.files() {
		if f.Size > int64(maxSize) {
			return nil, ledgerKey{}, true, nil
		}
	}

	read := func(name string) ([]byte, bool, error) {
		b, err := d.readFile(filepath.Join(dir, name))
		if err != nil {
			return nil, false, err
		}

		return b, len(b) > maxSize, nil
	}

	if r.Companion == nil {
		data, tooLarge, err = read(r.Primary.Name)
		if err != nil || tooLarge {
			return nil, ledgerKey{}, tooLarge, err
		}

		return data, ledgerKey{Hash: sha256.Sum256(data), ModTime: r.Primary.ModTime}, false, nil
	}

	dfd, tooLarge, err := read(r.Companion.Name)
	if err != nil || tooLarge {
		return nil, ledgerKey{}, tooLarge, err
	}
	dfx, tooLarge, err := read(r.Primary.Name)
	if err != nil || tooLarge {
		return nil, ledgerKey{}, tooLarge, err
	}

	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(dfd)))
	sum := sha256.New()
	sum.Write(length[:])
	sum.Write(dfd)
	sum.Write(dfx)
	key.ModTime = r.Primary.ModTime
	copy(key.Hash[:], sum.Sum(nil))

	values := bytes.TrimPrefix(dfx, utf8BOM)
	data = make([]byte, 0, len(dfd)+1+len(values))
	data = append(data, dfd...)
	data = append(data, '\n')
	data = append(data, values...)

	return data, key, false, nil
}

// parseReport parses the text of a report in its format.
func parseReport(p *plan, format string, data []byte, modTime time.Time) ([]model.Sample, []string, error) {
	if format == FormatQDAS {
		return parseQDAS(p, data, modTime)
	}

	return parseCSV(p, data, modTime)
}

// maxSuffix bounds the search for a free name.
const maxSuffix = 100000

// errNoFreeName is a directory holding every suffixed name up to maxSuffix.
var errNoFreeName = errors.New("file: no free name in the target directory")

// freeName returns the path in dir under which name can be stored without
// replacing a file (see freeNames).
func freeName(dir, name string) (string, error) {
	paths, err := freeNames(dir, []string{name})
	if err != nil {
		return "", err
	}

	return paths[0], nil
}

// freeNames returns the paths in dir under which the names can be stored
// without replacing a file, all with one suffix: the names themselves, else
// each with -1, -2, ... before its extension, the first suffix free for
// every name. os.Rename onto an existing name replaces it, on Windows too,
// so this check is what keeps a file in the target directory from being
// replaced.
func freeNames(dir string, names []string) ([]string, error) {
	for n := 0; n <= maxSuffix; n++ {
		paths := make([]string, 0, len(names))
		for _, name := range names {
			candidate := name
			if n > 0 {
				candidate = fmt.Sprintf("%s-%d%s", stem(name), n, filepath.Ext(name))
			}
			path := filepath.Join(dir, candidate)
			_, err := os.Lstat(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			if err == nil {
				break
			}
			paths = append(paths, path)
		}
		if len(paths) == len(names) {
			return paths, nil
		}
	}

	return nil, errNoFreeName
}
