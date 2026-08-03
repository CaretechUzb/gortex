package indexer

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

type metadataFixtureExtractor struct {
	result *parser.ExtractionResult
}

func (e metadataFixtureExtractor) Language() string     { return "rust" }
func (e metadataFixtureExtractor) Extensions() []string { return []string{".rs"} }
func (e metadataFixtureExtractor) Extract(string, []byte) (*parser.ExtractionResult, error) {
	return e.result, nil
}

func TestNormalizeExtractionMetadataFileOnlyDoesNotCopySource(t *testing.T) {
	small := []byte("{}")
	large := bytes.Repeat([]byte("file-only payload\n"), 1<<20)
	run := func(src []byte) {
		node := &graph.Node{ID: "data.json", Kind: graph.KindFile, Name: "data.json"}
		normalizeExtractionMetadata(&parser.ExtractionResult{Nodes: []*graph.Node{node}}, src)
		if got := node.RetrievalMetadata(); got != (graph.RetrievalMetadata{}) {
			t.Fatalf("file metadata = %#v", got)
		}
	}

	smallAllocs := testing.AllocsPerRun(5, func() { run(small) })
	largeAllocs := testing.AllocsPerRun(5, func() { run(large) })
	if largeAllocs > smallAllocs+1 {
		t.Fatalf("file-only normalization allocations scale with source size: small=%v large=%v", smallAllocs, largeAllocs)
	}
}

func BenchmarkNormalizeExtractionMetadataFileOnly(b *testing.B) {
	src := bytes.Repeat([]byte("file-only payload\n"), 1<<20)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		node := &graph.Node{ID: "data.json", Kind: graph.KindFile, Name: "data.json"}
		normalizeExtractionMetadata(&parser.ExtractionResult{Nodes: []*graph.Node{node}}, src)
	}
}

func TestNormalizeExtractionMetadataBoundsHugeDeclarationLine(t *testing.T) {
	src := append([]byte("var key = "), bytes.Repeat([]byte("value"), 1<<20)...)
	node := &graph.Node{
		ID: "data.json::key", Kind: graph.KindVariable, Name: "key",
		FilePath: "data.json", StartLine: 1, EndLine: 1, Language: "json",
	}
	normalizeExtractionMetadata(&parser.ExtractionResult{Nodes: []*graph.Node{node}}, src)

	got := node.RetrievalMetadata().Signature
	if !strings.HasPrefix(got, "var key = value") || len(got) > 512 {
		t.Fatalf("signature len=%d value=%q", len(got), got)
	}
	before := got
	src[4] = 'X'
	if after := node.RetrievalMetadata().Signature; after != before {
		t.Fatalf("bounded metadata aliases source: before=%q after=%q", before, after)
	}
}

func BenchmarkNormalizeExtractionMetadataHugeDeclaration(b *testing.B) {
	src := append([]byte("var key = "), bytes.Repeat([]byte("value"), 1<<20)...)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		node := &graph.Node{
			ID: "data.json::key", Kind: graph.KindVariable, Name: "key",
			FilePath: "data.json", StartLine: 1, EndLine: 1, Language: "json",
		}
		normalizeExtractionMetadata(&parser.ExtractionResult{Nodes: []*graph.Node{node}}, src)
	}
}

func TestExtractFileNormalizesMetadataAtSharedBoundary(t *testing.T) {
	src := []byte("fn resolve(value: Input) -> Output { value.into() }\n")
	n := &graph.Node{
		ID: "src/lib.rs::resolve", Kind: graph.KindFunction, Name: "resolve",
		FilePath: "src/lib.rs", StartLine: 1, EndLine: 1, Language: "rust",
		Meta: map[string]any{"signature": "fn resolve(...)"},
	}
	idx := &Indexer{}
	result, skipped, err := idx.extractFile(nil, nil, "src/lib.rs", "src/lib.rs", "rust", metadataFixtureExtractor{
		result: &parser.ExtractionResult{Nodes: []*graph.Node{n}},
	}, src)
	if err != nil || skipped {
		t.Fatalf("extractFile() err = %v, skipped = %v", err, skipped)
	}
	if got := result.Nodes[0].RetrievalMetadata().Signature; got != "fn resolve(value: Input) -> Output" {
		t.Fatalf("search signature = %q", got)
	}
}

