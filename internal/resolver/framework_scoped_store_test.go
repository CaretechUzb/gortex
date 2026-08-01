package resolver

import (
	"encoding/json"
	"fmt"
	"iter"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// frameworkScopeTrapStore makes an accidental whole-store predicate scan
// observable while forwarding the set-oriented scoped capabilities to its
// backing store. It also counts point reads so representative passes pin the
// edge-source hydration and changed-file adjacency batching.
type frameworkScopeTrapStore struct {
	graph.Store
	globalNodeScans   int
	globalEdgeScans   int
	allNodeScans      int
	allEdgeScans      int
	repoEdgeScans     int
	repoKindEdgeScans int
	repoKindNodeScans int
	scopedNodeScans   int
	scopedEdgeScans   int
	scopedLightScan   int
	pointNodes        int
	pointInEdges      int
	pointOutEdges     int
	batchNodes        int
	batchInEdges      int
	batchOutEdges     int
	batchNames        int
}

func (s *frameworkScopeTrapStore) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	s.globalNodeScans++
	return s.Store.NodesByKind(kind)
}

func (s *frameworkScopeTrapStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	s.globalEdgeScans++
	return s.Store.EdgesByKind(kind)
}

func (s *frameworkScopeTrapStore) AllNodes() []*graph.Node {
	s.allNodeScans++
	return s.Store.AllNodes()
}

func (s *frameworkScopeTrapStore) AllEdges() []*graph.Edge {
	s.allEdgeScans++
	return s.Store.AllEdges()
}

func (s *frameworkScopeTrapStore) GetRepoEdges(repoPrefix string) []*graph.Edge {
	s.repoEdgeScans++
	return s.Store.GetRepoEdges(repoPrefix)
}

func (s *frameworkScopeTrapStore) GetNode(id string) *graph.Node {
	s.pointNodes++
	return s.Store.GetNode(id)
}

func (s *frameworkScopeTrapStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.batchNodes++
	return s.Store.GetNodesByIDs(ids)
}

func (s *frameworkScopeTrapStore) GetInEdges(id string) []*graph.Edge {
	s.pointInEdges++
	return s.Store.GetInEdges(id)
}

func (s *frameworkScopeTrapStore) GetOutEdges(id string) []*graph.Edge {
	s.pointOutEdges++
	return s.Store.GetOutEdges(id)
}

func (s *frameworkScopeTrapStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.batchInEdges++
	return s.Store.GetInEdgesByNodeIDs(ids)
}

func (s *frameworkScopeTrapStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.batchOutEdges++
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *frameworkScopeTrapStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	s.batchNames++
	return s.Store.FindNodesByNames(names)
}

func (s *frameworkScopeTrapStore) NodesInScopeSeq(
	repos, files []string,
	kinds ...graph.NodeKind,
) iter.Seq[*graph.Node] {
	s.scopedNodeScans++
	return graph.NodesInScopeSeq(s.Store, repos, files, kinds...)
}

func (s *frameworkScopeTrapStore) EdgesInScopeSeq(
	repos, files []string,
	kinds ...graph.EdgeKind,
) iter.Seq[graph.ScopedEdgeRow] {
	s.scopedEdgeScans++
	return graph.EdgesInScopeSeq(s.Store, repos, files, kinds...)
}

func (s *frameworkScopeTrapStore) NodesLightInScopeSeq(
	repos, files []string,
) iter.Seq[*graph.Node] {
	s.scopedLightScan++
	return graph.NodesLightInScopeSeq(s.Store, repos, files)
}

func (s *frameworkScopeTrapStore) RepoEdgesByKinds(
	repos []string,
	kinds []graph.EdgeKind,
) []graph.RepoEdgeRow {
	s.repoKindEdgeScans++
	return graph.ReadRepoEdgesByKinds(s.Store, repos, kinds)
}

func (s *frameworkScopeTrapStore) RepoNodeIDsByKinds(
	repos []string,
	kinds []graph.NodeKind,
) []string {
	s.repoKindNodeScans++
	return graph.ReadRepoNodeIDsByKinds(s.Store, repos, kinds)
}

func (s *frameworkScopeTrapStore) RepoNodesLight(repos []string) []*graph.Node {
	return graph.ReadRepoNodesLight(s.Store, repos)
}

