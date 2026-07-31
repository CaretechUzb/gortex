package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/persistence"
)

// TestNotesManager_MigratesLegacyGobGz proves a pre-existing
// notes.gob.gz is imported into the sidecar on first manager open and
// the legacy file is renamed to *.bak (never deleted).
func TestNotesManager_MigratesLegacyGobGz(t *testing.T) {
	cache := tempSidecarDir(t)
	repo := "/tmp/migrate-notes-repo"
	legacyDir := persistence.NotesDir(cache, repo)
	require.NoError(t, persistence.SaveNotes(legacyDir, &persistence.NoteStore{
		Entries: []persistence.NoteEntry{
			{ID: "nt-legacy", Body: "legacy note", SessionID: "s1", Pinned: true},
		},
	}))

	nm := newNotesManager(cache, repo)
	require.True(t, nm.HasData(), "legacy note imported")
	got, ok := nm.Get("nt-legacy")
	require.True(t, ok)
	assert.Equal(t, "legacy note", got.Body)
	assert.True(t, got.Pinned)

	// Legacy gob.gz renamed to .bak.
	_, errOrig := os.Stat(filepath.Join(legacyDir, "notes.gob.gz"))
	assert.True(t, os.IsNotExist(errOrig), "legacy notes.gob.gz renamed away")
	_, errBak := os.Stat(filepath.Join(legacyDir, "notes.gob.gz.bak"))
	assert.NoError(t, errBak, ".bak preserved")

	// A fresh manager over the same cache sees the migrated note from
	// the sidecar and does not re-import (idempotent).
	nm2 := newNotesManager(cache, repo)
	assert.Equal(t, 1, nm2.Count())
}

// TestMemoryManager_MigratesLegacyGobGz proves the same for memories.
func TestMemoryManager_MigratesLegacyGobGz(t *testing.T) {
	cache := tempSidecarDir(t)
	repo := "/tmp/migrate-mem-repo"
	legacyDir := persistence.MemoriesDir(cache, repo)
	require.NoError(t, persistence.SaveMemories(legacyDir, &persistence.MemoryStore{
		Entries: []persistence.MemoryEntry{
			{ID: "mem-legacy", Body: "legacy memory", Kind: "invariant", Importance: 5},
		},
	}))

	mm := newMemoryManager(cache, repo)
	out := mm.Query(MemoryQueryFilter{})
	require.Len(t, out, 1)
	assert.Equal(t, "mem-legacy", out[0].ID)
	assert.Equal(t, "invariant", out[0].Kind)

	_, errBak := os.Stat(filepath.Join(legacyDir, "memories.gob.gz.bak"))
	assert.NoError(t, errBak)
}

// TestScopeStore_MigratesLegacyJSON proves a pre-existing scopes.json
// is imported into the sidecar and renamed to *.bak.
func TestScopeStore_MigratesLegacyJSON(t *testing.T) {
	dir := tempSidecarDir(t)
	legacyPath := filepath.Join(dir, "scopes.json")
	require.NoError(t, os.WriteFile(legacyPath,
		[]byte(`[{"name":"backend","description":"be","repos":["api","core"]}]`), 0o644))

	st := newScopeStore(legacyPath)
	got, ok := st.get("backend")
	require.True(t, ok, "legacy scope imported")
	assert.Equal(t, []string{"api", "core"}, got.Repos)

	_, errBak := os.Stat(legacyPath + ".bak")
	assert.NoError(t, errBak, ".bak preserved")

	// A fresh store over the same dir reads from the sidecar.
	st2 := newScopeStore(legacyPath)
	_, ok = st2.get("backend")
	assert.True(t, ok)
}

// TestNotebookManager_MigratesLegacyMarkdown proves pre-existing
// <repo>/.gortex/notebook/<id>.md files are imported into the sidecar
// and renamed to *.bak.
func TestNotebookManager_MigratesLegacyMarkdown(t *testing.T) {
	repo := tempNotebookRepo(t)
	mdDir := filepath.Join(repo, ".gortex", "notebook")
	require.NoError(t, os.MkdirAll(mdDir, 0o755))
	md := notebookMarshal(notebookEntry{
		ID:    "nbold",
		Title: "legacy nb",
		Tags:  []string{"design"},
		Body:  "legacy body\n",
	})
	require.NoError(t, os.WriteFile(filepath.Join(mdDir, "nbold.md"), []byte(md), 0o644))

	nm := newNotebookManager(repo)
	got, ok := nm.Get("nbold")
	require.True(t, ok, "legacy markdown entry imported")
	assert.Equal(t, "legacy nb", got.Title)
	assert.Contains(t, got.Body, "legacy body")
	assert.Equal(t, []string{"design"}, got.Tags)

	_, errBak := os.Stat(filepath.Join(mdDir, "nbold.md.bak"))
	assert.NoError(t, errBak, ".bak preserved")
}

// TestNotebookManager_PersistsAcrossRestart proves notebook entries
// survive a manager restart (the sidecar is the durable store).
func TestNotebookManager_PersistsAcrossRestart(t *testing.T) {
	repo := tempNotebookRepo(t)
	nm1 := newNotebookManager(repo)
	saved, err := nm1.Save(notebookEntry{Title: "t1", Body: "b1", Tags: []string{"x"}})
	require.NoError(t, err)

	nm2 := newNotebookManager(repo)
	got, ok := nm2.Get(saved.ID)
	require.True(t, ok, "entry survives a manager restart via the sidecar")
	assert.Equal(t, "t1", got.Title)
	assert.Equal(t, "b1", got.Body)
}