func TestNormalizeExtractionMetadataRustMethod(t *testing.T) {
	src := []byte("impl Worker {\n    /// Runs a queued job.\n    pub fn run<T>(\n        &self,\n        item: T,\n    ) -> Result<(), Error> {\n    }\n}\n")
	owner := &graph.Node{ID: "src/worker.rs::Worker", Name: "Worker", FilePath: "src/worker.rs", StartLine: 1, Language: "rust"}
	method := &graph.Node{
		ID: "src/worker.rs::Worker.run", Kind: graph.KindMethod, Name: "run", QualName: "Worker::run",
		FilePath: "src/worker.rs", StartLine: 3, EndLine: 7, Language: "rust",
		Meta: map[string]any{"signature": "fn run(...)", "receiver": "Worker", "doc": "/// Runs a queued job."},
	}
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{owner, method},
		Edges: []*graph.Edge{{From: method.ID, To: owner.ID, Kind: graph.EdgeMemberOf}},
	}

	normalizeExtractionMetadata(result, src)

	if got := method.Meta["signature"]; got != "fn run(...)" {
		t.Fatalf("parser signature mutated: %v", got)
	}
	if method.QualName != "Worker::run" {
		t.Fatalf("resolver QualName mutated: %q", method.QualName)
	}
	retrieval := method.RetrievalMetadata()
	if retrieval.Signature != "pub fn run<T>( &self, item: T, ) -> Result<(), Error>" {
		t.Fatalf("search signature = %q", retrieval.Signature)
	}
	if retrieval.QualName != "worker::Worker::run" {
		t.Fatalf("search qualifier = %q", retrieval.QualName)
	}
	if retrieval.Doc != "Runs a queued job." {
		t.Fatalf("search doc = %q", retrieval.Doc)
	}
}

func TestNormalizeExtractionMetadataTypeScriptFallbacks(t *testing.T) {
	src := []byte("/**\n * Validates an incoming request.\n */\nexport async function validate(\n  input: Request,\n): Promise<Result> {\n  return check(input)\n}\n")
	n := &graph.Node{
		ID: "src/auth/index.ts::validate", Kind: graph.KindFunction, Name: "validate",
		FilePath: "src/auth/index.ts", StartLine: 4, EndLine: 8, Language: "typescript",
		Meta: map[string]any{"signature": "function validate()"},
	}

	normalizeExtractionMetadata(&parser.ExtractionResult{Nodes: []*graph.Node{n}}, src)

	retrieval := n.RetrievalMetadata()
	if retrieval.Signature != "export async function validate( input: Request, ): Promise<Result>" {
		t.Fatalf("search signature = %q", retrieval.Signature)
	}
	if retrieval.Doc != "Validates an incoming request." {
		t.Fatalf("search doc = %q", retrieval.Doc)
	}
	if retrieval.QualName != "src.auth.validate" {
		t.Fatalf("search qualifier = %q", retrieval.QualName)
	}
}

func TestNormalizeExtractionMetadataPreservesExplicitQualName(t *testing.T) {
	n := &graph.Node{
		ID: "service.go::Service.Handle", Kind: graph.KindMethod, Name: "Handle",
		QualName: "example.Service.Handle", FilePath: "service.go", StartLine: 1,
		Meta: map[string]any{"signature": "func (s *Service) Handle(ctx context.Context)", "doc": "  Handles   requests.  "},
	}

	normalizeExtractionMetadata(&parser.ExtractionResult{Nodes: []*graph.Node{n}}, nil)

	if n.QualName != "example.Service.Handle" {
		t.Fatalf("QualName mutated: %q", n.QualName)
	}
	retrieval := n.RetrievalMetadata()
	if retrieval.QualName != n.QualName {
		t.Fatalf("search qualifier = %q", retrieval.QualName)
	}
	if retrieval.Doc != "Handles requests." {
		t.Fatalf("search doc = %q", retrieval.Doc)
	}
}

