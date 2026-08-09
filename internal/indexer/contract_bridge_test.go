package indexer

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// mkBridgeLink builds one matched CrossLink between hand-rolled
// provider/consumer contracts for the unit-level materialisation tests.
func mkBridgeLink(groupID string, provider, consumer contracts.Contract) contracts.CrossLink {
	return contracts.CrossLink{
		ContractID: groupID,
		Provider:   provider,
		Consumer:   consumer,
		CrossRepo:  provider.RepoPrefix != consumer.RepoPrefix,
	}
}

func collectBridgeNodes(g graph.Store) []*graph.Node {
	var out []*graph.Node
	for n := range g.NodesByKind(graph.KindContractBridge) {
		out = append(out, n)
	}
	return out
}

func collectBridgeEdges(g graph.Store) []*graph.Edge {
	var out []*graph.Edge
	for e := range g.EdgesByKind(graph.EdgeBridges) {
		out = append(out, e)
	}
	return out
}

// TestMaterializeContractBridges_GroupsAndSides covers the core
// materialisation contract: one bridge node per matched group, repo
// spread + counts in Meta, and EdgeBridges fan-out with side meta —
// including the "both" collapse when an exact-ID match shares one
// contract node across roles.
func TestMaterializeContractBridges_GroupsAndSides(t *testing.T) {
	g := graph.New()

	httpProvider := contracts.Contract{
		ID: "http::GET::/api/users", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		SymbolID: "svc-a/routes.go::listUsers", FilePath: "svc-a/routes.go", Line: 10,
		RepoPrefix: "svc-a", WorkspaceID: "acme", ProjectID: "users",
	}
	httpConsumer := contracts.Contract{
		ID: "http::GET::/api/users", Type: contracts.ContractHTTP, Role: contracts.RoleConsumer,
		SymbolID: "svc-b/client.go::fetchUsers", FilePath: "svc-b/client.go", Line: 7,
		RepoPrefix: "svc-b", WorkspaceID: "acme", ProjectID: "users",
	}
	grpcIDL := contracts.Contract{
		ID: "grpc::Users::GetUser", Type: contracts.ContractGRPC, Role: contracts.RoleProvider,
		FilePath: "svc-a/users.proto", Line: 5,
		RepoPrefix: "svc-a", WorkspaceID: "acme", ProjectID: "users",
		Meta: map[string]any{"service": "Users", "method": "GetUser"},
	}
	grpcStub := contracts.Contract{
		ID: "grpc::Users::getUser", Type: contracts.ContractGRPC, Role: contracts.RoleConsumer,
		SymbolID: "web/api.ts::loadUser", FilePath: "web/api.ts", Line: 3,
		RepoPrefix: "webapp", WorkspaceID: "acme", ProjectID: "users",
		Meta: map[string]any{"service": "Users", "method": "getUser"},
	}

	// Contract nodes as commitContracts would mint them.
	for _, id := range []string{"http::GET::/api/users", "grpc::Users::GetUser", "grpc::Users::getUser"} {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindContract, Name: id, Language: "contract"})
	}

	matched := []contracts.CrossLink{
		mkBridgeLink("http::GET::/api/users", httpProvider, httpConsumer),
		mkBridgeLink("grpc::Users::GetUser", grpcIDL, grpcStub),
	}

	minted := MaterializeContractBridges(g, matched)
	require.Equal(t, 2, minted, "one bridge per matched group")

	httpBridge := g.GetNode("bridge::acme::users::http::GET::/api/users")
	require.NotNil(t, httpBridge, "http bridge node missing")
	assert.Equal(t, graph.KindContractBridge, httpBridge.Kind)
	assert.Equal(t, ContractBridgeFilePath, httpBridge.FilePath)
	assert.Equal(t, "GET /api/users", httpBridge.Meta["canonical_key"])
	assert.Equal(t, "http", httpBridge.Meta["contract_type"])
	assert.Equal(t, []string{"svc-a", "svc-b"}, httpBridge.Meta["repos"])
	assert.Equal(t, 1, httpBridge.Meta["provider_count"])
	assert.Equal(t, 1, httpBridge.Meta["consumer_count"])
	assert.Equal(t, true, httpBridge.Meta["cross_repo"])
	assert.Equal(t, "svc-a", httpBridge.RepoPrefix, "bridge owner is the provider repo")
	assert.Equal(t, "acme", httpBridge.WorkspaceID)

	// Exact-ID match: provider and consumer collapse into one contract
	// node — a single side="both" edge.
	httpEdges := g.GetOutEdges(httpBridge.ID)
	require.Len(t, httpEdges, 1)
	assert.Equal(t, graph.EdgeBridges, httpEdges[0].Kind)
	assert.Equal(t, "http::GET::/api/users", httpEdges[0].To)
	assert.Equal(t, "both", httpEdges[0].Meta["side"])

	// Canonical join: provider and consumer keep distinct contract
	// nodes — one edge per side.
	grpcBridge := g.GetNode("bridge::acme::users::grpc::Users::GetUser")
	require.NotNil(t, grpcBridge, "grpc bridge node missing")
	assert.Equal(t, "Users.GetUser", grpcBridge.Meta["canonical_key"])
	sides := map[string]string{}
	for _, e := range g.GetOutEdges(grpcBridge.ID) {
		side, _ := e.Meta["side"].(string)
		sides[e.To] = side
	}
	assert.Equal(t, map[string]string{
		"grpc::Users::GetUser": "provider",
		"grpc::Users::getUser": "consumer",
	}, sides)
}

