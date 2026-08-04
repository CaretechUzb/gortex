package store_sqlite

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// flatRoundTrip encodes via the flat codec only (asserting the fast path was
// taken) and decodes back through decodeMeta.
func flatRoundTrip(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	b, ok := encodeMetaFast(in)
	if !ok {
		t.Fatalf("encodeMetaFast bailed on a modelled map: %#v", in)
	}
	if !isFlatMeta(b) {
		t.Fatalf("encodeMetaFast did not stamp the flat magic: %q", b)
	}
	out, err := decodeMetaFast(b)
	if err != nil {
		t.Fatalf("decodeMetaFast: %v", err)
	}
	return out
}

// TestFlatCodecEveryValueType round-trips one value of every type the codec
// models and asserts exact Go-type fidelity end to end.
func TestFlatCodecEveryValueType(t *testing.T) {
	shape := &contracts.Shape{
		Kind:   "struct",
		Fields: []contracts.ShapeField{{Name: "id", Type: "int64", Required: true}},
		Notes:  []string{"partial"},
	}
	in := map[string]any{
		"str":       "hello",
		"unicode":   "héllo – мир – 世界 – 🚀",
		"bool_t":    true,
		"bool_f":    false,
		"int":       -7,
		"int64":     int64(1700000000),
		"float":     0.875,
		"float_int": 2.0, // integral float must stay float64
		"strs":      []string{"a", "b", "c"},
		"nested":    map[string]any{"inner": 5, "rate": 1.5, "deep": map[string]any{"x": int64(9)}},
		"map_slice": []map[string]any{{"k": "v", "n": 1}, {"k": "w", "n": 2}},
		"any_slice": []any{"x", 3, true, 4.5},
		"shape":     shape,
		"nilval":    nil,
		"empty_map": map[string]any{},
	}
	got := flatRoundTrip(t, in)

	if !reflect.DeepEqual(got, in) {
		t.Fatalf("flat round-trip mismatch:\n got: %#v\nwant: %#v", got, in)
	}

	// Spot-check the load-bearing concrete types explicitly.
	mustType[string](t, got, "str")
	mustType[bool](t, got, "bool_t")
	mustType[int](t, got, "int")
	mustType[int64](t, got, "int64")
	mustType[float64](t, got, "float")
	mustType[float64](t, got, "float_int")
	mustType[[]string](t, got, "strs")
	mustType[map[string]any](t, got, "nested")
	mustType[[]map[string]any](t, got, "map_slice")
	mustType[[]any](t, got, "any_slice")
	mustType[*contracts.Shape](t, got, "shape")
	if got["nilval"] != nil {
		t.Errorf("nilval: want nil, got %#v", got["nilval"])
	}
}

// TestFlatCodecLargeValues exercises long keys and values that cross the
// single-byte varint boundary (> 127 bytes), proving the length prefixes
// round-trip.
func TestFlatCodecLargeValues(t *testing.T) {
	big := string(bytes.Repeat([]byte("x"), 5000))
	bigKey := string(bytes.Repeat([]byte("k"), 300))
	in := map[string]any{
		bigKey:  big,
		"slice": []string{big, "", big},
	}
	got := flatRoundTrip(t, in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("large-value round-trip mismatch")
	}
}

// TestFlatCodecDeterministic proves the encoding is byte-stable across
// encodes (keys are sorted), which matters for any content-hash / dedup.
func TestFlatCodecDeterministic(t *testing.T) {
	in := map[string]any{
		"z": 1, "a": "x", "m": []string{"p", "q"},
		"nested": map[string]any{"d": 4, "b": 2, "c": 3},
	}
	var prev []byte
	for i := 0; i < 16; i++ {
		b, ok := encodeMetaFast(in)
		if !ok {
			t.Fatalf("encodeMetaFast bailed")
		}
		if prev != nil && !bytes.Equal(prev, b) {
			t.Fatalf("encoding is not deterministic across encodes")
		}
		prev = b
	}
}

// TestEncodeMetaFallbackToJSON: a value whose type the flat codec does not
// model makes encodeMeta fall back to JSON (leading '{'), and decodeMeta
// still reads it. No data is dropped.
func TestEncodeMetaFallbackToJSON(t *testing.T) {
	// []int is deliberately outside the modelled type set. uint64 is now an
	// exact fast-codec type because reach generations must survive reloads.
	in := map[string]any{"weird": []int{42}, "name": "keep"}

	if _, ok := encodeMetaFast(in); ok {
		t.Fatal("encodeMetaFast should bail on an unmodelled value type")
	}

	b, err := encodeMeta(in)
	if err != nil {
		t.Fatalf("encodeMeta: %v", err)
	}
	if isFlatMeta(b) {
		t.Fatalf("encodeMeta should have fallen back to JSON, got a flat blob")
	}
	if !isJSONObject(b) {
		t.Fatalf("encodeMeta fallback did not produce a JSON object: %q", b)
	}
	got, err := decodeMeta(b)
	if err != nil {
		t.Fatalf("decodeMeta(json fallback): %v", err)
	}
	// The exotic tail may widen through JSON, but the string survives and no
	// row is lost.
	if got["name"] != "keep" {
		t.Errorf("name not preserved through JSON fallback: %#v", got["name"])
	}
	if _, ok := got["weird"]; !ok {
		t.Errorf("weird key dropped by JSON fallback")
	}
}

