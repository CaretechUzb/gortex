package mcp

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type localizationDirectAdjacencyReader struct {
	graph.Reader
	nodes   map[string]*graph.Node
	batches [][]string
}

func (reader *localizationDirectAdjacencyReader) GetNodesByIDs(ids []string) map[string]*graph.Node {
	reader.batches = append(reader.batches, append([]string(nil), ids...))
	result := make(map[string]*graph.Node, len(ids))
	for _, id := range ids {
		if node, exists := reader.nodes[id]; exists {
			result[id] = node
		}
	}
	return result
}

func TestPromoteLocalizationDirectAdjacencyAuthenticatesBothDirections(t *testing.T) {
	reader := &localizationDirectAdjacencyReader{nodes: map[string]*graph.Node{
		"callee": nodeForDirectAdjacency("callee", "normalizeException", "src/formatter.php", 41),
		"caller": nodeForDirectAdjacency("caller", "normalize", "src/formatter.php", 20),
		"forged": nodeForDirectAdjacency("different-id", "forged", "src/forged.php", 1),
	}}
	envelope := localizationExploreEnvelope{Evidence: []localizationEvidence{{
		Rank: 1, ID: "owner", Name: "JsonFormatter", Kind: "class", File: "src/formatter.php", Line: 10,
		Callers: []string{"caller", "missing"}, Callees: []string{"callee", "forged"},
	}}}
	digest := newLocalizationEvidenceDigestForTask("normalize exception", envelope)

	promoted, _ := promoteLocalizationDirectAdjacency("normalize exception", envelope, reader, 3, 1<<20, digest)

	if len(reader.batches) != 1 {
		t.Fatalf("GetNodesByIDs calls = %d, want exactly one", len(reader.batches))
	}
	assertLocalizationEvidenceIDs(t, promoted.Evidence, []string{"owner", "callee", "caller"})
	for _, row := range promoted.Evidence[1:] {
		if row.Provenance != localizationProvenanceDirectAdjacency {
			t.Fatalf("provenance for %q = %q", row.ID, row.Provenance)
		}
	}
}

func TestPromoteLocalizationDirectAdjacencyIsBoundedAndDeterministic(t *testing.T) {
	nodes := make(map[string]*graph.Node)
	evidence := make([]localizationEvidence, 0, 4)
	for owner := 0; owner < 4; owner++ {
		row := localizationEvidence{Rank: owner + 1, ID: fmt.Sprintf("owner-%d", owner), Name: "owner", Kind: "function", File: "src/owner.go", Line: owner + 1}
		for related := 0; related < localizationMaxNeighborIDs; related++ {
			id := fmt.Sprintf("target-%02d", owner*localizationMaxNeighborIDs+related)
			row.Callees = append(row.Callees, id)
			nodes[id] = nodeForDirectAdjacency(id, id, "src/target.go", owner*10+related+1)
		}
		evidence = append(evidence, row)
	}
	envelope := localizationExploreEnvelope{Evidence: evidence}
	firstReader := &localizationDirectAdjacencyReader{nodes: nodes}
	secondReader := &localizationDirectAdjacencyReader{nodes: nodes}

	first, _ := promoteLocalizationDirectAdjacency("target", envelope, firstReader, len(evidence)+localizationDirectAdjacencyCap, 1<<20, newLocalizationEvidenceDigestForTask("target", envelope))
	second, _ := promoteLocalizationDirectAdjacency("target", envelope, secondReader, len(evidence)+localizationDirectAdjacencyCap, 1<<20, newLocalizationEvidenceDigestForTask("target", envelope))

	if got := len(first.Evidence) - len(evidence); got != localizationDirectAdjacencyCap {
		t.Fatalf("promoted rows = %d, want cap %d", got, localizationDirectAdjacencyCap)
	}
	if !reflect.DeepEqual(localizationEvidenceIDs(first.Evidence), localizationEvidenceIDs(second.Evidence)) {
		t.Fatalf("non-deterministic order: %v != %v", localizationEvidenceIDs(first.Evidence), localizationEvidenceIDs(second.Evidence))
	}
	if got := len(firstReader.batches[0]); got != 4*localizationMaxNeighborIDs {
		t.Fatalf("lookup IDs = %d, want bounded %d", got, 4*localizationMaxNeighborIDs)
	}
}

