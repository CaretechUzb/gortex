package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCSharpExtractor_NestedNamespaceBlocksJoin: nested block
// namespaces join outer-to-inner — `B` alone would collide with any
// other `B` in the graph and never match a `using A.B;`.
func TestCSharpExtractor_NestedNamespaceBlocksJoin(t *testing.T) {
	src := []byte(`namespace App {
    namespace Core {
        public class Widget {
            public void Render() {}
        }
    }
}
`)
	res, err := NewCSharpExtractor().Extract("w.cs", src)
	require.NoError(t, err)

	for _, n := range res.Nodes {
		if n.Name == "Widget" || n.Name == "Render" {
			assert.Equal(t, "App.Core", n.Meta["scope_ns"], "%s must carry the joined namespace", n.Name)
		}
	}
}

// TestCSharpExtractor_FileScopedNamespaceScopeNS: a C#10 file-scoped
// namespace's declarations are AST siblings, not children — an
// ancestor walk alone loses every such file's scope_ns.
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
