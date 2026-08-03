package indexer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func TestGeneratedTreeSitterParserProjectionStrictSmallFixture(t *testing.T) {
	valid := generatedParserTestSource("\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) {\n  return &language;\n}\n")
	if len(valid) > 256 {
		t.Fatalf("strict generated parser fixture = %d bytes; want a small regression fixture", len(valid))
	}
	if result, ok := generatedTreeSitterParserProjection("vendor/demo/src/parser.c", "c", valid); !ok || result == nil {
		t.Fatal("expected strict small generated parser fixture to be projected")
	}

	type projectionCase struct {
		name string
		path string
		lang string
		data []byte
	}
	cases := []projectionCase{
		{name: "wrong basename", path: "vendor/demo/src/parser.cc", lang: "c", data: valid},
		{name: "wrong language", path: "vendor/demo/src/parser.c", lang: "cpp", data: valid},
		{
			name: "missing valid public entry",
			path: "vendor/demo/src/parser.c",
			lang: "c",
			data: generatedParserTestSource("\nstatic const TSLanguage *tree_sitter_demo(void) { return &language; }\n"),
		},
	}
	markerNames := []string{"parser include", "language version", "state count"}
	for i, marker := range generatedTreeSitterParserMarkers {
		missingMarker := append([]byte(nil), valid...)
		markerOffset := bytes.Index(missingMarker, marker)
		if markerOffset < 0 {
			t.Fatalf("fixture is missing marker %q", marker)
		}
		missingMarker[markerOffset+len(marker)-1] = '!'
		cases = append(cases, projectionCase{
			name: "missing " + markerNames[i],
			path: "vendor/demo/src/parser.c",
			lang: "c",
			data: missingMarker,
		})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, ok := generatedTreeSitterParserProjection(tc.path, tc.lang, tc.data)
			if ok || result != nil {
				t.Fatalf("projection = %#v, %v; want fail-closed full extraction", result, ok)
			}
		})
	}
}

func TestGeneratedTreeSitterParserDetectorRequiresMarkersInBoundedHead(t *testing.T) {
	tail := "\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) { return &language; }\n"
	src := paddedGeneratedParserTestSource(tail, generatedTreeSitterParserHeadBytes+1024)
	head := src[:generatedTreeSitterParserHeadBytes]
	marker := generatedTreeSitterParserMarkers[len(generatedTreeSitterParserMarkers)-1]
	pos := bytes.Index(head, marker)
	if pos < 0 {
		t.Fatalf("fixture is missing marker %q", marker)
	}
	mutateAt := pos + len(marker) - 1
	src[mutateAt] = '!'
	copy(src[generatedTreeSitterParserHeadBytes+64:], marker)
	if isGeneratedTreeSitterParserTable("vendor/demo/src/parser.c", "c", src) {
		t.Error("marker outside the bounded source head must not trigger projection")
	}
}

