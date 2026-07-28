package persistence

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Surfacing matches stored symbol anchors against the caller's ids by string
// equality, so a change to id SHAPE severs every anchor at once and returns
// nothing — indistinguishable from "nothing relevant". Making repo prefixes
// unconditional is such a change. These tests cover the one-shot rewrite that
// carries existing notes / memories / notebooks / suppressions across it.

func openAnchorSidecar(t *testing.T) *SidecarStore {
	t.Helper()
	s, err := OpenSidecar(filepath.Join(t.TempDir(), "sidecar.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// prefixIfMissing is the shape the daemon supplies: rewrite ONLY when the old
// id names no live node and the prefixed candidate does.
func prefixIfMissing(live map[string]bool, prefix string) SymbolIDRewriter {
	return func(oldID string) string {
		if live[oldID] {
			return "" // still resolves — never touch it
		}
		candidate := prefix + "/" + oldID
		if live[candidate] {
			return candidate
		}
		return "" // resolves nowhere — leave it rather than guess
	}
}

func TestPrefixSymbolIDsRewritesEveryAnchorTable(t *testing.T) {
	s := openAnchorSidecar(t)
	const repo = "repo-key"

	require.NoError(t, s.UpsertNote(repo, NoteEntry{
		ID: "n1", Body: "why Bar locks", SymbolID: "internal/foo.go::Bar",
	}))
	require.NoError(t, s.UpsertMemory(repo, MemoryEntry{
		ID: "m1", Body: "Bar must hold the lock",
		SymbolIDs: []string{"internal/foo.go::Bar", "internal/foo.go::Baz"},
		AutoLinks: []string{"internal/foo.go::Baz"},
	}))
	require.NoError(t, s.UpsertNotebook(repo, NotebookRow{
		ID: "nb1", Title: "runbook", SymbolIDs: []string{"internal/foo.go::Bar"},
	}))
	require.NoError(t, s.UpsertSuppression(repo, SuppressionEntry{
		IdentityKey: "k1", SymbolID: "internal/foo.go::Bar", Rule: "r",
	}))

	live := map[string]bool{
		"gortex/internal/foo.go::Bar": true,
		"gortex/internal/foo.go::Baz": true,
	}
	n, err := s.PrefixSymbolIDs(repo, prefixIfMissing(live, "gortex"))
	require.NoError(t, err)
	assert.Positive(t, n, "the rewrite must report the rows it changed")

	notes, err := s.LoadNotesRows(repo)
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, "gortex/internal/foo.go::Bar", notes[0].SymbolID)

	mems, err := s.LoadMemoriesRows(repo)
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Equal(t, []string{"gortex/internal/foo.go::Bar", "gortex/internal/foo.go::Baz"}, mems[0].SymbolIDs)
	assert.Equal(t, []string{"gortex/internal/foo.go::Baz"}, mems[0].AutoLinks,
		"auto_links anchor the same way symbol_ids do and must migrate with them")

	books, err := s.LoadNotebookRows(repo)
	require.NoError(t, err)
	require.Len(t, books, 1)
	assert.Equal(t, []string{"gortex/internal/foo.go::Bar"}, books[0].SymbolIDs)

	sups, err := s.LoadSuppressions(repo)
	require.NoError(t, err)
	require.Len(t, sups, 1)
	assert.Equal(t, "gortex/internal/foo.go::Bar", sups[0].SymbolID)
}

// The rewriter is only allowed to move an anchor that resolves nowhere onto
// one that resolves. Anything else must be left exactly as stored — this runs
// over irreplaceable user data.
func TestPrefixSymbolIDsLeavesResolvableAndUnknownAnchorsAlone(t *testing.T) {
	s := openAnchorSidecar(t)
	const repo = "repo-key"

	require.NoError(t, s.UpsertMemory(repo, MemoryEntry{
		ID: "m1", Body: "b",
		SymbolIDs: []string{
			"gortex/internal/foo.go::Bar", // already correct
			"deleted/gone.go::Gone",       // resolves nowhere, prefixed or not
		},
	}))

	live := map[string]bool{"gortex/internal/foo.go::Bar": true}
	_, err := s.PrefixSymbolIDs(repo, prefixIfMissing(live, "gortex"))
	require.NoError(t, err)

	mems, err := s.LoadMemoriesRows(repo)
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Equal(t, []string{"gortex/internal/foo.go::Bar", "deleted/gone.go::Gone"}, mems[0].SymbolIDs,
		"a resolvable anchor and an unresolvable one must both survive untouched")
}

// migration_marks makes this one-shot per repo: a second run must not
// re-prefix an already-prefixed id into gortex/gortex/...
func TestPrefixSymbolIDsIsOneShotPerRepo(t *testing.T) {
	s := openAnchorSidecar(t)
	const repo = "repo-key"
	require.NoError(t, s.UpsertMemory(repo, MemoryEntry{
		ID: "m1", Body: "b", SymbolIDs: []string{"internal/foo.go::Bar"},
	}))

	// A deliberately reckless rewriter that always prefixes. If the mark
	// did not hold, the second run would double-prefix.
	always := SymbolIDRewriter(func(oldID string) string {
		if strings.HasPrefix(oldID, "gortex/") {
			return "gortex/" + oldID // would corrupt, if ever reached twice
		}
		return "gortex/" + oldID
	})

	first, err := s.PrefixSymbolIDs(repo, always)
	require.NoError(t, err)
	assert.Equal(t, 1, first)

	second, err := s.PrefixSymbolIDs(repo, always)
	require.NoError(t, err)
	assert.Zero(t, second, "the second run must be a no-op")

	mems, err := s.LoadMemoriesRows(repo)
	require.NoError(t, err)
	assert.Equal(t, []string{"gortex/internal/foo.go::Bar"}, mems[0].SymbolIDs)
}

// One repo's rewrite must not touch another's rows, and must not mark them
// migrated.
func TestPrefixSymbolIDsIsScopedToItsRepo(t *testing.T) {
	s := openAnchorSidecar(t)
	require.NoError(t, s.UpsertMemory("repo-a", MemoryEntry{
		ID: "m1", Body: "b", SymbolIDs: []string{"internal/foo.go::Bar"},
	}))
	require.NoError(t, s.UpsertMemory("repo-b", MemoryEntry{
		ID: "m2", Body: "b", SymbolIDs: []string{"internal/foo.go::Bar"},
	}))

	live := map[string]bool{"alpha/internal/foo.go::Bar": true}
	_, err := s.PrefixSymbolIDs("repo-a", prefixIfMissing(live, "alpha"))
	require.NoError(t, err)

	a, err := s.LoadMemoriesRows("repo-a")
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha/internal/foo.go::Bar"}, a[0].SymbolIDs)

	b, err := s.LoadMemoriesRows("repo-b")
	require.NoError(t, err)
	assert.Equal(t, []string{"internal/foo.go::Bar"}, b[0].SymbolIDs,
		"another repo's anchors must be untouched")

	// repo-b is still unmarked, so its own rewrite still runs.
	liveB := map[string]bool{"beta/internal/foo.go::Bar": true}
	n, err := s.PrefixSymbolIDs("repo-b", prefixIfMissing(liveB, "beta"))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestPrefixSymbolIDsNilRewriterIsANoOp(t *testing.T) {
	s := openAnchorSidecar(t)
	n, err := s.PrefixSymbolIDs("repo", nil)
	require.NoError(t, err)
	assert.Zero(t, n)
}
