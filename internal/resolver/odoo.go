package resolver

import "github.com/zzet/gortex/internal/graph"

// Odoo binding.
//
// Odoo is registered as ONE framework named "odoo" — not split into
// odoo-model / odoo-xml / odoo-js — so a single index.frameworks.allow
// entry governs the whole framework and it can never end up half-indexed
// with models bound but templates dangling.
//
// The three binding families are therefore ordered sub-steps of one pass
// rather than three registry entries. The order is load-bearing and is
// enforced here, by call sequence, rather than by registry adjacency:
//
//  1. models — a <record model="sale.order"> can only bind once the
//     Python model classes are indexed by their `_name`;
//  2. XML IDs — view inheritance and menu/action chains;
//  3. JS — an OWL component's `static template` binds to a QWeb template
//     node, which step 2 is what mints.
//
// Every placeholder these binders consume was pre-tagged by the extractor
// with Meta["via"], so none of them needs the claiming-resolver tail.

// Odoo placeholder prefixes, matching what the extractors emit. The
// languages package does not import resolver, so the two sides agree by
// value — the same arrangement celery and mybatis use.
const (
	odooModelStubPrefix    = unresolvedPrefix + "odoo::model::"
	odooXMLIDStubPrefix    = unresolvedPrefix + "odoo::xmlid::"
	odooMethodStubPrefix   = unresolvedPrefix + "odoo::method::"
	odooTemplateStubPrefix = unresolvedPrefix + "odoo::template::"
	odooJSModuleStubPrefix = unresolvedPrefix + "odoo::jsmodule::"
	odooJSMethodStubPrefix = unresolvedPrefix + "odoo::jsmethod::"
)

// Odoo `via` tags. These are internal index keys that let each sub-binder
// scan only its own placeholders; they are NOT framework names and never
// appear in the exclusion inventory or in synthesized_by, which is always
// "odoo".
const (
	odooModelVia = "odoo-model"
	odooXMLVia   = "odoo-xml"
	odooJSVia    = "odoo-js"
)

// odooFanoutCap bounds how many targets one placeholder may bind. A model
// legitimately fans out — several addons extend one `_name` — but an
// unbounded fan-out on a pathological corpus would be a graph bomb.
const odooFanoutCap = 200

// ResolveOdooRefs binds every Odoo placeholder in the graph. It is the
// single registered synthesizer for the odoo framework; see the package
// comment above for why the three families run in this fixed order.
func ResolveOdooRefs(g graph.Store) int {
	return ResolveOdooRefsScoped(g, nil)
}

// ResolveOdooRefsScoped limits the recompute to edges declared in a
// changed repository, while every index it binds against is still built
// from the WHOLE store. A nil scope preserves the full/cold behaviour.
//
// The split is not an optimization — it is what makes the pass safe to
// run partially at all. Each family is a full recompute: an edge whose
// target is absent from the index is RESET to its placeholder, which is
// how a reference to a deleted record un-binds itself. Handed a scoped
// store, the pass would build its indexes from the changed repository
// alone and then apply that verdict to every Odoo edge in the workspace,
// so indexing addons after odoo reset all of odoo's edges to placeholders
// — 181,077 of them, with the records they name still sitting in the
// graph. Scoping the collection instead of the indexes keeps "absent from
// the index" meaning what it says.
func ResolveOdooRefsScoped(g graph.Store, scope map[string]bool) int {
	if g == nil {
		return 0
	}
	n := bindOdooModels(g, scope)
	n += bindOdooXMLIDs(g, scope)
	n += bindOdooJS(g, scope)
	return n
}

// odooBindPlan is one edge queued for rebinding. placeholder is the
// target to restore when target is empty, which is what makes the pass a
// full recompute rather than a one-way binding.
type odooBindPlan struct {
	edge        *graph.Edge
	target      string
	placeholder string
}