func TestNormalizeExtractionMetadataDoesNotCopyOwnerTextIntoParam(t *testing.T) {
	src := []byte("/// Resolves an input value.\nfn resolve(value: Input) -> Output { value.into() }\n")
	fn := &graph.Node{
		ID: "src/lib.rs::resolve", Kind: graph.KindFunction, Name: "resolve",
		FilePath: "src/lib.rs", StartLine: 2, EndLine: 2, Language: "rust",
		Meta: map[string]any{"signature": "fn resolve(...)"},
	}
	param := &graph.Node{
		ID: fn.ID + "#param:value", Kind: graph.KindParam, Name: "value",
		FilePath: fn.FilePath, StartLine: fn.StartLine, EndLine: fn.EndLine, Language: fn.Language,
	}

	normalizeExtractionMetadata(&parser.ExtractionResult{Nodes: []*graph.Node{fn, param}}, src)

	owner := fn.RetrievalMetadata()
	if owner.Signature != "fn resolve(value: Input) -> Output" || owner.Doc != "Resolves an input value." {
		t.Fatalf("owner metadata = %#v", owner)
	}
	child := param.RetrievalMetadata()
	if child.Signature != "" || child.Doc != "" || child.QualName != "" {
		t.Fatalf("parameter inherited owner metadata: %#v", child)
	}
	fields := searchIndexFields(param, "")
	if len(fields) != 5 || fields[2] != "" || fields[3] != "" || fields[4] != "" {
		t.Fatalf("parameter search fields contain owner payload: %#v", fields)
	}
	if joined := strings.Join(fields, " "); strings.Contains(joined, "resolve") || strings.Contains(joined, "Resolves") {
		t.Fatalf("parameter duplicated enclosing declaration: %q", joined)
	}
}

func TestShouldNormalizeDefinitionMetadata(t *testing.T) {
	allowed := []graph.NodeKind{
		graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface,
		graph.KindVariable, graph.KindField, graph.KindClosure, graph.KindConstant,
		graph.KindEnumMember, graph.KindMacro,
	}
	for _, kind := range allowed {
		if !shouldNormalizeDefinitionMetadata(kind) {
			t.Errorf("definition kind %q rejected", kind)
		}
	}
	denied := []graph.NodeKind{
		graph.KindParam, graph.KindLocal, graph.KindImport, graph.KindBuiltin,
		graph.KindFile, graph.KindPackage, graph.KindGenericParam, graph.KindContract,
		graph.KindModule, graph.KindDoc, graph.KindEvent, graph.KindString,
	}
	for _, kind := range denied {
		if shouldNormalizeDefinitionMetadata(kind) {
			t.Errorf("non-definition kind %q accepted", kind)
		}
	}
}

func TestNormalizeExtractionMetadataAcrossLanguages(t *testing.T) {
	tests := []struct {
		name      string
		language  string
		filePath  string
		kind      graph.NodeKind
		symbol    string
		source    string
		startLine int
		endLine   int
		wantQual  string
		wantSig   string
		wantDoc   string
	}{
		{
			name: "go", language: "go", filePath: "internal/api/service.go", kind: graph.KindFunction, symbol: "Serve",
			source: "// Serves requests.\nfunc Serve(ctx context.Context) error { return nil }\n", startLine: 2, endLine: 2,
			wantQual: "internal.api.Serve", wantSig: "func Serve(ctx context.Context) error", wantDoc: "Serves requests.",
		},
		{
			name: "java", language: "java", filePath: "src/main/java/com/acme/Builder.java", kind: graph.KindType, symbol: "Builder",
			source: "/** Builds values. */\n@Deprecated\npublic final class Builder {\n}\n", startLine: 3, endLine: 4,
			wantQual: "src.main.java.com.acme.Builder", wantSig: "public final class Builder", wantDoc: "Builds values.",
		},
		{
			name: "python", language: "python", filePath: "pkg/io.py", kind: graph.KindFunction, symbol: "load",
			source: "# Loads values.\ndef load(value: str) -> Result:\n    return Result(value)\n", startLine: 2, endLine: 3,
			wantQual: "pkg.io.load", wantSig: "def load(value: str) -> Result:", wantDoc: "Loads values.",
		},
		{
			name: "typescript", language: "typescript", filePath: "src/api/users.ts", kind: graph.KindFunction, symbol: "loadUsers",
			source: "/** Loads users. */\nexport function loadUsers(id: ID): User {\n  return users[id]\n}\n", startLine: 2, endLine: 4,
			wantQual: "src.api.users.loadUsers", wantSig: "export function loadUsers(id: ID): User", wantDoc: "Loads users.",
		},
		{
			name: "rust", language: "rust", filePath: "crates/alpha/src/parser.rs", kind: graph.KindFunction, symbol: "parse",
			source: "/// Parses values.\npub fn parse(input: &str) -> Value {\n    todo!()\n}\n", startLine: 2, endLine: 4,
			wantQual: "crates::alpha::parser::parse", wantSig: "pub fn parse(input: &str) -> Value", wantDoc: "Parses values.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := &graph.Node{
				ID: tt.filePath + "::" + tt.symbol, Kind: tt.kind, Name: tt.symbol,
				FilePath: tt.filePath, StartLine: tt.startLine, EndLine: tt.endLine, Language: tt.language,
			}
			normalizeExtractionMetadata(&parser.ExtractionResult{Nodes: []*graph.Node{n}}, []byte(tt.source))
			got := n.RetrievalMetadata()
			if got.QualName != tt.wantQual || got.Signature != tt.wantSig || got.Doc != tt.wantDoc {
				t.Fatalf("metadata = %#v, want qual=%q signature=%q doc=%q", got, tt.wantQual, tt.wantSig, tt.wantDoc)
			}
			if n.QualName != "" {
				t.Fatalf("parser QualName mutated: %q", n.QualName)
			}
		})
	}
}

