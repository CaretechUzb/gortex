package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestResolvedConversionPayloadFidelity characterizes the resolved-conversion
// update arm across the payload shapes the transport must not disturb: meta
// bytes with JSON metacharacters and unicode, absent meta, integral-valued
// confidence (REAL column, integer-looking number), NULL vs set terminal
// fields, and the kind-change variant. Storage classes are asserted with
// typeof() so a transport that silently retypes a column (TEXT meta, INTEGER
// confidence) fails here rather than in production forensics.
func TestResolvedConversionPayloadFidelity(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "fidelity.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const file = `r/pkg\caller.cs`
	mk := func(i int, to string) *graph.Edge {
		return &graph.Edge{
			From: file + "::Caller", To: to, Kind: graph.EdgeCalls,
			FilePath: file, Line: i, Confidence: 0.3, Origin: "heuristic",
		}
	}
	e1 := mk(1, graph.UnresolvedMarker+"Alpha")
	e2 := mk(2, graph.UnresolvedMarker+"Beta")
	e3 := mk(3, graph.UnresolvedMarker+"Gamma")
	store.AddBatch(nil, []*graph.Edge{e1, e2, e3})

	// e1: quoted/unicode meta, integral confidence, terminal stamp set false.
	e1.To = "r/pkg/impl.cs::Alpha"
	e1.Confidence = 1
	e1.ConfidenceLabel = "exact"
	e1.Origin = "resolved"
	e1.Meta = map[string]any{"note": `sa"ys → ☂`, "n": float64(2)}
	// e2: kind change rides the conversion; no meta; NULL terminal fields.
	e2.To = "r/pkg/impl.cs::Beta"
	e2.Kind = graph.EdgeReferences
	e2.Confidence = 0.9
	// e3: stays unresolved-shaped conversion partner for batch realism.
	e3.To = "r/pkg/impl.cs::Gamma"
	e3.Confidence = 0.75
	e3.CrossRepo = true

	store.ReindexEdges([]graph.EdgeReindex{
		{Edge: e1, OldTo: graph.UnresolvedMarker + "Alpha"},
		{Edge: e2, OldTo: graph.UnresolvedMarker + "Beta", OldKind: graph.EdgeCalls},
		{Edge: e3, OldTo: graph.UnresolvedMarker + "Gamma"},
	})

	type rowFacts struct {
		to, kind, origin       string
		confidence             float64
		confTypeof, metaTypeof string
		termTypeof             string
		crossRepo              int
		metaText               string
	}
	readRow := func(line int) rowFacts {
		var f rowFacts
		err := store.db.QueryRow(`SELECT to_id, kind, origin, confidence,
			typeof(confidence), typeof(meta), typeof(resolve_terminal),
			cross_repo, COALESCE(CAST(meta AS TEXT), '')
			FROM edges WHERE file_path = ? AND line = ?`, file, line).
			Scan(&f.to, &f.kind, &f.origin, &f.confidence, &f.confTypeof,
				&f.metaTypeof, &f.termTypeof, &f.crossRepo, &f.metaText)
		if err != nil {
			t.Fatalf("line %d: %v", line, err)
		}
		return f
	}

	r1 := readRow(1)
	if r1.to != "r/pkg/impl.cs::Alpha" || r1.origin != "resolved" || r1.confidence != 1 {
		t.Fatalf("row1 payload = %+v", r1)
	}
	if r1.confTypeof != "real" {
		t.Fatalf("row1 confidence typeof = %q, want real", r1.confTypeof)
	}
	if r1.metaTypeof != "blob" {
		t.Fatalf("row1 meta typeof = %q, want blob", r1.metaTypeof)
	}
	// Byte fidelity against the production encoder: meta is a compact binary
	// encoding, and the transport must deliver it bit-identical.
	_, blobMeta := extractPromotedEdgeMeta(e1.Meta)
	wantMeta, err := encodeMeta(blobMeta)
	if err != nil {
		t.Fatal(err)
	}
	if r1.metaText != string(wantMeta) {
		t.Fatalf("row1 meta bytes = %q, want %q", r1.metaText, wantMeta)
	}

	r2 := readRow(2)
	if r2.to != "r/pkg/impl.cs::Beta" || r2.kind != string(graph.EdgeReferences) {
		t.Fatalf("row2 kind-change conversion = %+v", r2)
	}
	if r2.termTypeof != "null" {
		t.Fatalf("row2 resolve_terminal typeof = %q, want null", r2.termTypeof)
	}
	if r2.metaTypeof != "null" {
		t.Fatalf("row2 meta typeof = %q, want null", r2.metaTypeof)
	}

	r3 := readRow(3)
	if r3.to != "r/pkg/impl.cs::Gamma" || r3.crossRepo != 1 || r3.confidence != 0.75 {
		t.Fatalf("row3 payload = %+v", r3)
	}
}

func containsStr(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
