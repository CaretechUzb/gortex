package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepoPathMissing_LiveDirectory(t *testing.T) {
	assert.False(t, RepoPathMissing(t.TempDir()))
}

func TestRepoPathMissing_DeletedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.False(t, RepoPathMissing(dir))

	require.NoError(t, os.RemoveAll(dir))
	assert.True(t, RepoPathMissing(dir),
		"a tracked path deleted from disk must report missing")
}

// A file where a checkout used to be is just as unindexable as nothing
// at all, and the user still has to untrack it.
func TestRepoPathMissing_FileInsteadOfDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.WriteFile(path, []byte("not a repo"), 0o644))
	assert.True(t, RepoPathMissing(path))
}

func TestRepoPathMissing_EmptyPath(t *testing.T) {
	assert.True(t, RepoPathMissing(""),
		"an entry with no path can never be indexed either")
}

// The one case that must stay quiet: stat failed for a reason other than
// "it isn't there". Telling a user their repo was deleted when the truth
// is a permission denial sends them to `untrack` for a repo that is fine.
func TestRepoPathMissing_UnreadableParentIsNotMissing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root traverses 0000 directories")
	}
	parent := filepath.Join(t.TempDir(), "locked")
	repo := filepath.Join(parent, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	require.NoError(t, os.Chmod(parent, 0o000))
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	assert.False(t, RepoPathMissing(repo),
		"an undeterminable stat must not be reported as a deleted repo")
}

func TestMissingRepoPaths_KeepsInputOrder(t *testing.T) {
	live := t.TempDir()
	goneA := filepath.Join(t.TempDir(), "a")
	goneB := filepath.Join(t.TempDir(), "b")

	got := MissingRepoPaths([]RepoEntry{
		{Path: goneA},
		{Path: live},
		{Path: goneB},
	})
	assert.Equal(t, []string{goneA, goneB}, got)
}

func TestMissingRepoPaths_AllLive(t *testing.T) {
	assert.Empty(t, MissingRepoPaths([]RepoEntry{{Path: t.TempDir()}}))
}