func TestGeneratedTreeSitterParserProjectionRetainsPublicAPI(t *testing.T) {
	tail := "\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) {\n  const char *brace = \"}\";\n  if (brace != NULL) {\n    /* A generated nested initializer may contain { or }. */\n    return &language;\n  }\n  return &language;\n}\n"
	src := generatedParserTestSource(tail)
	result, ok := generatedTreeSitterParserProjection("vendor/demo/src/parser.c", "c", src)
	if !ok {
		t.Fatal("expected projection")
	}
	if len(result.Nodes) != 2 || len(result.Edges) != 2 {
		t.Fatalf("projection shape = %d nodes, %d edges; want 2 nodes, 2 edges", len(result.Nodes), len(result.Edges))
	}

	file := result.Nodes[0]
	if file.ID != "vendor/demo/src/parser.c" || file.Kind != graph.KindFile || file.Name != file.ID || file.FilePath != file.ID || file.Language != "c" {
		t.Fatalf("unexpected file node: %#v", file)
	}
	if file.StartLine != 1 || file.EndLine != bytes.Count(src, []byte{'\n'})+1 {
		t.Fatalf("file lines = %d..%d", file.StartLine, file.EndLine)
	}
	for key, want := range map[string]any{
		"generated":                             true,
		"codegen_tool":                          "tree-sitter",
		"skip_reason":                           "generated_parser_table",
		"skipped_due_to_generated_parser_table": true,
		"generated_parser_projection":           true,
		"generated_parser_projection_version":   generatedParserProjectionPolicyVersion,
		"generated_parser_internals_omitted":    true,
		"projection":                            "tree_sitter_public_entry",
		"source_bytes":                          len(src),
		"projected_symbol_count":                1,
	} {
		if got := file.Meta[key]; got != want {
			t.Errorf("file metadata %q = %#v; want %#v", key, got, want)
		}
	}

	fn := result.Nodes[1]
	entryOffset := bytes.Index(src, []byte("TS_PUBLIC"))
	wantStart := bytes.Count(src[:entryOffset], []byte{'\n'}) + 1
	closeOffset := bytes.LastIndexByte(src[entryOffset:], '}')
	wantEnd := wantStart + bytes.Count(src[entryOffset:entryOffset+closeOffset+1], []byte{'\n'})
	if fn.ID != "vendor/demo/src/parser.c::tree_sitter_demo" || fn.Kind != graph.KindFunction || fn.Name != "tree_sitter_demo" {
		t.Fatalf("unexpected function identity: %#v", fn)
	}
	if fn.FilePath != file.ID || fn.Language != "c" || fn.StartLine != wantStart || fn.EndLine != wantEnd {
		t.Fatalf("unexpected function location: %#v", fn)
	}
	if got := fn.Meta["signature"]; got != "TS_PUBLIC const TSLanguage *tree_sitter_demo(void)" {
		t.Errorf("signature = %#v", got)
	}
	if fn.Meta["generated"] != true || fn.Meta["codegen_tool"] != "tree-sitter" ||
		fn.Meta["generated_parser_projection"] != true ||
		metaInt(fn.Meta, generatedParserProjectionVersionMetaKey) != generatedParserProjectionPolicyVersion {
		t.Errorf("unexpected function metadata: %#v", fn.Meta)
	}
	retrieval := fn.RetrievalMetadata()
	if retrieval.Signature != "TS_PUBLIC const TSLanguage *tree_sitter_demo(void)" ||
		retrieval.QualName != "vendor.demo.src.parser.tree_sitter_demo" {
		t.Errorf("unexpected retrieval metadata: %#v", retrieval)
	}

	importEdge := result.Edges[0]
	if importEdge.From != file.ID || importEdge.To != "unresolved::import::tree_sitter/parser.h" ||
		importEdge.Kind != graph.EdgeImports || importEdge.FilePath != file.ID || importEdge.Line != 1 ||
		importEdge.Meta["include_kind"] != "quoted" {
		t.Fatalf("unexpected import edge: %#v", importEdge)
	}
	definesEdge := result.Edges[1]
	if definesEdge.From != file.ID || definesEdge.To != fn.ID || definesEdge.Kind != graph.EdgeDefines ||
		definesEdge.FilePath != file.ID || definesEdge.Line != wantStart {
		t.Fatalf("unexpected defines edge: %#v", definesEdge)
	}
}