// TestDecodeLegacyJSON proves rows written by the previous JSON encoder still
// decode (routed through metaWire for exact types) after the flat-codec
// switch — existing on-disk databases must keep loading.
func TestDecodeLegacyJSON(t *testing.T) {
	orig := map[string]any{
		"visibility":       "private",
		"complexity":       9,
		"confidence":       1.0, // integral float — metaWire must keep it float64
		"path_param_names": []string{"id"},
		"last_authored":    map[string]any{"timestamp": int64(1700000000)},
	}
	blob, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if isFlatMeta(blob) {
		t.Fatalf("JSON blob unexpectedly looks like a flat blob")
	}
	got, err := decodeMeta(blob)
	if err != nil {
		t.Fatalf("decodeMeta(json): %v", err)
	}
	mustType[string](t, got, "visibility")
	mustType[int](t, got, "complexity")
	mustType[float64](t, got, "confidence")
	mustType[[]string](t, got, "path_param_names")
	la, ok := got["last_authored"].(map[string]any)
	if !ok {
		t.Fatalf("last_authored: want map[string]any, got %T", got["last_authored"])
	}
	mustType[int64](t, la, "timestamp")
}

// TestDecodeLegacyJSON_CSharpVisibilityKeys: every using stamp the C#
// visibility rules read must survive the JSON decode path as []string —
// a []any here silently erases the file's visibility for the resolver.
func TestDecodeLegacyJSON_CSharpVisibilityKeys(t *testing.T) {
	orig := map[string]any{
		"usings":              []string{"LibA"},
		"global_usings":       []string{"LibB"},
		"using_static":        []string{"System.Math"},
		"scoped_usings":       []string{"A|W", "|X"},
		"global_using_static": []string{"LibA.E1"},
	}
	blob, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	got, err := decodeMeta(blob)
	if err != nil {
		t.Fatalf("decodeMeta(json): %v", err)
	}
	for k := range orig {
		mustType[[]string](t, got, k)
	}
}

// TestDecodeMetaFastMalformed: corrupt / truncated flat blobs return an error
// rather than panicking — a single bad row must not crash a store scan.
func TestDecodeMetaFastMalformed(t *testing.T) {
	good, ok := encodeMetaFast(map[string]any{"k": "value", "n": 7, "s": []string{"a", "b"}})
	if !ok {
		t.Fatalf("encodeMetaFast bailed")
	}
	cases := map[string][]byte{
		"magic only":         {metaFlatMagic0, metaFlatVersion},
		"count then nothing": {metaFlatMagic0, metaFlatVersion, 0x05},
		"truncated mid-blob": good[:len(good)-3],
		"unknown value tag":  {metaFlatMagic0, metaFlatVersion, 0x01, 0x01, 'k', 0x7E},
		"giant key length":   {metaFlatMagic0, metaFlatVersion, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0x0F},
	}
	for name, blob := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := decodeMeta(blob)
			if err == nil {
				t.Errorf("expected an error for %q, got nil", name)
			}
		})
	}
}