func TestPromoteLocalizationDirectAdjacencyNeverEscalatesAuthority(t *testing.T) {
	reader := &localizationDirectAdjacencyReader{nodes: map[string]*graph.Node{
		"target": nodeForDirectAdjacency("target", "criticalTarget", "src/target.go", 7),
	}}
	envelope := localizationExploreEnvelope{Evidence: []localizationEvidence{{
		Rank: 1, ID: "owner", Name: "owner", Kind: "function", File: "src/owner.go", Line: 1, Callees: []string{"target"},
	}}}
	digest := newLocalizationEvidenceDigestForTask("critical target", envelope)
	promoted, promotedDigest := promoteLocalizationDirectAdjacency("critical target", envelope, reader, 2, 1<<20, digest)
	if len(promoted.Evidence) != 2 {
		t.Fatalf("evidence = %d, want 2", len(promoted.Evidence))
	}
	for _, row := range localizationFinalResponseRows("critical target", nil, promotedDigest.Evidence) {
		if row.row.ID == "target" && row.primary {
			t.Fatal("direct adjacency evidence became primary authority")
		}
	}
}

func TestPromoteLocalizationDirectAdjacencyPreservesExactAndRoutePriorities(t *testing.T) {
	t.Run("exact symbol", func(t *testing.T) {
		evidence := make([]localizationEvidence, 0, 7)
		for index := 0; index < localizationFinalResponsePrimaryLimit; index++ {
			evidence = append(evidence, localizationEvidence{
				Rank: index + 1, ID: fmt.Sprintf("primary-%d", index), Name: "primary", Kind: "function", File: "src/primary.go", Line: index + 1,
			})
		}
		evidence[0].Callees = []string{"target"}
		evidence = append(evidence,
			localizationEvidence{Rank: 6, ID: "weak", Name: "weak", Kind: "function", File: "src/weak.go", Line: 1},
			localizationEvidence{Rank: 7, ID: "exact", Name: "exact", Kind: "function", File: "src/exact.go", Line: 1},
		)
		envelope := localizationExploreEnvelope{
			Completion: newLocalizationExactReadCompletion("exact", false),
			Evidence:   evidence,
		}
		reader := &localizationDirectAdjacencyReader{nodes: map[string]*graph.Node{
			"target": nodeForDirectAdjacency("target", "target", "src/target.go", 7),
		}}

		promoted, _ := promoteLocalizationDirectAdjacency(
			"target", envelope, reader, len(evidence), 1<<20,
			newLocalizationEvidenceDigestForTask("target", envelope),
		)

		assertLocalizationEvidenceIDs(t, promoted.Evidence, []string{
			"primary-0", "primary-1", "primary-2", "primary-3", "primary-4", "exact", "target",
		})
		if promoted.Completion.ExactSymbol != "exact" {
			t.Fatalf("exact symbol = %q, want preserved exact", promoted.Completion.ExactSymbol)
		}
	})

	t.Run("allowed symbols and route proofs", func(t *testing.T) {
		allowed := make([]string, localizationRefinementAllowedSymbolCap)
		evidence := make([]localizationEvidence, 0, len(allowed)+3)
		for index := range allowed {
			allowed[index] = fmt.Sprintf("allowed-%d", index)
			evidence = append(evidence, localizationEvidence{
				Rank: index + 1, ID: allowed[index], Name: allowed[index], Kind: "function", File: "src/allowed.go", Line: index + 1,
			})
		}
		evidence[0].Callees = []string{"target"}
		evidence[0].Provenance = localizationProvenanceImplementationRoute
		evidence[1].Provenance = localizationProvenanceImplementationTarget
		evidence = append(evidence,
			localizationEvidence{Rank: 9, ID: "weak", Name: "weak", Kind: "function", File: "src/weak.go", Line: 1},
			localizationEvidence{Rank: 10, ID: "implementation", Name: "implementation", Kind: "function", File: "src/implementation.go", Line: 1, Provenance: localizationProvenanceImplementationTarget},
			localizationEvidence{Rank: 11, ID: "proof", Name: "proof", Kind: "function", File: "src/proof.go", Line: 1, Provenance: localizationProvenanceImplementationRoute},
		)
		completion := newLocalizationRefinementCompletionForSymbols(allowed[0], allowed)
		completion.refinementRoutes = map[string]localizationRefinementRoute{
			allowed[0]: {implementationSymbol: "implementation"},
			allowed[1]: {proofSymbol: "proof"},
		}
		envelope := localizationExploreEnvelope{Completion: completion, Evidence: evidence}
		reader := &localizationDirectAdjacencyReader{nodes: map[string]*graph.Node{
			"target": nodeForDirectAdjacency("target", "target", "src/target.go", 7),
		}}

		promoted, _ := promoteLocalizationDirectAdjacency(
			"target", envelope, reader, len(evidence), 1<<20,
			newLocalizationEvidenceDigestForTask("target", envelope),
		)

		want := append(append([]string(nil), allowed...), "implementation", "proof", "target")
		assertLocalizationEvidenceIDs(t, promoted.Evidence, want)
		if !reflect.DeepEqual(promoted.Completion.AllowedSymbols, allowed) {
			t.Fatalf("allowed symbols = %v, want %v", promoted.Completion.AllowedSymbols, allowed)
		}
		if route := promoted.Completion.refinementRoutes[allowed[0]]; route.implementationSymbol != "implementation" {
			t.Fatalf("implementation route = %+v, want implementation proof retained", route)
		}
		if route := promoted.Completion.refinementRoutes[allowed[1]]; route.proofSymbol != "proof" {
			t.Fatalf("proof route = %+v, want proof retained", route)
		}
	})
}