func TestGeneratedTreeSitterParserProjectionAccountsForWholeFile(t *testing.T) {
	late := "\nTS_PUBLIC const TSLanguage *tree_sitter_late(void) { return &language; }\n"
	src := paddedGeneratedParserTestSource(late, generatedTreeSitterParserHeadBytes+(2<<20))
	early := []byte("\nextern const TSLanguage *tree_sitter_early(void) { return &language; }\n")
	earlyOffset := generatedTreeSitterParserHeadBytes + 1024
	copy(src[earlyOffset:], early)
	if earlyOffset >= len(src)-(1<<20) {
		t.Fatal("fixture entry must remain outside the former tail-only window")
	}

	result, ok := generatedTreeSitterParserProjection("vendor/demo/src/parser.c", "c", src)
	if !ok {
		t.Fatal("expected projection")
	}
	if len(result.Nodes) != 3 || len(result.Edges) != 3 {
		t.Fatalf("projection shape = %d nodes, %d edges; want 3 nodes, 3 edges", len(result.Nodes), len(result.Edges))
	}
	got := map[string]*graph.Node{}
	for _, node := range result.Nodes[1:] {
		got[node.Name] = node
	}
	if got["tree_sitter_early"] == nil || got["tree_sitter_late"] == nil {
		t.Fatalf("whole-file entries = %#v", got)
	}
	if got["tree_sitter_early"].StartLine >= got["tree_sitter_late"].StartLine {
		t.Fatalf("entry ordering/locations are wrong: early=%d late=%d", got["tree_sitter_early"].StartLine, got["tree_sitter_late"].StartLine)
	}
}

func TestGeneratedTreeSitterParserProjectionFailsClosedOnUncertainCandidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail string
	}{
		{
			name: "unexpected parameters",
			tail: "\nTS_PUBLIC const TSLanguage *tree_sitter_demo(int state) { return &language; }\n",
		},
		{
			name: "duplicate exported identity",
			tail: "\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) { return &language; }\n" +
				"extern const TSLanguage *tree_sitter_demo(void) { return &language; }\n",
		},
		{
			name: "oversized exported body",
			tail: "\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) {\n" +
				string(bytes.Repeat([]byte{' '}, generatedTreeSitterPublicBodyMaxBytes)) +
				"\nreturn &language;\n}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, ok := generatedTreeSitterParserProjection("vendor/demo/src/parser.c", "c", generatedParserTestSource(tc.tail))
			if ok || result != nil {
				t.Fatalf("projection = %#v, %v; want fail-closed full extraction", result, ok)
			}
		})
	}
}

func TestGeneratedTreeSitterParserProjectionAcceptsExternEntry(t *testing.T) {
	tail := "\nextern const TSLanguage *tree_sitter_lean(void) {\n  return &language;\n}\n"
	result, ok := generatedTreeSitterParserProjection("vendor/lean/src/parser.c", "c", generatedParserTestSource(tail))
	if !ok {
		t.Fatal("expected generated file projection")
	}
	if len(result.Nodes) != 2 || len(result.Edges) != 2 {
		t.Fatalf("projection shape = %d nodes, %d edges; want 2 nodes, 2 edges", len(result.Nodes), len(result.Edges))
	}
	fn := result.Nodes[1]
	if fn.ID != "vendor/lean/src/parser.c::tree_sitter_lean" || fn.Name != "tree_sitter_lean" {
		t.Fatalf("unexpected extern function identity: %#v", fn)
	}
	if got := fn.Meta["signature"]; got != "extern const TSLanguage *tree_sitter_lean(void)" {
		t.Fatalf("signature = %#v", got)
	}
}

func TestGeneratedTreeSitterParserProjectionFailsClosedWithoutPublicEntry(t *testing.T) {
	for _, tail := range []string{
		"\nstatic const TSLanguage *tree_sitter_demo(void) { return &language; }\n",
		"\n/* generated parser table without an exported entry */\n",
	} {
		result, ok := generatedTreeSitterParserProjection("parser.c", "c", generatedParserTestSource(tail))
		if ok || result != nil {
			t.Fatalf("projection = %#v, %v; want fail-closed normal extraction", result, ok)
		}
	}
}

