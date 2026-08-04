package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// contractStateGraph embeds *graph.Graph and adds the ContractStateStore
// capability, standing in for the on-disk backend the daemon runs on.
type contractStateGraph struct {
	*graph.Graph

	state map[string]graph.ContractState
}

func (c *contractStateGraph) SetContractState(st graph.ContractState) error {
	c.state[st.RepoPrefix] = st
	return nil
}

func (c *contractStateGraph) GetContractState(repoPrefix string) (graph.ContractState, bool, error) {
	st, ok := c.state[repoPrefix]
	if !ok {
		return graph.ContractState{RepoPrefix: repoPrefix}, false, nil
	}
	return st, true, nil
}

// newContractStateGraph builds a store that persists contract state, so the
// marker gate is live (setupTestServer's plain in-memory graph does not
// implement the capability).
func newContractStateGraph() *contractStateGraph {
	return &contractStateGraph{Graph: graph.New(), state: map[string]graph.ContractState{}}
}

// dropContractMarkers simulates the reported failure: the index completed and
// the graph is populated, but no whole-repo contract pass ever committed, so
// nothing recorded a marker.
func (c *contractStateGraph) dropContractMarkers() {
	c.state = map[string]graph.ContractState{}
}

func newContractTierServer(t *testing.T, g graph.Store) *Server {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644))
	cfg := config.Default()
	idx := indexer.New(g, testRegistry(), cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	return NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
}

func analyzeRoutesPayload(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{"kind": "routes"}
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "analyze routes errored: %+v", res.Content)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))
	return out
}

// The failure this guards: a repo whose contract tail was lost answers
// `analyze kind=routes` with a clean zero that reads exactly like "this repo
// registers no routes". With no completion marker on record, the zero must say
// so. See https://github.com/zzet/gortex/issues/436.
func TestAnalyzeRoutes_ZeroRowsFlagsUnbuiltContractTier(t *testing.T) {
	g := newContractStateGraph()
	srv := newContractTierServer(t, g)
	g.dropContractMarkers()

	out := analyzeRoutesPayload(t, srv)
	require.Equal(t, float64(0), out["total"])

	caveat, ok := out["contract_tier"].(map[string]any)
	require.True(t, ok, "a zero-row route answer over an unmarked tier must carry a contract_tier caveat, got: %v", out)
	require.Equal(t, contractTierCaveatKey, caveat["caveat"])
	require.Contains(t, caveat, "remedy")
	repos, _ := caveat["repos"].([]any)
	require.Len(t, repos, 1)
}

// A recorded marker means the pass ran: the zero is then a true zero and must
// NOT be qualified, or every repo without routes would carry a permanent
// caveat.
func TestAnalyzeRoutes_ZeroRowsWithMarkerIsATrueZero(t *testing.T) {
	g := newContractStateGraph()
	srv := newContractTierServer(t, g)
	// The index above ran a whole-repo contract pass, which recorded the
	// marker — exactly what a healthy repo looks like.
	_, found, err := g.GetContractState("")
	require.NoError(t, err)
	require.True(t, found, "a completed index must record the contract-tier marker")

	out := analyzeRoutesPayload(t, srv)
	require.Equal(t, float64(0), out["total"])
	require.NotContains(t, out, "contract_tier")
}

// Rows on the table are their own proof that the tier was built, so an answer
// that returned routes is never qualified — not even on a store written before
// the marker existed.
func TestAnalyzeRoutes_RowsPresentSuppressCaveat(t *testing.T) {
	g := newContractStateGraph()
	srv := newContractTierServer(t, g)
	g.dropContractMarkers()
	addContractNode(srv.graph, "http::GET::/v1/users", "http", map[string]any{"method": "GET", "path": "/v1/users"})
	addHandlesRouteEdge(srv.graph, "handlers/users.go::List", "http::GET::/v1/users", "handlers/users.go", 12)

	out := analyzeRoutesPayload(t, srv)
	require.Equal(t, float64(1), out["total"])
	require.NotContains(t, out, "contract_tier")
}

// A backend that does not persist contract state (the in-memory graph, which
// rebuilds its tier on every start) has no signal to report, and must not
// invent one.
func TestAnalyzeRoutes_NoCaveatWithoutContractStateCapability(t *testing.T) {
	srv := newContractTierServer(t, graph.New())

	out := analyzeRoutesPayload(t, srv)
	require.Equal(t, float64(0), out["total"])
	require.NotContains(t, out, "contract_tier")
}

// The compact encoding carries the same signal — an agent reading "no routes"
// there is as misled as one reading rows=0 in JSON.
func TestAnalyzeRoutes_CompactCarriesContractTierCaveat(t *testing.T) {
	g := newContractStateGraph()
	srv := newContractTierServer(t, g)
	g.dropContractMarkers()

	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{"kind": "routes", "compact": true}
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	text := res.Content[0].(mcplib.TextContent).Text
	require.Contains(t, text, "no routes")
	require.Contains(t, text, contractTierCaveatKey)
}

// The empty repo prefix — the single-repo daemon's sole repo — renders as a
// readable placeholder rather than an empty string in the middle of a list.
func TestContractTierCaveatRendersDefaultRepoPrefix(t *testing.T) {
	line := contractTierCaveatLine([]string{""})
	require.True(t, strings.HasPrefix(line, contractTierCaveatKey+": (default)"), "got %q", line)
}
