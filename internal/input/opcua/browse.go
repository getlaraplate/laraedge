package opcua

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
)

// The browse command walks the address space of a server to help a technician
// write point addresses. It is a read-only tool: it sends Browse, BrowseNext
// and Read requests only, never a request that can fault (a Read of the data
// types is chunked at the server's MaxNodesPerRead, an empty one is never
// sent) and never reads a value.
const (
	// BrowseMaxNodes is the number of nodes browse prints at most.
	BrowseMaxNodes = 1000
	// DefaultBrowseDepth and MaxBrowseDepth bound the --depth option.
	DefaultBrowseDepth = 2
	MaxBrowseDepth     = 10
)

// browseMaxReferences bounds the references taken from one node: a server
// that never stops answering BrowseNext cannot hold the walk.
const browseMaxReferences = 10000

var errBrowse = errors.New("the server refused to browse the node")

// BrowseEntry is one printed node of a browse.
type BrowseEntry struct {
	// Depth is 0 for the children of the start node, 1 for theirs, and so on.
	Depth int
	// NodeID is the NodeId in the nsu= form when its namespace index is not
	// 0 and the server's NamespaceArray names it, in plain form otherwise.
	NodeID string
	// Name is the BrowseName without its namespace index.
	Name string
	// Class is the node class: Object, Variable, Method, ...
	Class string
	// DataType is the data type name of a variable (Double, Int32, ...), the
	// NodeId of an unknown one; empty for other nodes and when unreadable.
	DataType string
}

// Browse connects like Probe (own certificate, trust check included: an
// untrusted server is an error wrapping ErrUntrusted and naming its
// thumbprint), then walks the hierarchical references depth first from start
// (the zero Node means the Objects folder, i=85) and calls emit for each
// node, at most BrowseMaxNodes; truncated tells the walk stopped there. depth
// counts the levels printed below the start node (1 prints its children
// only). A node reached twice is printed once and expanded once. Values are
// not read.
func (in *Input) Browse(ctx context.Context, start Node, depth int, emit func(BrowseEntry)) (truncated bool, err error) {
	if depth < 1 || depth > MaxBrowseDepth {
		return false, fmt.Errorf("opcua input %q: browse: depth must be from 1 to %d", in.cfg.ID, MaxBrowseDepth)
	}

	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("opcua input %q: browse: %w", in.cfg.ID, err)
	}

	own, err := in.ownCertificate()
	if err != nil {
		return false, fmt.Errorf("opcua input %q: browse: %w", in.cfg.ID, err)
	}

	s, err := in.dial(ctx, in.conn, in.dialDeps(own, nil))
	if err != nil {
		return false, fmt.Errorf("opcua input %q: browse: %w", in.cfg.ID, err)
	}
	defer s.Close()

	startID := ua.NewNumericNodeID(0, id.ObjectsFolder)

	if start.ID != nil || start.NamespaceURI != "" {
		if startID, err = s.Resolve(start); err != nil {
			return false, fmt.Errorf("opcua input %q: browse: %w", in.cfg.ID, err)
		}
	}

	w := &walker{s: s, namespaces: s.Namespaces(), depth: depth, emit: emit, printed: map[string]bool{}}

	err = contain(func() error { return w.visit(ctx, startID, 0) })
	if err != nil {
		return false, fmt.Errorf("opcua input %q: browse: %w", in.cfg.ID, err)
	}

	return w.truncated, nil
}

// walker is one depth first walk.
type walker struct {
	s          session
	namespaces []string
	depth      int
	emit       func(BrowseEntry)

	printed   map[string]bool
	count     int
	truncated bool
}

// candidate is a reference to print, with what is read about it.
type candidate struct {
	ref      *ua.ReferenceDescription
	key      string
	foreign  bool
	dataType string
}

// visit prints the children of node, found at level, and recurses into
// them. It returns when the walk is done or the node cap was reached.
func (w *walker) visit(ctx context.Context, node *ua.NodeID, level int) error {
	refs, err := w.s.Browse(ctx, node)
	if err != nil {
		return err
	}

	remaining := BrowseMaxNodes - w.count
	cands := make([]*candidate, 0, min(len(refs), remaining+1))
	seen := map[string]bool{}

	for _, ref := range refs {
		if ref == nil || ref.NodeID == nil || ref.NodeID.NodeID == nil {
			continue
		}

		c := &candidate{
			ref:     ref,
			key:     fmt.Sprintf("%d|%s|%s", ref.NodeID.ServerIndex, ref.NodeID.NamespaceURI, ref.NodeID.NodeID.String()),
			foreign: ref.NodeID.ServerIndex != 0 || ref.NodeID.NamespaceURI != "",
		}
		if w.printed[c.key] || seen[c.key] {
			continue
		}

		seen[c.key] = true
		cands = append(cands, c)
	}

	if err := w.readDataTypes(ctx, cands[:min(len(cands), remaining)]); err != nil {
		return err
	}

	for _, c := range cands {
		if w.printed[c.key] {
			continue
		}

		if w.count >= BrowseMaxNodes {
			w.truncated = true

			return nil
		}

		w.printed[c.key] = true
		w.count++
		w.emit(w.entry(c, level))

		if level+1 < w.depth && !c.foreign {
			if err := w.visit(ctx, c.ref.NodeID.NodeID, level+1); err != nil {
				return err
			}

			if w.truncated {
				return nil
			}
		}
	}

	return nil
}

