package opcua

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
)

// treeSession is a session over a fixed address space: Browse answers from
// refs by the NodeId text (refused for the nodes of refused),
// VariableAttributes from types and access.
type treeSession struct {
	session
	namespaces []string
	refs       map[string][]*ua.ReferenceDescription
	refused    map[string]bool
	types      map[string]*ua.NodeID
	access     map[string]byte
	browsed    []string
	typeReads  []int
}

func (s *treeSession) Namespaces() []string { return s.namespaces }

func (s *treeSession) Browse(_ context.Context, node *ua.NodeID) ([]*ua.ReferenceDescription, error) {
	s.browsed = append(s.browsed, node.String())

	if s.refused[node.String()] {
		return nil, fmt.Errorf("opcua browse: %w: %w", errBrowse, ua.StatusBadUserAccessDenied)
	}

	return s.refs[node.String()], nil
}

func (s *treeSession) VariableAttributes(_ context.Context, ids []*ua.NodeID) ([]variableAttributes, error) {
	s.typeReads = append(s.typeReads, len(ids))

	unreadable := &ua.DataValue{EncodingMask: ua.DataValueStatusCode, Status: ua.StatusBadAttributeIDInvalid}

	out := make([]variableAttributes, len(ids))
	for i, nodeID := range ids {
		out[i] = variableAttributes{DataType: unreadable, AccessLevel: unreadable}

		if dt := s.types[nodeID.String()]; dt != nil {
			out[i].DataType = &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(dt)}
		}

		if level, ok := s.access[nodeID.String()]; ok {
			out[i].AccessLevel = &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(level)}
		}
	}

	return out, nil
}

func ref(nodeID *ua.NodeID, name string, class ua.NodeClass) *ua.ReferenceDescription {
	return &ua.ReferenceDescription{
		NodeID:     ua.NewExpandedNodeID(nodeID, "", 0),
		BrowseName: &ua.QualifiedName{Name: name},
		NodeClass:  class,
	}
}

// walkAll runs a walk and returns the printed lines as "depth:nodeid name
// class type access".
func walkAll(t *testing.T, s session, start *ua.NodeID, depth int) (lines []string, truncated bool) {
	t.Helper()

	w := &walker{s: s, namespaces: s.Namespaces(), depth: depth, printed: map[string]bool{}, emit: func(e BrowseEntry) {
		lines = append(lines, strings.TrimRight(fmt.Sprintf("%d:%s %s %s %s %s", e.Depth, e.NodeID, e.Name, e.Class, e.DataType, e.Access), " "))
	}}

	if err := w.visit(context.Background(), start, 0); err != nil {
		t.Fatalf("visit: %v", err)
	}

	return lines, w.truncated
}

func TestWalkPrintsByDepthWithNamespaceURIsDataTypesAndAccess(t *testing.T) {
	root := ua.NewNumericNodeID(0, id.ObjectsFolder)
	line := ua.NewStringNodeID(2, "Line1")
	speed := ua.NewStringNodeID(2, "speed")
	custom := ua.NewStringNodeID(2, "custom")
	setpoint := ua.NewStringNodeID(2, "setpoint")
	command := ua.NewStringNodeID(2, "command")
	hidden := ua.NewStringNodeID(2, "hidden")
	deep := ua.NewStringNodeID(2, "deep")
	server := ua.NewNumericNodeID(0, id.Server)

	s := &treeSession{
		namespaces: []string{"http://opcfoundation.org/UA/", "urn:local", "urn:plant"},
		refs: map[string][]*ua.ReferenceDescription{
			root.String(): {ref(server, "Server", ua.NodeClassObject), ref(line, "Line1", ua.NodeClassObject)},
			line.String(): {
				ref(speed, "speed", ua.NodeClassVariable), ref(custom, "custom", ua.NodeClassVariable),
				ref(setpoint, "setpoint", ua.NodeClassVariable), ref(command, "command", ua.NodeClassVariable),
				ref(hidden, "hidden", ua.NodeClassVariable), ref(deep, "deep", ua.NodeClassObject),
			},
		},
		types: map[string]*ua.NodeID{
			speed.String():    ua.NewNumericNodeID(0, id.Double),
			custom.String():   ua.NewNumericNodeID(2, 4711),
			setpoint.String(): ua.NewNumericNodeID(0, id.Double),
			command.String():  ua.NewNumericNodeID(0, id.Boolean),
			hidden.String():   ua.NewNumericNodeID(0, id.Int32),
		},
		// Bits other than CurrentRead (0x01) and CurrentWrite (0x02) are not
		// printed: 0x04 is HistoryRead.
		access: map[string]byte{
			speed.String():    0x01 | 0x04,
			setpoint.String(): 0x03,
			command.String():  0x02,
			hidden.String():   0x04,
		},
	}

	got, truncated := walkAll(t, s, root, 2)

	want := []string{
		"0:i=2253 Server Object",
		"0:nsu=urn:plant;s=Line1 Line1 Object",
		"1:nsu=urn:plant;s=speed speed Variable Double r",
		"1:nsu=urn:plant;s=custom custom Variable nsu=urn:plant;i=4711",
		"1:nsu=urn:plant;s=setpoint setpoint Variable Double rw",
		"1:nsu=urn:plant;s=command command Variable Boolean w",
		"1:nsu=urn:plant;s=hidden hidden Variable Int32 -",
		"1:nsu=urn:plant;s=deep deep Object",
	}
	if !slices.Equal(got, want) || truncated {
		t.Fatalf("walk = %q (truncated %v), want %q", got, truncated, want)
	}

	// Depth 2 prints two levels: the second level is not expanded.
	if want := []string{"i=85", "i=2253", "ns=2;s=Line1"}; !slices.Equal(s.browsed, want) {
		t.Fatalf("browsed %q, want %q", s.browsed, want)
	}

	// One batched Read of the attributes per parent holding variables.
	if !slices.Equal(s.typeReads, []int{5}) {
		t.Fatalf("attribute reads = %v, want [5]", s.typeReads)
	}
}