func TestNormalizeExtractionMetadataRustDocsThroughAttributes(t *testing.T) {
	src := []byte("/// Base docs.\n#[cfg(\n    feature = \"fast\",\n)]\n#[inline]\n#[doc = \"Attribute docs.\"]\n#[doc = r#\"Raw docs.\"#]\npub fn run(value: Input) -> Output { value.into() }\n")
	n := &graph.Node{
		ID: "src/feature.rs::run", Kind: graph.KindFunction, Name: "run",
		FilePath: "src/feature.rs", StartLine: 8, EndLine: 8, Language: "rust",
	}
	normalizeExtractionMetadata(&parser.ExtractionResult{Nodes: []*graph.Node{n}}, src)

	got := n.RetrievalMetadata()
	if got.Doc != "Base docs. Attribute docs. Raw docs." {
		t.Fatalf("doc = %q", got.Doc)
	}
	if got.QualName != "feature::run" {
		t.Fatalf("qualifier = %q", got.QualName)
	}
}

func TestNormalizeExtractionMetadataDisambiguatesRustModules(t *testing.T) {
	alpha := &graph.Node{
		ID: "crates/alpha/src/lib.rs::parse", Kind: graph.KindFunction, Name: "parse",
		FilePath: "crates/alpha/src/lib.rs", Language: "rust", Meta: map[string]any{"signature": "fn parse()", "doc": "Parses alpha."},
	}
	beta := &graph.Node{
		ID: "crates/beta/src/lib.rs::parse", Kind: graph.KindFunction, Name: "parse",
		FilePath: "crates/beta/src/lib.rs", Language: "rust", Meta: map[string]any{"signature": "fn parse()", "doc": "Parses beta."},
	}
	normalizeExtractionMetadata(&parser.ExtractionResult{Nodes: []*graph.Node{alpha, beta}}, nil)

	if got := alpha.RetrievalMetadata().QualName; got != "crates::alpha::parse" {
		t.Fatalf("alpha qualifier = %q", got)
	}
	if got := beta.RetrievalMetadata().QualName; got != "crates::beta::parse" {
		t.Fatalf("beta qualifier = %q", got)
	}
	if alpha.QualName != "" || beta.QualName != "" {
		t.Fatalf("parser qualifiers mutated: alpha=%q beta=%q", alpha.QualName, beta.QualName)
	}
}

func TestDeclarationSignaturePrefersColumnSpan(t *testing.T) {
	line := "ignored prefix fn exact(value: Input) -> Output { body() } trailing"
	start := strings.Index(line, "fn exact")
	end := strings.Index(line, " { body")
	n := &graph.Node{
		Kind: graph.KindFunction, Name: "exact", StartLine: 1, EndLine: 1,
		StartColumn: start, EndColumn: end,
	}
	if got := declarationSignature([]string{line}, n); got != "fn exact(value: Input) -> Output" {
		t.Fatalf("signature = %q", got)
	}
}