func TestGeneratedParserProjectionPolicyInvalidatesUnchangedWarmFile(t *testing.T) {
	if got := extractorVersionsSnapshot()["c"]; got != generatedParserProjectionPolicyVersion {
		t.Fatalf("persisted C extractor version = %d; want %d", got, generatedParserProjectionPolicyVersion)
	}

	root := t.TempDir()
	path := filepath.Join(root, "parser.c")
	if err := os.WriteFile(path, []byte("int main(void) { return 0; }\n"), 0o600); err != nil {
		t.Fatalf("write parser fixture: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat parser fixture: %v", err)
	}

	registry := parser.NewRegistry()
	registry.Register(&generatedParserSpyExtractor{})
	g := graph.New()
	idx := New(g, registry, config.IndexConfig{}, zap.NewNop())
	idx.SetRepoPrefix("demo")
	idx.SetFileMtimes(map[string]int64{"parser.c": info.ModTime().UnixNano()})
	fileNode := &graph.Node{
		ID:         "demo/parser.c",
		Kind:       graph.KindFile,
		Name:       "parser.c",
		FilePath:   "demo/parser.c",
		Language:   "c",
		RepoPrefix: "demo",
		Meta: map[string]any{
			generatedParserProjectionMetaKey: true,
		},
	}
	g.AddNode(fileNode)

	changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
	if err != nil {
		t.Fatalf("changedSinceMtimesCensus: %v", err)
	}
	if detected != 1 || len(deleted) != 0 || len(changed) != 1 || changed[0] != "parser.c" {
		t.Fatalf("old policy census changed=%v deleted=%v detected=%d", changed, deleted, detected)
	}

	fileNode.Meta[generatedParserProjectionVersionMetaKey] = generatedParserProjectionPolicyVersion
	changed, deleted, detected, err = idx.changedSinceMtimesCensus(root)
	if err != nil {
		t.Fatalf("current policy census: %v", err)
	}
	if detected != 1 || len(deleted) != 0 || len(changed) != 0 {
		t.Fatalf("current policy census changed=%v deleted=%v detected=%d", changed, deleted, detected)
	}
}

func TestExtractFileBypassesExtractorForGeneratedParser(t *testing.T) {
	ext := &generatedParserSpyExtractor{}
	idx := &Indexer{config: config.IndexConfig{}, logger: zap.NewNop()}
	src := generatedParserTestSource("\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) { return &language; }\n")

	result, skipped, err := idx.extractFile(nil, nil, "/repo/parser.c", "parser.c", "c", ext, src)
	if err != nil {
		t.Fatalf("extractFile: %v", err)
	}
	if skipped || ext.calls != 0 {
		t.Fatalf("skipped=%v extractor calls=%d; want false, 0", skipped, ext.calls)
	}
	if result == nil || len(result.Nodes) != 2 {
		t.Fatalf("unexpected projection: %#v", result)
	}
}

func TestExtractFileGeneratedParserOptInUsesFullExtractor(t *testing.T) {
	ext := &generatedParserSpyExtractor{}
	idx := &Indexer{
		config: config.IndexConfig{IndexGeneratedParsers: true},
		logger: zap.NewNop(),
	}
	src := generatedParserTestSource("\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) { return &language; }\n")

	result, skipped, err := idx.extractFile(nil, nil, "/repo/parser.c", "parser.c", "c", ext, src)
	if err != nil {
		t.Fatalf("extractFile: %v", err)
	}
	if skipped || ext.calls != 1 {
		t.Fatalf("skipped=%v extractor calls=%d; want false, 1", skipped, ext.calls)
	}
	if result != nil {
		t.Fatalf("result = %#v; want spy's nil result", result)
	}
}

func TestGeneratedTreeSitterProjectionDoesNotRecordNativeParsePressure(t *testing.T) {
	projected, ok := generatedTreeSitterParserProjection(
		"parser.c",
		"c",
		generatedParserTestSource("\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) { return &language; }\n"),
	)
	if !ok {
		t.Fatal("expected projection")
	}
	if shouldRecordNativeParsePressure(projected) {
		t.Fatal("projection bypassed tree-sitter and must not contribute native parse bytes")
	}
	if !shouldRecordNativeParsePressure(quarantineResult("parser.c", "c", "panic")) {
		t.Fatal("panic/quarantine results may have allocated a native tree")
	}
}

