package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// fakeEndpointStore is an in-memory EndpointConstStore: a name → const-node
// index plus a node-id → literal-value map. It stands in for the graph store
// so the resolver's graph-wide const dereference can be exercised without a
// full indexer run — a "single graph built from several files" in map form.
type fakeEndpointStore struct {
	nodes map[string][]*graph.Node
	vals  map[string]string

	nameLookups  int
	valueLookups int
}

func (f *fakeEndpointStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	f.nameLookups++
	out := make(map[string][]*graph.Node, len(names))
	for _, n := range names {
		if v, ok := f.nodes[n]; ok {
			out[n] = v
		}
	}
	return out
}

func (f *fakeEndpointStore) ConstantValuesByNodeIDs(ids []string) (map[string]string, error) {
	f.valueLookups++
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		if v, ok := f.vals[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

func constStore(rows ...[3]string) *fakeEndpointStore {
	// each row is {id, name, file}; values supplied separately.
	s := &fakeEndpointStore{nodes: map[string][]*graph.Node{}, vals: map[string]string{}}
	for _, r := range rows {
		id, name, file := r[0], r[1], r[2]
		s.nodes[name] = append(s.nodes[name], &graph.Node{
			ID:       id,
			Name:     name,
			Kind:     graph.KindConstant,
			FilePath: file,
		})
	}
	return s
}

// goCallArg parses a Go snippet and returns the argIdx'th argument node of the
// first call_expression whose function-selector field name is field. The
// returned tree owns the node's memory, so the caller must Release() it after
// use.
func goCallArg(t *testing.T, src []byte, field string, argIdx int) (*sitter.Node, *parser.ParseTree) {
	t.Helper()
	tree := ParseTreeForLang("go", src)
	if tree == nil || tree.Tree() == nil {
		t.Fatalf("ParseTreeForLang returned nil tree")
	}
	var found *sitter.Node
	walkGoCallExprs(tree.Tree().RootNode(), func(call *sitter.Node) {
		if found != nil {
			return
		}
		fn := call.ChildByFieldName("function")
		if fn == nil || fn.Type() != "selector_expression" {
			return
		}
		f := fn.ChildByFieldName("field")
		if f == nil || f.Content(src) != field {
			return
		}
		args := namedChildren(call.ChildByFieldName("arguments"))
		if argIdx < len(args) {
			found = args[argIdx]
		}
	})
	if found == nil {
		tree.Release()
		t.Fatalf("no call %q arg %d found", field, argIdx)
	}
	return found, tree
}

func TestResolveEndpointArg_StringLiteralUnchanged(t *testing.T) {
	src := []byte("package p\nfunc setup(r R) { r.GET(\"/api/users\", h) }\n")
	arg, tree := goCallArg(t, src, "GET", 0)
	defer tree.Release()

	// A plain literal resolves the same with or without a store, and is never
	// gated through the route guard (forRoute has no effect on a literal).
	for _, forRoute := range []bool{true, false} {
		got, ok := ResolveEndpointArg(arg, src, "repo/a.go", "repo", nil, forRoute)
		if !ok || got != "/api/users" {
			t.Fatalf("literal forRoute=%v: got (%q,%v), want (/api/users,true)", forRoute, got, ok)
		}
	}
}

func TestResolveEndpointArg_ConstSameFile(t *testing.T) {
	src := []byte("package p\nconst P = \"/api/x\"\nfunc setup(r R) { r.GET(P, h) }\n")
	arg, tree := goCallArg(t, src, "GET", 0)
	defer tree.Release()

	store := constStore([3]string{"repo/a.go::P", "P", "repo/a.go"})
	store.vals["repo/a.go::P"] = "/api/x"

	got, ok := ResolveEndpointArg(arg, src, "repo/a.go", "repo", store, true)
	if !ok || got != "/api/x" {
		t.Fatalf("same-file const: got (%q,%v), want (/api/x,true)", got, ok)
	}
}

func TestResolveEndpointArg_ConstCrossFile(t *testing.T) {
	// The route lives in b.go; the const is declared in a.go of the SAME repo.
	// A repo-wide unique value resolves — the graph-wide win over a per-file map.
	src := []byte("package p\nfunc setup(r R) { r.GET(P, h) }\n")
	arg, tree := goCallArg(t, src, "GET", 0)
	defer tree.Release()

	store := constStore([3]string{"repo/a.go::P", "P", "repo/a.go"})
	store.vals["repo/a.go::P"] = "/api/x"

	got, ok := ResolveEndpointArg(arg, src, "repo/b.go", "repo", store, true)
	if !ok || got != "/api/x" {
		t.Fatalf("cross-file const: got (%q,%v), want (/api/x,true)", got, ok)
	}
}

func TestResolveEndpointArg_AmbiguousDropped(t *testing.T) {
	src := []byte("package p\nfunc setup(r R) { r.GET(P, h) }\n")
	arg, tree := goCallArg(t, src, "GET", 0)
	defer tree.Release()

	// Two distinct values for P across the repo, neither in the enclosing file.
	store := constStore(
		[3]string{"repo/a.go::P", "P", "repo/a.go"},
		[3]string{"repo/b.go::P", "P", "repo/b.go"},
	)
	store.vals["repo/a.go::P"] = "/x"
	store.vals["repo/b.go::P"] = "/y"

	if got, ok := ResolveEndpointArg(arg, src, "repo/c.go", "repo", store, true); ok {
		t.Fatalf("ambiguous const should drop, got (%q,%v)", got, ok)
	}

	// Same name + same value in two files is NOT ambiguous — it resolves.
	store.vals["repo/b.go::P"] = "/x"
	if got, ok := ResolveEndpointArg(arg, src, "repo/c.go", "repo", store, true); !ok || got != "/x" {
		t.Fatalf("repo-wide identical value: got (%q,%v), want (/x,true)", got, ok)
	}

	// A same-file definition wins outright even when the repo is otherwise
	// ambiguous: resolving from a.go takes a.go's value.
	store.vals["repo/b.go::P"] = "/y"
	if got, ok := ResolveEndpointArg(arg, src, "repo/a.go", "repo", store, true); !ok || got != "/x" {
		t.Fatalf("same-file preference under ambiguity: got (%q,%v), want (/x,true)", got, ok)
	}
}

func TestResolveEndpointArg_RouteGuardRejectsFilesystemPath(t *testing.T) {
	src := []byte("package p\nfunc setup(r R) { r.GET(P, h) }\n")
	arg, tree := goCallArg(t, src, "GET", 0)
	defer tree.Release()

	store := constStore([3]string{"repo/a.go::P", "P", "repo/a.go"})
	store.vals["repo/a.go::P"] = "/etc/passwd"

	// ROUTE context: the guard rejects a resolved filesystem path.
	if got, ok := ResolveEndpointArg(arg, src, "repo/a.go", "repo", store, true); ok {
		t.Fatalf("route guard should reject /etc/passwd, got (%q,%v)", got, ok)
	}
	// TOPIC context (forRoute=false): the same value resolves — the guard
	// does not apply to queue/topic endpoints.
	if got, ok := ResolveEndpointArg(arg, src, "repo/a.go", "repo", store, false); !ok || got != "/etc/passwd" {
		t.Fatalf("topic context: got (%q,%v), want (/etc/passwd,true)", got, ok)
	}
}

func TestResolveEndpointArg_CompositeQueueLiteralField(t *testing.T) {
	src := []byte("package p\nfunc send(c C) { c.SendMessage(&sqs.SendMessageInput{QueueUrl: \"https://sqs/q\"}) }\n")
	arg, tree := goCallArg(t, src, "SendMessage", 0)
	defer tree.Release()

	// The field value is a literal, so no store is required.
	got, ok := ResolveEndpointArg(arg, src, "repo/a.go", "repo", nil, false)
	if !ok || got != "https://sqs/q" {
		t.Fatalf("composite literal field: got (%q,%v), want (https://sqs/q,true)", got, ok)
	}
}

func TestResolveEndpointArg_CompositeQueueConstField(t *testing.T) {
	src := []byte("package p\nfunc send(c C) { c.SendMessage(&sqs.SendMessageInput{QueueUrl: Q}) }\n")
	arg, tree := goCallArg(t, src, "SendMessage", 0)
	defer tree.Release()

	store := constStore([3]string{"repo/a.go::Q", "Q", "repo/a.go"})
	store.vals["repo/a.go::Q"] = "https://sqs/myqueue"

	got, ok := ResolveEndpointArg(arg, src, "repo/a.go", "repo", store, false)
	if !ok || got != "https://sqs/myqueue" {
		t.Fatalf("composite const field: got (%q,%v), want (https://sqs/myqueue,true)", got, ok)
	}
}

func TestHTTPExtractor_ExtractWithStore_ConstRoute(t *testing.T) {
	src := []byte("package p\nconst P = \"/api/x\"\nfunc setup(r R) { r.GET(P, h) }\n")
	store := constStore([3]string{"repo/a.go::P", "P", "repo/a.go"})
	store.vals["repo/a.go::P"] = "/api/x"

	h := &HTTPExtractor{}
	got := h.ExtractWithStore("repo/a.go", src, nil, nil, nil, store, "repo")

	if !hasRoute(got, "GET", "/api/x") {
		t.Fatalf("expected a GET /api/x route from a const path, got %v", routeSummaries(got))
	}
	if store.nameLookups != 1 || store.valueLookups != 1 {
		t.Fatalf("accepted const route lookups = names %d, values %d; want 1, 1", store.nameLookups, store.valueLookups)
	}
}

func TestHTTPExtractor_ExtractWithStore_UnsupportedSelectorsSkipConstLookup(t *testing.T) {
	src := []byte(`package p
func work(db DB, format, haystack, needle, query, path string) {
	fmt.Sprintf(format, 1)
	strings.Contains(haystack, needle)
	db.Exec(query)
	os.Open(path)
}
`)
	store := &fakeEndpointStore{}

	got := (&HTTPExtractor{}).ExtractWithStore("repo/a.go", src, nil, nil, nil, store, "repo")

	if len(got) != 0 {
		t.Fatalf("unsupported selectors emitted routes: %v", routeSummaries(got))
	}
	if store.nameLookups != 0 || store.valueLookups != 0 {
		t.Fatalf("unsupported selector lookups = names %d, values %d; want 0, 0", store.nameLookups, store.valueLookups)
	}
}

func TestHTTPExtractor_ExtractWithStore_LiteralRouteUnchanged(t *testing.T) {
	src := []byte("package p\nfunc setup(r R) { r.GET(\"/api/users\", h) }\n")
	h := &HTTPExtractor{}
	got := h.ExtractWithStore("repo/a.go", src, nil, nil, nil, nil, "repo")
	if !hasRoute(got, "GET", "/api/users") {
		t.Fatalf("expected a GET /api/users literal route, got %v", routeSummaries(got))
	}
}

func TestTopicExtractor_ExtractWithStore_SQSStruct(t *testing.T) {
	src := []byte("package p\nconst Q = \"https://sqs.amazonaws.com/123/myqueue\"\nfunc send(c C) { c.SendMessage(&sqs.SendMessageInput{QueueUrl: Q}) }\n")
	store := constStore([3]string{"repo/a.go::Q", "Q", "repo/a.go"})
	store.vals["repo/a.go::Q"] = "https://sqs.amazonaws.com/123/myqueue"

	e := &TopicExtractor{}
	got := e.ExtractWithStore("repo/a.go", src, nil, nil, nil, store, "repo")

	found := false
	for _, c := range got {
		if c.Type == ContractTopic && c.Role == RoleProvider &&
			c.Meta["broker"] == "sqs" && c.Meta["topic"] == "https://sqs.amazonaws.com/123/myqueue" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an sqs provider topic for the resolved QueueUrl const, got %d contracts", len(got))
	}
}

func hasRoute(cs []Contract, method, path string) bool {
	for _, c := range cs {
		if c.Type == ContractHTTP && c.Meta["method"] == method && c.Meta["path"] == path {
			return true
		}
	}
	return false
}

func routeSummaries(cs []Contract) []string {
	var out []string
	for _, c := range cs {
		if c.Type == ContractHTTP {
			m, _ := c.Meta["method"].(string)
			p, _ := c.Meta["path"].(string)
			out = append(out, m+" "+p)
		}
	}
	return out
}