// TestMaterializeContractBridges_IdempotentAndEvicting: re-running
// with the same matches replaces the prior generation 1:1; running
// with a shrunken match set drops the stale bridge; running with no
// matches clears the subgraph entirely.
func TestMaterializeContractBridges_IdempotentAndEvicting(t *testing.T) {
	g := graph.New()

	provider := contracts.Contract{
		ID: "http::GET::/api/users", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		SymbolID: "a/routes.go::listUsers", FilePath: "a/routes.go", RepoPrefix: "a",
	}
	consumer := contracts.Contract{
		ID: "http::GET::/api/users", Type: contracts.ContractHTTP, Role: contracts.RoleConsumer,
		SymbolID: "b/client.go::fetchUsers", FilePath: "b/client.go", RepoPrefix: "b",
	}
	provider2 := contracts.Contract{
		ID: "topic::kafka::orders", Type: contracts.ContractTopic, Role: contracts.RoleProvider,
		SymbolID: "a/pub.go::publish", FilePath: "a/pub.go", RepoPrefix: "a",
	}
	consumer2 := contracts.Contract{
		ID: "topic::kafka::orders", Type: contracts.ContractTopic, Role: contracts.RoleConsumer,
		SymbolID: "b/sub.go::consume", FilePath: "b/sub.go", RepoPrefix: "b",
	}

	full := []contracts.CrossLink{
		mkBridgeLink("http::GET::/api/users", provider, consumer),
		mkBridgeLink("topic::kafka::orders", provider2, consumer2),
	}

	require.Equal(t, 2, MaterializeContractBridges(g, full))
	require.Len(t, collectBridgeNodes(g), 2)
	edgesBefore := len(collectBridgeEdges(g))
	require.Greater(t, edgesBefore, 0)

	// Idempotent re-run: same input, same persisted state.
	require.Equal(t, 2, MaterializeContractBridges(g, full))
	assert.Len(t, collectBridgeNodes(g), 2, "re-run must not duplicate bridge nodes")
	assert.Equal(t, edgesBefore, len(collectBridgeEdges(g)), "re-run must not duplicate bridge edges")

	// The topic group disappears (e.g. its file was deleted): its
	// bridge must go with it.
	require.Equal(t, 1, MaterializeContractBridges(g, full[:1]))
	assert.Nil(t, g.GetNode("bridge::a::a::topic::kafka::orders"), "stale bridge must be evicted")
	assert.NotNil(t, g.GetNode("bridge::a::a::http::GET::/api/users"))

	// Everything disappears.
	require.Equal(t, 0, MaterializeContractBridges(g, nil))
	assert.Empty(t, collectBridgeNodes(g))
	assert.Empty(t, collectBridgeEdges(g))
}

