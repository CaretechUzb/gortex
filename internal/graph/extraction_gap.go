package graph

// ZeroEdgeClass classifies why a symbol's graph query came back empty.
// An empty result has two very different causes that an agent cannot
// otherwise tell apart, and a pre-edit safety check that trusts a
// false "0 usages" is silently disarmed.
type ZeroEdgeClass string

const (
	// ZeroEdgeNone means the symbol has incoming call/reference edges:
	// the query was not empty and no caveat is warranted.
	ZeroEdgeNone ZeroEdgeClass = "none"

	// ZeroEdgeLikelyUnused means the symbol has no incoming
	// call/reference edges but DOES carry other graph edges — the
	// structural edge from its file (`defines`), a method's
	// `member_of`, or outgoing calls / references / type references.
	// This is consistent with genuine dead code: the extractor saw
	// the symbol, nothing uses it.
	ZeroEdgeLikelyUnused ZeroEdgeClass = "likely_unused"

	// ZeroEdgePossibleExtractionGap means the symbol has zero edges of
	// any kind. A normally indexed function or method always carries
	// at least one structural edge — the file `defines` it, a method
	// is `member_of` its type — so zero total edges most likely means
	// the extractor never processed the symbol or its file. The
	// symbol may well be live; the graph just does not know. This is
	// the dangerous case for a delete-or-rewrite decision.
	ZeroEdgePossibleExtractionGap ZeroEdgeClass = "possible_extraction_gap"

	// ZeroEdgeCoverageIncomplete means the symbol has no resolved
	// incoming call/reference edges, but the graph DOES carry
	// import-level evidence that consumers exist: inbound `imports` /
	// `re_exports` edges land on the symbol itself, or (for a public
	// JS/TS symbol) on its file. The usage query is incomplete, not
	// empty — reference-level resolution failed somewhere upstream —
	// so an agent must NOT read the empty result as safe-to-remove.
	ZeroEdgeCoverageIncomplete ZeroEdgeClass = "coverage_incomplete"
)

// ZeroEdgeCaveat is the structured caveat attached to an empty graph
// query result. Class is machine-checkable so a safety gate can branch
// on it; Message is a short human-readable explanation.
type ZeroEdgeCaveat struct {
	Class   ZeroEdgeClass `json:"class" toon:"class"`
	Message string        `json:"message" toon:"message"`
}

// ZeroImpactCaveat is the per-symbol caveat attached to an empty impact
// analysis result, which is computed over a list of symbols. It carries
// the symbol ID alongside the same machine-checkable classification.
type ZeroImpactCaveat struct {
	ID      string        `json:"id" toon:"id"`
	Class   ZeroEdgeClass `json:"class" toon:"class"`
	Message string        `json:"message" toon:"message"`
}

// TierFilteredClass is the machine-checkable class of a min_tier-emptied
// result — distinct from ZeroEdgeClass so a safety gate never confuses
// "filtered out by min_tier" with "genuinely has no usages".
const TierFilteredClass = "tier_filtered"

// TierFilteredCaveat is attached to a find_usages / get_* result whose edge
// list was emptied (or thinned) by a min_tier filter while lower-tier edges
// still exist. Without it a min_tier that filters every edge is
// indistinguishable from "no usages" — the honesty bug it fixes. Class is
// always TierFilteredClass; EdgesBelowMinTier counts the edges dropped by the
// filter and MaxAvailableTier names the best tier actually present.
type TierFilteredCaveat struct {
	Class            string `json:"class" toon:"class"`
	EdgesBelowMinTier int   `json:"edges_below_min_tier" toon:"edges_below_min_tier"`
	MaxAvailableTier string `json:"max_available_tier" toon:"max_available_tier"`
}

// usageEdgeKinds are the incoming edge kinds that count as a symbol
// being "used" — calls, references, and the type/instantiation edges
// that find_usages itself treats as usages. An incoming edge of any of
// these kinds means the symbol is not dead code.
var usageEdgeKinds = map[EdgeKind]bool{
	EdgeCalls:        true,
	EdgeReferences:   true,
	EdgeInstantiates: true,
	EdgeImplements:   true,
	EdgeExtends:      true,
	EdgeReads:        true,
	EdgeWrites:       true,
	EdgeTests:        true,
}

// UsageInboundEdgeKinds returns the canonical list of incoming edge
// kinds that classify a symbol as "used" by ClassifyZeroEdge. Exposed
// for capability callers (NodeDegreeAggregator) that need to mirror
// the in-graph usage filter server-side. Order is stable so the slice
// is safe to pass directly to a query parameter binding.
func UsageInboundEdgeKinds() []EdgeKind {
	return []EdgeKind{
		EdgeCalls,
		EdgeReferences,
		EdgeInstantiates,
		EdgeImplements,
		EdgeExtends,
		EdgeReads,
		EdgeWrites,
		EdgeTests,
	}
}