func TestPromoteLocalizationDirectAdjacencyUsesBestDuplicateRelationContext(t *testing.T) {
	reader := &localizationDirectAdjacencyReader{nodes: map[string]*graph.Node{
		"shared":     nodeForDirectAdjacency("shared", "sharedTarget", "src/shared.go", 7),
		"competitor": nodeForDirectAdjacency("competitor", "competitorTarget", "src/competitor.go", 8),
	}}
	allowed := []string{"owner-0", "owner-1", "owner-2"}
	envelope := localizationExploreEnvelope{
		Completion: newLocalizationRefinementCompletionForSymbols(allowed[0], allowed),
		Evidence: []localizationEvidence{
			{Rank: 1, ID: allowed[0], Name: "owner0", Kind: "function", File: "src/other.go", Line: 1, Callees: []string{"shared"}},
			{Rank: 2, ID: allowed[1], Name: "owner1", Kind: "function", File: "src/shared.go", Line: 1, Callees: []string{"shared"}},
			{Rank: 3, ID: allowed[2], Name: "owner2", Kind: "function", File: "src/competitor.go", Line: 1, Callees: []string{"competitor"}},
		},
	}

	promoted, _ := promoteLocalizationDirectAdjacency(
		"target", envelope, reader, len(envelope.Evidence)+1, 1<<20,
		newLocalizationEvidenceDigestForTask("target", envelope),
	)

	assertLocalizationEvidenceIDs(t, promoted.Evidence, []string{"owner-0", "owner-1", "owner-2", "shared"})
	if len(reader.batches) != 1 || !reflect.DeepEqual(reader.batches[0], []string{"shared", "competitor"}) {
		t.Fatalf("deduplicated batch = %v, want [shared competitor]", reader.batches)
	}
}

func TestLocalizationDirectAdjacencyReplacementRenumbersEvidence(t *testing.T) {
	evidence := make([]localizationEvidence, 0, 8)
	for index := 0; index < localizationFinalResponsePrimaryLimit; index++ {
		evidence = append(evidence, localizationEvidence{
			Rank: index + 1, ID: fmt.Sprintf("primary-%d", index), Name: "primary", Kind: "function", File: "src/primary.go", Line: index + 1,
		})
	}
	evidence = append(evidence,
		localizationEvidence{Rank: 6, ID: "replaceable", Name: "replaceable", Kind: "function", File: "src/weak.go", Line: 1},
		localizationEvidence{Rank: 7, ID: "body", Name: "body", Kind: "function", File: "src/body.go", Line: 1, Provenance: localizationProvenanceBodyMention},
		localizationEvidence{Rank: 8, ID: "adjacent", Name: "adjacent", Kind: "function", File: "src/adjacent.go", Line: 1, Provenance: localizationProvenanceDirectAdjacency},
	)
	envelope := localizationExploreEnvelope{Evidence: evidence}
	candidate, admitted := localizationDirectAdjacencyEnvelopeWithRow(
		"target", envelope, newLocalizationEvidenceDigestForTask("target", envelope),
		localizationEvidence{ID: "target", Name: "target", Kind: "function", File: "src/target.go", Line: 1, Provenance: localizationProvenanceDirectAdjacency},
		len(evidence),
	)
	if !admitted {
		t.Fatal("replacement was not admitted")
	}
	for index, row := range candidate.Evidence {
		if row.Rank != index+1 {
			t.Fatalf("evidence[%d] rank = %d, want %d", index, row.Rank, index+1)
		}
	}
	assertLocalizationEvidenceIDs(t, candidate.Evidence, []string{
		"primary-0", "primary-1", "primary-2", "primary-3", "primary-4", "body", "adjacent", "target",
	})
}