// TestContractBridge_TwoRepoIntegration drives the full pipeline over
// two tracked repos sharing one workspace: a .proto IDL provider repo
// (with a Go server registration) and a Go stub-consumer repo. The
// reconcile pass must persist one bridge spanning both repos, and a
// second reconcile must leave the subgraph unchanged.
func TestContractBridge_TwoRepoIntegration(t *testing.T) {
	providerRoot := setupGRPCProtoProviderRepo(t, "auth-service")
	consumerRoot := setupGRPCGoConsumerRepo(t, "client-svc")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: providerRoot, Name: "auth-service"},
			{Path: consumerRoot, Name: "client-svc"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newMultiLangRegistry(), search.NewNull(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}

	bridge := g.GetNode("bridge::shared-test::shared-test::grpc::Users::GetUser")
	require.NotNil(t, bridge,
		"expected persisted bridge for the matched gRPC group; bridges: %v", bridgeIDs(g))
	assert.Equal(t, graph.KindContractBridge, bridge.Kind)
	assert.Equal(t, "grpc", bridge.Meta["contract_type"])
	assert.Equal(t, "Users.GetUser", bridge.Meta["canonical_key"])
	assert.Equal(t, true, bridge.Meta["cross_repo"])

	repos, _ := bridge.Meta["repos"].([]string)
	assert.Equal(t, []string{"auth-service", "client-svc"}, repos,
		"bridge must span both participating repos")

	// EdgeBridges fan-out: the shared-ID contract node carries both
	// roles; the registration-site provider contract (grpc::Users,
	// joined canonically) rides as a separate provider edge.
	sides := map[string]string{}
	for _, e := range g.GetOutEdges(bridge.ID) {
		require.Equal(t, graph.EdgeBridges, e.Kind)
		side, _ := e.Meta["side"].(string)
		sides[e.To] = side
	}
	assert.Equal(t, "both", sides["grpc::Users::GetUser"],
		"exact-ID provider+consumer collapse into one contract node: %v", sides)
	assert.Equal(t, "provider", sides["grpc::Users"],
		"registration-site provider must join the group: %v", sides)

	// Idempotency across reconciles: re-running the pass replaces the
	// generation in place.
	nodesBefore := len(collectBridgeNodes(g))
	edgesBefore := len(collectBridgeEdges(g))
	mi.ReconcileContractEdges()
	assert.Equal(t, nodesBefore, len(collectBridgeNodes(g)), "reconcile must not duplicate bridges")
	assert.Equal(t, edgesBefore, len(collectBridgeEdges(g)), "reconcile must not duplicate bridge edges")

	// Untracking the consumer dissolves the group: the next reconcile
	// rebuilds bridges from the remaining contracts only.
	mi.UntrackRepo("client-svc")
	assert.Nil(t, g.GetNode("bridge::shared-test::shared-test::grpc::Users::GetUser"),
		"bridge must dissolve when the consumer repo is untracked; bridges: %v", bridgeIDs(g))
}

