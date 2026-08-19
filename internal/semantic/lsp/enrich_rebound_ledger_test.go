package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Rebound ledger (#605): the definition fallback can answer an unconfirmed
// edge two ways — the server agrees with the heuristic target, or it lands
// on a DIFFERENT same-name declaration and the edge is rewritten (tagged
// rebound_from). The second outcome is a correction of the heuristic
// graph, not a confirmation of it, and the result must count it
// separately: a pass that "confirmed" 5,000 edges by rewriting half of
// them describes accuracy the graph never had.

// reboundFixture writes two same-name C declarations in separate served
// files plus a caller, and seeds one ambiguous call edge bound to the
// same-file declaration. references never confirm it, so the definition
// fallback adjudicates; the scripted definition answer decides which
// outcome the test observes. NeedsCompileDB with no database keeps the
// pass degraded — reference-confirm and definition-fallback only, no
// sweep noise.
func reboundFixture(t *testing.T) (string, graph.Store) {
	t.Helper()
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "a.c"),
		[]byte("int target(void) { return 0; }\nint caller(void) { return target(); }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "b.c"),
		[]byte("int target(void) { return 1; }\n"), 0o644))

	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.c::target", Kind: graph.KindFunction, Name: "target",
		FilePath: "a.c", StartLine: 1, EndLine: 1, Language: "c"})
	g.AddNode(&graph.Node{ID: "a.c::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "a.c", StartLine: 2, EndLine: 2, Language: "c"})
	g.AddNode(&graph.Node{ID: "b.c::target", Kind: graph.KindFunction, Name: "target",
		FilePath: "b.c", StartLine: 1, EndLine: 1, Language: "c"})
	g.AddEdge(&graph.Edge{From: "a.c::caller", To: "a.c::target", Kind: graph.EdgeCalls,
		FilePath: "a.c", Line: 2, Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched})
	return repoRoot, g
}

// reboundProvider wires the fixture's provider: references never confirm,
// definition answers with the given location.
func reboundProvider(t *testing.T, defLoc Location) (*Provider, func()) {
	t.Helper()
	server := newInstrumentedServer()
	server.handle("textDocument/references", func(params json.RawMessage) (any, *jsonRPCError) {
		return []Location{}, nil
	})
	server.handle("textDocument/definition", func(params json.RawMessage) (any, *jsonRPCError) {
		return []Location{defLoc}, nil
	})
	p, cleanup := providerWithInstrumentedServer(t, server, []string{"c", "cpp"}, 2)
	p.spec = clangdLikeSpec()
	return p, cleanup
}

func callEdgeFrom(t *testing.T, g graph.Store, from string) *graph.Edge {
	t.Helper()
	for _, e := range g.GetOutEdges(from) {
		if e.Kind == graph.EdgeCalls {
			return e
		}
	}
	t.Fatalf("no call edge from %s", from)
	return nil
}

func TestLSP_Enrich_DefinitionRebindCountedAsRebound(t *testing.T) {
	repoRoot, g := reboundFixture(t)
	p, cleanup := reboundProvider(t, Location{
		URI:   pathToURI(filepath.Join(repoRoot, "b.c")),
		Range: Range{Start: Position{Line: 0, Character: 4}},
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, 1, result.EdgesRebound, "a rewritten target is a correction, counted as rebound")
	assert.Zero(t, result.EdgesConfirmed, "a rebind must not inflate the confirmed count")

	e := callEdgeFrom(t, g, "a.c::caller")
	assert.Equal(t, "b.c::target", e.To, "the edge follows the server's answer")
	assert.Equal(t, "a.c::target", e.Meta["rebound_from"], "the ledger tag names the heuristic target")
}

func TestLSP_Enrich_DefinitionAgreementCountedAsConfirmed(t *testing.T) {
	repoRoot, g := reboundFixture(t)
	p, cleanup := reboundProvider(t, Location{
		URI:   pathToURI(filepath.Join(repoRoot, "a.c")),
		Range: Range{Start: Position{Line: 0, Character: 4}},
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, 1, result.EdgesConfirmed, "server agreement is a genuine confirmation")
	assert.Zero(t, result.EdgesRebound, "nothing was rewritten")

	e := callEdgeFrom(t, g, "a.c::caller")
	assert.Equal(t, "a.c::target", e.To, "the heuristic target stands")
	assert.NotContains(t, e.Meta, "rebound_from")
}