func TestPromoteLocalizationDirectAdjacencyPromotesOneTaskCitedPrimary(t *testing.T) {
	task := "hydrateOnVisible waits before performHydrate checks connectivity; secondExplicit is supporting"
	primaryNames := []string{
		"getLeavingNodesForType", "hydrate", "moveTeleport", "isTeleportDisabled", "hydrateOnVisible",
	}
	evidence := make([]localizationEvidence, 0, localizationReplayEvidenceLimit)
	for index, name := range primaryNames {
		evidence = append(evidence, localizationEvidence{
			Rank: index + 1, ID: fmt.Sprintf("primary-%d", index), Name: name,
			Kind: "function", File: "src/primary.ts", Line: index + 1,
			primaryCohortOrder: index + 1,
		})
	}
	evidence[0].Callees = []string{"perform", "extra-0", "extra-1"}
	evidence[1].Callees = []string{"extra-2", "extra-3", "extra-4"}
	evidence[2].Callees = []string{"extra-5", "extra-6"}
	for len(evidence) < localizationReplayEvidenceLimit {
		index := len(evidence)
		evidence = append(evidence, localizationEvidence{
			Rank: index + 1, ID: fmt.Sprintf("weak-%d", index), Name: "ordinaryCandidate",
			Kind: "function", File: "src/weak.ts", Line: index + 1,
		})
	}
	envelope := localizationExploreEnvelope{Evidence: evidence}
	reader := &localizationDirectAdjacencyReader{nodes: map[string]*graph.Node{
		"perform": nodeForDirectAdjacency("perform", "performHydrate", "src/apiAsyncComponent.ts", 73),
		"extra-0": nodeForDirectAdjacency("extra-0", "secondExplicit", "src/extra.ts", 80),
		"extra-1": nodeForDirectAdjacency("extra-1", "ordinaryNeighbor1", "src/extra.ts", 81),
		"extra-2": nodeForDirectAdjacency("extra-2", "ordinaryNeighbor2", "src/extra.ts", 82),
		"extra-3": nodeForDirectAdjacency("extra-3", "ordinaryNeighbor3", "src/extra.ts", 83),
		"extra-4": nodeForDirectAdjacency("extra-4", "ordinaryNeighbor4", "src/extra.ts", 84),
		"extra-5": nodeForDirectAdjacency("extra-5", "ordinaryNeighbor5", "src/extra.ts", 85),
		"extra-6": nodeForDirectAdjacency("extra-6", "ordinaryNeighbor6", "src/extra.ts", 86),
	}}

	promoted, digest := promoteLocalizationDirectAdjacency(
		task, envelope, reader, len(evidence), 1<<20,
		newLocalizationEvidenceDigestForTask(task, envelope),
	)

	if len(reader.batches) != 1 {
		t.Fatalf("GetNodesByIDs calls = %d, want exactly one", len(reader.batches))
	}
	if len(promoted.Evidence) != localizationReplayEvidenceLimit {
		t.Fatalf("evidence rows = %d, want %d", len(promoted.Evidence), localizationReplayEvidenceLimit)
	}
	wantPrimary := []string{"primary-0", "primary-1", "primary-2", "perform", "primary-4"}
	if got := localizationFinalResponsePrimaryIDs(task, nil, digest.Evidence); !reflect.DeepEqual(got, wantPrimary) {
		t.Fatalf("primary IDs = %v, want %v", got, wantPrimary)
	}
	rebuilt := newLocalizationEvidenceDigestForTask(task, promoted)
	if got := localizationFinalResponsePrimaryIDs(task, nil, rebuilt.Evidence); !reflect.DeepEqual(got, wantPrimary) {
		t.Fatalf("rebuilt primary IDs = %v, want %v", got, wantPrimary)
	}
	rows := cloneLocalizationDigestRows(rebuilt.Evidence)
	for {
		var removed bool
		rows, removed = shedLocalizationDigestSupportingOnly(rows)
		if !removed {
			break
		}
	}
	if got := localizationFinalResponsePrimaryIDs(task, nil, rows); !reflect.DeepEqual(got, wantPrimary) {
		t.Fatalf("primary IDs after supporting-row pressure = %v, want %v", got, wantPrimary)
	}
}