// A node below the start node that the server refuses to browse is printed
// and not expanded; the walk goes on with its siblings. Refused at the start
// node, the walk is an error.
func TestWalkSkipsANodeTheServerRefusesToBrowse(t *testing.T) {
	root := ua.NewNumericNodeID(0, id.ObjectsFolder)
	locked := ua.NewStringNodeID(1, "locked")
	open := ua.NewStringNodeID(1, "open")
	inside := ua.NewStringNodeID(1, "inside")

	s := &treeSession{
		namespaces: []string{"http://opcfoundation.org/UA/", "urn:x"},
		refs: map[string][]*ua.ReferenceDescription{
			root.String(): {ref(locked, "locked", ua.NodeClassObject), ref(open, "open", ua.NodeClassObject)},
			open.String(): {ref(inside, "inside", ua.NodeClassObject)},
		},
		refused: map[string]bool{locked.String(): true},
	}

	got, _ := walkAll(t, s, root, 3)
	want := []string{"0:nsu=urn:x;s=locked locked Object", "0:nsu=urn:x;s=open open Object", "1:nsu=urn:x;s=inside inside Object"}
	if !slices.Equal(got, want) {
		t.Fatalf("walk = %q, want %q", got, want)
	}

	s.refused[root.String()] = true
	w := &walker{s: s, namespaces: s.Namespaces(), depth: 3, printed: map[string]bool{}, emit: func(BrowseEntry) {}}
	if err := w.visit(context.Background(), root, 0); !errors.Is(err, errBrowse) {
		t.Fatalf("refused start node = %v, want errBrowse", err)
	}
}

func TestWalkDepthOneBrowsesTheStartNodeOnly(t *testing.T) {
	root := ua.NewNumericNodeID(0, id.ObjectsFolder)
	a := ua.NewStringNodeID(1, "a")
	s := &treeSession{
		namespaces: []string{"http://opcfoundation.org/UA/", "urn:x"},
		refs:       map[string][]*ua.ReferenceDescription{root.String(): {ref(a, "a", ua.NodeClassObject)}},
	}

	got, _ := walkAll(t, s, root, 1)
	if len(got) != 1 || !slices.Equal(s.browsed, []string{"i=85"}) {
		t.Fatalf("walk = %q, browsed %q; want one node and one browse", got, s.browsed)
	}
}

// A reference loop prints each node once and expands it once.
func TestWalkPrintsALoopOnce(t *testing.T) {
	root := ua.NewNumericNodeID(0, id.ObjectsFolder)
	a := ua.NewStringNodeID(1, "a")
	b := ua.NewStringNodeID(1, "b")
	s := &treeSession{
		namespaces: []string{"http://opcfoundation.org/UA/", "urn:x"},
		refs: map[string][]*ua.ReferenceDescription{
			root.String(): {ref(a, "a", ua.NodeClassObject), ref(a, "a", ua.NodeClassObject)},
			a.String():    {ref(b, "b", ua.NodeClassObject)},
			b.String():    {ref(a, "a", ua.NodeClassObject), ref(root, "Objects", ua.NodeClassObject)},
		},
	}

	got, truncated := walkAll(t, s, root, 10)

	want := []string{"0:nsu=urn:x;s=a a Object", "1:nsu=urn:x;s=b b Object", "2:i=85 Objects Object"}
	if !slices.Equal(got, want) || truncated {
		t.Fatalf("walk = %q (truncated %v), want %q", got, truncated, want)
	}

	if want := []string{"i=85", "ns=1;s=a", "ns=1;s=b", "i=85"}; !slices.Equal(s.browsed, want) {
		t.Fatalf("browsed %q, want %q", s.browsed, want)
	}
}