func TestNormalizeExtractionMetadataSanitizesParserSignature(t *testing.T) {
	n := &graph.Node{
		ID: "src/lib.rs::run", Kind: graph.KindFunction, Name: "run", Language: "rust",
		Meta: map[string]any{"signature": "fn run(value: Input) -> Output { let INLINE_SECRET = \"token\"; } " + strings.Repeat("x", 700)},
	}
	normalizeExtractionMetadata(&parser.ExtractionResult{Nodes: []*graph.Node{n}}, nil)

	got := n.RetrievalMetadata().Signature
	if got != "fn run(value: Input) -> Output" || strings.Contains(got, "INLINE_SECRET") || len(got) > 512 {
		t.Fatalf("signature not compact: len=%d value=%q", len(got), got)
	}
	if !strings.Contains(n.Meta["signature"].(string), "INLINE_SECRET") {
		t.Fatalf("parser signature mutated: %q", n.Meta["signature"])
	}
}

func TestDeclarationSignatureLanguageAwareBodyBoundary(t *testing.T) {
	t.Run("go receive-only channel", func(t *testing.T) {
		line := "func relay(dst chan<- string, src <-chan string) { INLINE_SECRET := token }"
		n := &graph.Node{Kind: graph.KindFunction, Name: "relay", Language: "go", StartLine: 1, EndLine: 1}
		got := declarationSignature([]string{line}, n)
		if got != "func relay(dst chan<- string, src <-chan string)" || strings.Contains(got, "INLINE_SECRET") {
			t.Fatalf("signature = %q", got)
		}
	})
	t.Run("rust lifetimes", func(t *testing.T) {
		source := "pub fn borrow<'a, T>(\n    value: &'a T,\n) -> impl for<'b> Trait<'b>\nwhere\n    T: 'a,\n{\n    let INLINE_SECRET = token;\n}\n"
		n := &graph.Node{Kind: graph.KindFunction, Name: "borrow", Language: "rust", StartLine: 1, EndLine: 8}
		got := declarationSignature(strings.Split(source, "\n"), n)
		want := "pub fn borrow<'a, T>( value: &'a T, ) -> impl for<'b> Trait<'b> where T: 'a,"
		if got != want || strings.Contains(got, "INLINE_SECRET") {
			t.Fatalf("signature = %q, want %q", got, want)
		}
	})
}

func TestNormalizeExtractionMetadataRustQualifierOverlap(t *testing.T) {
	method := &graph.Node{
		ID: "crates/alpha/src/parser.rs::Type.method", Kind: graph.KindMethod, Name: "method",
		QualName: "crate::parser::Type::method", FilePath: "crates/alpha/src/parser.rs", Language: "rust",
		Meta: map[string]any{"signature": "fn method(&self)", "doc": "Runs."},
	}
	normalizeExtractionMetadata(&parser.ExtractionResult{Nodes: []*graph.Node{method}}, nil)
	if got := method.RetrievalMetadata().QualName; got != "crates::alpha::parser::Type::method" {
		t.Fatalf("qualifier = %q", got)
	}
	if method.QualName != "crate::parser::Type::method" {
		t.Fatalf("parser qualifier mutated: %q", method.QualName)
	}
}

func TestNormalizeExtractionMetadataAvoidsDefaultFileStemDuplication(t *testing.T) {
	n := &graph.Node{
		ID: "pkg/Foo.widget::Foo", Kind: graph.KindType, Name: "Foo",
		FilePath: "pkg/Foo.widget", Language: "widget", Meta: map[string]any{"signature": "type Foo", "doc": "Foo type."},
	}
	normalizeExtractionMetadata(&parser.ExtractionResult{Nodes: []*graph.Node{n}}, nil)
	if got := n.RetrievalMetadata().QualName; got != "pkg.Foo" {
		t.Fatalf("duplicated qualifier = %q", got)
	}
}

