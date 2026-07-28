package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A computed callee names no symbol; emitting its source text produced targets
// that can never bind, such as `unresolved::*.$fn`.
func TestPHPCallHygiene_DynamicCalleesEmitNothing(t *testing.T) {
	res, err := NewPHPExtractor().Extract("hyg.php", []byte(`<?php
class Runner {
    public function run(callable $fn, string $method, array $handlers): void {
        $fn();
        $this->$method();
        static::$method();
        ($this->resolver)();
        $handlers[0]();
    }
}
`))
	require.NoError(t, err)
	for _, e := range res.Edges {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		assert.NotContains(t, e.To, "$", "unbindable dynamic callee target %q", e.To)
		assert.NotContains(t, e.To, "(", "unbindable dynamic callee target %q", e.To)
		assert.NotContains(t, e.To, "[", "unbindable dynamic callee target %q", e.To)
	}
}

// A static callee alongside dynamic ones must still be recorded.
func TestPHPCallHygiene_StaticCalleesStillEmitted(t *testing.T) {
	res, err := NewPHPExtractor().Extract("hyg.php", []byte(`<?php
class Runner {
    public function run(callable $fn): void {
        $fn();
        $this->tick();
        strlen('x');
    }
}
`))
	require.NoError(t, err)
	got := map[string]bool{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls {
			got[e.To] = true
		}
	}
	assert.True(t, got["unresolved::*.tick"])
	assert.True(t, got["unresolved::*.strlen"])
}

// Instantiating a class is the only way to call its constructor, and the
// instantiates edge points at the class rather than at the method that runs —
// so without this lowering a PHP constructor had no callers at all.
func TestPHPCallHygiene_NewLowersToConstructorCall(t *testing.T) {
	res, err := NewPHPExtractor().Extract("hyg.php", []byte(`<?php
class Boot {
    public function run(): void {
        $h = new StreamHandler('php://stdout');
    }
}
`))
	require.NoError(t, err)

	var ctor, inst *graph.Edge
	for _, e := range res.Edges {
		switch {
		case e.Kind == graph.EdgeCalls && e.To == "unresolved::*.__construct":
			ctor = e
		case e.Kind == graph.EdgeInstantiates && e.To == "unresolved::StreamHandler":
			inst = e
		}
	}
	require.NotNil(t, inst, "the instantiation edge must survive")
	require.NotNil(t, ctor, "new Foo() must also lower to a constructor call")
	assert.Equal(t, "StreamHandler", ctor.Meta["receiver_type"])
	assert.Equal(t, "constructor", ctor.Meta["via"])
	assert.Equal(t, "hyg.php::Boot.run", ctor.From)
}

// `new $cls` and `new class {}` name no constructible type statically.
func TestPHPCallHygiene_DynamicNewEmitsNoConstructorCall(t *testing.T) {
	res, err := NewPHPExtractor().Extract("hyg.php", []byte(`<?php
function boot(string $cls): void {
    $a = new $cls();
    $b = new class {};
}
`))
	require.NoError(t, err)
	for _, e := range res.Edges {
		assert.NotEqual(t, "unresolved::*.__construct", e.To, "edge %v", e)
	}
}

func TestPHPCallHygiene_CatchClauseTypesAreReferences(t *testing.T) {
	res, err := NewPHPExtractor().Extract("hyg.php", []byte(`<?php
class Boot {
    public function run(): void {
        try {
            risky();
        } catch (ConnectionException | TimeoutException $e) {
            report($e);
        }
    }
}
`))
	require.NoError(t, err)
	got := map[string]bool{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeReferences && strings.HasPrefix(e.To, "unresolved::") {
			got[e.To] = true
		}
	}
	assert.True(t, got["unresolved::ConnectionException"], "got %v", got)
	assert.True(t, got["unresolved::TimeoutException"], "got %v", got)
}
