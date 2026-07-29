package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCSharpExtractor_FileScopedNamespaceScopeNS: a C#10 file-scoped
// namespace spans only its own line in the AST — the declarations it
// governs are siblings, not children, so the ancestor walk alone never
// finds it and every type in such a file loses its scope_ns.
func TestCSharpExtractor_FileScopedNamespaceScopeNS(t *testing.T) {
	src := []byte(`namespace App.Core.Metrics;

public class MetricsCollector
{
    public void Configure() {}
}

public interface IMetrics {}
`)
	res, err := NewCSharpExtractor().Extract("a.cs", src)
	require.NoError(t, err)

	for _, name := range []string{"MetricsCollector", "Configure", "IMetrics"} {
		found := false
		for _, n := range res.Nodes {
			if n.Name == name {
				found = true
				assert.Equal(t, "App.Core.Metrics", n.Meta["scope_ns"],
					"%s must carry the file-scoped namespace", name)
			}
		}
		require.True(t, found, "node %s missing", name)
	}
}