// TestStoreReloadMetaFidelity is the wired-path proof: persist a node and an
// edge with rich Meta through the real store, reopen it (warm restart), and
// assert Meta is byte-for-byte type-identical. Also runs PRAGMA
// integrity_check.
func TestStoreReloadMetaFidelity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	nodeMeta := map[string]any{
		"complexity":        7,
		"loop_depth":        2,
		"confidence":        0.875,
		"coverage_pct":      1.0, // integral float
		"candidate_count":   2,
		"path_param_names":  []string{"id", "org"},
		"status_codes":      []string{"200", "404"},
		"churn":             map[string]any{"commit_count": 12, "churn_rate": 2.0, "last_author": "a@b.c"},
		"last_authored":     map[string]any{"timestamp": int64(1700000000), "email": "x@y.z"},
		"response_envelope": []map[string]any{{"name": "data", "n": 1}},
		"shape": &contracts.Shape{
			Kind:   "struct",
			Fields: []contracts.ShapeField{{Name: "id", Type: "int64", Required: true}},
			Notes:  []string{"partial"},
		},
		"unicode_doc":  "héllo 世界 🚀",
		"is_generated": false,
	}
	edgeMeta := map[string]any{
		"candidate_count": 3,
		"similarity":      0.5,
		"score":           1.0, // integral float
		"count":           5,
		"clone_tokens":    128,
		"synthesized_by":  "grpc",
		"arg_names":       []string{"ctx", "req"},
	}

	func() {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer s.Close()
		s.AddNode(&graph.Node{ID: "n1", Kind: "function", Name: "Foo", FilePath: "f.go", Meta: cloneMeta(nodeMeta)})
		s.AddNode(&graph.Node{ID: "n2", Kind: "function", Name: "Bar", FilePath: "f.go"})
		s.AddEdge(&graph.Edge{From: "n1", To: "n2", Kind: "calls", FilePath: "f.go", Line: 10, Meta: cloneMeta(edgeMeta)})
	}()

	// Reopen — the warm-restart path that reads every blob back.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	n := s.GetNode("n1")
	if n == nil {
		t.Fatal("GetNode(n1) = nil after reload")
	}
	if !reflect.DeepEqual(n.Meta, nodeMeta) {
		t.Fatalf("node Meta mismatch after reload:\n got: %#v\nwant: %#v", n.Meta, nodeMeta)
	}

	edges := s.GetOutEdges("n1")
	var got *graph.Edge
	for _, e := range edges {
		if e.To == "n2" && e.Kind == "calls" {
			got = e
			break
		}
	}
	if got == nil {
		t.Fatalf("edge n1->n2 not found after reload (got %d edges)", len(edges))
	}
	if !reflect.DeepEqual(got.Meta, edgeMeta) {
		t.Fatalf("edge Meta mismatch after reload:\n got: %#v\nwant: %#v", got.Meta, edgeMeta)
	}

	var res string
	if err := s.db.QueryRow(`PRAGMA integrity_check`).Scan(&res); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if res != "ok" {
		t.Fatalf("integrity_check = %q, want ok", res)
	}
}

func cloneMeta(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func mustType[T any](t *testing.T, m map[string]any, key string) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("%s: missing from decoded map", key)
		return
	}
	if _, ok := v.(T); !ok {
		var zero T
		t.Errorf("%s: want type %T, got %T (value %v)", key, zero, v, v)
	}
}

// -- benchmarks -----------------------------------------------------------

var metaSink any

// benchMetaSample is a representative node/edge meta map restricted to the
// types gob auto-registers (scalars + []string), so all three encoders run on
// identical input for an apples-to-apples comparison. Shape / nested-map /
// map-slice values also ride the flat path (see the round-trip tests); they
// are omitted here only because gob refuses unregistered interface types.
func benchMetaSample() map[string]any {
	return map[string]any{
		"complexity":       7,
		"loop_depth":       2,
		"parse_errors":     0,
		"position":         3,
		"line":             42,
		"confidence":       0.875,
		"coverage_pct":     83.5,
		"candidate_count":  2,
		"count":            5,
		"clone_tokens":     128,
		"timestamp":        int64(1700000000),
		"path_param_names": []string{"id", "org"},
		"status_codes":     []string{"200", "404"},
		"signature":        "func F(ctx context.Context, x int) (T, error)",
		"some_plugin_flag": "go_linkname",
		"is_generated":     false,
		"synthesized_by":   "grpc",
	}
}

func BenchmarkEncodeMetaGob(b *testing.B) {
	m := benchMetaSample()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(m); err != nil {
			b.Fatalf("gob encode: %v", err)
		}
		metaSink = buf.Bytes()
	}
}

func BenchmarkEncodeMetaJSON(b *testing.B) {
	m := benchMetaSample()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := json.Marshal(m)
		if err != nil {
			b.Fatalf("json marshal: %v", err)
		}
		metaSink = out
	}
}

func BenchmarkEncodeMetaFlat(b *testing.B) {
	m := benchMetaSample()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, ok := encodeMetaFast(m)
		if !ok {
			b.Fatal("encodeMetaFast bailed")
		}
		metaSink = out
	}
}

func BenchmarkDecodeMetaGob(b *testing.B) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(benchMetaSample()); err != nil {
		b.Fatalf("gob encode: %v", err)
	}
	blob := buf.Bytes()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, err := decodeMeta(blob)
		if err != nil {
			b.Fatalf("decodeMeta: %v", err)
		}
		metaSink = m
	}
}

func BenchmarkDecodeMetaJSON(b *testing.B) {
	blob, err := json.Marshal(benchMetaSample())
	if err != nil {
		b.Fatalf("json marshal: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, err := decodeMeta(blob)
		if err != nil {
			b.Fatalf("decodeMeta: %v", err)
		}
		metaSink = m
	}
}

func BenchmarkDecodeMetaFlat(b *testing.B) {
	blob, ok := encodeMetaFast(benchMetaSample())
	if !ok {
		b.Fatal("encodeMetaFast bailed")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, err := decodeMeta(blob)
		if err != nil {
			b.Fatalf("decodeMeta: %v", err)
		}
		metaSink = m
	}
}
