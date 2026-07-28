package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestPHPMetrics_ComplexityStampedOnMethods(t *testing.T) {
	nodes := phpNodesByName(t, `<?php
class Router {
    public function dispatch(array $routes, string $path): ?string {
        foreach ($routes as $prefix => $handler) {
            if (str_starts_with($path, $prefix)) {
                foreach ($handler as $h) {
                    if ($h !== null) {
                        return $h;
                    }
                }
            } elseif ($prefix === '*') {
                return $handler;
            }
        }
        return null;
    }
}
`)
	m := nodes["dispatch"]
	require.NotNil(t, m)
	assert.Greater(t, m.Meta["complexity"], 1, "cyclomatic complexity must be stamped: %v", m.Meta)
	assert.Greater(t, m.Meta["cognitive"], 1, "cognitive complexity must be stamped: %v", m.Meta)
	assert.Equal(t, 2, m.Meta["loop_depth"], "nested foreach is loop depth 2: %v", m.Meta)
}

func TestPHPMetrics_TrivialFunctionUnstamped(t *testing.T) {
	nodes := phpNodesByName(t, `<?php
function name(): string { return 'x'; }
`)
	n := nodes["name"]
	require.NotNil(t, n)
	assert.NotContains(t, n.Meta, "complexity")
	assert.NotContains(t, n.Meta, "loop_depth")
}

// A closure owns its own branches; folding them into the enclosing function
// would overstate the enclosing function's score.
func TestPHPMetrics_ClosureBranchesNotFoldedIntoEnclosing(t *testing.T) {
	nodes := phpNodesByName(t, `<?php
function outer(array $xs): callable {
    return function ($x) use ($xs) {
        if ($x) { return 1; }
        if (!$x) { return 2; }
        foreach ($xs as $y) { if ($y) { return $y; } }
        return 0;
    };
}
`)
	n := nodes["outer"]
	require.NotNil(t, n)
	assert.NotContains(t, n.Meta, "complexity",
		"the closure's branches belong to the closure, got %v", n.Meta)
}

func TestPHPMetrics_PhtmlTemplatesAreIndexed(t *testing.T) {
	exts := map[string]bool{}
	for _, x := range NewPHPExtractor().Extensions() {
		exts[x] = true
	}
	assert.True(t, exts[".phtml"], "PHP template files must be indexed as PHP")

	res, err := NewPHPExtractor().Extract("view.phtml", []byte(`<h1>Title</h1>
<?php foreach ($items as $item): ?>
  <li><?= $item->label() ?></li>
<?php endforeach; ?>
`))
	require.NoError(t, err)
	var sawCall bool
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls && e.To == "unresolved::*.label" {
			sawCall = true
		}
	}
	assert.True(t, sawCall, "calls inside a .phtml template must reach the graph")
}