// odooRebind applies a batch of rebindings, stamping Odoo provenance and
// keeping the graph's edge buckets consistent.
//
// Like the other framework binders this is a FULL RECOMPUTE, and each
// plan therefore carries both the resolved target and the placeholder to
// fall back to. Every pass recomputes an edge's destination from its own
// Meta rather than from its current target, so re-running the pass is a
// no-op, and an edge whose target has since left the graph is reset to
// its placeholder and loses its resolution metadata instead of silently
// pointing at a node that no longer exists.
func odooRebind(g graph.Store, plans []odooBindPlan, confidence float64) int {
	var batch []graph.EdgeReindex
	resolved := 0
	stillLive := odooLiveRebindTargets(g, plans)
	for _, p := range plans {
		e := p.edge
		if e == nil {
			continue
		}
		want, bound := p.target, p.target != ""
		if !bound {
			// About to un-bind. A recompute may only do that because the
			// target is genuinely gone — never because the index it was
			// built from could not see it. Those two look identical from
			// here, so the target is asked directly, and an edge whose
			// target still answers to the edge's own key keeps its
			// binding. Without this, one pass over a partial view resets
			// a whole repository's edges to placeholders while the
			// records they name are still in the graph, and nothing ever
			// puts them back.
			if stillLive[odooEdgeIdentityOf(e)] {
				continue
			}
			want = p.placeholder
		}
		if want == "" {
			continue
		}
		if bound {
			resolved++
		}
		if e.To == want {
			continue
		}
		oldTo := e.To
		e.To = want
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		if bound {
			e.Origin = graph.OriginASTInferred
			e.Confidence = confidence
			e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, confidence)
			e.Meta[MetaSynthesizedBy] = SynthOdoo
			e.Meta[MetaProvenance] = ProvenanceFramework
		} else {
			// Re-orphaned since the last pass: drop the resolution-tier
			// metadata so the edge reads as a plain placeholder again.
			e.Origin = ""
			e.Confidence = 0
			e.ConfidenceLabel = ""
			delete(e.Meta, MetaSynthesizedBy)
			delete(e.Meta, MetaProvenance)
		}
		batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(batch) > 0 {
		g.ReindexEdges(batch)
	}
	return resolved
}

// odooMetaString reads a string value off an edge's Meta.
func odooMetaString(e *graph.Edge, key string) string {
	if e == nil || e.Meta == nil {
		return ""
	}
	v, _ := e.Meta[key].(string)
	return v
}

// odooIsFanout reports whether an edge is one of the extra targets
// bindOdooModels materialised for a multiply-declared model. Those are
// derived edges: recomputing them would collapse the fan-out back onto
// its first target.
func odooIsFanout(e *graph.Edge) bool {
	if e == nil || e.Meta == nil {
		return false
	}
	v, _ := e.Meta["odoo_fanout"].(bool)
	return v
}

// odooLiveRebindTargets reports which of the plans that would UN-BIND an
// edge have a target still satisfying that edge's own key.
//
// Only currently-bound edges are asked about — a plan that leaves a
// placeholder as a placeholder changes nothing — so the batch read costs
// one query on the rare pass that would drop bindings, and nothing at all
// on a steady-state run.
func odooLiveRebindTargets(g graph.Store, plans []odooBindPlan) map[odooEdgeIdentity]bool {
	var ids []string
	for _, p := range plans {
		if p.edge == nil || p.target != "" || graph.IsUnresolvedTarget(p.edge.To) {
			continue
		}
		ids = append(ids, p.edge.To)
	}
	if len(ids) == 0 {
		return nil
	}
	nodes := g.GetNodesByIDs(ids)
	out := map[odooEdgeIdentity]bool{}
	for _, p := range plans {
		if p.edge == nil || p.target != "" || graph.IsUnresolvedTarget(p.edge.To) {
			continue
		}
		if odooTargetSatisfies(nodes[p.edge.To], p.edge) {
			out[odooEdgeIdentityOf(p.edge)] = true
		}
	}
	return out
}

// odooTargetSatisfies reports whether n is still a valid target for e,
// judged by the same key the binder would have used.
//
// A key the binder cannot re-check here — a JS module specifier names a
// path rather than anything stored on the file node — reports false, which
// leaves the existing un-bind behaviour untouched for that family.
func odooTargetSatisfies(n *graph.Node, e *graph.Edge) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	if k := odooMetaString(e, "odoo_xml_id"); k != "" {
		v, _ := n.Meta["odoo_xml_id"].(string)
		if v == k || (v != "" && odooBareXMLID(v) == odooBareXMLID(k)) {
			return true
		}
		// The implicit index binds `model_<name>` to the class itself,
		// which carries no external ID of its own.
		if m, _ := n.Meta["odoo_model"].(string); m != "" && odooIsImplicitXMLID(k) {
			return true
		}
		return false
	}
	if k := odooMetaString(e, "odoo_model"); k != "" {
		v, _ := n.Meta["odoo_model"].(string)
		return v == k
	}
	if k := odooMetaString(e, "odoo_template"); k != "" {
		v, _ := n.Meta["odoo_template"].(string)
		return v == k || (v != "" && odooBareXMLID(v) == odooBareXMLID(k))
	}
	return false
}

// odooEdgeIdentity is an edge's (From, To, Kind) triple — the same key
// AddEdge is idempotent on, and the one RemoveEdge deletes by.
type odooEdgeIdentity struct {
	from, to string
	kind     graph.EdgeKind
}