func TestPrepareFileDeltaAcceptsGeneratedParserProjection(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "parser.c")
	src := generatedParserTestSource("\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) { return &language; }\n")
	if err := os.WriteFile(path, src, 0o600); err != nil {
		t.Fatalf("write parser fixture: %v", err)
	}

	ext := &generatedParserSpyExtractor{}
	registry := parser.NewRegistry()
	registry.Register(ext)
	idx := New(graph.New(), registry, config.IndexConfig{}, zap.NewNop())
	idx.rootPath = root

	probe, accepted, admissionBusy := idx.prepareFileDeltaWithAdmission(path, false)
	if admissionBusy || !accepted {
		t.Fatalf("prepared delta accepted=%v admission_busy=%v; want true, false", accepted, admissionBusy)
	}
	if ext.calls != 0 {
		t.Fatalf("full extractor calls = %d; want 0", ext.calls)
	}
	if probe.fingerprints.semantic == "" || probe.fingerprints.metadata == "" || probe.fingerprints.core == "" {
		t.Fatalf("incomplete graph fingerprints: %#v", probe.fingerprints)
	}
	if !probe.derived.complete() {
		t.Fatalf("incomplete derived fingerprints: %#v", probe.derived)
	}

	prepared, ok := idx.takePreparedSnapshot(path)
	if !ok {
		t.Fatal("successful projection was not retained as a prepared extraction")
	}
	defer prepared.release()
	if got := extractionDispositionFor(prepared.result); got != extractionDispositionGeneratedParserProjection {
		t.Fatalf("prepared disposition = %v; want generated-parser projection", got)
	}
	if stored := storedExtractionGraphFingerprints(prepared.result.Nodes); stored != probe.fingerprints {
		t.Fatalf("stored graph fingerprints = %#v; want %#v", stored, probe.fingerprints)
	}
	storedDerived := storedDerivedFingerprints(prepared.result.Nodes)
	if !storedDerived.complete() {
		t.Fatalf("stored derived fingerprints are incomplete: %#v", storedDerived)
	}
	plan := derivedPlanForDelta(
		storedDerived, probe.derived, false, "parser.c",
		prepared.result.Nodes, prepared.result.Nodes,
	)
	if plan.LegacyFallback || plan.Flags != 0 {
		t.Fatalf("unchanged projected delta requested broad invalidation: %#v", plan)
	}
}

type generatedParserSpyExtractor struct {
	calls int
}

func (e *generatedParserSpyExtractor) Language() string     { return "c" }
func (e *generatedParserSpyExtractor) Extensions() []string { return []string{".c"} }
func (e *generatedParserSpyExtractor) Extract(string, []byte) (*parser.ExtractionResult, error) {
	e.calls++
	return nil, nil
}

func generatedParserTestSource(tail string) []byte {
	header := []byte("#include \"tree_sitter/parser.h\"\n#define LANGUAGE_VERSION 14\n#define STATE_COUNT 321\n")
	src := make([]byte, 0, len(header)+len(tail))
	src = append(src, header...)
	return append(src, tail...)
}

func paddedGeneratedParserTestSource(tail string, size int) []byte {
	header := []byte("#include \"tree_sitter/parser.h\"\n#define LANGUAGE_VERSION 14\n#define STATE_COUNT 321\n")
	if size < len(header)+len(tail)+1 {
		size = len(header) + len(tail) + 1
	}
	src := bytes.Repeat([]byte{' '}, size)
	copy(src, header)
	tailStart := len(src) - len(tail)
	src[tailStart-1] = '\n'
	copy(src[tailStart:], tail)
	return src
}