func TestNormalizeExtractionMetadataMemberOfSelectionIsStable(t *testing.T) {
	qualifier := func(reverse bool) string {
		alpha := &graph.Node{ID: "types.go::Alpha", Kind: graph.KindType, Name: "Alpha", QualName: "pkg.Alpha", Language: "go"}
		beta := &graph.Node{ID: "types.go::Beta", Kind: graph.KindType, Name: "Beta", QualName: "pkg.Beta", Language: "go"}
		method := &graph.Node{ID: "types.go::Run", Kind: graph.KindMethod, Name: "Run", Language: "go"}
		edges := []*graph.Edge{
			{From: method.ID, To: alpha.ID, Kind: graph.EdgeMemberOf},
			{From: method.ID, To: beta.ID, Kind: graph.EdgeMemberOf},
		}
		if reverse {
			edges[0], edges[1] = edges[1], edges[0]
		}
		normalizeExtractionMetadata(&parser.ExtractionResult{Nodes: []*graph.Node{alpha, beta, method}, Edges: edges}, nil)
		return method.RetrievalMetadata().QualName
	}
	forward, reverse := qualifier(false), qualifier(true)
	if forward != "pkg.Alpha.Run" || reverse != forward {
		t.Fatalf("unstable qualifiers: forward=%q reverse=%q", forward, reverse)
	}
}

func TestNormalizeExtractionMetadataSuppressesLegacyVariableLocalsOnly(t *testing.T) {
	fn := &graph.Node{ID: "scope.go::resolve", Kind: graph.KindFunction, Name: "resolve", QualName: "pkg.resolve", Language: "go"}
	local := &graph.Node{
		ID: "scope.go::value", Kind: graph.KindVariable, Name: "value", Language: "go",
		Meta: map[string]any{"signature": "var value = INLINE_SECRET", "doc": "local value"},
	}
	metadataLocal := &graph.Node{
		ID: "scope.go::other", Kind: graph.KindVariable, Name: "other", Language: "go",
		Meta: map[string]any{"scope": "block", "signature": "var other = INLINE_SECRET", "doc": "other local"},
	}
	global := &graph.Node{
		ID: "globals.go::Global", Kind: graph.KindVariable, Name: "Global", Language: "go",
		Meta: map[string]any{"scope": "global", "signature": "var Global string", "doc": "global value"},
	}
	owner := &graph.Node{ID: "types.go::Record", Kind: graph.KindType, Name: "Record", QualName: "pkg.Record", Language: "go"}
	field := &graph.Node{
		ID: "types.go::Record.Value", Kind: graph.KindField, Name: "Value", Language: "go",
		Meta: map[string]any{"signature": "Value string", "doc": "field value"},
	}
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{fn, local, metadataLocal, global, owner, field},
		Edges: []*graph.Edge{
			{From: local.ID, To: fn.ID, Kind: graph.EdgeMemberOf},
			{From: field.ID, To: owner.ID, Kind: graph.EdgeMemberOf},
		},
	}
	normalizeExtractionMetadata(result, nil)

	for _, n := range []*graph.Node{local, metadataLocal} {
		if got := n.RetrievalMetadata(); got != (graph.RetrievalMetadata{}) {
			t.Errorf("local %s leaked metadata: %#v", n.Name, got)
		}
		if !strings.Contains(n.Meta["signature"].(string), "INLINE_SECRET") {
			t.Errorf("parser metadata mutated for %s: %#v", n.Name, n.Meta)
		}
	}
	if got := global.RetrievalMetadata(); got.Signature != "var Global string" || got.Doc != "global value" {
		t.Fatalf("global suppressed: %#v", got)
	}
	if got := field.RetrievalMetadata(); got.QualName != "pkg.Record.Value" || got.Signature != "Value string" {
		t.Fatalf("field suppressed: %#v", got)
	}
}

func TestSearchIndexFieldsUseNormalizedRetrievalMetadata(t *testing.T) {
	n := &graph.Node{
		Kind: graph.KindFunction, Name: "validate", QualName: "legacy.validate", FilePath: "repo/src/auth.ts",
		Meta: map[string]any{
			"signature": "function validate()",
			"doc":       "legacy docs",
		},
	}
	graph.SetRetrievalMetadata(n, graph.RetrievalMetadata{
		Signature: "function validate(input: Request): Result",
		QualName:  "auth.validate",
		Doc:       "Validates incoming requests.",
	})

	want := []string{"validate", "repo/src/auth.ts", "auth.validate", "function validate(input: Request): Result", "Validates incoming requests."}
	if got := searchIndexFields(n, ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %#v, want %#v", got, want)
	}
	tokens := strings.Fields(ftsTokensFor(n, ""))
	count := 0
	for _, token := range tokens {
		if token == "auth" {
			count++
		}
	}
	if count != 2 { // path plus retrieval qualifier; no duplicate QualName append
		t.Fatalf("auth token count = %d in %q", count, strings.Join(tokens, " "))
	}
}
