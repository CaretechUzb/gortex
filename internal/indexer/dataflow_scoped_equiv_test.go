package indexer

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestMaterializeDataflowParamsForFile_EquivalentToWholeGraph proves the
// correctness claim behind the scoped per-file dataflow materialisation:
// materializeDataflowParamsForFile, run once per file, rewrites EXACTLY
// the same EdgeArgOf / EdgeReturnsTo edges — to the same (From, To, Kind)
// tuples — as the whole-graph materializeDataflowParams does in a single
// AllEdges scan.
//
// Why this holds (the invariant under test): returns_to's From is the
// enclosing caller function (a file node), while arg_of's From is the
// argument's source — a file local for a bare in-scope identifier, but a
// synthetic `unresolved::` id for selector / package-qualified / global /
// nested-call arguments, which is NOT a file node. The scoped pass
// therefore probes the union of (the file's nodes) and (the synthetic
// From ids the file's freshly-extracted edges carry), then keeps only
// edges whose FilePath is this file — exactly the arg_of+returns_to set
// the whole-graph pass would touch for it. The fixture below exercises
// all four argument shapes so the synthetic-From cases are covered.
//
// Method: build ONE resolved-but-not-yet-materialised graph from a small
// multi-file Go fixture (a caller file that calls a callee in another
// file, passing a parameter as an argument and assigning the return
// value), deep-clone it into two byte-identical graphs, then:
//
//	(a) run materializeDataflowParams() once on gGlobal
//	(b) run materializeDataflowParamsForFile(path) for each file on gScoped
//
// and assert the arg_of+returns_to {From,To,Kind} tuple sets are
// IDENTICAL. Cloning (not two independent indexings) removes any
// node-id / ordering nondeterminism, so any divergence is the scoping
// logic, not the build.
func TestMaterializeDataflowParamsForFile_EquivalentToWholeGraph(t *testing.T) {
	dir := t.TempDir()

	// callee.go: a function with a declared parameter and a return value.
	// The param node gives rewriteArgOf a #param: target to lift the
	// arg_of edge onto; the return value gives the caller a returns_to
	// edge to rewrite onto the resolved callee.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sink"), 0o755))
	writeFile(t, filepath.Join(dir, "sink", "callee.go"), `package sink

// Transform consumes payload and returns a derived value. The declared
// parameter is what rewriteArgOf lifts an arg_of edge onto.
func Transform(payload string) string {
	return payload + "!"
}
`)

	// caller.go: calls sink.Transform passing its own parameter as the
	// argument (so arg_of's From is a dataflow node, not a literal) and
	// assigns the return value (so returns_to is emitted). Both edges are
	// anchored to nodes in THIS file.
	writeFile(t, filepath.Join(dir, "caller.go"), `package main

import "fmt"

import "`+goModName+`/sink"

var GlobalCfg = "cfg"

type Box struct{ Payload string }

func Drive(input string, b Box) {
	out := sink.Transform(input) // bare in-scope arg: From resolves to a file local
	fmt.Println(out)             // arg_of(out) + returns_to
	sink.Transform(b.Payload)    // selector arg: From = synthetic unresolved::*.Payload
	sink.Transform(GlobalCfg)    // global arg: From = synthetic unresolved::GlobalCfg
	sink.Transform(echo(input))  // nested-call arg: From = synthetic unresolved::echo
}

func echo(s string) string { return s }
`)

	// A go.mod so the cross-file import resolves to a real callee node
	// (resolver.ResolveAll lifts unresolved::Transform → the sink node).
	writeFile(t, filepath.Join(dir, "go.mod"), "module "+goModName+"\n\ngo 1.22\n")

	// Build ONE raw graph: index every file WITHOUT the per-file dataflow
	// pass, then run the cross-file resolver so unresolved:: call targets
	// are lifted — but stop short of any materialisation. This is exactly
	// the state both materialise passes are designed to consume.
	gRaw := graph.New()
	idx := newTestIndexer(gRaw)
	files := goFilesUnder(t, dir)
	require.NotEmpty(t, files)
	for _, f := range files {
		// This fixture deliberately stops before resolver/dataflow work. The
		// public IndexFileNoResolve compatibility alias now owns the complete
		// coordinated point-mutation pipeline, so use the package-private raw
		// helper to preserve the test's pre-materialisation graph state.
		require.NoError(t, idx.indexFile(f, false))
	}
	idx.resolver.ResolveAll()

	// Sanity: the fixture must actually emit the edges we claim to test.
	// If it doesn't, an "equivalent" result is vacuously true and proves
	// nothing — fail loudly instead.
	preArg, preRet := countKinds(gRaw)
	require.Greaterf(t, preArg, 0,
		"fixture produced no EdgeArgOf edges; nothing to materialise (edges: %s)", dumpDataflow(gRaw))
	require.Greaterf(t, preRet, 0,
		"fixture produced no EdgeReturnsTo edges; nothing to materialise (edges: %s)", dumpDataflow(gRaw))
	// Guard against a vacuous pass: the fixture MUST produce at least one
	// arg_of edge whose From is a synthetic (unresolved::/external::) id —
	// the selector / global / nested-call shape a node-membership scope
	// misses. This is the exact regression the scoped pass must handle, so
	// fail loudly if the fixture stops exercising it.
	require.Truef(t, hasSyntheticArgFrom(gRaw),
		"fixture produced no synthetic-From arg_of edge; the regression case is not exercised (edges: %s)", dumpDataflow(gRaw))

	// Two byte-identical clones of the raw graph.
	gGlobal := cloneGraph(gRaw)
	gScoped := cloneGraph(gRaw)
	require.Equal(t, dataflowTupleSet(gRaw), dataflowTupleSet(gGlobal),
		"clone must reproduce the raw graph's dataflow edges before any pass runs")
	require.Equal(t, dataflowTupleSet(gRaw), dataflowTupleSet(gScoped),
		"clone must reproduce the raw graph's dataflow edges before any pass runs")

	// (a) whole-graph pass on gGlobal.
	idxGlobal := newTestIndexer(gGlobal)
	idxGlobal.materializeDataflowParams()

	// (b) scoped per-file pass on gScoped — once per file, mirroring the
	// incremental re-index path that calls it after ResolveFile.
	idxScoped := newTestIndexer(gScoped)
	for _, gp := range graphFilePaths(gScoped) {
		idxScoped.materializeDataflowParamsForFile(gp, fileEdgesOf(gScoped, gp))
	}

	globalSet := dataflowTupleSet(gGlobal)
	scopedSet := dataflowTupleSet(gScoped)

	// The whole point: a rewrite must have actually occurred (at least one
	// arg_of lifted to a #param: target, at least one returns_to lifted to
	// the resolved callee), otherwise both sets equalling the raw set
	// would pass trivially without exercising the rewrite logic.
	require.Truef(t, rewriteOccurred(gGlobal),
		"whole-graph pass performed no rewrite; test would be vacuous (edges: %s)", dumpDataflow(gGlobal))

	if globalSet != scopedSet {
		t.Fatalf("scoped per-file dataflow materialisation diverged from the whole-graph pass\n%s",
			diffTupleSets(globalSet, scopedSet))
	}
}