func TestPromoteLocalizationDirectAdjacencyReplacesWeakPrimaryOnTightPage(t *testing.T) {
	task := "hydrateOnVisible then performHydrate"
	evidence := make([]localizationEvidence, 0, localizationFinalResponsePrimaryLimit)
	for index, name := range []string{"weakZero", "weakOne", "weakTwo", "weakThree", "hydrateOnVisible"} {
		evidence = append(evidence, localizationEvidence{
			Rank: index + 1, ID: fmt.Sprintf("primary-%d", index), Name: name,
			Kind: "function", File: "src/primary.ts", Line: index + 1,
			primaryCohortOrder: index + 1,
		})
	}
	evidence[0].Callees = []string{"perform"}
	envelope := localizationExploreEnvelope{Evidence: evidence}
	reader := &localizationDirectAdjacencyReader{nodes: map[string]*graph.Node{
		"perform": nodeForDirectAdjacency("perform", "performHydrate", "src/apiAsyncComponent.ts", 73),
	}}

	promoted, digest := promoteLocalizationDirectAdjacency(
		task, envelope, reader, len(evidence), 1<<20,
		newLocalizationEvidenceDigestForTask(task, envelope),
	)

	assertLocalizationEvidenceIDs(t, promoted.Evidence, []string{"primary-0", "primary-1", "primary-2", "primary-4", "perform"})
	wantPrimary := []string{"primary-0", "primary-1", "primary-2", "perform", "primary-4"}
	if got := localizationFinalResponsePrimaryIDs(task, nil, digest.Evidence); !reflect.DeepEqual(got, wantPrimary) {
		t.Fatalf("tight-page primary IDs = %v, want %v", got, wantPrimary)
	}
}

func TestPromoteLocalizationDirectAdjacencyDoesNotReplaceProtectedPrimary(t *testing.T) {
	task := "KeepZero KeepOne KeepTwo KeepThree KeepFour performHydrate"
	primaryNames := []string{"KeepZero", "KeepOne", "KeepTwo", "KeepThree", "KeepFour"}
	evidence := make([]localizationEvidence, 0, localizationFinalResponsePrimaryLimit+1)
	for index, name := range primaryNames {
		evidence = append(evidence, localizationEvidence{
			Rank: index + 1, ID: fmt.Sprintf("primary-%d", index), Name: name,
			Kind: "function", File: "src/primary.ts", Line: index + 1,
			primaryCohortOrder: index + 1,
		})
	}
	evidence[0].Callees = []string{"perform"}
	evidence = append(evidence, localizationEvidence{
		Rank: 6, ID: "weak", Name: "ordinaryCandidate", Kind: "function", File: "src/weak.ts", Line: 1,
	})
	envelope := localizationExploreEnvelope{Evidence: evidence}
	reader := &localizationDirectAdjacencyReader{nodes: map[string]*graph.Node{
		"perform": nodeForDirectAdjacency("perform", "performHydrate", "src/apiAsyncComponent.ts", 73),
	}}

	promoted, digest := promoteLocalizationDirectAdjacency(
		task, envelope, reader, len(evidence), 1<<20,
		newLocalizationEvidenceDigestForTask(task, envelope),
	)

	wantPrimary := []string{"primary-0", "primary-1", "primary-2", "primary-3", "primary-4"}
	if got := localizationFinalResponsePrimaryIDs(task, nil, digest.Evidence); !reflect.DeepEqual(got, wantPrimary) {
		t.Fatalf("primary IDs = %v, want protected cohort %v", got, wantPrimary)
	}
	wantEvidence := append(append([]string(nil), wantPrimary...), "perform")
	if got := localizationEvidenceIDs(promoted.Evidence); !reflect.DeepEqual(got, wantEvidence) {
		t.Fatalf("evidence IDs = %v, want task-cited adjacency retained as supporting %v", got, wantEvidence)
	}
	for _, row := range localizationFinalResponseRows(task, nil, digest.Evidence) {
		if row.row.ID == "perform" && row.primary {
			t.Fatal("task-cited adjacency displaced an all-protected primary cohort")
		}
	}
}

