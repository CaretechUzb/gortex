package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/search"
)

// forceCaseInsensitive flips the process-global pathkey.CaseInsensitivePaths
// for the test and restores it afterwards. Tests using it must not run in
// parallel because the flag is process-global.
func forceCaseInsensitive(t *testing.T, v bool) {
	t.Helper()
	prev := pathkey.CaseInsensitivePaths
	pathkey.CaseInsensitivePaths = v
	t.Cleanup(func() { pathkey.CaseInsensitivePaths = prev })
}

// caseVariantOf returns p with the case of its final path component
// toggled, so it names the same directory on a case-insensitive
// filesystem while differing byte-wise.
func caseVariantOf(p string) string {
	return filepath.Join(filepath.Dir(p), swapCaseASCII(filepath.Base(p)))
}

func swapCaseASCII(s string) string {
	b := []byte(s)
	for i := range b {
		switch {
		case b[i] >= 'a' && b[i] <= 'z':
			b[i] -= 32
		case b[i] >= 'A' && b[i] <= 'Z':
			b[i] += 32
		}
	}
	return string(b)
}

// Tracking a case-only variant of an already-tracked directory must be a
// no-op: no second entry, the repo count stays 1, and the daemon stays in
// single-repo (unprefixed) mode. This is the #270 macOS regression.
func TestTrackRepoCtx_CaseVariantIsNoOp(t *testing.T) {
	forceCaseInsensitive(t, true)
	mi, dir := indexSingleRepoForTest(t)
	require.False(t, mi.IsMultiRepo())

	variant := caseVariantOf(dir)
	if _, err := os.Stat(variant); err != nil {
		t.Skipf("host filesystem is case-sensitive; %q does not resolve", variant)
	}

	res, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: variant})
	require.NoError(t, err)
	require.Nil(t, res, "tracking a case variant of an already-tracked dir must be a no-op")

	assert.Len(t, mi.AllMetadata(), 1, "repo count must stay 1")
	assert.False(t, mi.IsMultiRepo(), "mode must stay single-repo")
}

// ScopeForCWD is pure string folding (no stat), so a case-mismatched cwd
// resolves to the tracked repo on any platform once folding is forced on.
// This is the #277 Windows 0-tools scenario at the workspace layer.
func TestScopeForCWD_CaseMismatchedCwd(t *testing.T) {
	forceCaseInsensitive(t, true)
	mi, dir := indexSingleRepoForTest(t)

	variant := caseVariantOf(dir)
	_, _, prefix, ok := mi.ScopeForCWD(variant)
	require.True(t, ok, "case-variant cwd must resolve to the tracked repo")
	assert.Equal(t, "myrepo", prefix)

	// A sub-path with mismatched case resolves too.
	_, _, prefix, ok = mi.ScopeForCWD(filepath.Join(variant, "pkg", "file.go"))
	require.True(t, ok)
	assert.Equal(t, "myrepo", prefix)
}

// The reverse direction folds too: a case-variant of the PARENT above
// the tracked repo (a workspace-root cwd) still resolves to the
// contained repo. The #277 scenario one level up. The repo sits under a
// LETTERED intermediate directory on purpose: t.TempDir's own basename
// is digits-only, which swapCaseASCII leaves unchanged — toggling it
// would exercise nothing.
func TestScopeForCWD_CaseMismatchedWorkspaceRoot(t *testing.T) {
	forceCaseInsensitive(t, true)
	dir := setupRepoDir(t, filepath.Join("Work", "myrepo"))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: dir, Name: "myrepo"}},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)
	mi := NewMultiIndexer(graph.New(), newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	parent := caseVariantOf(filepath.Dir(dir))
	require.NotEqual(t, filepath.Dir(dir), parent,
		"the case variant must differ byte-wise or the test is vacuous")
	_, _, prefix, ok := mi.ScopeForCWD(parent)
	require.True(t, ok, "case-variant workspace root must resolve to the contained repo")
	assert.Equal(t, "myrepo", prefix)
}

// ScopeForCWD must NOT resolve a genuinely unrelated cwd.
func TestScopeForCWD_UnrelatedCwdMisses(t *testing.T) {
	forceCaseInsensitive(t, true)
	mi, dir := indexSingleRepoForTest(t)
	_, _, _, ok := mi.ScopeForCWD(filepath.Join(filepath.Dir(dir), "somewhere-else"))
	assert.False(t, ok)
}