func odooEdgeIdentityOf(e *graph.Edge) odooEdgeIdentity {
	return odooEdgeIdentity{from: e.From, to: e.To, kind: e.Kind}
}

// odooReconcileFanout writes the fan-out siblings a pass just computed
// and retires the observed ones that are no longer justified.
//
// A sibling carries no placeholder of its own, so odooRebind cannot
// recompute it — which is why every odooCollect callback skips it. That
// skip left these edges outside the pass's full-recompute contract: when
// one of several classes declaring the same `_name` was deleted, its
// sibling survived pointing at a node that no longer exists, and
// file-scoped edge cleanup could not reach it either, because a
// sibling's FilePath is the SOURCE file rather than the vanished
// target's.
//
// Retirement is driven by `stale`, which asks whether an edge's target is
// still a valid target FOR THAT EDGE'S OWN KEY, and never by "the pass
// did not recompute it". The difference is not cosmetic.
// graph.RemoveEdgesExact deletes by (From, To, Kind, FilePath, Line), and
// odooSiblingEdge clones its primary, so a sibling and the primary it came
// from are indistinguishable at the store level whenever they share a
// target. Meanwhile `want` only ever holds targets[1:], and which target
// sorts into targets[0] is not stable across passes. Diffing against the
// recomputed set therefore condemns an identity the pass had just bound as
// a primary, and the delete takes the primary with it — silently, and in
// proportion to fan-out width. A key-relative predicate cannot rotate, and
// `keep` (pre-seeded with every identity this pass bound) closes the
// residual case where a stale sibling collides with a live primary.
func odooReconcileFanout(g graph.Store, observed map[odooEdgeIdentity]*graph.Edge, want []*graph.Edge, keep map[odooEdgeIdentity]bool, stale func(*graph.Edge) bool) {
	for _, e := range want {
		if e == nil {
			continue
		}
		keep[odooEdgeIdentityOf(e)] = true
		// AddEdge is idempotent on (From, To, Kind), so re-running the
		// pass re-offers the same siblings without duplicating them.
		g.AddEdge(e)
	}
	var retire []*graph.Edge
	for id, e := range observed {
		if keep[id] || !stale(e) {
			continue
		}
		retire = append(retire, e)
	}
	if len(retire) > 0 {
		graph.RemoveEdgesExact(g, retire)
	}
}

// odooBoundIdentities collects the identities a pass bound as primaries,
// so reconciliation can never retire one of them by tuple collision.
func odooBoundIdentities(plans []odooBindPlan) map[odooEdgeIdentity]bool {
	keep := make(map[odooEdgeIdentity]bool, len(plans))
	for _, p := range plans {
		if p.edge != nil {
			keep[odooEdgeIdentityOf(p.edge)] = true
		}
	}
	return keep
}

// odooEdgeVia reports the edge's Odoo via tag.
func odooEdgeVia(e *graph.Edge) string {
	if e == nil || e.Meta == nil {
		return ""
	}
	v, _ := e.Meta["via"].(string)
	return v
}

// odooCollect walks the kinds an Odoo placeholder can ride and yields the
// edges carrying the given via tag. Odoo placeholders do NOT ride
// EdgeCalls alone — they use extends / composes / references / imports /
// renders_child / overrides — which is also why the framework registry
// gates this pass on a node marker rather than on the call-edge census.
func odooCollect(g graph.Store, scope map[string]bool, via string, kinds []graph.EdgeKind, fn func(*graph.Edge)) {
	if scope == nil {
		// Stream on the cold path. The Odoo families run to a million
		// edges on a full workspace, and frameworkRepoEdges would
		// materialise all of them into a slice only to have most
		// filtered out by `via` a line later.
		for _, kind := range kinds {
			for e := range g.EdgesByKind(kind) {
				if e == nil || odooEdgeVia(e) != via {
					continue
				}
				fn(e)
			}
		}
		return
	}
	for _, e := range frameworkRepoEdges(g, scope, kinds...) {
		if e == nil || odooEdgeVia(e) != via {
			continue
		}
		fn(e)
	}
}

// odooNodeMarker reports whether a node belongs to the Odoo framework, in
// any of its three halves. It feeds the registry's node preflight: the
// pass runs iff the graph contains anything Odoo at all.
func odooNodeMarker(n *graph.Node) bool {
	if n == nil {
		return false
	}
	if n.Language == "odoo_xml" {
		return true
	}
	if n.Meta == nil {
		return false
	}
	for _, key := range []string{"odoo_model", "odoo_xml_id", "odoo_js_module", "odoo_module"} {
		if v, ok := n.Meta[key]; ok && v != nil && v != "" {
			return true
		}
	}
	return false
}
