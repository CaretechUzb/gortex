package resolver

import (
	"sort"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// The changed-file frontier for the Odoo pass.
//
// ResolveOdooRefsScoped narrows a partial run to one REPOSITORY, which is not a
// narrowing at all on the workspace this exists for: a 38-file edit collected
// 923,510 edges, every one of them carrying a compressed meta blob that has to
// be decompressed and JSON-decoded in Go to read a single `via` tag. That is
// ~199s of the ~215s the pass spent, against 2.3s for the SQL underneath it —
// the cost is in the rows crossing the boundary, not in finding them. Scoping
// to a repository was measurably WORSE than the cold whole-workspace path,
// which streams kind buckets and never materialises or sorts.
//
// The fix is to collect by the changed frontier instead. What makes that sound
// is that the pass is a full recompute per COLLECTED EDGE: an edge it does not
// collect keeps whatever binding it has. So the frontier has to hold every edge
// whose verdict this change can move, and an edge's verdict is a function of
// its own key (which lives in its Meta, so it moves only when its source file
// is re-parsed) and of the declaration index entry for that key (which moves
// only when a declaring file is re-parsed). That yields four sources of work,
// and the fourth is the one a naive "edges in the changed files" frontier
// misses:
//
//  1. nodes in the changed files — their references were just re-parsed;
//  2. whoever points INTO a changed file — the target may have changed its key,
//     or stopped declaring one;
//  3. whoever points at a target NOTHING answers to any more — the deleted
//     declaration. This cannot be found by asking the graph about live nodes,
//     because the node is precisely what is gone, and it is the case that
//     un-binding exists for;
//  4. whoever points at a placeholder for a key a changed file now declares, or
//     at another node already declaring it — the new-bind case, and the
//     fan-out-widening case where a model gains a third declaring class.
//
// Measured on the MR this was built for: 923,510 edges collected before,
// ~6,000 after, and the frontier itself costs ~0.4s of indexed queries.

// odooFrontierEdgeKinds is the union of the three families' kinds — the only
// edge kinds an Odoo placeholder ever rides.
func odooFrontierEdgeKinds() []graph.EdgeKind {
	seen := map[graph.EdgeKind]bool{}
	var kinds []graph.EdgeKind
	for _, family := range [][]graph.EdgeKind{odooModelEdgeKinds, odooXMLEdgeKinds, odooJSEdgeKinds} {
		for _, kind := range family {
			if seen[kind] {
				continue
			}
			seen[kind] = true
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// odooFrontierStats reports what each step of the frontier cost and admitted.
// Every entry rides the pass's phase map: this frontier replaced a step that
// was 199s of the pass's 215s, and an aggregate number could not have told the
// four components apart — the first cut of it admitted 15,380 sources when
// 2,046 were at stake, and only a per-step count showed which one was wrong.
type odooFrontierStats map[string]int64

// odooChangedFrontierSources returns the source node ids whose Odoo edges this
// change can move, sorted so a pass collects deterministically.
//
// The result is a set of SOURCES, not of edges, and that is deliberate. A
// global pass owns an edge by its source node, and bindOdooModels reconciles a
// model's fan-out siblings as a SET keyed on the shared source: collecting a
// primary without its siblings would hand odooReconcileFanout a partial
// `observed` map. Closing the frontier under the source node makes the
// collected set exactly "every Odoo edge out of every node this change can
// affect", which is the whole-repository contract restricted to a subset of
// sources rather than a different contract.
func odooChangedFrontierSources(
	g graph.Store,
	d *odooDecls,
	repoPrefixes []string,
	filePaths []string,
) ([]string, odooFrontierStats) {
	stats := odooFrontierStats{}
	if g == nil || len(filePaths) == 0 {
		return nil, stats
	}
	step := func(name string, fn func()) {
		start := time.Now()
		fn()
		stats["frontier_"+name+"_ms"] = time.Since(start).Milliseconds()
	}

	sources := map[string]struct{}{}
	changedIDs := make([]string, 0, 512)
	step("changed_nodes", func() {
		for _, nodes := range g.GetFileNodesByPaths(filePaths) {
			for _, node := range nodes {
				if node == nil || node.ID == "" {
					continue
				}
				changedIDs = append(changedIDs, node.ID)
				// (1) a changed file's own nodes are sources in their own right.
				sources[node.ID] = struct{}{}
			}
		}
	})

	targets := make([]string, 0, len(changedIDs)+256)
	targets = append(targets, changedIDs...) // (2) points into a changed file
	step("dangling", func() {
		targets = append(targets, graph.DanglingEdgeTargets(
			g, odooDeletionIDPrefixes(repoPrefixes, filePaths), odooFrontierEdgeKinds(),
		)...) // (3) points at a target that is gone
	})
	step("touched_keys", func() {
		targets = append(targets, odooTouchedKeyTargets(d, changedIDs)...) // (4) new bind / wider fan-out
	})

	targets = dedupeSortedStrings(targets)
	stats["frontier_targets"] = int64(len(targets))
	inEdges := 0
	step("in_edges", func() {
		for _, edges := range g.GetInEdgesByNodeIDs(targets) {
			for _, edge := range edges {
				inEdges++
				// Only an Odoo edge can have its verdict moved by an Odoo
				// declaration, and the difference is not academic: the dangling
				// sweep surfaces unresolved OWL component names that thousands
				// of unrelated edges point at, and taking every in-edge's source
				// admitted 15,380 sources where 2,046 were at stake.
				if edge == nil || edge.From == "" || odooEdgeVia(edge) == "" {
					continue
				}
				sources[edge.From] = struct{}{}
			}
		}
	})
	stats["frontier_in_edges"] = int64(inEdges)

	out := make([]string, 0, len(sources))
	for id := range sources {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, stats
}

// odooSyntheticIDInfix is the namespace every Odoo node that is not a file's
// own symbol lives in: `<repo>/odoo::record::…`, `::template::`, `::menu::`,
// `::registry::`. See internal/parser/languages/odoo_xml.go and odoo_csv.go.
const odooSyntheticIDInfix = "/odoo::"

// odooDeletionIDPrefixes bounds the dangling-target sweep to the id space where
// THIS change could have deleted a declaration.
//
// A node disappears only when its file is re-indexed, so for a file-derived id —
// `<repo>/<path>::<symbol>` — the changed paths are the exact bound. An Odoo
// record does not encode its file in its id, so its whole synthetic namespace is
// swept instead and the liveness check does the narrowing; that range holds
// 27,188 targets and answers in ~22ms.
//
// Sweeping the whole repository instead is both slower and wrong-headed. It
// surfaces every target left dangling by any past change — on the live
// workspace, 537 of them, mostly unresolved OWL component names that survived
// many whole-repository passes untouched — and pulling those hubs into the
// frontier cost 106s of edge collection for work this change did not create.
// Repairing them is a re-derive's job, not an incremental pass's.
func odooDeletionIDPrefixes(repoPrefixes, filePaths []string) []string {
	out := make([]string, 0, len(repoPrefixes)+len(filePaths))
	for _, prefix := range repoPrefixes {
		if prefix == "" {
			// A single unprefixed repository has no id namespace to bound, and
			// "/" would be a whole-table scan wearing a prefix's clothes.
			continue
		}
		// Named as `<repo>/odoo::`, never the bare prefix: `local@wt/…` starts
		// with `local`, so a bare-prefix sweep would reach into a sibling
		// checkout and drag its edges into this repository's frontier.
		out = append(out, prefix+odooSyntheticIDInfix)
	}
	out = append(out, filePaths...)
	return out
}

// odooTouchedKeyTargets returns, for every declaration key a changed node
// declares, both the placeholder that key's unbound references point at and
// every OTHER node currently declaring it.
//
// The placeholder half is what admits a new binding: an unbound reference
// points at `unresolved::odoo::model::<key>`, so the edges that a new
// declaration would newly satisfy are reachable by one in-edge lookup on a
// known id rather than by a scan. The other-declarers half is what admits a
// wider fan-out: an already-bound reference to a model that gains a third
// declaring class must be revisited, and it is bound — so the placeholder does
// not lead to it.
//
// The walk is over the declaration indexes rather than over node metadata on
// purpose. Keyed by index, every family's vocabulary is covered by
// construction; keyed by meta field, it would have to restate which meta key
// feeds which index, and would go quietly stale the next time a family learns
// a new one.
func odooTouchedKeyTargets(d *odooDecls, changedIDs []string) []string {
	if d == nil || len(changedIDs) == 0 {
		return nil
	}
	changed := make(map[string]struct{}, len(changedIDs))
	for _, id := range changedIDs {
		changed[id] = struct{}{}
	}

	var out []string
	fanout := func(stub string, byKey map[string][]string) {
		for key, ids := range byKey {
			if !anyChanged(changed, ids) {
				continue
			}
			out = append(out, stub+key)
			out = append(out, ids...)
		}
	}
	single := func(stub string, byKey map[string]string) {
		for key, id := range byKey {
			if _, ok := changed[id]; !ok {
				continue
			}
			out = append(out, stub+key, id)
		}
	}
	indexed := func(stub string, ix odooIndex) {
		for key, byRepo := range ix {
			hit := false
			ids := make([]string, 0, len(byRepo))
			for _, id := range byRepo {
				ids = append(ids, id)
				if _, ok := changed[id]; ok {
					hit = true
				}
			}
			if !hit {
				continue
			}
			out = append(out, stub+key)
			out = append(out, ids...)
		}
	}

	fanout(odooModelStubPrefix, d.models)
	indexed(odooXMLIDStubPrefix, d.xmlIDs)
	indexed(odooTemplateStubPrefix, d.templates)
	indexed(odooJSModuleStubPrefix, d.jsModules)
	indexed(odooMethodStubPrefix, d.modelMethods)
	indexed(odooJSMethodStubPrefix, d.jsMethods)
	if d.implicit != nil {
		// The ORM-minted external IDs ride the xmlid placeholder, not one of
		// their own — bindOdooXMLIDs reaches them through the same key.
		fanout(odooXMLIDStubPrefix, d.implicit.models)
		single(odooXMLIDStubPrefix, d.implicit.fields)
		single(odooXMLIDStubPrefix, d.implicit.modules)
	}
	return out
}

func anyChanged(changed map[string]struct{}, ids []string) bool {
	for _, id := range ids {
		if _, ok := changed[id]; ok {
			return true
		}
	}
	return false
}

func dedupeSortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// odooCollectFamiliesForSources fills every family's edge slice from the Odoo
// edges leaving the frontier's source nodes.
//
// Iteration follows the sorted source list rather than the adjacency map so a
// pass collects in a stable order; the whole-repository path restores the same
// property with a Go-side sort after materialising.
func odooCollectFamiliesForSources(g graph.Store, sources []string, stats odooFrontierStats, families ...*odooFamily) {
	if g == nil || len(sources) == 0 {
		return
	}
	start := time.Now()
	bySource := g.GetOutEdgesByNodeIDs(sources)
	if stats != nil {
		stats["frontier_out_edges_ms"] = time.Since(start).Milliseconds()
		fetched := 0
		for _, edges := range bySource {
			fetched += len(edges)
		}
		stats["frontier_out_edges"] = int64(fetched)
	}
	for _, source := range sources {
		edges := bySource[source]
		if len(edges) == 0 {
			continue
		}
		ordered := make([]*graph.Edge, 0, len(edges))
		ordered = append(ordered, edges...)
		sort.SliceStable(ordered, func(i, j int) bool {
			a, b := ordered[i], ordered[j]
			if a == nil || b == nil {
				return b != nil
			}
			if a.To != b.To {
				return a.To < b.To
			}
			if a.Kind != b.Kind {
				return a.Kind < b.Kind
			}
			return a.Line < b.Line
		})
		for _, edge := range ordered {
			for _, family := range families {
				if family.wants(edge) {
					family.edges = append(family.edges, edge)
					break
				}
			}
		}
	}
}