func TestPromoteLocalizationDirectAdjacencyPreservesTaskCitedInitialEvidence(t *testing.T) {
	task := "change FMT_FORMAT_AS for custom allocator support"
	evidence := make([]localizationEvidence, 0, localizationReplayEvidenceLimit)
	nodes := make(map[string]*graph.Node, localizationDirectAdjacencyCap)
	for index := 0; index < localizationFinalResponsePrimaryLimit; index++ {
		row := localizationEvidence{
			Rank: index + 1, ID: fmt.Sprintf("primary-%d", index), Name: "primaryCandidate",
			Kind: "function", File: "include/fmt/base.h", Line: index + 1,
			primaryCohortOrder: index + 1,
		}
		if index < 3 {
			for related := 0; related < localizationMaxNeighborIDs; related++ {
				id := fmt.Sprintf("adjacent-%d-%d", index, related)
				row.Callees = append(row.Callees, id)
				nodes[id] = nodeForDirectAdjacency(id, "unrelatedNeighbor", "include/fmt/other.h", index*10+related+1)
			}
		}
		evidence = append(evidence, row)
	}
	evidence = append(evidence, localizationEvidence{
		Rank: 6, ID: "macro", Name: "FMT_FORMAT_AS", Kind: "macro", File: "include/fmt/format.h", Line: 4009,
	})
	for len(evidence) < localizationReplayEvidenceLimit {
		index := len(evidence)
		evidence = append(evidence, localizationEvidence{
			Rank: index + 1, ID: fmt.Sprintf("weak-%d", index), Name: "ordinaryCandidate",
			Kind: "function", File: "include/fmt/base.h", Line: index + 1,
		})
	}
	envelope := localizationExploreEnvelope{Evidence: evidence}
	reader := &localizationDirectAdjacencyReader{nodes: nodes}

	promoted, _ := promoteLocalizationDirectAdjacency(
		task, envelope, reader, len(evidence), 1<<20,
		newLocalizationEvidenceDigestForTask(task, envelope),
	)

	if got := len(promoted.Evidence); got != localizationReplayEvidenceLimit {
		t.Fatalf("evidence rows = %d, want %d", got, localizationReplayEvidenceLimit)
	}
	promotedCount, macroPresent := 0, false
	for _, row := range promoted.Evidence {
		if row.Provenance == localizationProvenanceDirectAdjacency {
			promotedCount++
		}
		if row.ID == "macro" {
			macroPresent = true
		}
	}
	if promotedCount != localizationDirectAdjacencyCap {
		t.Fatalf("promoted adjacency rows = %d, want %d", promotedCount, localizationDirectAdjacencyCap)
	}
	if !macroPresent {
		t.Fatal("task-cited FMT_FORMAT_AS evidence was evicted")
	}
}

func TestPromoteLocalizationDirectAdjacencyRejectsOverBudgetRow(t *testing.T) {
	reader := &localizationDirectAdjacencyReader{nodes: map[string]*graph.Node{
		"target": nodeForDirectAdjacency("target", "target", "src/target.go", 7),
	}}
	envelope := localizationExploreEnvelope{Evidence: []localizationEvidence{{
		Rank: 1, ID: "owner", Name: "owner", Kind: "function", File: "src/owner.go", Line: 1, Callees: []string{"target"},
	}}}
	digest := newLocalizationEvidenceDigestForTask("target", envelope)
	promoted, _ := promoteLocalizationDirectAdjacency("target", envelope, reader, 2, 1, digest)
	assertLocalizationEvidenceIDs(t, promoted.Evidence, []string{"owner"})
}

