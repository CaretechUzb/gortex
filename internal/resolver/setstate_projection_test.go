package resolver

import (
	"fmt"
	"iter"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type setStateProjectionSpyStore struct {
	graph.Store

	membershipReads int
	adjacencyReads  int
	methodScans     int
	callScans       int
	adjacencyIDs    [][]string
}

func (s *setStateProjectionSpyStore) MemberMethodsByType() map[string][]graph.MemberMethodInfo {
	s.membershipReads++
	return s.Store.(graph.MemberMethodsByType).MemberMethodsByType()
}

func (s *setStateProjectionSpyStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.adjacencyReads++
	s.adjacencyIDs = append(s.adjacencyIDs, append([]string(nil), ids...))
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *setStateProjectionSpyStore) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	if kind == graph.KindMethod {
		s.methodScans++
	}
	return s.Store.NodesByKind(kind)
}

func (s *setStateProjectionSpyStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	if kind == graph.EdgeCalls {
		s.callScans++
	}
	return s.Store.EdgesByKind(kind)
}

type setStateOutputSnapshot struct {
	from, to, kind, filePath string
	line                     int
	confidence               float64
	confidenceLabel, origin  string
	meta                     map[string]string
}

func setStateOutputSnapshots(g graph.Store, via string) []setStateOutputSnapshot {
	var out []setStateOutputSnapshot
	for edge := range g.EdgesByKind(graph.EdgeCalls) {
		if edge == nil || edge.Meta == nil || fmt.Sprint(edge.Meta["via"]) != via {
			continue
		}
		meta := make(map[string]string, len(edge.Meta))
		for key, value := range edge.Meta {
			meta[key] = fmt.Sprint(value)
		}
		out = append(out, setStateOutputSnapshot{
			from: edge.From, to: edge.To, kind: string(edge.Kind),
			filePath: edge.FilePath, line: edge.Line, confidence: edge.Confidence,
			confidenceLabel: edge.ConfidenceLabel, origin: edge.Origin, meta: meta,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].from != out[j].from {
			return out[i].from < out[j].from
		}
		if out[i].to != out[j].to {
			return out[i].to < out[j].to
		}
		if out[i].filePath != out[j].filePath {
			return out[i].filePath < out[j].filePath
		}
		return out[i].line < out[j].line
	})
	return out
}

// resolveSetStateLifecycleCallsLegacy is the former graph-wide reference: it
// materializes every method and every outgoing method edge before filtering.
func resolveSetStateLifecycleCallsLegacy(
	g graph.Store,
	lifecycleName string,
	edgeBuilder setStateLifecycleEdgeBuilder,
) int {
	methods := nodesByKindsOrAll(g, graph.KindMethod)
	methodIDs := make([]string, 0, len(methods))
	for _, method := range methods {
		if method != nil {
			methodIDs = append(methodIDs, method.ID)
		}
	}
	outByMethod := g.GetOutEdgesByNodeIDs(methodIDs)
	classByMethod := map[string]string{}
	targetByClass := map[string]*graph.Node{}
	for _, method := range methods {
		if method == nil {
			continue
		}
		for _, edge := range outByMethod[method.ID] {
			if edge == nil || edge.Kind != graph.EdgeMemberOf {
				continue
			}
			classByMethod[method.ID] = edge.To
			if method.Name == lifecycleName {
				targetByClass[edge.To] = method
			}
			break
		}
	}
	if len(targetByClass) == 0 {
		return 0
	}

	var sources []*graph.Node
	for _, method := range methods {
		if method == nil {
			continue
		}
		classID := classByMethod[method.ID]
		target := targetByClass[classID]
		if target == nil || target.ID == method.ID || !edgesCallSetState(outByMethod[method.ID]) {
			continue
		}
		sources = append(sources, method)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })

	batch := make([]*graph.Edge, 0, len(sources))
	for _, source := range sources {
		classID := classByMethod[source.ID]
		batch = append(batch, edgeBuilder(source, targetByClass[classID], classID))
	}
	if len(batch) > 0 {
		g.AddBatch(nil, batch)
	}
	return len(batch)
}

func addSetStateNoise(g graph.Store, prefix string, count int) []string {
	classID := prefix + "::Noise"
	g.AddNode(&graph.Node{ID: classID, Kind: graph.KindType, Name: "Noise", FilePath: prefix + ".txt"})
	payload := strings.Repeat("n", 256)
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		methodID := fmt.Sprintf("%s.noise%04d", classID, i)
		ids = append(ids, methodID)
		g.AddNode(&graph.Node{
			ID: methodID, Kind: graph.KindMethod, Name: fmt.Sprintf("noise%04d", i),
			FilePath: prefix + ".txt", StartLine: i + 1,
		})
		g.AddEdge(&graph.Edge{From: methodID, To: classID, Kind: graph.EdgeMemberOf})
		g.AddEdge(&graph.Edge{
			From: methodID, To: "external::noise", Kind: graph.EdgeCalls,
			FilePath: prefix + ".txt", Line: i + 1,
			Meta: map[string]any{"payload": payload, "ordinal": i},
		})
	}
	return ids
}