// TestMaterializeContractBridges_BoundaryIsolation: two unrelated
// workspaces that each serve the same contract (`GET /api/users`) must
// materialise as TWO distinct bridge nodes, never one merged bridge.
// The matcher pairs only inside a (workspace, project) boundary, so a
// bridge keyed on the bare contract ID would assert a provider_count /
// cross-repo blast radius the matcher never produced.
func TestMaterializeContractBridges_BoundaryIsolation(t *testing.T) {
	g := graph.New()

	// Workspace "acme": one repo internally serving + consuming the route.
	acmeProv := contracts.Contract{
		ID: "http::GET::/api/users", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		SymbolID: "acme-api/routes.go::list", FilePath: "acme-api/routes.go", Line: 10,
		RepoPrefix: "acme-api", WorkspaceID: "acme", ProjectID: "acme",
	}
	acmeCons := contracts.Contract{
		ID: "http::GET::/api/users", Type: contracts.ContractHTTP, Role: contracts.RoleConsumer,
		SymbolID: "acme-api/client.go::fetch", FilePath: "acme-api/client.go", Line: 4,
		RepoPrefix: "acme-api", WorkspaceID: "acme", ProjectID: "acme",
	}
	// Workspace "globex": a completely unrelated service that happens to
	// expose and consume the identical route string.
	globexProv := contracts.Contract{
		ID: "http::GET::/api/users", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		SymbolID: "globex-api/routes.go::list", FilePath: "globex-api/routes.go", Line: 22,
		RepoPrefix: "globex-api", WorkspaceID: "globex", ProjectID: "globex",
	}
	globexCons := contracts.Contract{
		ID: "http::GET::/api/users", Type: contracts.ContractHTTP, Role: contracts.RoleConsumer,
		SymbolID: "globex-api/client.go::fetch", FilePath: "globex-api/client.go", Line: 8,
		RepoPrefix: "globex-api", WorkspaceID: "globex", ProjectID: "globex",
	}

	for _, id := range []string{"http::GET::/api/users"} {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindContract, Name: id, Language: "contract"})
	}

	matched := []contracts.CrossLink{
		mkBridgeLink("http::GET::/api/users", acmeProv, acmeCons),
		mkBridgeLink("http::GET::/api/users", globexProv, globexCons),
	}

	minted := MaterializeContractBridges(g, matched)
	require.Equal(t, 2, minted, "two unrelated workspaces must not collapse into one bridge")

	acmeBridge := g.GetNode("bridge::acme::acme::http::GET::/api/users")
	require.NotNil(t, acmeBridge, "acme bridge missing; bridges: %v", bridgeIDs(g))
	globexBridge := g.GetNode("bridge::globex::globex::http::GET::/api/users")
	require.NotNil(t, globexBridge, "globex bridge missing; bridges: %v", bridgeIDs(g))

	// Each bridge counts only its own workspace's provider — never the
	// summed count a merged bridge would assert.
	assert.Equal(t, 1, acmeBridge.Meta["provider_count"])
	assert.Equal(t, 1, globexBridge.Meta["provider_count"])
	assert.Equal(t, "acme-api", acmeBridge.RepoPrefix)
	assert.Equal(t, "globex-api", globexBridge.RepoPrefix)
	assert.Equal(t, "acme", acmeBridge.Meta["workspace"])
	assert.Equal(t, "globex", globexBridge.Meta["workspace"])
	// Neither is cross-repo: each pairs inside its own single repo.
	assert.Equal(t, false, acmeBridge.Meta["cross_repo"])
	assert.Equal(t, false, globexBridge.Meta["cross_repo"])
}

// TestMaterializeContractBridges_StartLineIsOrderIndependent: a group
// with multiple provider records at different lines must pin its bridge
// StartLine to the true minimum regardless of the (map-ordered) match
// iteration, so reconciles stay byte-stable instead of flapping the
// persisted field.
func TestMaterializeContractBridges_StartLineIsOrderIndependent(t *testing.T) {
	provLow := contracts.Contract{
		ID: "grpc::Users::GetUser", Type: contracts.ContractGRPC, Role: contracts.RoleProvider,
		SymbolID: "svc/a.go::A", FilePath: "svc/a.go", Line: 5,
		RepoPrefix: "svc", WorkspaceID: "w", ProjectID: "p",
		Meta: map[string]any{"service": "Users", "method": "GetUser"},
	}
	provHigh := contracts.Contract{
		ID: "grpc::Users::GetUser", Type: contracts.ContractGRPC, Role: contracts.RoleProvider,
		SymbolID: "svc/b.go::B", FilePath: "svc/b.go", Line: 99,
		RepoPrefix: "svc", WorkspaceID: "w", ProjectID: "p",
		Meta: map[string]any{"service": "Users", "method": "GetUser"},
	}
	consumer := contracts.Contract{
		ID: "grpc::Users::GetUser", Type: contracts.ContractGRPC, Role: contracts.RoleConsumer,
		SymbolID: "web/api.ts::load", FilePath: "web/api.ts", Line: 3,
		RepoPrefix: "web", WorkspaceID: "w", ProjectID: "p",
		Meta: map[string]any{"service": "Users", "method": "GetUser"},
	}

	// Two orderings of the same matched group: high-line link first, then
	// low-line link first. Both must yield StartLine = 5 (the true min).
	orderings := [][]contracts.CrossLink{
		{
			mkBridgeLink("grpc::Users::GetUser", provHigh, consumer),
			mkBridgeLink("grpc::Users::GetUser", provLow, consumer),
		},
		{
			mkBridgeLink("grpc::Users::GetUser", provLow, consumer),
			mkBridgeLink("grpc::Users::GetUser", provHigh, consumer),
		},
	}
	for i, matched := range orderings {
		g := graph.New()
		require.Equal(t, 1, MaterializeContractBridges(g, matched), "ordering %d", i)
		bridge := g.GetNode("bridge::w::p::grpc::Users::GetUser")
		require.NotNil(t, bridge, "ordering %d: bridge missing", i)
		assert.Equal(t, 5, bridge.StartLine,
			"ordering %d: StartLine must be the true minimum provider line, order-independent", i)
	}
}