func (s *frameworkScopeTrapStore) RemoveEdgesExact(edges []*graph.Edge) int {
	return graph.RemoveEdgesExact(s.Store, edges)
}

func requireNoFrameworkGlobalScans(t *testing.T, store *frameworkScopeTrapStore) {
	t.Helper()
	require.Zero(t, store.globalNodeScans, "partial synth used global NodesByKind")
	require.Zero(t, store.globalEdgeScans, "partial synth used global EdgesByKind")
	require.Zero(t, store.allNodeScans, "partial synth used AllNodes")
	require.Zero(t, store.allEdgeScans, "partial synth used AllEdges")
	require.Zero(t, store.repoEdgeScans, "partial synth used broad GetRepoEdges")
}

func frameworkTestNode(repo, file, id string, kind graph.NodeKind, name, language string, meta map[string]any) *graph.Node {
	return &graph.Node{
		ID: id, Kind: kind, Name: name, FilePath: file, Language: language,
		RepoPrefix: repo, WorkspaceID: "workspace-" + repo, ProjectID: "project-" + repo,
		Meta: meta,
	}
}

func frameworkEdgeSnapshot(g graph.Store, repo string) []string {
	rows := g.GetRepoEdges(repo)
	out := make([]string, 0, len(rows))
	for _, edge := range rows {
		if edge == nil {
			continue
		}
		meta, _ := json.Marshal(edge.Meta)
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%d|%s|%.3f|%s",
			edge.From, edge.To, edge.Kind, edge.FilePath, edge.Line,
			edge.Origin, edge.Confidence, meta))
	}
	sort.Strings(out)
	return out
}

func buildGinScopedFixture() *graph.Graph {
	g := graph.New()
	for _, repo := range []string{"a", "b"} {
		changed := repo + "/router.go"
		handlerFile := repo + "/handler.go"
		dispatcher := repo + "::Context.Next"
		registrar := repo + "::setup"
		handler := repo + "::listUsers"
		g.AddBatch([]*graph.Node{
			frameworkTestNode(repo, changed, dispatcher, graph.KindMethod, "Next", "go", map[string]any{"gin_dispatcher": true}),
			frameworkTestNode(repo, changed, registrar, graph.KindFunction, "setup", "go", map[string]any{"gin_handlers": []string{"listUsers"}}),
			frameworkTestNode(repo, handlerFile, handler, graph.KindFunction, "listUsers", "go", nil),
		}, nil)
	}
	return g
}

func buildStoreFactoryScopedFixture() *graph.Graph {
	g := graph.New()
	for _, repo := range []string{"a", "b"} {
		callFile := repo + "/caller.ts"
		storeFile := repo + "/store.ts"
		caller := repo + "::hardReset"
		target := repo + "::useStore.reset"
		g.AddBatch([]*graph.Node{
			frameworkTestNode(repo, callFile, caller, graph.KindFunction, "hardReset", "typescript", nil),
			frameworkTestNode(repo, storeFile, target, graph.KindFunction, "reset", "typescript", map[string]any{
				"store_factory": "useStore", "store_member": "reset",
			}),
		}, []*graph.Edge{{
			From: caller, To: "unresolved::*.reset", Kind: graph.EdgeCalls, FilePath: callFile,
			Meta: map[string]any{"via": storeFactoryVia, "store_binding": "useStore", "store_action": "reset"},
		}})
	}
	return g
}

func buildFastAPIScopedFixture() *graph.Graph {
	g := graph.New()
	for _, repo := range []string{"a", "b"} {
		callFile := repo + "/routers/users.py"
		caller := repo + "::list_users"
		name := "get_db_" + repo
		target := repo + "::" + name
		g.AddBatch([]*graph.Node{
			frameworkTestNode(repo, callFile, caller, graph.KindFunction, "list_users", "python", nil),
			frameworkTestNode(repo, repo+"/dependencies/db.py", target, graph.KindFunction, name, "python", nil),
		}, []*graph.Edge{{
			From: caller, To: "unresolved::" + name, Kind: graph.EdgeCalls, FilePath: callFile,
			Meta: map[string]any{"via": "fastapi.Depends"},
		}})
	}
	return g
}