func populateReactSetStateProjectionFixture(g graph.Store, noise int) []string {
	reactClass(g, "Counter.tsx", "Counter",
		[]string{"increment", "render", "noop"},
		map[string]bool{"increment": true})
	addSetStateNoise(g, "react-noise", noise)
	return []string{
		"Counter.tsx::Counter.increment",
		"Counter.tsx::Counter.noop",
		"Counter.tsx::Counter.render",
	}
}

func populateFlutterSetStateProjectionFixture(g graph.Store, noise int) []string {
	flutterState(g, "counter.dart", "_CounterState",
		[]string{"increment", "build", "noop"},
		map[string]bool{"increment": true})
	addSetStateNoise(g, "flutter-noise", noise)
	return []string{
		"counter.dart::_CounterState.build",
		"counter.dart::_CounterState.increment",
		"counter.dart::_CounterState.noop",
	}
}

func assertTargetedSetStateProjection(
	t *testing.T,
	populate func(graph.Store, int) []string,
	run func(graph.Store) int,
) {
	t.Helper()
	store := graph.New()
	wantIDs := populate(store, 512)
	spy := &setStateProjectionSpyStore{Store: store}

	if got := run(spy); got != 1 {
		t.Fatalf("synthesized edges = %d, want 1", got)
	}
	if spy.membershipReads != 1 {
		t.Fatalf("membership projection reads = %d, want 1", spy.membershipReads)
	}
	if spy.adjacencyReads != 1 {
		t.Fatalf("outgoing adjacency reads = %d, want 1", spy.adjacencyReads)
	}
	if spy.methodScans != 0 {
		t.Fatalf("graph-wide method scans = %d, want 0", spy.methodScans)
	}
	if spy.callScans != 0 {
		t.Fatalf("graph-wide call-edge scans = %d, want 0", spy.callScans)
	}
	if len(spy.adjacencyIDs) != 1 {
		t.Fatalf("adjacency request batches = %d, want 1", len(spy.adjacencyIDs))
	}

	gotIDs := append([]string(nil), spy.adjacencyIDs[0]...)
	sort.Strings(gotIDs)
	sort.Strings(wantIDs)
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("targeted adjacency IDs = %#v, want only lifecycle-class methods %#v", gotIDs, wantIDs)
	}
}

func TestResolveReactSetState_TargetsLifecycleClassAdjacency(t *testing.T) {
	assertTargetedSetStateProjection(t, populateReactSetStateProjectionFixture, ResolveReactSetStateCalls)
}

func TestResolveFlutterSetState_TargetsLifecycleClassAdjacency(t *testing.T) {
	assertTargetedSetStateProjection(t, populateFlutterSetStateProjectionFixture, ResolveFlutterSetStateCalls)
}

func TestResolveReactSetState_TargetedProjectionMatchesLegacy(t *testing.T) {
	targeted := graph.New()
	legacy := graph.New()
	populateReactSetStateProjectionFixture(targeted, 37)
	populateReactSetStateProjectionFixture(legacy, 37)
	reactClass(targeted, "Panel.tsx", "Panel", []string{"refresh", "render"}, map[string]bool{"refresh": true})
	reactClass(legacy, "Panel.tsx", "Panel", []string{"refresh", "render"}, map[string]bool{"refresh": true})

	gotCount := ResolveReactSetStateCalls(targeted)
	wantCount := resolveSetStateLifecycleCallsLegacy(legacy, "render", reactSetStateEdge)
	if gotCount != wantCount {
		t.Fatalf("targeted count = %d, legacy count = %d", gotCount, wantCount)
	}
	got := setStateOutputSnapshots(targeted, reactSetStateVia)
	want := setStateOutputSnapshots(legacy, reactSetStateVia)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targeted React output differs from legacy output:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestResolveFlutterSetState_TargetedProjectionMatchesLegacy(t *testing.T) {
	targeted := graph.New()
	legacy := graph.New()
	populateFlutterSetStateProjectionFixture(targeted, 37)
	populateFlutterSetStateProjectionFixture(legacy, 37)
	flutterState(targeted, "panel.dart", "_PanelState", []string{"refresh", "build"}, map[string]bool{"refresh": true})
	flutterState(legacy, "panel.dart", "_PanelState", []string{"refresh", "build"}, map[string]bool{"refresh": true})

	gotCount := ResolveFlutterSetStateCalls(targeted)
	wantCount := resolveSetStateLifecycleCallsLegacy(legacy, "build", flutterSetStateEdge)
	if gotCount != wantCount {
		t.Fatalf("targeted count = %d, legacy count = %d", gotCount, wantCount)
	}
	got := setStateOutputSnapshots(targeted, flutterSetStateVia)
	want := setStateOutputSnapshots(legacy, flutterSetStateVia)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targeted Flutter output differs from legacy output:\n got: %#v\nwant: %#v", got, want)
	}
}
