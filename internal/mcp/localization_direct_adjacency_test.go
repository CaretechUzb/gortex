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