func TestPromoteLocalizationDirectAdjacencyReservesTaskCitedSlot(t *testing.T) {
	task := "alpha beta gamma calls performHydrate"
	primaryNames := []string{"firstPrimary", "secondPrimary", "thirdPrimary", "weakPrimary", "fifthPrimary"}
	evidence := make([]localizationEvidence, 0, localizationReplayEvidenceLimit)
	for index, name := range primaryNames {
		evidence = append(evidence, localizationEvidence{
			Rank: index + 1, ID: fmt.Sprintf("primary-%d", index), Name: name,
			Kind: "function", File: "src/primary.ts", Line: index + 1,
			primaryCohortOrder: index + 1,
		})
	}
	reader := &localizationDirectAdjacencyReader{nodes: make(map[string]*graph.Node)}
	for index := 0; index < localizationDirectAdjacencyCap; index++ {
		id := fmt.Sprintf("ordinary-%d", index)
		ownerIndex := index / localizationMaxNeighborIDs
		evidence[ownerIndex].Callees = append(evidence[ownerIndex].Callees, id)
		reader.nodes[id] = nodeForDirectAdjacency(id, fmt.Sprintf("ordinaryNeighbor%d", index), "src/ordinary.ts", 20+index)
		reader.nodes[id].Meta = map[string]any{"signature": "alpha beta gamma"}
	}
	evidence[2].Callees = append(evidence[2].Callees, "perform")
	reader.nodes["perform"] = nodeForDirectAdjacency("perform", "performHydrate", "src/apiAsyncComponent.ts", 73)
	for len(evidence) < localizationReplayEvidenceLimit {
		index := len(evidence)
		evidence = append(evidence, localizationEvidence{
			Rank: index + 1, ID: fmt.Sprintf("weak-%d", index), Name: "weakCandidate",
			Kind: "function", File: "src/weak.ts", Line: index + 1,
		})
	}
	envelope := localizationExploreEnvelope{Evidence: evidence}

	promoted, digest := promoteLocalizationDirectAdjacency(
		task, envelope, reader, len(evidence), 1<<20,
		newLocalizationEvidenceDigestForTask(task, envelope),
	)

	if got := promoted.Evidence[len(promoted.Evidence)-1].ID; got != "perform" {
		t.Fatalf("last admitted adjacency = %q, want reserved task-cited perform; evidence=%v", got, localizationEvidenceIDs(promoted.Evidence))
	}
	wantPrimary := []string{"primary-0", "primary-1", "primary-2", "primary-3", "perform"}
	if got := localizationFinalResponsePrimaryIDs(task, nil, digest.Evidence); !reflect.DeepEqual(got, wantPrimary) {
		t.Fatalf("primary IDs = %v, want %v", got, wantPrimary)
	}
}

