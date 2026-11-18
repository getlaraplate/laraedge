package file

import (
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

// fileInfo is what a directory listing says about one regular file.
type fileInfo struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// listDir lists the regular files at the top level of dir. Directories and
// anything that is not a regular file are left out.
func listDir(dir string) ([]fileInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	files := make([]fileInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			// Removed between the listing and the stat: it is gone.
			continue
		}
		files = append(files, fileInfo{Name: entry.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}

	return files, nil
}

// observation is the state a file was last seen in and since when.
type observation struct {
	size    int64
	modTime time.Time
	since   time.Time
}

// watcher decides when a listed file is complete: its size and modification
// time must stay unchanged for StableFor (spec section 3, F4).
type watcher struct {
	pattern   string
	format    string
	stableFor time.Duration
	seen      map[string]observation
}

// newWatcher returns a watcher for the connection.
func newWatcher(conn Connection) *watcher {
	return &watcher{
		pattern:   strings.ToLower(conn.Pattern),
		format:    conn.Format,
		stableFor: conn.StableFor,
		seen:      map[string]observation{},
	}
}

// skipped reports whether a name is never read: hidden files and the
// temporary names exporters write before a rename.
func skipped(name string) bool {
	name = strings.ToLower(name)

	return strings.HasPrefix(name, ".") ||
		strings.HasSuffix(name, ".tmp") ||
		strings.HasSuffix(name, ".part") ||
		strings.HasSuffix(name, "~")
}

// matches reports whether the pattern matches the name, ignoring case.
func (w *watcher) matches(name string) bool {
	ok, err := path.Match(w.pattern, strings.ToLower(name))

	return err == nil && ok
}

// considers reports whether the input considers a listed name at all: not a
// skipped name, the pattern matches, and under qdas the extension is .dfq,
// .dfd or .dfx in any case (decision Q8). It is the one filter of the
// scanner, probe and browse.
func (w *watcher) considers(name string) bool {
	if skipped(name) || !w.matches(name) {
		return false
	}

	return w.format != FormatQDAS || isQDASName(name)
}

// considered is considers for the connection conn.
func considered(conn Connection, name string) bool {
	return newWatcher(conn).considers(name)
}

// Ready takes the current listing at now and returns the files unchanged for
// at least StableFor, oldest modification time first then by name. Files no
// longer listed are forgotten, so one that reappears waits again.
func (w *watcher) Ready(now time.Time, files []fileInfo) []fileInfo {
	listed := make(map[string]bool, len(files))
	var ready []fileInfo

	for _, f := range files {
		if !w.considers(f.Name) {
			continue
		}
		listed[f.Name] = true

		prev, known := w.seen[f.Name]
		if !known || prev.size != f.Size || !prev.modTime.Equal(f.ModTime) {
			prev = observation{size: f.Size, modTime: f.ModTime, since: now}
			w.seen[f.Name] = prev
		}
		if now.Sub(prev.since) >= w.stableFor {
			ready = append(ready, f)
		}
	}

	for name := range w.seen {
		if !listed[name] {
			delete(w.seen, name)
		}
	}

	slices.SortFunc(ready, func(a, b fileInfo) int {
		if c := a.ModTime.Compare(b.ModTime); c != 0 {
			return c
		}

		return strings.Compare(a.Name, b.Name)
	})

	return ready
}
