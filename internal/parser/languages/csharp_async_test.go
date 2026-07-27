package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestCSharpExtractor_AwaitCallsNotDropped locks the async path: calls
// wrapped in `await` (both a member call `await _repo.GetAsync()` and a
// bare call `await LogAsync()`) must still emit EdgeCalls, and an
// `async Task<T>` method must still be extracted as a method node. The
// invocation_expression sits under an await_expression, but the query
// matches it anywhere in the tree, so the call edge survives.
func TestCSharpExtractor_AwaitCallsNotDropped(t *testing.T) {
	src := []byte(`public class Svc {
    private Repo _repo;

    public async Task<User> Load(int id) {
        var u = await _repo.GetAsync(id);
        await LogAsync(u);
        return u;
    }

    private Task LogAsync(User u) { return Task.CompletedTask; }
}
`)
	result, err := NewCSharpExtractor().Extract("Svc.cs", src)
	require.NoError(t, err)

	// The async Task<User> method is still a method node.
	var hasLoad bool
	for _, n := range nodesOfKind(result.Nodes, graph.KindMethod) {
		if n.Name == "Load" {
			hasLoad = true
		}
	}
	assert.True(t, hasLoad, "async Task<T> method must still be extracted")

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	hasSuffix := func(suffix string) bool {
		for _, c := range calls {
			if strings.HasSuffix(c.To, suffix) {
				return true
			}
		}
		return false
	}
	assert.True(t, hasSuffix("GetAsync"), "awaited member call must emit an EdgeCalls")
	assert.True(t, hasSuffix("LogAsync"), "awaited bare call must emit an EdgeCalls")
}
