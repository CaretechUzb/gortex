package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

func newContentCensusFixture(t *testing.T, imageLimit int64) *sizeCensusFixture {
	t.Helper()
	f := &sizeCensusFixture{t: t, root: t.TempDir(), dbPath: filepath.Join(t.TempDir(), "graph.sqlite")}
	t.Cleanup(f.close)
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "main.go"), []byte("package fixture\nfunc Main() {}\n"), 0600))
	writeSizeCensusGIF(t, filepath.Join(f.root, "demo.gif"), 512)
	info, err := os.Stat(filepath.Join(f.root, "demo.gif"))
	require.NoError(t, err)
	f.mtime = info.ModTime()
	f.open(4096) // the content cap, never MaxFileSize, rejects this GIF
	f.idx.config.Content = config.ContentAdmissionConfig{MaxImageBytes: imageLimit}
	_, err = f.idx.IndexCtx(context.Background(), f.root)
	require.NoError(t, err)
	f.idx.RunDeferredPasses(context.Background())
	return f
}

func reopenContentCensusFixture(f *sizeCensusFixture, imageLimit int64) {
	f.reopen(4096)
	f.idx.config.Content = config.ContentAdmissionConfig{MaxImageBytes: imageLimit}
}

func TestContentSkipCensusColdGIFRepeatedRestartNoop(t *testing.T) {
	f := newContentCensusFixture(t, 128)
	stub := f.fileNode()
	require.Equal(t, true, stub.Meta["skipped_due_to_content"])
	require.Equal(t, skipReasonLargeImage, stub.Meta["skip_reason"])
	require.EqualValues(t, 512, stub.Meta["file_size_bytes"])
	require.NotEqual(t, true, stub.Meta["skipped_due_to_size"])
	require.Less(t, int64(512), f.idx.config.MaxFileSize)
	seeded := []*graph.Edge{
		{From: "repo/main.go", To: "repo/demo.gif", Kind: "co_change", FilePath: "repo/main.go"},
		{From: "repo/demo.gif", To: "repo/main.go", Kind: "co_change", FilePath: "repo/demo.gif"},
	}
	f.store.AddBatch(nil, seeded)
	edges := f.store.GetOutEdgesByNodeIDs([]string{"repo/main.go", "repo/demo.gif"})
	for _, want := range seeded {
		found := 0
		for _, got := range edges[want.From] {
			if got.From == want.From && got.To == want.To && got.Kind == want.Kind {
				found++
			}
		}
		require.Equal(t, 1, found, "incoming/outgoing co_change prerequisite")
	}
	before := sizeCensusGraphRows(t, f.store)
	assert.Equal(t, f.mtime.UnixNano(), f.store.LoadFileMtimes("repo")["demo.gif"], "cold content stub needs a published version receipt")
	for restart := 0; restart < 2; restart++ {
		reopenContentCensusFixture(f, 128)
		changed, deleted, detected, err := f.idx.changedSinceMtimesCensus(f.root)
		require.NoError(t, err)
		assert.Empty(t, changed)
		require.Empty(t, deleted)
		require.Equal(t, 2, detected)
		result, err := f.idx.incrementalReindexPathsMode(f.root, []string{"main.go", "demo.gif"}, incrementalPathMode{})
		require.NoError(t, err)
		assert.Zero(t, result.StaleFileCount)
		assert.Equal(t, before, sizeCensusGraphRows(t, f.store), "unchanged content-policy asset must not lose co_change edges")
	}
}

func TestContentSkipCensusEqualMtimePolicyChanges(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after int64
		stale, skip   bool
	}{
		{"cap_increase_admits", 128, 1024, true, false},
		{"strict_boundary_admits", 128, 512, true, false},
		{"cap_decrease_skips", 1024, 128, true, true},
		{"same_rejection_no_work", 128, 256, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContentCensusFixture(t, tc.before)
			f.establishKnownReceipt() // isolate policy comparison from missing cold receipt
			reopenContentCensusFixture(f, tc.after)
			changed, deleted, detected, err := f.idx.changedSinceMtimesCensus(f.root)
			require.NoError(t, err)
			require.Empty(t, deleted)
			require.Equal(t, 2, detected)
			if tc.stale {
				require.Equal(t, []string{"demo.gif"}, changed)
				f.applyCensus(changed)
			} else {
				require.Empty(t, changed)
			}
			node := f.fileNode()
			if tc.skip {
				require.Equal(t, true, node.Meta["skipped_due_to_content"])
				require.Equal(t, skipReasonLargeImage, node.Meta["skip_reason"])
			} else {
				require.NotEqual(t, true, node.Meta["skipped_due_to_content"])
				found := false
				for _, candidate := range f.store.GetRepoNodes("repo") {
					if candidate.FilePath == "repo/demo.gif" && candidate.Kind != graph.KindFile {
						found = true
					}
				}
				require.True(t, found, "eligible real GIF must be parsed, not merely lose a flag")
			}
			reopenContentCensusFixture(f, tc.after)
			f.census(nil, nil, 2)
		})
	}
}
