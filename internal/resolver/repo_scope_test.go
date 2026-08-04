package resolver

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestEdgeInResolveScope covers the three include-rules of the pending-edge
// repo-scope filter plus the exclusion of an edge that belongs wholly to a
// foreign (unchanged) repo.
func TestEdgeInResolveScope(t *testing.T) {
	scope := map[string]struct{}{"repoa": {}}

	cases := []struct {
		name string
		from string
		to   string
		want bool
	}{
		{
			// (a) source repo in scope; target repo-qualified elsewhere so
			// only rule (a) can fire.
			name: "source repo in scope",
			from: "repoa/pkg/a.go::Caller",
			to:   "repob::unresolved::Foo",
			want: true,
		},
		{
			// (b) foreign source, but the unresolved target is repo-qualified
			// to a scope repo.
			name: "target repo-qualified to scope repo",
			from: "repob/lib/b.go::Caller",
			to:   "repoa::unresolved::Bar",
			want: true,
		},
		{
			// (c) foreign source, bare unqualified unresolved target — could
			// bind into any changed repo, so it is always reconsidered.
			name: "bare unqualified unresolved target",
			from: "repob/lib/b.go::Caller",
			to:   "unresolved::Baz",
			want: true,
		},
		{
			// exclusion: foreign source and the target is repo-qualified to a
			// foreign repo (unresolved encoding).
			name: "foreign repo-qualified unresolved stub excluded",
			from: "repob/lib/b.go::Caller",
			to:   "repob::unresolved::Ghost",
			want: false,
		},
		{
			// exclusion via the general stub encoding (StubRepoPrefix), not the
			// unresolved one: a foreign repo-qualified external-call stub.
			name: "foreign repo-qualified stub excluded",
			from: "repob/lib/b.go::Caller",
			to:   "repob::external_call::pkg::Fn",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &graph.Edge{From: tc.from, To: tc.to, Kind: graph.EdgeCalls}
			assert.Equal(t, tc.want, edgeInResolveScope(e, scope))
		})
	}
}

// TestFilterPendingByScope exercises the in-place slice filter over a mixed
// pending set: it keeps the in-scope edges and drops the foreign one.
func TestFilterPendingByScope(t *testing.T) {
	scope := map[string]struct{}{"repoa": {}}
	inScopeSrc := &graph.Edge{From: "repoa/pkg/a.go::Caller", To: "repob::unresolved::Foo", Kind: graph.EdgeCalls}
	inScopeTgt := &graph.Edge{From: "repob/lib/b.go::Caller", To: "repoa::unresolved::Bar", Kind: graph.EdgeCalls}
	bare := &graph.Edge{From: "repoc/x.go::Caller", To: "unresolved::Baz", Kind: graph.EdgeCalls}
	foreign := &graph.Edge{From: "repob/lib/b.go::Caller", To: "repob::unresolved::Ghost", Kind: graph.EdgeCalls}

	got := filterPendingByScope([]*graph.Edge{inScopeSrc, foreign, inScopeTgt, bare}, scope)
	assert.ElementsMatch(t, []*graph.Edge{inScopeSrc, inScopeTgt, bare}, got)
}

// TestResolveAll_NilScopeMatchesUnscoped is the byte-for-byte-equivalence
// contract: SetScope(nil) — and an empty (non-nil) scope — resolve an
// identical corpus to the same final edge set as never touching the scope.
func TestResolveAll_NilScopeMatchesUnscoped(t *testing.T) {
	unscoped := resolverCorpusEdgeSet(t, func(r *Resolver) {})
	nilScoped := resolverCorpusEdgeSet(t, func(r *Resolver) { r.SetScope(nil) })
	assert.Equal(t, unscoped, nilScoped)

	emptyScoped := resolverCorpusEdgeSet(t, func(r *Resolver) { r.SetScope(map[string]struct{}{}) })
	assert.Equal(t, unscoped, emptyScoped)
}

// resolverCorpusEdgeSet builds a small multi-tier resolution corpus, applies
// cfg to the resolver, runs ResolveAll, and returns the sorted final edge set.
func resolverCorpusEdgeSet(t *testing.T, cfg func(*Resolver)) []string {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "pkg/b.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "pkg/b.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/b.go::Server.Start", Kind: graph.KindMethod, Name: "Start", FilePath: "pkg/b.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "main.go", Language: "go"})

	g.AddEdge(&graph.Edge{From: "pkg/a.go::Foo", To: "unresolved::Bar", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 5})
	g.AddEdge(&graph.Edge{From: "pkg/a.go::Foo", To: "unresolved::*.Start", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 6})
	g.AddEdge(&graph.Edge{From: "pkg/a.go::Foo", To: "unresolved::NonExistent", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 7})
	g.AddEdge(&graph.Edge{From: "main.go", To: "unresolved::import::fmt", Kind: graph.EdgeImports, FilePath: "main.go", Line: 3})

	r := New(g)
	cfg(r)
	require.NotNil(t, r.ResolveAll())
	return storeEdgeSet(g)
}

