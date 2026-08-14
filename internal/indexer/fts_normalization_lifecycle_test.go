package indexer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

var errFTSLifecycleWrite = errors.New("injected FTS replacement failure")
var errFTSLifecycleReset = errors.New("injected FTS reset failure")
var errFTSLifecycleBatch = errors.New("injected FTS batch failure")
var errFTSLifecycleFinalize = errors.New("injected FTS finalization failure")

type ftsLifecycleProbeStore struct {
	*store_sqlite.Store
	replaceCalls int
	markerCalls  int
	resetCalls   int
	batchCalls   int
	replaceErr   error
	resetErr     error
	batchFailAt  int
	finalizeErr  error
}

func (s *ftsLifecycleProbeStore) ReplaceSymbolFTS(
	repoPrefix string,
	produce func(emit func([]graph.SymbolFTSItem) error) error,
) error {
	s.replaceCalls++
	if s.replaceErr != nil {
		return s.replaceErr
	}
	return s.Store.ReplaceSymbolFTS(repoPrefix, produce)
}

func (s *ftsLifecycleProbeStore) SetSymbolFTSNormalization(repoPrefix, mode string) error {
	s.markerCalls++
	return s.Store.SetSymbolFTSNormalization(repoPrefix, mode)
}

func (s *ftsLifecycleProbeStore) ResetSymbolFTS(repoPrefix string) error {
	s.resetCalls++
	if s.resetErr != nil {
		return s.resetErr
	}
	return s.Store.ResetSymbolFTS(repoPrefix)
}

func (s *ftsLifecycleProbeStore) BatchUpsertSymbolFTS(items []graph.SymbolFTSItem) error {
	s.batchCalls++
	if s.batchFailAt > 0 && s.batchCalls == s.batchFailAt {
		return errFTSLifecycleBatch
	}
	return s.Store.BatchUpsertSymbolFTS(items)
}

func (s *ftsLifecycleProbeStore) BuildSymbolIndex() error {
	if s.finalizeErr != nil {
		return s.finalizeErr
	}
	return s.Store.BuildSymbolIndex()
}

func TestFTSNormalizationMarkerFollowsAuthoritativeIndexModes(t *testing.T) {
	cases := []struct {
		name            string
		shadowMaxFiles  string
		streaming       string
		wantReplacement bool
	}{
		{name: "direct", shadowMaxFiles: "0", streaming: "0", wantReplacement: true},
		{name: "streaming", shadowMaxFiles: "0", streaming: "1", wantReplacement: true},
		{name: "cold_shadow", shadowMaxFiles: "1000000", streaming: "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORTEX_SHADOW_MAX_FILES", tc.shadowMaxFiles)
			t.Setenv("GORTEX_STREAMING_FLUSH", tc.streaming)
			t.Setenv("GORTEX_STREAMING_CHUNK_SIZE", "1")

			repo := t.TempDir()
			writeFile(t, filepath.Join(repo, "orders.go"), `package shop

func ReconcilePonies() int { return 1 }
`)
			base := openFTSLifecycleStore(t)
			probe := &ftsLifecycleProbeStore{Store: base}
			idx := newTestIndexer(probe)

			_, err := idx.Index(repo)
			require.NoError(t, err)

			got, ok, err := base.GetSymbolFTSNormalization(idx.RepoPrefix())
			require.NoError(t, err)
			require.True(t, ok, "authoritative index did not persist an FTS normalization marker")
			require.Equal(t, search.FTSNormalizationMode(), got)
			require.Positive(t, probe.markerCalls)
			if tc.wantReplacement {
				require.Positive(t, probe.replaceCalls, "direct durable path must publish an authoritative corpus")
			}
		})
	}
}

func TestFTSNormalizationCleanWarmReconcileIsNoOp(t *testing.T) {
	base := openFTSLifecycleStore(t)
	probe := &ftsLifecycleProbeStore{Store: base}
	idx := newTestIndexer(probe)
	require.NoError(t, base.SetSymbolFTSNormalization(idx.RepoPrefix(), search.FTSNormalizationMode()))

	_, err := idx.cleanCensusResult(context.Background(), 0, time.Now())
	require.NoError(t, err)
	require.Zero(t, probe.replaceCalls, "a current warm corpus must not be rebuilt")
	require.Zero(t, probe.markerCalls, "a current marker must not be rewritten")
}

