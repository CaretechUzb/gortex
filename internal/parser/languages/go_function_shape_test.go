package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// runGoExtract is a small harness used by the function-shape tests
// — wraps NewGoExtractor().Extract and returns the result so tests
// can assert directly against nodes/edges.
func runGoExtract(t *testing.T, src string) *extractedFixture {
	t.Helper()
	ext := NewGoExtractor()
	result, err := ext.Extract("pkg/foo.go", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	fix := &extractedFixture{
		nodesByID:    map[string]*graph.Node{},
		nodesByKind:  map[graph.NodeKind][]*graph.Node{},
		edgesByKind:  map[graph.EdgeKind][]*graph.Edge{},
		edgesByOwner: map[string][]*graph.Edge{},
	}
	for _, n := range result.Nodes {
		fix.nodesByID[n.ID] = n
		fix.nodesByKind[n.Kind] = append(fix.nodesByKind[n.Kind], n)
	}
	for _, e := range result.Edges {
		fix.edgesByKind[e.Kind] = append(fix.edgesByKind[e.Kind], e)
		fix.edgesByOwner[e.From] = append(fix.edgesByOwner[e.From], e)
	}
	return fix
}

// runGoExtractWithEnvHelpers is runGoExtract with a per-repo corporate
// env-helper allow-list installed on the extractor first — exercises the
// config-driven promotion path.
func runGoExtractWithEnvHelpers(t *testing.T, src string, envHelpers []string) *extractedFixture {
	t.Helper()
	ext := NewGoExtractor()
	opts := parser.NewExtractionOptions(envHelpers)
	result, err := ext.ExtractWithOptions("pkg/foo.go", []byte(src), opts)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	fix := &extractedFixture{
		nodesByID:    map[string]*graph.Node{},
		nodesByKind:  map[graph.NodeKind][]*graph.Node{},
		edgesByKind:  map[graph.EdgeKind][]*graph.Edge{},
		edgesByOwner: map[string][]*graph.Edge{},
	}
	for _, n := range result.Nodes {
		fix.nodesByID[n.ID] = n
		fix.nodesByKind[n.Kind] = append(fix.nodesByKind[n.Kind], n)
	}
	for _, e := range result.Edges {
		fix.edgesByKind[e.Kind] = append(fix.edgesByKind[e.Kind], e)
		fix.edgesByOwner[e.From] = append(fix.edgesByOwner[e.From], e)
	}
	return fix
}

type extractedFixture struct {
	nodesByID    map[string]*graph.Node
	nodesByKind  map[graph.NodeKind][]*graph.Node
	edgesByKind  map[graph.EdgeKind][]*graph.Edge
	edgesByOwner map[string][]*graph.Edge
}

func TestGoFunctionShape_ParamsAndReturns(t *testing.T) {
	src := `package foo

func Add(a, b int) (int, error) {
	return a + b, nil
}
`
	fix := runGoExtract(t, src)

	params := fix.nodesByKind[graph.KindParam]
	if len(params) != 2 {
		t.Fatalf("expected 2 params, got %d: %+v", len(params), params)
	}
	wantNames := map[string]bool{"a": false, "b": false}
	for _, p := range params {
		if _, ok := wantNames[p.Name]; !ok {
			t.Errorf("unexpected param name %q", p.Name)
			continue
		}
		wantNames[p.Name] = true
		if p.Meta["type"] != "int" {
			t.Errorf("param %q: type meta = %v, want int", p.Name, p.Meta["type"])
		}
	}
	for n, seen := range wantNames {
		if !seen {
			t.Errorf("missing param %q", n)
		}
	}

	paramOf := fix.edgesByKind[graph.EdgeParamOf]
	if len(paramOf) != 2 {
		t.Errorf("EdgeParamOf count = %d, want 2", len(paramOf))
	}
	for _, e := range paramOf {
		if e.To != "pkg/foo.go::Add" {
			t.Errorf("param edge target = %q, want owner", e.To)
		}
	}

	typedAs := fix.edgesByKind[graph.EdgeTypedAs]
	if len(typedAs) != 2 {
		t.Errorf("EdgeTypedAs count = %d, want 2", len(typedAs))
	}
	for _, e := range typedAs {
		if e.To != "unresolved::int" {
			t.Errorf("typed_as target = %q", e.To)
		}
	}

	returns := fix.edgesByKind[graph.EdgeReturns]
	if len(returns) != 2 {
		t.Fatalf("EdgeReturns count = %d, want 2 (int + error)", len(returns))
	}
	gotTargets := map[string]bool{}
	for _, e := range returns {
		gotTargets[e.To] = true
		if e.From != "pkg/foo.go::Add" {
			t.Errorf("return edge from = %q", e.From)
		}
	}
	if !gotTargets["unresolved::int"] || !gotTargets["external::error"] {
		t.Errorf("return targets wrong: %v", gotTargets)
	}
}

func TestGoFunctionShape_GenericParams(t *testing.T) {
	src := `package foo

func Map[T any, U comparable](in []T) []U {
	return nil
}
`
	fix := runGoExtract(t, src)

	gens := fix.nodesByKind[graph.KindGenericParam]
	if len(gens) != 2 {
		t.Fatalf("expected 2 generic params, got %d: %+v", len(gens), gens)
	}
	want := map[string]string{"T": "any", "U": "comparable"}
	for _, g := range gens {
		bound, _ := g.Meta["bound"].(string)
		if want[g.Name] != bound {
			t.Errorf("generic %q bound = %q, want %q", g.Name, bound, want[g.Name])
		}
	}
}

func TestGoFunctionShape_ClosureCaptures(t *testing.T) {
	src := `package foo

func Run(prefix string) func(string) string {
	count := 0
	return func(s string) string {
		count++
		return prefix + s
	}
}
`
	fix := runGoExtract(t, src)

	captures := fix.edgesByKind[graph.EdgeCaptures]
	captured := map[string]bool{}
	for _, e := range captures {
		if name, _ := e.Meta["name"].(string); name != "" {
			captured[name] = true
		}
	}
	if !captured["count"] {
		t.Errorf("EdgeCaptures missing for 'count'; got %v", captured)
	}
	if !captured["prefix"] {
		t.Errorf("EdgeCaptures missing for 'prefix'; got %v", captured)
	}
	// 's' is a parameter of the closure, not a capture.
	if captured["s"] {
		t.Errorf("'s' is a closure parameter, should not be captured")
	}
}

func TestGoFunctionShape_ClosureLocalsShadowOuter(t *testing.T) {
	src := `package foo

func Outer() {
	v := 1
	go func() {
		v := 99
		_ = v
	}()
}
`
	fix := runGoExtract(t, src)

	captures := fix.edgesByKind[graph.EdgeCaptures]
	for _, e := range captures {
		if name, _ := e.Meta["name"].(string); name == "v" {
			// The closure re-declares v with `:=`. Inside the
			// closure body, `v` resolves to the inner binding,
			// so EdgeCaptures should NOT be emitted for v.
			t.Errorf("EdgeCaptures emitted for shadowed local 'v'")
		}
	}
}

func TestGoFunctionShape_ClosureNoCaptureWithoutOuterRefs(t *testing.T) {
	src := `package foo

func Run() {
	go func() {
		x := 1
		_ = x
	}()
}
`
	fix := runGoExtract(t, src)

	captures := fix.edgesByKind[graph.EdgeCaptures]
	if len(captures) != 0 {
		names := []string{}
		for _, e := range captures {
			n, _ := e.Meta["name"].(string)
			names = append(names, n)
		}
		t.Errorf("expected no EdgeCaptures; got %v", names)
	}
}

func TestGoFunctionShape_Closure(t *testing.T) {
	src := `package foo

func Run() {
	go func() {
		println("hi")
	}()
}
`
	fix := runGoExtract(t, src)

	closures := fix.nodesByKind[graph.KindClosure]
	if len(closures) != 1 {
		t.Fatalf("expected 1 closure, got %d: %+v", len(closures), closures)
	}
	c := closures[0]
	if c.Meta == nil && c.Name == "" {
		t.Errorf("closure name empty")
	}
	memberOf := fix.edgesByKind[graph.EdgeMemberOf]
	hasClosureLink := false
	for _, e := range memberOf {
		if e.From == c.ID && e.To == "pkg/foo.go::Run" {
			hasClosureLink = true
		}
	}
	if !hasClosureLink {
		t.Errorf("closure has no member_of link to enclosing function")
	}
}

func TestGoFunctionShape_MethodReceiverShape(t *testing.T) {
	src := `package foo

type Server struct{}

func (s *Server) Handle(req string) error {
	return nil
}
`
	fix := runGoExtract(t, src)

	params := fix.nodesByKind[graph.KindParam]
	if len(params) != 1 || params[0].Name != "req" {
		t.Fatalf("expected single 'req' param, got %+v", params)
	}
	owner := "pkg/foo.go::Server.Handle"
	for _, e := range fix.edgesByKind[graph.EdgeParamOf] {
		if e.From != params[0].ID || e.To != owner {
			t.Errorf("param edge wrong: %s -> %s", e.From, e.To)
		}
	}
	returnEdges := fix.edgesByKind[graph.EdgeReturns]
	if len(returnEdges) != 1 || returnEdges[0].To != "external::error" {
		t.Errorf("return edges wrong: %+v", returnEdges)
	}
}

func TestGoFunctionShape_VariadicAndBlankIdentifier(t *testing.T) {
	src := `package foo

func Log(_ string, args ...any) {}
`
	fix := runGoExtract(t, src)
	params := fix.nodesByKind[graph.KindParam]
	if len(params) != 1 {
		t.Fatalf("expected only 'args' (blank identifier skipped), got %+v", params)
	}
	if params[0].Name != "args" {
		t.Errorf("param name = %q", params[0].Name)
	}
	if v, _ := params[0].Meta["variadic"].(bool); !v {
		t.Errorf("variadic flag missing on args param")
	}
}