// readDataTypes sets the data type of the variables of cands with one
// batched Read (chunked by the session).
func (w *walker) readDataTypes(ctx context.Context, cands []*candidate) error {
	var (
		ids   []*ua.NodeID
		which []*candidate
	)

	for _, c := range cands {
		if c.ref.NodeClass == ua.NodeClassVariable && !c.foreign {
			ids = append(ids, c.ref.NodeID.NodeID)
			which = append(which, c)
		}
	}

	if len(ids) == 0 {
		return nil
	}

	values, err := w.s.DataTypes(ctx, ids)
	if err != nil {
		return err
	}

	for i, dv := range values {
		which[i].dataType = w.dataTypeName(dv)
	}

	return nil
}

func (w *walker) entry(c *candidate, level int) BrowseEntry {
	e := BrowseEntry{
		Depth:    level,
		NodeID:   Printable(formatNodeID(c.ref.NodeID.NodeID, c.ref.NodeID.NamespaceURI, w.namespaces)),
		Class:    strings.TrimPrefix(c.ref.NodeClass.String(), "NodeClass"),
		DataType: Printable(c.dataType),
	}
	if c.ref.BrowseName != nil {
		e.Name = Printable(c.ref.BrowseName.Name)
	}

	return e
}

// dataTypeName names the DataType attribute of a variable: the standard name
// of a namespace 0 type, the NodeId of any other, empty when unreadable.
func (w *walker) dataTypeName(dv *ua.DataValue) string {
	if dv == nil || uint32(dv.Status)>>30 != 0 || dv.Value == nil {
		return ""
	}

	nodeID, ok := dv.Value.Value().(*ua.NodeID)
	if !ok || nodeID == nil {
		return ""
	}

	switch nodeID.Type() {
	case ua.NodeIDTypeTwoByte, ua.NodeIDTypeFourByte, ua.NodeIDTypeNumeric:
		if nodeID.Namespace() == 0 {
			// id.Name gives the number itself for a NodeId it does not know.
			if name := id.Name(nodeID.IntID()); name != strconv.FormatUint(uint64(nodeID.IntID()), 10) {
				return name
			}
		}
	}

	return formatNodeID(nodeID, "", w.namespaces)
}

// formatNodeID writes a NodeId in the nsu=<uri>;<id> form when it is outside
// namespace 0 and the namespace URI is known (uri, or the NamespaceArray at
// the NodeId's index), in the plain textual form otherwise.
func formatNodeID(nodeID *ua.NodeID, uri string, namespaces []string) string {
	text := nodeID.String()

	if uri == "" {
		index := int(nodeID.Namespace())
		if index == 0 || index >= len(namespaces) {
			return text
		}

		uri = namespaces[index]
	}

	if strings.HasPrefix(text, "ns=") {
		_, text, _ = strings.Cut(text, ";")
	}

	return "nsu=" + uri + ";" + text
}

// Printable makes a string from the server safe for a terminal: invalid UTF-8
// and control characters become '?'.
func Printable(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || r == utf8.RuneError }) {
		return s
	}

	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return '?'
		}

		return r
	}, s)
}

// Browse lists the forward hierarchical references of node, following
// continuation points, up to browseMaxReferences.
func (s *uaSession) Browse(ctx context.Context, node *ua.NodeID) ([]*ua.ReferenceDescription, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("opcua browse: %w", errClosed)
	}

	req := &ua.BrowseRequest{
		View: &ua.ViewDescription{ViewID: ua.NewTwoByteNodeID(0)},
		NodesToBrowse: []*ua.BrowseDescription{{
			NodeID:          node,
			BrowseDirection: ua.BrowseDirectionForward,
			ReferenceTypeID: ua.NewNumericNodeID(0, id.HierarchicalReferences),
			IncludeSubtypes: true,
			NodeClassMask:   uint32(ua.NodeClassAll),
			ResultMask:      uint32(ua.BrowseResultMaskAll),
		}},
	}

	var res *ua.BrowseResponse

	err := contain(func() (err error) {
		res, err = s.client.Browse(ctx, req)

		return err
	})
	if err != nil {
		return nil, s.browseError(ctx, "browse", err)
	}

	if res == nil {
		return nil, fmt.Errorf("opcua browse: %w", errMalformed)
	}

	result, err := browseResult(res.Results)
	if err != nil {
		return nil, err
	}

	refs := result.References

	for point := result.ContinuationPoint; len(point) > 0; {
		release := len(refs) >= browseMaxReferences

		next := &ua.BrowseNextRequest{ContinuationPoints: [][]byte{point}, ReleaseContinuationPoints: release}

		var nextRes *ua.BrowseNextResponse

		err := contain(func() (err error) {
			nextRes, err = s.client.BrowseNext(ctx, next)

			return err
		})
		if err != nil {
			return nil, s.browseError(ctx, "browse next", err)
		}

		if release {
			break
		}

		if nextRes == nil {
			return nil, fmt.Errorf("opcua browse next: %w", errMalformed)
		}

		result, err = browseResult(nextRes.Results)
		if err != nil {
			return nil, err
		}

		refs = append(refs, result.References...)
		point = result.ContinuationPoint
	}

	return refs[:min(len(refs), browseMaxReferences)], nil
}

// browseResult checks that the response holds exactly the one result asked
// for, with a Good status.
func browseResult(rs []*ua.BrowseResult) (*ua.BrowseResult, error) {
	if len(rs) != 1 || rs[0] == nil {
		return nil, fmt.Errorf("opcua browse: %w", errMalformed)
	}

	if uint32(rs[0].StatusCode)>>30 != 0 {
		return nil, fmt.Errorf("opcua browse: %w: %w", errBrowse, rs[0].StatusCode)
	}

	return rs[0], nil
}

func (s *uaSession) browseError(ctx context.Context, op string, err error) error {
	if s.isClosed() {
		return fmt.Errorf("opcua %s: %w", op, errClosed)
	}

	return classify(ctx, s.log, s.inputID, op, err)
}