func TestWalkStopsAtTheNodeCap(t *testing.T) {
	for _, tc := range []struct {
		name      string
		children  int
		truncated bool
		printed   int
	}{
		{"exactly the cap", BrowseMaxNodes, false, BrowseMaxNodes},
		{"one over", BrowseMaxNodes + 1, true, BrowseMaxNodes},
		{"many over", 3 * BrowseMaxNodes, true, BrowseMaxNodes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := ua.NewNumericNodeID(0, id.ObjectsFolder)
			s := &treeSession{namespaces: []string{"http://opcfoundation.org/UA/"}, refs: map[string][]*ua.ReferenceDescription{}, types: map[string]*ua.NodeID{}}

			for i := range tc.children {
				v := ua.NewNumericNodeID(0, uint32(100000+i))
				s.refs[root.String()] = append(s.refs[root.String()], ref(v, "v", ua.NodeClassVariable))
				s.types[v.String()] = ua.NewNumericNodeID(0, id.Int32)
			}

			got, truncated := walkAll(t, s, root, 3)
			if len(got) != tc.printed || truncated != tc.truncated {
				t.Fatalf("printed %d (truncated %v), want %d (%v)", len(got), truncated, tc.printed, tc.truncated)
			}

			// Attributes are read only for the nodes that can be printed.
			total := 0
			for _, n := range s.typeReads {
				total += n
			}
			if total != tc.printed {
				t.Fatalf("read the attributes of %d nodes, want %d", total, tc.printed)
			}
		})
	}
}

// The cap holds across levels and stops the whole walk.
func TestWalkCapAcrossLevels(t *testing.T) {
	root := ua.NewNumericNodeID(0, id.ObjectsFolder)
	s := &treeSession{namespaces: []string{"http://opcfoundation.org/UA/"}, refs: map[string][]*ua.ReferenceDescription{}}

	for i := range 40 {
		folder := ua.NewNumericNodeID(0, uint32(200000+i))
		s.refs[root.String()] = append(s.refs[root.String()], ref(folder, "f", ua.NodeClassObject))

		for j := range 40 {
			s.refs[folder.String()] = append(s.refs[folder.String()], ref(ua.NewNumericNodeID(0, uint32(300000+i*100+j)), "c", ua.NodeClassObject))
		}
	}

	got, truncated := walkAll(t, s, root, 2)
	if len(got) != BrowseMaxNodes || !truncated {
		t.Fatalf("printed %d (truncated %v), want %d and truncated", len(got), truncated, BrowseMaxNodes)
	}
}

func TestPrintableReplacesControlCharacters(t *testing.T) {
	for in, want := range map[string]string{
		"plain":       "plain",
		"a\x1b[31mb":  "a?[31mb",
		"line\nbreak": "line?break",
		"bad\xffutf":  "bad?utf",
		"ünïcode":     "ünïcode",
	} {
		if got := Printable(in); got != want {
			t.Errorf("Printable(%q) = %q, want %q", in, got, want)
		}
	}
}

func browseResponse(status ua.StatusCode, point []byte, refs ...*ua.ReferenceDescription) *ua.BrowseResponse {
	return &ua.BrowseResponse{Results: []*ua.BrowseResult{{StatusCode: status, ContinuationPoint: point, References: refs}}}
}

func TestSessionBrowseFollowsContinuationPoints(t *testing.T) {
	deps, _ := testDeps(t)
	f := &fakeClient{}
	withFake(&deps, f)

	var requests []*ua.BrowseRequest

	f.browseFn = func(req *ua.BrowseRequest) (*ua.BrowseResponse, error) {
		requests = append(requests, req)

		return browseResponse(ua.StatusOK, []byte{1}, ref(ua.NewNumericNodeID(0, 1), "a", ua.NodeClassObject)), nil
	}

	pages := 0
	f.browseNextFn = func(req *ua.BrowseNextRequest) (*ua.BrowseNextResponse, error) {
		pages++
		res := &ua.BrowseResult{References: []*ua.ReferenceDescription{ref(ua.NewNumericNodeID(0, uint32(1+pages)), "n", ua.NodeClassObject)}}
		if pages < 2 {
			res.ContinuationPoint = []byte{byte(pages + 1)}
		}

		return &ua.BrowseNextResponse{Results: []*ua.BrowseResult{res}}, nil
	}

	s := mustDial(t, noneConnection("opc.tcp://fake:4840"), deps)

	refs, err := s.Browse(context.Background(), ua.NewNumericNodeID(0, id.ObjectsFolder))
	if err != nil || len(refs) != 3 {
		t.Fatalf("Browse = %d refs, %v; want 3", len(refs), err)
	}

	d := requests[0].NodesToBrowse[0]
	if d.BrowseDirection != ua.BrowseDirectionForward || !d.IncludeSubtypes || d.ReferenceTypeID.IntID() != id.HierarchicalReferences {
		t.Fatalf("browse description = %+v, want forward hierarchical references with subtypes", d)
	}
}