// ClassifyZeroEdge inspects a symbol's incoming and outgoing edges and
// returns how an empty usage/caller/impact query for it should be read.
//
//   - ZeroEdgeNone — the symbol has at least one incoming usage edge.
//   - ZeroEdgeLikelyUnused — no incoming usage edge, but the symbol has
//     other graph edges (structural defines/member_of, or any outgoing
//     edge). Consistent with genuine dead code.
//   - ZeroEdgePossibleExtractionGap — the symbol has no edges at all,
//     which a normally indexed symbol never has; the extractor most
//     likely missed it.
//
// An unknown symbol ID is reported as an extraction gap: a query whose
// target is not even in the graph is exactly as untrustworthy as one
// whose target was never wired up.
func ClassifyZeroEdge(g Store, symbolID string) ZeroEdgeClass {
	if g == nil || symbolID == "" {
		return ZeroEdgePossibleExtractionGap
	}
	if g.GetNode(symbolID) == nil {
		return ZeroEdgePossibleExtractionGap
	}

	in := g.GetInEdges(symbolID)
	out := g.GetOutEdges(symbolID)

	if len(in) == 0 && len(out) == 0 {
		return ZeroEdgePossibleExtractionGap
	}
	for _, e := range in {
		if usageEdgeKinds[e.Kind] {
			return ZeroEdgeNone
		}
	}
	if importConsumerCount(g, symbolID) > 0 {
		return ZeroEdgeCoverageIncomplete
	}
	// The graph still carries unresolved call candidates that name this
	// symbol — `unresolved::*.<name>` call edges the resolver / enrichment
	// pass never bound to it. Their existence is direct evidence that call
	// sites reference this name; an empty usage set is a resolution/coverage
	// gap, not proof the symbol is unused.
	if hasUnresolvedSameNameCandidates(g, symbolID) {
		return ZeroEdgeCoverageIncomplete
	}
	return ZeroEdgeLikelyUnused
}

// hasUnresolvedSameNameCandidates reports whether the graph holds any
// unresolved member-call edge (`unresolved::*.<name>`) naming this callable
// symbol. Unresolved call stubs are indexed by their target string, so this is
// a single reverse-edge lookup — no scan. Non-callable symbols never match.
func hasUnresolvedSameNameCandidates(g Store, symbolID string) bool {
	n := g.GetNode(symbolID)
	if n == nil || n.Name == "" {
		return false
	}
	if n.Kind != KindFunction && n.Kind != KindMethod {
		return false
	}
	return len(g.GetInEdges(UnresolvedMarker+"*."+n.Name)) > 0
}

// importConsumerCount counts the import-level consumer evidence for a
// symbol: inbound `imports` / `re_exports` edges on the symbol itself
// (any language — a per-binding import that resolved onto the symbol is
// direct proof a consumer names it), plus, for a public JS/TS symbol,
// inbound module-level import edges on its file node (a module-level
// `import ... from './file'` lands on the file, but still proves the
// file's exports have consumers).
func importConsumerCount(g Store, symbolID string) int {
	count := 0
	for _, e := range g.GetInEdges(symbolID) {
		if e.Kind == EdgeImports || e.Kind == EdgeReExports {
			count++
		}
	}
	if count > 0 {
		return count
	}
	n := g.GetNode(symbolID)
	if n == nil || n.FilePath == "" || n.FilePath == symbolID {
		return 0
	}
	switch n.Language {
	case "typescript", "tsx", "javascript", "jsx":
	default:
		return 0 // module-level file imports imply consumers only for JS/TS
	}
	if vis, _ := n.Meta["visibility"].(string); vis != "public" {
		return 0
	}
	for _, e := range g.GetInEdges(n.FilePath) {
		if e.Kind == EdgeImports || e.Kind == EdgeReExports {
			count++
		}
	}
	return count
}

// zeroEdgeMessages maps each classification to its human-readable
// caveat text.
var zeroEdgeMessages = map[ZeroEdgeClass]string{
	ZeroEdgeLikelyUnused: "no incoming call or reference edges, but the symbol is " +
		"indexed (it has structural or outgoing edges) — consistent with genuine " +
		"unused code. It is also what an import specifier the resolver could not " +
		"expand looks like, so this is evidence of no callers, not proof: confirm " +
		"with a text search for the symbol name before removing it.",
	ZeroEdgePossibleExtractionGap: "the symbol has no graph edges of any kind. A " +
		"normally indexed symbol always has at least a structural edge, so the " +
		"extractor most likely did not process it — treat this empty result as " +
		"unverified, not as proof the symbol is unused.",
	ZeroEdgeCoverageIncomplete: "no resolved call or reference edges, but the graph " +
		"still carries consumer evidence — import/re-export edges pointing at this " +
		"symbol or its file, or unresolved same-name call candidates the resolver / " +
		"enrichment pass never bound to it. Reference-level resolution is incomplete, " +
		"so treat this empty result as UNVERIFIED coverage, not as proof the symbol is " +
		"unused or safe to remove.",
}

// zeroEdgeNotFoundMessage is the caveat text when the queried id is not in
// the graph at all — almost always a mistyped id or one missing its repo
// prefix, rather than a true extraction gap.
const zeroEdgeNotFoundMessage = "no symbol with this id is in the graph — the id is " +
	"probably mistyped or missing its repo prefix (ids look like " +
	"<repo>/<path>::<symbol>, e.g. gortex/internal/x.go::Foo). Run a symbol search to " +
	"get the exact id; treat this empty result as unverified, not as proof of no usages."

// CaveatForZeroEdge builds the structured caveat for an empty graph
// query result on symbolID. It returns nil when the symbol has
// incoming usage edges (ZeroEdgeNone) — a non-empty result carries no
// caveat — so callers can attach the return value unconditionally.
func CaveatForZeroEdge(g Store, symbolID string) *ZeroEdgeCaveat {
	// A target that is not even in the graph is the most common cause of a
	// "0 usages" surprise — usually a mistyped id or one missing its repo
	// prefix. Keep the untrustworthy extraction-gap class so safety gates
	// still trip, but point the message at the id rather than the extractor.
	if g != nil && symbolID != "" && g.GetNode(symbolID) == nil {
		return &ZeroEdgeCaveat{Class: ZeroEdgePossibleExtractionGap, Message: zeroEdgeNotFoundMessage}
	}
	class := ClassifyZeroEdge(g, symbolID)
	if class == ZeroEdgeNone {
		return nil
	}
	return &ZeroEdgeCaveat{Class: class, Message: zeroEdgeMessages[class]}
}