// TestReconcileContractEdges_ConcurrentNoRaceOrTear stresses the
// serialisation that ReconcileContractEdges needs. The janitor, the
// file-watcher, and MCP-triggered track/index all drive it on
// independent goroutines, and the pass evicts the prior EdgeMatches /
// topic / bridge generation and mints a fresh one across many
// non-atomic store writes. Concurrent runs whose registry snapshots
// disagree (one mid-flight while another track/untrack mutates the
// registry) can interleave an evict over the other's freshly-minted
// bridge and persist a stale generation. Here a writer goroutine
// repeatedly untracks and re-tracks the consumer repo — flipping the
// matched set between "bridge present" and "bridge absent" — while many
// reader goroutines reconcile concurrently. Under -race this surfaces
// any unsynchronised access; the terminal state (both repos tracked)
// must hold the single complete bridge generation, never a torn one.
func TestReconcileContractEdges_ConcurrentNoRaceOrTear(t *testing.T) {
	providerRoot := setupGRPCProtoProviderRepo(t, "auth-service")
	consumerRoot := setupGRPCGoConsumerRepo(t, "client-svc")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	consumerEntry := config.RepoEntry{Path: consumerRoot, Name: "client-svc"}
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: providerRoot, Name: "auth-service"},
			consumerEntry,
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newMultiLangRegistry(), search.NewNull(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}

	bridgeID := "bridge::shared-test::shared-test::grpc::Users::GetUser"
	require.NotNil(t, g.GetNode(bridgeID), "baseline bridge missing; bridges: %v", bridgeIDs(g))

	var wg sync.WaitGroup

	// Writer: flip the matched set under the readers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 12; i++ {
			mi.UntrackRepo("client-svc")
			_, _ = mi.TrackRepoCtx(context.Background(), consumerEntry)
		}
	}()

	// Readers: reconcile concurrently with the flips.
	const readers = 12
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				mi.ReconcileContractEdges()
			}
		}()
	}
	wg.Wait()

	// Settle on the final registry state with one last reconcile, then
	// assert the terminal generation is complete and not duplicated.
	mi.ReconcileContractEdges()

	got := bridgeIDs(g)
	count := 0
	for _, id := range got {
		if id == bridgeID {
			count++
		}
	}
	require.Equal(t, 1, count, "expected exactly one bridge after concurrent reconciles; bridges: %v", got)

	// The fan-out must be complete: the exact-ID provider+consumer node
	// plus the registration-site provider. A torn generation would miss
	// one of these.
	sides := map[string]string{}
	for _, e := range g.GetOutEdges(bridgeID) {
		side, _ := e.Meta["side"].(string)
		sides[e.To] = side
	}
	assert.Equal(t, "both", sides["grpc::Users::GetUser"], "edge set torn by interleave: %v", sides)
	assert.Equal(t, "provider", sides["grpc::Users"], "edge set torn by interleave: %v", sides)
}

func bridgeIDs(g graph.Store) []string {
	var out []string
	for _, n := range collectBridgeNodes(g) {
		out = append(out, n.ID)
	}
	return out
}