func TestPromoteLocalizationDirectAdjacencyRequiresConcreteIdentifierCitation(t *testing.T) {
	tests := []struct {
		name      string
		task      string
		candidate *graph.Node
	}{
		{
			name: "identifier suffix", task: "performHydrateLater handles the request",
			candidate: nodeForDirectAdjacency("candidate", "performHydrate", "src/candidate.ts", 7),
		},
		{
			name: "ordinary prose word", task: "the target handles the request",
			candidate: nodeForDirectAdjacency("candidate", "target", "src/candidate.ts", 7),
		},
		{
			name: "file and signature only", task: "inspect src/candidate.ts and func performHydrate()",
			candidate: &graph.Node{
				ID: "candidate", Name: "unrelatedConcrete", Kind: graph.KindFunction,
				FilePath: "src/candidate.ts", StartLine: 7,
				Meta: map[string]any{"signature": "func performHydrate()"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := make([]localizationEvidence, 0, localizationFinalResponsePrimaryLimit)
			for index := 0; index < localizationFinalResponsePrimaryLimit; index++ {
				evidence = append(evidence, localizationEvidence{
					Rank: index + 1, ID: fmt.Sprintf("primary-%d", index), Name: fmt.Sprintf("primaryName%d", index),
					Kind: "function", File: "src/primary.ts", Line: index + 1,
					primaryCohortOrder: index + 1,
				})
			}
			evidence[0].Callees = []string{"candidate"}
			envelope := localizationExploreEnvelope{Evidence: evidence}
			reader := &localizationDirectAdjacencyReader{nodes: map[string]*graph.Node{"candidate": test.candidate}}
			promoted, digest := promoteLocalizationDirectAdjacency(
				test.task, envelope, reader, len(evidence), 1<<20,
				newLocalizationEvidenceDigestForTask(test.task, envelope),
			)
			want := []string{"primary-0", "primary-1", "primary-2", "primary-3", "primary-4"}
			if got := localizationEvidenceIDs(promoted.Evidence); !reflect.DeepEqual(got, want) {
				t.Fatalf("evidence IDs = %v, want %v", got, want)
			}
			if got := localizationFinalResponsePrimaryIDs(test.task, nil, digest.Evidence); !reflect.DeepEqual(got, want) {
				t.Fatalf("primary IDs = %v, want %v", got, want)
			}
		})
	}
}

func TestTaskCitedAdjacencyPrimarySurvivesDigestPressureAndMerge(t *testing.T) {
	task := "performHydrate handles the request"
	evidence := []localizationEvidence{
		{Rank: 1, ID: "primary-0", Name: "firstPrimary", Kind: "function", File: "src/primary.ts", Line: 1, primaryCohortOrder: 1},
		{Rank: 2, ID: "primary-1", Name: "secondPrimary", Kind: "function", File: "src/primary.ts", Line: 2, primaryCohortOrder: 2},
		{Rank: 3, ID: "primary-2", Name: "thirdPrimary", Kind: "function", File: "src/primary.ts", Line: 3, primaryCohortOrder: 3},
		{Rank: 4, ID: "primary-4", Name: "fifthPrimary", Kind: "function", File: "src/primary.ts", Line: 5, primaryCohortOrder: 5},
	}
	for index := 0; index < localizationReplayEvidenceLimit-len(evidence)-1; index++ {
		evidence = append(evidence, localizationEvidence{
			Rank: len(evidence) + 1,
			ID:   fmt.Sprintf("ordinary-%02d-%s", index, strings.Repeat("x", 700)),
			Name: "ordinaryCandidate", Kind: "function",
			File: fmt.Sprintf("src/%s/%02d.ts", strings.Repeat("y", 700), index), Line: index + 10,
		})
	}
	evidence = append(evidence, localizationEvidence{
		Rank: len(evidence) + 1, ID: "perform", Name: "performHydrate", Kind: "function",
		File: "src/apiAsyncComponent.ts", Line: 73,
		Provenance:               localizationProvenanceDirectAdjacency,
		taskCitedPrimaryEligible: true, primaryCohortOrder: 4,
	})
	digest := newLocalizationEvidenceDigestForTask(task, localizationExploreEnvelope{Evidence: evidence})
	wantPrimary := []string{"primary-0", "primary-1", "primary-2", "perform", "primary-4"}
	if len(digest.Evidence) >= len(evidence) {
		t.Fatalf("byte pressure retained %d rows, want fewer than %d", len(digest.Evidence), len(evidence))
	}
	if len(digest.Evidence) == 0 || digest.Evidence[0].ID != "perform" {
		t.Fatalf("digest first row = %#v, want task-cited primary perform", digest.Evidence)
	}
	if !reflect.DeepEqual(digest.primaryIDs, wantPrimary) {
		t.Fatalf("digest primary IDs = %v, want %v", digest.primaryIDs, wantPrimary)
	}
	merged := mergeLocalizationEvidenceDigestForTask(task, nil, digest)
	if got := localizationFinalResponsePrimaryIDs(task, nil, merged.Evidence); !reflect.DeepEqual(got, wantPrimary) {
		t.Fatalf("merged primary IDs = %v, want %v", got, wantPrimary)
	}
}

func nodeForDirectAdjacency(id, name, file string, line int) *graph.Node {
	return &graph.Node{ID: id, Name: name, QualName: name, Kind: graph.KindFunction, FilePath: file, StartLine: line, EndLine: line + 1}
}

func localizationEvidenceIDs(rows []localizationEvidence) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func assertLocalizationEvidenceIDs(t *testing.T, rows []localizationEvidence, want []string) {
	t.Helper()
	if got := localizationEvidenceIDs(rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("evidence IDs = %v, want %v", got, want)
	}
}
