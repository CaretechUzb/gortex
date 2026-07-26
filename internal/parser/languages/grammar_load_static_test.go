//go:build !windows && static_link

package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/parser"
)

// The static build must DECLINE a custom grammar, not attempt the load. A
// statically linked glibc SEGVs inside the loader when it dlopen's on a host
// whose glibc differs from the build host's, so "returns an error" is the
// invariant that keeps an opt-in config entry from taking the daemon down.
func TestLoadGrammarLanguage_DeclinesInStaticBuild(t *testing.T) {
	ptr, err := loadGrammarLanguage("/any/libtree-sitter-thing.so", "tree_sitter_thing")
	require.Error(t, err)
	assert.Nil(t, ptr)
	assert.ErrorIs(t, err, errNoDlopen)
	// The message has to tell the user what to do instead — this is the only
	// place they learn the release binary can't load their grammar.
	assert.Contains(t, err.Error(), "build gortex from source")
}

// End to end through the caller: a declared grammar is skipped with a warning
// and never registered, exactly as a missing library already behaves.
func TestRegisterCustomGrammars_SkippedInStaticBuild(t *testing.T) {
	reg := parser.NewRegistry()
	assert.NotPanics(t, func() {
		RegisterCustomGrammars(reg, []config.GrammarSpec{
			{Language: "fictional", Library: "/any/libtree-sitter-fictional.so", Extensions: []string{".fic"}},
		}, nil)
	})
	_, ok := reg.GetByLanguage("fictional")
	assert.False(t, ok)
}
