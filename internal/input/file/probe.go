package file

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	edgeinput "github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
)

// ProbeNoFile is what probe says of a point when no file qualifies.
const ProbeNoFile = "no file"

// matching returns the listed files the input would consider (see
// watcher.considers), newest modification time first, then by name. It is
// the listing of probe and browse; they take no part in the stability
// tracking of a running input.
func (in *Input) matching(files []fileInfo) []fileInfo {
	w := newWatcher(in.conn)
	var out []fileInfo
	for _, f := range files {
		if w.considers(f.Name) {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b fileInfo) int {
		if c := b.ModTime.Compare(a.ModTime); c != 0 {
			return c
		}

		return strings.Compare(a.Name, b.Name)
	})

	return out
}

// pointRefs lists the points of the input in the order of the document.
func (in *Input) pointRefs() []edgeinput.PointRef {
	var refs []edgeinput.PointRef
	for _, dev := range in.cfg.Devices {
		for _, p := range dev.Points {
			refs = append(refs, edgeinput.PointRef{Device: dev.Device, Signal: p.Signal})
		}
	}

	return refs
}

// ProbeSource returns the name of the file the last ProbePoints read, empty
// when none qualified.
func (in *Input) ProbeSource() string {
	in.mu.Lock()
	defer in.mu.Unlock()

	return in.probeSource
}

// errListing is the error of a probe whose directory cannot be listed; the
// cause stays in the debug log, as it can name a path.
var errListing = errors.New("the directory cannot be listed")

// Probe reads the newest unread file once and returns its samples (see
// ProbePoints).
func (in *Input) Probe(ctx context.Context) ([]model.Sample, error) {
	samples, _, err := in.ProbePoints(ctx)

	return samples, err
}

// ProbePoints lists the directory once and parses the newest matching file
// that is at least stable_for old and not in the ledger, writing nothing: the
// ledger is read as it is, empty when it is missing or corrupt. kinds says
// why a point has no good sample: the kind of a bad one (conversion,
// malformed_row or bad_timestamp, the first the file counted), missing_field
// for a point the file lacks, malformed_file or file_too_large for every point
// of a refused file, and ProbeNoFile for every point when no file qualifies. A
// directory that cannot be listed is an error with a fixed text, as for
// Browse.
func (in *Input) ProbePoints(ctx context.Context) ([]model.Sample, map[edgeinput.PointRef]string, error) {
	in.setProbeSource("")
	refs := in.pointRefs()

	noFile := func() ([]model.Sample, map[edgeinput.PointRef]string, error) {
		kinds := make(map[edgeinput.PointRef]string, len(refs))
		for _, r := range refs {
			kinds[r] = ProbeNoFile
		}

		return nil, kinds, nil
	}
	refused := func(kind string) map[edgeinput.PointRef]string {
		kinds := make(map[edgeinput.PointRef]string, len(refs))
		for _, r := range refs {
			kinds[r] = kind
		}

		return kinds
	}

	files, err := in.deps.list(in.conn.Directory)
	if err != nil {
		in.health.CountError(KindListing)
		in.log.Debug("file: the directory cannot be listed", "input", in.cfg.ID, "error", err.Error())

		return nil, nil, errListing
	}

	led := peekLedger(in.opts.DataDir, in.cfg.ID)
	now := in.clk.Now()
	for _, f := range in.matching(files) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if now.Sub(f.ModTime) < in.conn.StableFor {
			continue
		}
		if f.Size > int64(in.conn.MaxFileSize) {
			in.setProbeSource(f.Name)

			return nil, refused(KindFileTooLarge), nil
		}
		data, err := in.deps.readFile(filepath.Join(in.conn.Directory, f.Name))
		if err != nil {
			continue
		}
		if len(data) > in.conn.MaxFileSize {
			in.setProbeSource(f.Name)

			return nil, refused(KindFileTooLarge), nil
		}
		if led.Has(ledgerKey{Hash: sha256.Sum256(data), ModTime: f.ModTime}) {
			continue
		}

		in.setProbeSource(f.Name)
		samples, errKinds, err := parseCSV(in.plan, data, f.ModTime)
		if errors.Is(err, errMalformedFile) {
			return nil, refused(KindMalformedFile), nil
		}
		if err != nil {
			return nil, nil, err
		}

		return samples, in.sampleKinds(refs, samples, errKinds), nil
	}

	return noFile()
}

// setProbeSource records the file a probe read.
func (in *Input) setProbeSource(name string) {
	in.mu.Lock()
	defer in.mu.Unlock()

	in.probeSource = name
}

// sampleKinds says why a point is bad or missing. The parser counts kinds per
// file, not per point, so a bad sample takes the first of the file's counted
// kinds that can make a sample bad, and a point with no sample at all
// missing_field when the file counted one.
func (in *Input) sampleKinds(refs []edgeinput.PointRef, samples []model.Sample, counted []string) map[edgeinput.PointRef]string {
	kinds := map[edgeinput.PointRef]string{}
	badKind := ""
	for _, k := range counted {
		if k == KindConversion || k == KindMalformedRow || k == KindBadTimestamp {
			badKind = k

			break
		}
	}
	has := make(map[edgeinput.PointRef]bool, len(refs))
	for _, s := range samples {
		ref := edgeinput.PointRef{Device: s.Device, Signal: s.Signal}
		has[ref] = true
		if s.Quality != model.Good && badKind != "" {
			kinds[ref] = badKind
		}
	}
	if slices.Contains(counted, KindMissingField) {
		for _, r := range refs {
			if !has[r] {
				kinds[r] = KindMissingField
			}
		}
	}

	return kinds
}
