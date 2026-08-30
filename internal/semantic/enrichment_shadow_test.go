package semantic

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// shadowOver returns an indexer-shaped shadow: the freshly parsed content sits
// in the in-memory graph, the durable tables belong to the store behind it.
// That split is the whole point — production never enriches a shadow that also
// owns the bookkeeping.
func shadowOver(owner graph.Store, path graph.StructuralDropPath) *graph.Graph {
	shadow := graph.NewStructuralIntegrityShadow(owner, applRepo, path)
	file := applRepo + "/src/main.go"
	shadow.AddBatch([]*graph.Node{{
		ID: file + "::F", Kind: graph.KindFunction, Name: "F",
		FilePath: file, RepoPrefix: applRepo, Language: "go",
	}}, nil)
	return shadow
}

// The indexer mounts an in-memory shadow over the real store while it indexes,
// and enrichment runs inside that window. Nodes and edges are drained into the
// owner afterwards, so a provider's edges survive — but the enrichment
// bookkeeping tables have no in-memory form at all. A state write addressed to
// the shadow is never merged; it is dropped when the shadow is. Every writer
// type-asserts and returns silently when the backend does not model state, so
// the loss leaves no trace: the passes complete, the coverage is real, and the
// repo reports "enriched: unknown" for good.
//
// Observed live on 2026-08-29 — a retrack ran five providers over a
// 106k-node repo to completion and persisted exactly nothing.
func TestEnrichmentThroughAShadowLandsInTheOwningStore(t *testing.T) {
	owner := applStore(t, "go")
	require.NoError(t, owner.BulkSetFileMtimes(applRepo, map[string]int64{"src/main.go": 1}))
	shadow := shadowOver(owner, graph.StructuralPathShadowCold)
	mgr := applManager(t, applProvider("test-go", "go", true))

	_, _, err := mgr.EnrichAll(shadow, map[string]string{applRepo: t.TempDir()}, EnrichOptions{})
	require.NoError(t, err)

	gens, err := owner.EnrichmentContentGens(applRepo)
	require.NoError(t, err)
	require.Contains(t, gens, "test-go",
		"enrichment ran against the shadow; its state belongs to the store the shadow stands in front of")

	// Presence is not enough. observeContentGen has to resolve to the owner
	// too: read the counter off the shadow instead and the stamp clamps to
	// MIN(0, …) = 0, which leaves a row that reads "partial" forever — the
	// same user-visible symptom as the bug, from a different cause.
	current, err := owner.RepoContentGen(applRepo)
	require.NoError(t, err)
	require.Equal(t, current, gens["test-go"],
		"the pass must be stamped against the content the owner actually holds")
}

// The streaming path builds a chunk shadow over a disk target that is itself
// the mounted graph, so the walk has to keep going rather than unwrap once.
func TestNestedShadowsResolveToTheDurableStore(t *testing.T) {
	owner := applStore(t, "go")
	inner := shadowOver(owner, graph.StructuralPathShadowStreaming)
	outer := graph.NewStructuralIntegrityShadow(inner, applRepo, graph.StructuralPathShadowStreaming)

	require.Same(t, graph.Store(owner), durableStore(outer),
		"a shadow over a shadow still resolves to the one store that persists")
}

// A graph that stands alone owns nothing, so the walk must stop at it and the
// writers must go on failing closed. Resolving to something that merely accepts
// the calls would be worse than the bug: state would look recorded and be gone.
func TestAPlainGraphIsItsOwnDurableStoreAndRecordsNothing(t *testing.T) {
	standalone := graph.New()
	require.Same(t, graph.Store(standalone), durableStore(standalone))

	mgr := applManager(t, applProvider("test-go", "go", true))
	_, ok := mgr.applicabilityStore(standalone)
	require.False(t, ok,
		"the in-memory graph models no enrichment state; claiming otherwise would lose it silently")
}
