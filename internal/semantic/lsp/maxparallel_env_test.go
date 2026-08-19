package lsp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// GORTEX_LSP_MAX_PARALLEL lets an operator raise a server's concurrent
// request cap without editing the registry — the same experiment knob
// shape as GORTEX_LSP_SWEEP. Env wins over the spec default; an unusable
// value falls through rather than failing the spawn.
func TestNewProviderFromSpec_MaxParallelEnvOverride(t *testing.T) {
	spec := &ServerSpec{Name: "csharp-ls", Command: "csharp-ls",
		Languages: []string{"csharp"}, MaxParallel: 6}

	t.Setenv(MaxParallelEnv, "")
	p := NewProviderFromSpec(spec, zap.NewNop())
	assert.Equal(t, 6, p.maxParallel, "spec default when the env is unset")

	t.Setenv(MaxParallelEnv, "12")
	p = NewProviderFromSpec(spec, zap.NewNop())
	assert.Equal(t, 12, p.maxParallel, "env override wins over the spec default")

	for _, junk := range []string{"0", "-3", "garbage"} {
		t.Setenv(MaxParallelEnv, junk)
		p = NewProviderFromSpec(spec, zap.NewNop())
		assert.Equal(t, 6, p.maxParallel, "unusable value %q falls through to the spec", junk)
	}

	// A spec with no cap of its own keeps the package default unless the
	// env names one.
	bare := &ServerSpec{Name: "x", Command: "x", Languages: []string{"go"}}
	t.Setenv(MaxParallelEnv, "")
	assert.Equal(t, 10, NewProviderFromSpec(bare, zap.NewNop()).maxParallel)
	t.Setenv(MaxParallelEnv, "4")
	assert.Equal(t, 4, NewProviderFromSpec(bare, zap.NewNop()).maxParallel)
}