func buildMediatRScopedFixture() *graph.Graph {
	g := graph.New()
	for _, repo := range []string{"a", "b"} {
		callFile := repo + "/Controller.cs"
		caller := repo + "::Controller.Place"
		handler := repo + "::CreateOrderHandler.Handle"
		g.AddBatch([]*graph.Node{
			frameworkTestNode(repo, callFile, caller, graph.KindMethod, "Place", "csharp", nil),
			frameworkTestNode(repo, repo+"/Handler.cs", handler, graph.KindMethod, "Handle", "csharp", map[string]any{
				"mediatr_request_type": "CreateOrder", "mediatr_kind": "request",
			}),
		}, []*graph.Edge{{
			From: caller, To: "unresolved::*.Handle", Kind: graph.EdgeCalls, FilePath: callFile,
			Meta: map[string]any{"via": mediatrVia, "mediatr_request_type": "CreateOrder", "mediatr_kind": "request"},
		}})
	}
	return g
}

func TestFrameworkScopedLegacyPassesRepresentativeParityAndNoGlobalScans(t *testing.T) {
	tests := []struct {
		name        string
		changedFile string
		build       func() *graph.Graph
		resolve     func(graph.Store) int
	}{
		{name: "go gin", changedFile: "a/router.go", build: buildGinScopedFixture, resolve: ResolveGinMiddlewareCalls},
		{name: "typescript store factory", changedFile: "a/caller.ts", build: buildStoreFactoryScopedFixture, resolve: ResolveStoreFactoryCalls},
		{name: "python fastapi", changedFile: "a/routers/users.py", build: buildFastAPIScopedFixture, resolve: ResolveFastAPIDeps},
		{name: "csharp mediatr", changedFile: "a/Controller.cs", build: buildMediatRScopedFixture, resolve: ResolveMediatRCalls},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			full := tt.build()
			require.Positive(t, tt.resolve(full), "full pass must exercise the fixture")

			scopedGraph := tt.build()
			trap := &frameworkScopeTrapStore{Store: scopedGraph}
			view := newFrameworkScopedStore(trap, map[string]bool{"a": true}, []string{tt.changedFile})
			require.Positive(t, tt.resolve(view), "scoped pass must process the changed frontier")

			require.Equal(t, frameworkEdgeSnapshot(full, "a"), frameworkEdgeSnapshot(scopedGraph, "a"),
				"changed repository must reconcile exactly like a full pass")
			require.NotEqual(t, frameworkEdgeSnapshot(full, "b"), frameworkEdgeSnapshot(scopedGraph, "b"),
				"unchanged repository must not be recomputed")
			requireNoFrameworkGlobalScans(t, trap)
			require.Zero(t, trap.pointNodes, "edge sources and exact dependencies must be batch-hydrated")
			require.Zero(t, trap.pointInEdges, "incident reads must be batched")
			require.Zero(t, trap.pointOutEdges, "incident reads must be batched")
			require.LessOrEqual(t, trap.scopedNodeScans, 8)
			require.LessOrEqual(t, trap.scopedEdgeScans, 4)
		})
	}
}

func TestRunFrameworkSynthesizersScopedForFilesHasNoLegacyGlobalFallback(t *testing.T) {
	g := graph.New()
	g.AddNode(frameworkTestNode("a", "a/main.go", "a::main", graph.KindFunction, "main", "go", nil))
	trap := &frameworkScopeTrapStore{Store: g}

	RunFrameworkSynthesizersScopedForFiles(
		trap,
		map[string]bool{"a": true},
		[]string{"a/main.go"},
		false,
	)

	requireNoFrameworkGlobalScans(t, trap)
	require.Equal(t, 1, trap.scopedLightScan, "candidate census must be one scoped stream")
}