func TestSessionBrowseCapsTheReferencesOfANode(t *testing.T) {
	deps, _ := testDeps(t)
	f := &fakeClient{}
	withFake(&deps, f)

	page := func() []*ua.ReferenceDescription {
		out := make([]*ua.ReferenceDescription, 4000)
		for i := range out {
			out[i] = ref(ua.NewNumericNodeID(0, uint32(i+1)), "n", ua.NodeClassObject)
		}

		return out
	}

	f.browseFn = func(*ua.BrowseRequest) (*ua.BrowseResponse, error) {
		return browseResponse(ua.StatusOK, []byte{1}, page()...), nil
	}

	var released bool

	f.browseNextFn = func(req *ua.BrowseNextRequest) (*ua.BrowseNextResponse, error) {
		released = req.ReleaseContinuationPoints

		return &ua.BrowseNextResponse{Results: []*ua.BrowseResult{{ContinuationPoint: []byte{1}, References: page()}}}, nil
	}

	s := mustDial(t, noneConnection("opc.tcp://fake:4840"), deps)

	refs, err := s.Browse(context.Background(), ua.NewNumericNodeID(0, id.ObjectsFolder))
	if err != nil || len(refs) != browseMaxReferences {
		t.Fatalf("Browse = %d refs, %v; want %d", len(refs), err, browseMaxReferences)
	}

	if !released {
		t.Fatal("the continuation point was not released")
	}
}

func TestSessionBrowseErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(*ua.BrowseRequest) (*ua.BrowseResponse, error)
		want error
	}{
		{"a bad node", func(*ua.BrowseRequest) (*ua.BrowseResponse, error) {
			return browseResponse(ua.StatusBadNodeIDUnknown, nil), nil
		}, errBrowse},
		{"no result", func(*ua.BrowseRequest) (*ua.BrowseResponse, error) {
			return &ua.BrowseResponse{}, nil
		}, errMalformed},
		{"a panic", func(*ua.BrowseRequest) (*ua.BrowseResponse, error) { panic("injected") }, errLibraryPanic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, _ := testDeps(t)
			f := &fakeClient{browseFn: tc.fn}
			withFake(&deps, f)

			s := mustDial(t, noneConnection("opc.tcp://fake:4840"), deps)

			_, err := s.Browse(context.Background(), ua.NewNumericNodeID(0, id.ObjectsFolder))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Browse = %v, want %v", err, tc.want)
			}

			if strings.Contains(err.Error(), "fake:4840") {
				t.Fatalf("error names the endpoint: %v", err)
			}
		})
	}
}

// The data type and the access level of each variable travel in one batch,
// two nodes per variable, chunked at the server's MaxNodesPerRead (32 for
// the fake).
func TestSessionVariableAttributesChunkAtTheServerLimit(t *testing.T) {
	deps, _ := testDeps(t)
	f := &fakeClient{}
	withFake(&deps, f)

	s := mustDial(t, noneConnection("opc.tcp://fake:4840"), deps)

	ids := make([]*ua.NodeID, 70)
	for i := range ids {
		ids[i] = ua.NewNumericNodeID(0, uint32(i+1))
	}

	got, err := s.VariableAttributes(context.Background(), ids)
	if err != nil || len(got) != 70 {
		t.Fatalf("VariableAttributes = %d values, %v; want 70", len(got), err)
	}

	for i, attrs := range got {
		dataType, _ := attrs.DataType.Value.Value().(*ua.NodeID)
		level, _ := attrs.AccessLevel.Value.Value().(uint8)
		if dataType == nil || dataType.IntID() != uint32(i+1) || level != uint8(i+1) {
			t.Fatalf("attributes %d = %v, %v; want those of i=%d", i, attrs.DataType.Value, attrs.AccessLevel.Value, i+1)
		}
	}

	if _, err := s.VariableAttributes(context.Background(), nil); err != nil {
		t.Fatalf("VariableAttributes of nothing = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if !slices.Equal(f.attributeReads, []int{32, 32, 32, 32, 12}) {
		t.Fatalf("attribute reads = %v, want [32 32 32 32 12] and none for no ids", f.attributeReads)
	}
}
