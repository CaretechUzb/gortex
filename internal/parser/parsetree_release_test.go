package parser

import (
	"testing"

	"github.com/stretchr/testify/require"

	gosrc "github.com/zzet/gortex/internal/parser/tsitter/golang"
)

func newTestParseTree(t *testing.T) *ParseTree {
	t.Helper()
	src := []byte("package main\n\nfunc Value() int { return 0 }\n")
	tree, err := ParseFile(src, gosrc.GetLanguage())
	require.NoError(t, err)
	require.NotNil(t, tree)
	return NewParseTree(tree, src, "go")
}

func TestExtractionResultReleaseTree(t *testing.T) {
	pt := newTestParseTree(t)
	result := &ExtractionResult{Tree: pt}
	require.NotNil(t, pt.Tree())

	result.ReleaseTree()
	require.Nil(t, result.Tree, "ReleaseTree must clear the field")
	require.Nil(t, pt.Tree(), "ReleaseTree must close the underlying tree")
}

// Callers defer ReleaseTree on paths that may also release explicitly,
// so a repeat call must be a no-op rather than a second decrement — a
// second decrement would close a tree another holder still owns.
func TestExtractionResultReleaseTreeIsIdempotent(t *testing.T) {
	pt := newTestParseTree(t)
	// A consumer that retains the handle across calls.
	pt.Acquire()
	result := &ExtractionResult{Tree: pt}

	result.ReleaseTree()
	result.ReleaseTree()
	result.ReleaseTree()

	require.NotNil(t, pt.Tree(), "the retained Acquire must still hold the tree open")
	pt.Release()
	require.Nil(t, pt.Tree(), "the last Release must close the tree")
}

func TestExtractionResultReleaseTreeNilSafe(t *testing.T) {
	var nilResult *ExtractionResult
	require.NotPanics(t, func() { nilResult.ReleaseTree() })

	// The common case: an extractor that never retains a tree.
	empty := &ExtractionResult{}
	require.NotPanics(t, func() { empty.ReleaseTree() })
}
