package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// cppHeaderSrc reduces the shape a real header-only C++ library ships: macro
// namespace markers the preprocessor would expand, an inner `namespace
// detail`, and a free template function defined at namespace scope. The
// extension is `.h`, which C, C++ and Objective-C all claim, so the language
// is decidable only from the content. Parsed as C the template function
// vanishes — the C grammar cannot see past `template <…>`.
const cppHeaderSrc = `#ifndef LIB_PRINTF_H_
#define LIB_PRINTF_H_

#include "format.h"

FMT_BEGIN_NAMESPACE
FMT_BEGIN_EXPORT

template <typename Char> class basic_printf_context {
 public:
  auto out() -> basic_appender<Char> { return out_; }

 private:
  basic_appender<Char> out_;
};

namespace detail {

template <typename Char, typename Context>
void vprintf(buffer<Char>& buf, basic_string_view<Char> format,
             basic_format_args<Context> args) {
  auto out = basic_appender<Char>(buf);
  write(out, format);
}

}  // namespace detail

FMT_END_EXPORT
FMT_END_NAMESPACE

#endif  // LIB_PRINTF_H_
`

// newTestIndexerCFamily registers the C and C++ extractors, the pair that
// makes `.h` ambiguous: C claims the extension, C++ does not, so only a
// content probe can route a C++ header to the right extractor.
func newTestIndexerCFamily(g graph.Store) *Indexer {
	reg := parser.NewRegistry()
	reg.Register(languages.NewCExtractor())
	reg.Register(languages.NewCppExtractor())
	cfg := config.Default().Index
	cfg.Workers = 2
	return New(g, reg, cfg, zap.NewNop())
}

// TestIndex_AmbiguousHeaderLanguageProbedFromContent proves the bulk index
// walk resolves a `.h` file's language from its bytes rather than from the
// extension's default mapping. The walk detects a language before any file is
// read, so an ambiguous extension can only yield a provisional answer there;
// reusing it verbatim indexed every C++ header as C and dropped its templates.
func TestIndex_AmbiguousHeaderLanguageProbedFromContent(t *testing.T) {
	dir := t.TempDir()
	inc := filepath.Join(dir, "include", "lib")
	require.NoError(t, os.MkdirAll(inc, 0o755))
	writeFile(t, filepath.Join(inc, "printf.h"), cppHeaderSrc)

	g := graph.New()
	idx := newTestIndexerCFamily(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	nodes := g.FindNodesByName("vprintf")
	require.NotEmpty(t, nodes, "the free template function must be indexed")
	assert.Equal(t, "cpp", nodes[0].Language,
		"a header carrying C++ markers must be parsed by the C++ extractor")
	assert.Equal(t, graph.KindFunction, nodes[0].Kind)
}