func TestFTSNormalizationAuthoritativeFailureLeavesPendingMarkerForRetry(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
	t.Setenv("GORTEX_STREAMING_FLUSH", "0")

	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "orders.go"), `package shop

func ReconcilePonies() int { return 1 }
`)
	base := openFTSLifecycleStore(t)
	probe := &ftsLifecycleProbeStore{Store: base, finalizeErr: errFTSLifecycleFinalize}
	idx := newTestIndexer(probe)
	current := search.FTSNormalizationMode()
	require.NoError(t, base.SetSymbolFTSNormalization(idx.RepoPrefix(), current))

	_, err := idx.Index(repo)
	require.ErrorIs(t, err, errFTSLifecycleFinalize)
	pending, ok, readErr := base.GetSymbolFTSNormalization(idx.RepoPrefix())
	require.NoError(t, readErr)
	require.True(t, ok)
	require.NotEqual(t, current, pending, "failed authoritative write left a falsely-current marker")

	probe.finalizeErr = nil
	probe.replaceCalls = 0
	probe.markerCalls = 0
	rebuilt, err := idx.reconcileSymbolFTSNormalization(nil)
	require.NoError(t, err)
	require.True(t, rebuilt, "pending marker did not force an authoritative retry")
	require.Positive(t, probe.replaceCalls)
	require.Positive(t, probe.markerCalls)
	got, ok, readErr := base.GetSymbolFTSNormalization(idx.RepoPrefix())
	require.NoError(t, readErr)
	require.True(t, ok)
	require.Equal(t, current, got)
}

func TestFTSNormalizationShadowFailureLeavesPendingMarkerForRetry(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "1000000")
	t.Setenv("GORTEX_STREAMING_FLUSH", "0")

	for _, tc := range []struct {
		name        string
		resetErr    error
		batchFailAt int
		finalizeErr error
		wantErr     error
	}{
		{name: "reset", resetErr: errFTSLifecycleReset, wantErr: errFTSLifecycleReset},
		{name: "batch", batchFailAt: 1, wantErr: errFTSLifecycleBatch},
		{name: "finalization", finalizeErr: errFTSLifecycleFinalize, wantErr: errFTSLifecycleFinalize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			writeFile(t, filepath.Join(repo, "orders.go"), `package shop

func ReconcilePonies() int { return 1 }
`)
			base := openFTSLifecycleStore(t)
			probe := &ftsLifecycleProbeStore{
				Store:       base,
				resetErr:    tc.resetErr,
				batchFailAt: tc.batchFailAt,
				finalizeErr: tc.finalizeErr,
			}
			idx := newTestIndexer(probe)
			current := search.FTSNormalizationMode()
			require.NoError(t, base.SetSymbolFTSNormalization(idx.RepoPrefix(), current))

			_, err := idx.Index(repo)
			require.ErrorIs(t, err, tc.wantErr)
			pending, ok, readErr := base.GetSymbolFTSNormalization(idx.RepoPrefix())
			require.NoError(t, readErr)
			require.True(t, ok)
			require.NotEqual(t, current, pending, "failed shadow publication left a falsely-current marker")

			probe.resetErr = nil
			probe.batchFailAt = 0
			probe.finalizeErr = nil
			probe.replaceCalls = 0
			probe.markerCalls = 0
			rebuilt, err := idx.reconcileSymbolFTSNormalization(nil)
			require.NoError(t, err)
			require.True(t, rebuilt, "pending shadow marker did not force an authoritative retry")
			require.Positive(t, probe.replaceCalls)
			got, ok, readErr := base.GetSymbolFTSNormalization(idx.RepoPrefix())
			require.NoError(t, readErr)
			require.True(t, ok)
			require.Equal(t, current, got)
		})
	}
}

func TestFTSNormalizationMarkerDoesNotAdvanceOnRebuildFailure(t *testing.T) {
	cases := []struct {
		name        string
		replaceErr  error
		finalizeErr error
		wantErr     error
	}{
		{name: "write", replaceErr: errFTSLifecycleWrite, wantErr: errFTSLifecycleWrite},
		{name: "finalization", finalizeErr: errFTSLifecycleFinalize, wantErr: errFTSLifecycleFinalize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := openFTSLifecycleStore(t)
			probe := &ftsLifecycleProbeStore{
				Store:       base,
				replaceErr:  tc.replaceErr,
				finalizeErr: tc.finalizeErr,
			}
			idx := newTestIndexer(probe)
			prior := mismatchedFTSNormalizationMode()
			require.NoError(t, base.SetSymbolFTSNormalization(idx.RepoPrefix(), prior))

			rebuilt, err := idx.reconcileSymbolFTSNormalization(nil)
			require.ErrorIs(t, err, tc.wantErr)
			require.False(t, rebuilt)
			require.Positive(t, probe.markerCalls, "rebuild did not invalidate the old marker before writing")
			got, ok, readErr := base.GetSymbolFTSNormalization(idx.RepoPrefix())
			require.NoError(t, readErr)
			require.True(t, ok)
			require.NotEqual(t, search.FTSNormalizationMode(), got, "incomplete rebuild left a falsely-current marker")
		})
	}
}

