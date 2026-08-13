package mcp

import (
	"fmt"
	"reflect"
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