const goModName = "dataflowfixture"

// goFilesUnder returns absolute paths to every .go file under dir, sorted
// for determinism.
func goFilesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			out = append(out, path)
		}
		return nil
	}))
	sort.Strings(out)
	return out
}

// graphFilePaths returns the distinct file-node paths in the graph
// (the keys GetFileNodes / materializeDataflowParamsForFile accept),
// sorted for determinism.
func graphFilePaths(g graph.Store) []string {
	seen := map[string]struct{}{}
	for _, n := range g.AllNodes() {
		if n == nil || n.FilePath == "" {
			continue
		}
		seen[n.FilePath] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// fileEdgesOf returns the edges the given file emitted, matched by the
// edge's own FilePath — the test stand-in for indexFile's result.Edges,
// from which materializeDataflowParamsForFile reads From endpoints
// (including the synthetic ids that are not file nodes).
func fileEdgesOf(g graph.Store, filePath string) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range g.AllEdges() {
		if e != nil && e.FilePath == filePath {
			out = append(out, e)
		}
	}
	return out
}

// dataflowTupleSet renders the EdgeArgOf + EdgeReturnsTo edges as a sorted,
// newline-joined set of "Kind|From|To" tuples. Two graphs with an equal
// set are indistinguishable for the dataflow edges this pass owns.
func dataflowTupleSet(g graph.Store) string {
	var lines []string
	for _, e := range g.AllEdges() {
		if e == nil {
			continue
		}
		if e.Kind != graph.EdgeArgOf && e.Kind != graph.EdgeReturnsTo {
			continue
		}
		lines = append(lines, string(e.Kind)+"|"+e.From+"|"+e.To)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// countKinds counts arg_of and returns_to edges in the graph.
func countKinds(g graph.Store) (argOf, returnsTo int) {
	for _, e := range g.AllEdges() {
		if e == nil {
			continue
		}
		switch e.Kind {
		case graph.EdgeArgOf:
			argOf++
		case graph.EdgeReturnsTo:
			returnsTo++
		}
	}
	return
}

// rewriteOccurred reports whether the materialise pass actually moved an
// edge: an arg_of now points at a #param: node, or a returns_to no longer
// originates from an unresolved/placeholder caller (its From was lifted to
// the resolved callee, observable as a From that is itself the To of a
// resolved EdgeArgOf's owner — pragmatically we detect the arg_of lift,
// which is unambiguous).
func rewriteOccurred(g graph.Store) bool {
	for _, e := range g.AllEdges() {
		if e == nil {
			continue
		}
		if e.Kind == graph.EdgeArgOf && strings.Contains(e.To, "#param:") {
			return true
		}
	}
	return false
}

// hasSyntheticArgFrom reports whether any arg_of edge's From is a
// synthetic placeholder (unresolved::/external::) rather than a real file
// node — the shape that a node-membership-only scope would skip.
func hasSyntheticArgFrom(g graph.Store) bool {
	for _, e := range g.AllEdges() {
		if e == nil || e.Kind != graph.EdgeArgOf {
			continue
		}
		if strings.HasPrefix(e.From, "unresolved::") || strings.HasPrefix(e.From, "external::") {
			return true
		}
	}
	return false
}

// dumpDataflow renders the arg_of/returns_to edges (with the Meta keys the
// rewrites read) for failure diagnostics.
func dumpDataflow(g graph.Store) string {
	var lines []string
	for _, e := range g.AllEdges() {
		if e == nil || (e.Kind != graph.EdgeArgOf && e.Kind != graph.EdgeReturnsTo) {
			continue
		}
		lines = append(lines, string(e.Kind)+" "+e.From+" -> "+e.To+
			"  meta{arg_position="+metaVal(e.Meta, "arg_position")+
			" returns_to_call="+metaVal(e.Meta, "returns_to_call")+
			" call_line="+metaVal(e.Meta, "call_line")+
			" callee_target="+metaVal(e.Meta, "callee_target")+"}")
	}
	sort.Strings(lines)
	return "\n  " + strings.Join(lines, "\n  ")
}

func metaVal(m map[string]any, k string) string {
	if m == nil {
		return "<nil-meta>"
	}
	v, ok := m[k]
	if !ok {
		return "<absent>"
	}
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.Itoa(int(x))
	case float64:
		return strconv.Itoa(int(x))
	default:
		return "?"
	}
}

// diffTupleSets renders a unified line-diff of two sorted tuple sets.
func diffTupleSets(global, scoped string) string {
	g := map[string]struct{}{}
	for _, l := range strings.Split(global, "\n") {
		if l != "" {
			g[l] = struct{}{}
		}
	}
	s := map[string]struct{}{}
	for _, l := range strings.Split(scoped, "\n") {
		if l != "" {
			s[l] = struct{}{}
		}
	}
	var onlyGlobal, onlyScoped []string
	for l := range g {
		if _, ok := s[l]; !ok {
			onlyGlobal = append(onlyGlobal, l)
		}
	}
	for l := range s {
		if _, ok := g[l]; !ok {
			onlyScoped = append(onlyScoped, l)
		}
	}
	sort.Strings(onlyGlobal)
	sort.Strings(onlyScoped)
	var b strings.Builder
	b.WriteString("only in WHOLE-GRAPH pass (missing from scoped):\n")
	for _, l := range onlyGlobal {
		b.WriteString("  - " + l + "\n")
	}
	b.WriteString("only in SCOPED pass (missing from whole-graph):\n")
	for _, l := range onlyScoped {
		b.WriteString("  + " + l + "\n")
	}
	return b.String()
}

// cloneGraph builds a fresh in-memory graph that is structurally identical
// to src, deep-copying every node and edge (including Meta) so a pass run
// on the clone cannot mutate src or the sibling clone.
func cloneGraph(src graph.Store) graph.Store {
	dst := graph.New()
	srcNodes := src.AllNodes()
	srcEdges := src.AllEdges()
	nodes := make([]*graph.Node, 0, len(srcNodes))
	for _, n := range srcNodes {
		if n == nil {
			continue
		}
		nc := *n
		nc.Meta = cloneMeta(n.Meta)
		nodes = append(nodes, &nc)
	}
	edges := make([]*graph.Edge, 0, len(srcEdges))
	for _, e := range srcEdges {
		if e == nil {
			continue
		}
		ec := *e
		ec.Meta = cloneMeta(e.Meta)
		edges = append(edges, &ec)
	}
	dst.AddBatch(nodes, edges)
	return dst
}

func cloneMeta(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	c := make(map[string]any, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}