// TestResolveAll_ScopedEqualsFull is the equivalence gate. A sqlite-backed
// two-repo fixture is put in the state a warm restart of only repo A leaves
// behind — repo A's own call is unresolved (re-stubbed), repo B's call is
// already resolved (served from the persisted store), and repo B carries a
// foreign repo-qualified unresolvable stub. Running a scoped ResolveAll on one
// store and a full ResolveAll on an identical twin must land the same final
// resolved edge set.
func TestResolveAll_ScopedEqualsFull(t *testing.T) {
	scopedStore := newTwoRepoWarmStore(t)
	twinStore := newTwoRepoWarmStore(t)

	scoped := New(scopedStore)
	scoped.SetScope(map[string]struct{}{"repoa": {}})
	scopedStats := scoped.ResolveAll()

	full := New(twinStore)
	fullStats := full.ResolveAll()

	// SQLite pushes the scope predicate into the pager, so an intentionally
	// uncounted scan observes only the scoped rows it actually decodes. Compare
	// with the full twin to prove the foreign repo-B stub was dropped without a
	// whole-frontier pre-count.
	require.Less(t, scopedStats.PendingBefore, fullStats.PendingBefore,
		"scoped pager must load fewer pending edges than a full pass")
	require.Equal(t, scopedStats.PendingBefore, scopedStats.PendingAfter,
		"all rows admitted by the scoped pager survive the exact Go filter")
	require.Equal(t, fullStats.PendingBefore, fullStats.PendingAfter,
		"full pass must not drop any pending edge")

	assert.Equal(t, storeEdgeSet(twinStore), storeEdgeSet(scopedStore),
		"scoped and full resolves must produce identical final edge sets")

	// Concrete anchors: repo A's call resolved, repo B's stayed resolved, and
	// the foreign stub stayed unresolved — all in the scoped store.
	assert.True(t, hasCallEdge(scopedStore, "repoa/pkg/a.go::CallerA", "repoa/pkg/a.go::Foo"),
		"repo A's re-stubbed call must resolve under the scoped pass")
	assert.True(t, hasCallEdge(scopedStore, "repob/lib/b.go::CallerB", "repob/lib/b.go::Bar"),
		"repo B's persisted-resolved call must stay resolved")
	assert.True(t, hasCallEdge(scopedStore, "repob/lib/b.go::CallerB", "repob::unresolved::Ghost"),
		"the foreign repo-qualified stub must stay unresolved (scope filter skipped it)")
}

// newTwoRepoWarmStore opens a fresh sqlite store and populates it with the
// warm-restart-of-repo-A state described on TestResolveAll_ScopedEqualsFull.
func newTwoRepoWarmStore(t *testing.T) graph.Store {
	t.Helper()
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "scope.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Repo A: CallerA -> Foo, still unresolved (re-stubbed by the reindex).
	s.AddNode(&graph.Node{ID: "repoa/pkg/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repoa/pkg/a.go", Language: "go", RepoPrefix: "repoa"})
	s.AddNode(&graph.Node{ID: "repoa/pkg/a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "repoa/pkg/a.go", Language: "go", RepoPrefix: "repoa"})
	s.AddNode(&graph.Node{ID: "repoa/pkg/a.go::CallerA", Kind: graph.KindFunction, Name: "CallerA", FilePath: "repoa/pkg/a.go", Language: "go", RepoPrefix: "repoa"})
	s.AddEdge(&graph.Edge{From: "repoa/pkg/a.go::CallerA", To: "unresolved::Foo", Kind: graph.EdgeCalls, FilePath: "repoa/pkg/a.go", Line: 5})

	// Repo B: CallerB -> Bar already resolved (persisted), plus a foreign
	// repo-qualified unresolvable stub the scope filter must skip.
	s.AddNode(&graph.Node{ID: "repob/lib/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "repob/lib/b.go", Language: "go", RepoPrefix: "repob"})
	s.AddNode(&graph.Node{ID: "repob/lib/b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "repob/lib/b.go", Language: "go", RepoPrefix: "repob"})
	s.AddNode(&graph.Node{ID: "repob/lib/b.go::CallerB", Kind: graph.KindFunction, Name: "CallerB", FilePath: "repob/lib/b.go", Language: "go", RepoPrefix: "repob"})
	s.AddEdge(&graph.Edge{From: "repob/lib/b.go::CallerB", To: "repob/lib/b.go::Bar", Kind: graph.EdgeCalls, FilePath: "repob/lib/b.go", Line: 7, Origin: graph.OriginASTResolved})
	s.AddEdge(&graph.Edge{From: "repob/lib/b.go::CallerB", To: "repob::unresolved::Ghost", Kind: graph.EdgeCalls, FilePath: "repob/lib/b.go", Line: 9})

	return s
}

// storeEdgeSet returns the store's edges as a sorted, comparable set of
// (from, to, kind, origin) tuples.
func storeEdgeSet(s graph.Store) []string {
	var out []string
	for _, e := range s.AllEdges() {
		if e == nil {
			continue
		}
		out = append(out, e.From+"\t"+e.To+"\t"+string(e.Kind)+"\t"+e.Origin)
	}
	sort.Strings(out)
	return out
}

// hasCallEdge reports whether s holds an EdgeCalls edge from → to.
func hasCallEdge(s graph.Store, from, to string) bool {
	for _, e := range s.GetOutEdges(from) {
		if e.Kind == graph.EdgeCalls && e.To == to {
			return true
		}
	}
	return false
}