func TestFrameworkFamilyGateForFilesUsesExactFrontier(t *testing.T) {
	g := graph.New()
	dropSource := frameworkTestNode("a", "a/changed.java", "a::drop", graph.KindMethod, "drop", "java", nil)
	keepSource := frameworkTestNode("a", "a/changed.java", "a::keep", graph.KindMethod, "keep", "java", nil)
	unrelatedSource := frameworkTestNode("a", "a/unrelated.java", "a::unrelated", graph.KindMethod, "unrelated", "java", nil)
	webTarget := frameworkTestNode("a", "a/web.ts", "a::web", graph.KindFunction, "web", "typescript", nil)
	jvmTarget := frameworkTestNode("a", "a/jvm.kt", "a::jvm", graph.KindMethod, "jvm", "kotlin", nil)
	g.AddBatch([]*graph.Node{dropSource, keepSource, unrelatedSource, webTarget, jvmTarget}, []*graph.Edge{
		{From: dropSource.ID, To: webTarget.ID, Kind: graph.EdgeReferences, FilePath: dropSource.FilePath, Meta: map[string]any{MetaSynthesizedBy: "test"}},
		{From: keepSource.ID, To: jvmTarget.ID, Kind: graph.EdgeReferences, FilePath: keepSource.FilePath, Meta: map[string]any{MetaSynthesizedBy: "test"}},
		{From: unrelatedSource.ID, To: webTarget.ID, Kind: graph.EdgeReferences, FilePath: unrelatedSource.FilePath, Meta: map[string]any{MetaSynthesizedBy: "test"}},
	})
	trap := &frameworkScopeTrapStore{Store: g}

	require.Equal(t, 1, applyFrameworkFamilyGateScopedForFiles(
		trap,
		map[string]bool{"a": true},
		[]string{"a/changed.java"},
	))

	require.Empty(t, g.GetOutEdges(dropSource.ID), "changed cross-family result must be removed")
	require.Len(t, g.GetOutEdges(keepSource.ID), 1, "same-family result must remain")
	require.Len(t, g.GetOutEdges(unrelatedSource.ID), 1, "unrelated file must not be rescanned")
	require.Equal(t, 1, trap.scopedEdgeScans, "gate must use one exact scoped projection")
	require.Zero(t, trap.repoKindEdgeScans, "exact frontier must not scan repository edge kinds")
	requireNoFrameworkGlobalScans(t, trap)
}

func TestFrameworkScopedStoreLargeRepoOneFileRetainsBoundedRows(t *testing.T) {
	g := graph.New()
	const candidates = frameworkScopeRetainedRowCap * 3
	for i := 0; i < candidates; i++ {
		g.AddNode(frameworkTestNode(
			"a", fmt.Sprintf("a/dependencies/db_%05d.py", i), fmt.Sprintf("a::get_db::%05d", i),
			graph.KindFunction, "get_db", "python", nil,
		))
	}
	caller := frameworkTestNode("a", "a/routers/users.py", "a::list_users", graph.KindFunction, "list_users", "python", nil)
	g.AddBatch([]*graph.Node{caller}, []*graph.Edge{{
		From: caller.ID, To: "unresolved::get_db", Kind: graph.EdgeCalls, FilePath: caller.FilePath,
		Meta: map[string]any{"via": "fastapi.Depends"},
	}})
	trap := &frameworkScopeTrapStore{Store: g}
	view := newFrameworkScopedStore(trap, map[string]bool{"a": true}, []string{caller.FilePath})

	// The exact-name fanout is intentionally larger than the cache. Ambiguity
	// remains unresolved (the precision-safe full-pass result), while retained
	// state stays under both hard limits and the changed edge is still scanned.
	require.Zero(t, ResolveFastAPIDeps(view))
	stats := view.stats()
	require.LessOrEqual(t, stats.RetainedRows, stats.RowCap)
	require.LessOrEqual(t, stats.RetainedBytes, stats.ByteCap)
	require.Positive(t, trap.scopedEdgeScans, "affected synthesizer must run, not be skipped")
	requireNoFrameworkGlobalScans(t, trap)
}

var (
	_ graph.ScopedProjectionSequencer = (*frameworkScopeTrapStore)(nil)
	_ graph.RepoEdgeKindReader        = (*frameworkScopeTrapStore)(nil)
	_ graph.RepoNodeKindIDReader      = (*frameworkScopeTrapStore)(nil)
	_ graph.RepoLightNodeReader       = (*frameworkScopeTrapStore)(nil)
	_ graph.ExactEdgeBatchRemover     = (*frameworkScopeTrapStore)(nil)
)
