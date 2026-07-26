package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A caller that asks to read "symbol" can only have meant one symbol's source.
// Refusing the singular costs it a whole turn — and a turn costs its entire
// prompt again — to learn a plural.
func TestReadingASymbolAcceptsTheSingularName(t *testing.T) {
	for _, guess := range []string{"symbol", "symbol_source", "get_symbol_source"} {
		require.Equal(t, "source", resolveFacadeOperationAlias("read", guess),
			"read(operation:%q) must resolve to the one-symbol source operation", guess)
	}
}

// The plural is a different operation, not a synonym: it serves the batch
// handler. Aliasing must not quietly collapse the two.
func TestTheBatchOperationKeepsItsOwnName(t *testing.T) {
	require.Equal(t, "symbols", resolveFacadeOperationAlias("read", "symbols"))
	require.NotEqual(t, resolveFacadeOperationAlias("read", "symbol"),
		resolveFacadeOperationAlias("read", "symbols"),
		"the singular and the batch form must stay distinct operations")
}

// Aliasing is scoped: a name that means something here must not be invented
// for a facade that never defined it.
func TestAliasesDoNotLeakAcrossFacades(t *testing.T) {
	for _, facade := range []string{"search", "relations", "change", "explore", "analyze"} {
		require.Equal(t, "symbol", resolveFacadeOperationAlias(facade, "symbol"),
			"%s must keep rejecting an operation it never defined", facade)
	}
}

// Real operation names pass through untouched, whatever the facade.
func TestKnownOperationsAreUnchanged(t *testing.T) {
	for facade, ops := range map[string][]string{
		"read":      {"source", "file", "editing_context", "summary", "history", "artifact"},
		"relations": {"usages", "callers", "dependents"},
	} {
		for _, op := range ops {
			require.Equal(t, op, resolveFacadeOperationAlias(facade, op))
		}
	}
}
