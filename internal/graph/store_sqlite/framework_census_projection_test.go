package store_sqlite

import (
	"bytes"
	"encoding/gob"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestDecodeFrameworkCensusMetaFormatsAndMasks(t *testing.T) {
	t.Parallel()
	meta := map[string]any{
		"via":                   "grpc.stub",
		"registry_value":        "value",
		"grpc_register_service": "Greeter",
		"grpc_service":          "Greeter",
		"grpc_method":           "SayHello",
		"express_handler_ref":   nil,
		"recv_const":            "User",
		"receiver_expr":         "factory()",
		"rust_path":             "crate::run",
		"rust_recv":             "self",
		"receiver_type":         "Service",
		"rust_recv_expr":        "self.inner",
		"synthesized_by":        "rust-scope",
		"unrelated":             []string{"a", "b"},
	}
	flat, err := encodeMeta(meta)
	if err != nil {
		t.Fatal(err)
	}
	jsonMeta := make(map[string]any, len(meta)+1)
	for key, value := range meta {
		jsonMeta[key] = value
	}
	jsonMeta["unsupported"] = []int{1, 2}
	jsonBlob, err := encodeMeta(jsonMeta)
	if err != nil {
		t.Fatal(err)
	}
	if isFlatMeta(jsonBlob) {
		t.Fatal("unsupported value did not force JSON fallback")
	}
	var gobBuf bytes.Buffer
	gobMeta := make(map[string]any, len(meta))
	for key, value := range meta {
		if value != nil {
			gobMeta[key] = value
		}
	}
	gobMeta["express_handler_ref"] = false
	if err := gob.NewEncoder(&gobBuf).Encode(gobMeta); err != nil {
		t.Fatal(err)
	}

	want := graph.FrameworkCensusEdge{
		Via: "grpc.stub", RegistryValue: "value",
		GRPCRegisterService: "Greeter", GRPCService: "Greeter", GRPCMethod: "SayHello",
		HasExpressHandlerRef: true, RecvConst: "User", ReceiverExpr: "factory()",
		RustPath: "crate::run", RustRecv: "self", ReceiverType: "Service", RustRecvExpr: "self.inner",
	}
	for name, blob := range map[string][]byte{
		"flat": flat,
		"json": jsonBlob,
		"gob":  gobBuf.Bytes(),
	} {
		t.Run(name, func(t *testing.T) {
			var got graph.FrameworkCensusEdge
			if err := decodeFrameworkCensusMeta(graph.EdgeCalls, blob, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("decoded %#v, want %#v", got, want)
			}
		})
	}

	var reference graph.FrameworkCensusEdge
	if err := decodeFrameworkCensusMeta(graph.EdgeReferences, flat, &reference); err != nil {
		t.Fatal(err)
	}
	if reference.Via != "grpc.stub" || reference.ReceiverExpr != "factory()" || reference.SynthesizedBy != "rust-scope" {
		t.Fatalf("reference projection lost required fields: %#v", reference)
	}
	reference.Via = ""
	reference.ReceiverExpr = ""
	reference.SynthesizedBy = ""
	if reference != (graph.FrameworkCensusEdge{}) {
		t.Fatalf("reference projection retained unrelated fields: %#v", reference)
	}

	var imported graph.FrameworkCensusEdge
	if err := decodeFrameworkCensusMeta(graph.EdgeImports, flat, &imported); err != nil {
		t.Fatal(err)
	}
	if imported.SynthesizedBy != "rust-scope" {
		t.Fatalf("import projection lost synthesized_by: %#v", imported)
	}
	imported.SynthesizedBy = ""
	if imported != (graph.FrameworkCensusEdge{}) {
		t.Fatalf("import projection retained unrelated fields: %#v", imported)
	}

	wrongTypes, err := encodeMeta(map[string]any{
		"via": 7, "receiver_expr": false, "express_handler_ref": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	var wrong graph.FrameworkCensusEdge
	if err := decodeFrameworkCensusMeta(graph.EdgeCalls, wrongTypes, &wrong); err != nil {
		t.Fatal(err)
	}
	if wrong.Via != "" || wrong.ReceiverExpr != "" || !wrong.HasExpressHandlerRef {
		t.Fatalf("wrongly typed selected values leaked through: %#v", wrong)
	}
	if len(flat) < 2 {
		t.Fatal("flat fixture unexpectedly short")
	}
	if err := decodeFrameworkCensusMeta(graph.EdgeCalls, flat[:len(flat)-1], &graph.FrameworkCensusEdge{}); err == nil {
		t.Fatal("truncated flat metadata was accepted")
	}
}

func TestFrameworkCensusEdgesSeqPreservesKindOrderAndClosesCursor(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.AddBatch(nil, []*graph.Edge{
		{From: "call-a", To: "unresolved::A", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1, Meta: map[string]any{"via": "grpc.stub", "ignored": strings.Repeat("x", 1024)}},
		{From: "ref-a", To: "unresolved::R", Kind: graph.EdgeReferences, FilePath: "r.go", Line: 2, Meta: map[string]any{"via": "fnptr.reg", "receiver_expr": "make()", "receiver_type": "drop", "synthesized_by": "rust-scope"}},
		{From: "call-b", To: "unresolved::B", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 3, Meta: map[string]any{"recv_const": "User"}},
		{From: "annotation", To: "java::WorkflowInterface", Kind: graph.EdgeAnnotated, FilePath: "w.java", Line: 4, Meta: map[string]any{"ignored": strings.Repeat("y", 1024)}},
		{From: "import-a", To: "pkg", Kind: graph.EdgeImports, FilePath: "i.go", Line: 5, Meta: map[string]any{"via": "drop", "synthesized_by": "rails"}},
	})

	var got []graph.FrameworkCensusEdge
	for row := range s.FrameworkCensusEdgesSeq(graph.EdgeReferences, graph.EdgeImports, graph.EdgeCalls) {
		got = append(got, row)
	}
	wantIDs := []graph.EdgeIdentity{
		{From: "ref-a", To: "unresolved::R", Kind: graph.EdgeReferences, FilePath: "r.go", Line: 2},
		{From: "import-a", To: "pkg", Kind: graph.EdgeImports, FilePath: "i.go", Line: 5},
		{From: "call-a", To: "unresolved::A", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1},
		{From: "call-b", To: "unresolved::B", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 3},
	}
	if len(got) != len(wantIDs) {
		t.Fatalf("rows=%d, want %d", len(got), len(wantIDs))
	}
	for i := range got {
		if got[i].EdgeIdentity != wantIDs[i] {
			t.Fatalf("row %d identity=%#v, want %#v", i, got[i].EdgeIdentity, wantIDs[i])
		}
	}
	if got[0].Via != "fnptr.reg" || got[0].ReceiverExpr != "make()" || got[0].ReceiverType != "" || got[0].SynthesizedBy != "rust-scope" {
		t.Fatalf("reference mask mismatch: %#v", got[0])
	}
	if got[1].SynthesizedBy != "rails" || got[1].Via != "" {
		t.Fatalf("import mask mismatch: %#v", got[1])
	}
	if got[2].Via != "grpc.stub" || got[3].RecvConst != "User" {
		t.Fatalf("call projection mismatch: %#v", got)
	}

	for range s.FrameworkCensusEdgesSeq(graph.EdgeCalls) {
		break
	}
	s.AddEdge(&graph.Edge{From: "after", To: "unresolved::after", Kind: graph.EdgeCalls})
	found := false
	for _, edge := range s.GetOutEdges("after") {
		if edge != nil && edge.To == "unresolved::after" && edge.Kind == graph.EdgeCalls {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("early-stop cursor was not closed before the following write")
	}
}

func TestFrameworkCensusEdgesSeqUsesKindIndexWithoutTempSort(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rows, err := s.db.Query(`EXPLAIN QUERY PLAN
SELECT from_id, to_id, file_path, line, meta
FROM edges INDEXED BY edges_by_kind
WHERE kind = ?
ORDER BY id`, string(graph.EdgeCalls))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, "edges_by_kind") {
		t.Fatalf("plan does not use edges_by_kind:\n%s", plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("plan sorts the complete kind range:\n%s", plan)
	}
}