func TestFTSNormalizationReconcilesBeforeIncrementalMutation(t *testing.T) {
	if os.Getenv("GORTEX_TEST_FTS_NORMALIZATION_CHILD") == "1" {
		runFTSNormalizationIncrementalChild(t)
		return
	}

	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(exe, "-test.run=^TestFTSNormalizationReconcilesBeforeIncrementalMutation$")
	cmd.Env = append(envWithoutFTSLifecycleKeys(os.Environ()),
		"GORTEX_TEST_FTS_NORMALIZATION_CHILD=1",
		"GORTEX_FTS_STEMMING=1",
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	require.NoError(t, cmd.Run(), output.String())
}

func runFTSNormalizationIncrementalChild(t *testing.T) {
	require.Equal(t, "porter-v1", search.FTSNormalizationMode())

	for _, tc := range []struct {
		name  string
		paths func(repo, changed string) []string
		mode  incrementalPathMode
	}{
		{
			name:  "scoped",
			paths: func(_ string, changed string) []string { return []string{changed} },
			mode:  incrementalPathMode{forceExplicitFiles: true},
		},
		{
			name:  "full_root",
			paths: func(_, _ string) []string { return nil },
			mode:  incrementalPathMode{detectDeletions: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORTEX_SHADOW_MAX_FILES", "1000000")
			t.Setenv("GORTEX_STREAMING_FLUSH", "0")

			repo := t.TempDir()
			changed := filepath.Join(repo, "first.go")
			writeFile(t, changed, `package herd

func PoniesFirst() int { return 1 }
`)
			writeFile(t, filepath.Join(repo, "second.go"), `package herd

func PoniesSecond() int { return 2 }
`)

			base := openFTSLifecycleStore(t)
			probe := &ftsLifecycleProbeStore{Store: base}
			idx := newTestIndexer(probe)
			_, err := idx.Index(repo)
			require.NoError(t, err)

			oldItems := make([]graph.SymbolFTSItem, 0, 2)
			for _, file := range []string{"first.go", "second.go"} {
				for _, node := range base.GetFileNodes(file) {
					if node != nil && node.Kind == graph.KindFunction && strings.HasPrefix(node.Name, "Ponies") {
						oldItems = append(oldItems, graph.SymbolFTSItem{NodeID: node.ID, Tokens: "ponies " + strings.ToLower(node.Name)})
					}
				}
			}
			require.Len(t, oldItems, 2)
			require.NoError(t, base.ReplaceSymbolFTS(idx.RepoPrefix(), func(emit func([]graph.SymbolFTSItem) error) error {
				return emit(oldItems)
			}))
			require.NoError(t, base.BuildSymbolIndex())
			require.NoError(t, base.SetSymbolFTSNormalization(idx.RepoPrefix(), ""))
			probe.replaceCalls = 0
			probe.markerCalls = 0

			writeFile(t, changed, `package herd

func PoniesFirst() int { return 11 }
`)
			future := time.Now().Add(2 * time.Second)
			require.NoError(t, os.Chtimes(changed, future, future))

			result, err := idx.incrementalReindexPathsMode(repo, tc.paths(repo, changed), tc.mode)
			require.NoError(t, err)
			require.Equal(t, 1, result.StaleFileCount)
			require.Positive(t, probe.replaceCalls, "mode mismatch was not repaired before the incremental patch")

			gotMode, ok, err := base.GetSymbolFTSNormalization(idx.RepoPrefix())
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, search.FTSNormalizationMode(), gotMode)

			names := make([]string, 0, 2)
			for _, hit := range searchAll(t, base, "pony") {
				if node := base.GetNode(hit.NodeID); node != nil {
					names = append(names, node.Name)
				}
			}
			require.Contains(t, names, "PoniesFirst")
			require.Contains(t, names, "PoniesSecond", "unchanged old-mode posting survived beside the patched corpus")
		})
	}
}

func openFTSLifecycleStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "fts-lifecycle.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func mismatchedFTSNormalizationMode() string {
	if search.FTSNormalizationMode() == "" {
		return "porter-v1"
	}
	return ""
}

func envWithoutFTSLifecycleKeys(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, "GORTEX_TEST_FTS_NORMALIZATION_CHILD=") ||
			strings.HasPrefix(entry, "GORTEX_FTS_STEMMING=") {
			continue
		}
		out = append(out, entry)
	}
	return out
}
