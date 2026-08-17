package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// searchIDs runs a query through the indexer's search backend and returns the
// hit IDs.
func searchIDs(idx *Indexer, query string, limit int) []string {
	hits := idx.Search().Search(query, limit)
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	return ids
}

// Indexing a directory with JSON + Go files must keep JSON keys out
// of the symbol search corpus by default (regression guard for the
// search-backend memory blowup) while still leaving them reachable
// via graph queries. Uses lowercase-only single-word identifiers so
// the query tokenizer (which does not split camelCase) hits exactly
// the tokens produced by the write-side tokenizer.
//
// The probe is ONE two-word query rather than two identifier queries, and
// that shape is load-bearing: the sqlite backend short-circuits an
// identifier-shaped query on an exact FindNodesByName hit and returns
// before the ranked FTS tier runs (store_fts.go tier 0). Asking for
// "uniquejsonkeyzzz" alone would therefore be answered out of the GRAPH —
// where the JSON key legitimately lives — and the exclusion would look
// broken even when it works. A whitespace-separated query skips tier 0, and
// the MATCH expression is a prefix-OR, so one query proves both halves at
// once: the Go symbol is in the corpus, the JSON key is not.
func TestIndex_JSONVariablesSkippedFromSearchByDefault(t *testing.T) {
	dir := t.TempDir()

	jsonPath := filepath.Join(dir, "package.json")
	require.NoError(t, os.WriteFile(jsonPath, []byte(`{
  "uniquejsonkeyzzz": "value"
}`), 0o644))

	goPath := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(goPath, []byte(`package main

func uniquegosymbolzzz() {}
`), 0o644))

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	reg.Register(languages.NewJSONExtractor())

	cfg := config.Default().Index
	cfg.Workers = 1
	// Mirror what ConfigManager.GetRepoConfig would do for a real run.
	cfg.SkipSearch = config.DefaultSkipSearch()

	idx, store := newFTSIndexer(t, dir, reg, cfg)

	// Graph still carries the JSON key — SkipSearch is about the search
	// corpus, not the graph. Users looking up a config key via
	// FindNodesByName / get_symbol must still find it.
	jsonNodes := store.FindNodesByName("uniquejsonkeyzzz")
	require.NotEmpty(t, jsonNodes, "JSON variable node should exist in graph")
	jsonID := jsonNodes[0].ID

	ids := searchIDs(idx, "uniquegosymbolzzz uniquejsonkeyzzz", 10)
	assert.Contains(t, ids, "main.go::uniquegosymbolzzz",
		"Go symbol should be in the symbol search corpus")
	assert.NotContains(t, ids, jsonID,
		"JSON variable node must be excluded from the search corpus by default (SkipSearch)")
}

// With SkipSearch cleared, JSON variable nodes are searchable again. Guards
// the config surface: users who actually want to search their package.json
// keys can opt in by overriding SkipSearch.
//
// The trailing "absentneedlezzz" token exists only to make the query
// non-identifier-shaped, for the tier-0 reason spelled out above; it appears
// in no document, so whether the JSON key comes back is decided entirely by
// the ranked FTS tier.
func TestIndex_JSONVariablesSearchableWhenSkipSearchCleared(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
  "overridekeyzzz": "value"
}`), 0o644))

	reg := parser.NewRegistry()
	reg.Register(languages.NewJSONExtractor())

	cfg := config.Default().Index
	cfg.Workers = 1
	cfg.SkipSearch = nil // override: index everything

	idx, store := newFTSIndexer(t, dir, reg, cfg)

	nodes := store.FindNodesByName("overridekeyzzz")
	require.NotEmpty(t, nodes, "JSON variable node should exist in graph")

	ids := searchIDs(idx, "overridekeyzzz absentneedlezzz", 10)
	assert.Contains(t, ids, nodes[0].ID,
		"JSON variable node should be searchable when SkipSearch is cleared")
}
