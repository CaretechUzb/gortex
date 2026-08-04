package indexer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type pollerReceiptPageCall struct {
	after          string
	highWater      string
	limit          int
	includeContent bool
}

type pollerReceiptStore struct {
	*graph.Graph
	highWater     string
	rows          []graph.FileReceipt
	calls         []pollerReceiptPageCall
	mtimeWriteErr error
}

func (s *pollerReceiptStore) BulkSetFileMtimes(string, map[string]int64) error {
	return s.mtimeWriteErr
}

func (s *pollerReceiptStore) FileReceiptHighWater(string) (string, error) {
	if s.highWater != "" {
		return s.highWater, nil
	}
	if len(s.rows) == 0 {
		return "", nil
	}
	return s.rows[len(s.rows)-1].FilePath, nil
}

func (s *pollerReceiptStore) FileReceiptPage(
	_, afterPath, highWaterPath string,
	limit int,
	includeContent bool,
) ([]graph.FileReceipt, error) {
	s.calls = append(s.calls, pollerReceiptPageCall{
		after: afterPath, highWater: highWaterPath, limit: limit, includeContent: includeContent,
	})
	out := make([]graph.FileReceipt, 0, limit)
	for _, row := range s.rows {
		if row.FilePath <= afterPath || row.FilePath > highWaterPath {
			continue
		}
		if !includeContent {
			row.ContentHash = ""
			row.Size = 0
		}
		out = append(out, row)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func TestPollMtimePageRowsTargetsOneHourAndCapsHeap(t *testing.T) {
	tests := []struct {
		name     string
		tracked  int
		interval time.Duration
	}{
		{name: "small floor", tracked: 1000, interval: 15 * time.Second},
		{name: "ten thousand at ceiling", tracked: 10_000, interval: 10 * time.Minute},
		{name: "non-divisible interval", tracked: 1001, interval: 17 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := pollMtimePageRows(tc.tracked, tc.interval)
			require.Positive(t, rows)
			require.LessOrEqual(t, rows, pollMtimePageMaxRows)
			polls := (tc.tracked + rows - 1) / rows
			assert.LessOrEqual(t, time.Duration(polls)*tc.interval,
				pollMtimeRotationTarget+tc.interval,
				"an uncapped typical rotation should finish in about one hour")
		})
	}

	assert.Equal(t, pollMtimePageMaxRows, pollMtimePageRows(100_000, 10*time.Minute),
		"very large repositories must honor the receipt-heap cap")
	assert.Zero(t, pollMtimePageRows(0, time.Minute))
}

func TestPollerReceiptRotationResetsAfterDeletedHighWater(t *testing.T) {
	store := &pollerReceiptStore{
		Graph:     graph.New(),
		highWater: "b.go",
		rows: []graph.FileReceipt{
			{FilePath: "a.go", MtimeNS: 1},
			{FilePath: "b.go", MtimeNS: 2},
		},
	}
	p := &Poller{}
	var rotation fileReceiptRotation

	page, exhausted, scan, err := p.readReceiptPage(store, "repo", &rotation, 1, false)
	require.NoError(t, err)
	require.True(t, scan)
	require.False(t, exhausted)
	require.Equal(t, []graph.FileReceipt{{FilePath: "a.go", MtimeNS: 1}}, page)
	p.advanceReceiptRotation(&rotation, page, len(page), exhausted)
	require.Equal(t, "a.go", rotation.afterPath)
	require.Equal(t, "b.go", rotation.highWaterPath)

	// Delete the frozen upper bound between exact-size pages. The empty page is
	// terminal for this existing scan (not "no durable receipts") and resets it.
	store.rows = store.rows[:1]
	page, exhausted, scan, err = p.readReceiptPage(store, "repo", &rotation, 1, false)
	require.NoError(t, err)
	require.True(t, scan)
	require.True(t, exhausted)
	require.Empty(t, page)
	p.advanceReceiptRotation(&rotation, page, 0, exhausted)
	require.Equal(t, fileReceiptRotation{}, rotation)
}

func TestPollerDetectsSameMtimeSameSizeEditFromSQLiteReceipt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	beforeSource := "package p\nfunc Alpha() int { return 1 }\n"
	afterSource := "package p\nfunc Beta_() int { return 2 }\n"
	require.Len(t, afterSource, len(beforeSource))
	require.NoError(t, os.WriteFile(path, []byte(beforeSource), 0o644))

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := newTestIndexer(store)
	idx.SetRootPath(dir)
	_, err = idx.Index(dir)
	require.NoError(t, err)
	indexedInfo, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(path, []byte(afterSource), 0o644))
	require.NoError(t, os.Chtimes(path, indexedInfo.ModTime(), indexedInfo.ModTime()))
	currentInfo, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, indexedInfo.ModTime().UnixNano(), currentInfo.ModTime().UnixNano())
	require.Equal(t, indexedInfo.Size(), currentInfo.Size())

	p := newPoller(nil, idx, zap.NewNop())
	require.Equal(t, []string{path}, p.pollFilesystem(),
		"content rotation must detect an edit hidden from both mtime and size")
	assert.Empty(t, p.contentRotation.afterPath,
		"a proven mismatch stays retryable until reconciliation refreshes its receipt")
}

func TestPollerContentBudgetRetriesUnprocessedAndOversizedRows(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int) graph.FileReceipt {
		t.Helper()
		src := bytes.Repeat([]byte{'x'}, size)
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, src, 0o644))
		info, err := os.Stat(path)
		require.NoError(t, err)
		return graph.FileReceipt{
			FilePath: name, MtimeNS: info.ModTime().UnixNano(),
			ContentHash: contentHashForSource(src), Size: int64(len(src)),
		}
	}
	a := write("a.go", 3<<20)
	b := write("b.go", 3<<20)
	oversized := write("oversized.go", 5<<20)
	snapshot := map[string]int64{
		a.FilePath: a.MtimeNS, b.FilePath: b.MtimeNS, oversized.FilePath: oversized.MtimeNS,
	}
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(dir)
	p := &Poller{indexer: idx, rootPath: dir, logger: zap.NewNop()}

	paths, processed := p.changedContentReceiptPaths(idx, snapshot, []graph.FileReceipt{a, b})
	require.Empty(t, paths)
	require.Equal(t, 1, processed, "the second row must wait rather than cross the 4 MiB budget")
	paths, processed = p.changedContentReceiptPaths(idx, snapshot, []graph.FileReceipt{b})
	require.Empty(t, paths)
	require.Equal(t, 1, processed, "the unprocessed row must make progress on the next page")

	paths, processed = p.changedContentReceiptPaths(idx, snapshot, []graph.FileReceipt{oversized})
	require.Empty(t, paths)
	require.Equal(t, 1, processed, "one individually oversized file must be admitted for progress")
}

func TestPollerContentReadFailureDoesNotAdvanceRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retry.go")
	src := []byte("package p\n")
	require.NoError(t, os.WriteFile(path, src, 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)
	receipt := graph.FileReceipt{
		FilePath: "retry.go", MtimeNS: info.ModTime().UnixNano(),
		ContentHash: contentHashForSource(src), Size: int64(len(src)),
	}
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(dir)
	p := &Poller{
		indexer: idx, rootPath: dir, logger: zap.NewNop(),
		readReceiptFile: func(string) ([]byte, error) { return nil, errors.New("transient read") },
		contentRotation: fileReceiptRotation{highWaterPath: receipt.FilePath},
	}
	paths, processed := p.changedContentReceiptPaths(
		idx, map[string]int64{receipt.FilePath: receipt.MtimeNS}, []graph.FileReceipt{receipt},
	)
	require.Empty(t, paths)
	require.Zero(t, processed)
	p.advanceReceiptRotation(&p.contentRotation, []graph.FileReceipt{receipt}, processed, true)
	require.Empty(t, p.contentRotation.afterPath)
	require.Equal(t, receipt.FilePath, p.contentRotation.highWaterPath,
		"an uncomparable row must retry on the next poll, not after a full rotation")
}

func TestPollerContentStatFailureDoesNotAdvanceRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retry.go")
	src := []byte("package p\n")
	require.NoError(t, os.WriteFile(path, src, 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)
	receipt := graph.FileReceipt{
		FilePath: "retry.go", MtimeNS: info.ModTime().UnixNano(),
		ContentHash: contentHashForSource(src), Size: int64(len(src)),
	}
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(dir)
	p := &Poller{
		indexer: idx, rootPath: dir, logger: zap.NewNop(),
		statReceiptFile: func(string) (os.FileInfo, error) { return nil, errors.New("transient stat") },
		contentRotation: fileReceiptRotation{highWaterPath: receipt.FilePath},
	}
	paths, processed := p.changedContentReceiptPaths(
		idx, map[string]int64{receipt.FilePath: receipt.MtimeNS}, []graph.FileReceipt{receipt},
	)
	require.Empty(t, paths)
	require.Zero(t, processed)
	p.advanceReceiptRotation(&p.contentRotation, []graph.FileReceipt{receipt}, processed, true)
	require.Empty(t, p.contentRotation.afterPath)
	require.Equal(t, receipt.FilePath, p.contentRotation.highWaterPath)
}

func TestPollerPagedReceiptsResetForReplacementIndexer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "current.go")
	require.NoError(t, os.WriteFile(path, []byte("package p\n"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)

	oldStore := &pollerReceiptStore{Graph: graph.New(), rows: []graph.FileReceipt{{FilePath: "old.go", MtimeNS: 1}}}
	oldIdx := newTestIndexer(oldStore)
	oldIdx.SetRootPath(dir)
	oldIdx.SetFileMtimes(map[string]int64{"old.go": 1})

	newStore := &pollerReceiptStore{Graph: graph.New(), rows: []graph.FileReceipt{{
		FilePath: "current.go", MtimeNS: info.ModTime().UnixNano() - 1,
	}}}
	newIdx := newTestIndexer(newStore)
	newIdx.SetRootPath(dir)
	newIdx.SetFileMtimes(map[string]int64{"current.go": info.ModTime().UnixNano() - 1})

	p := newPoller(nil, oldIdx, zap.NewNop())
	p.receiptIndexer = oldIdx
	p.mtimeRotation = fileReceiptRotation{afterPath: "old.go", highWaterPath: "old.go"}
	p.contentRotation = fileReceiptRotation{afterPath: "old.go", highWaterPath: "old.go"}
	p.indexer = newIdx

	require.Equal(t, []string{path}, p.pollFilesystem())
	require.Same(t, newIdx, p.receiptIndexer)
	require.Len(t, newStore.calls, 2)
	assert.Empty(t, newStore.calls[0].after,
		"replacement Indexer must not inherit the retired store's keyset cursor")
	assert.False(t, newStore.calls[0].includeContent)
	assert.True(t, newStore.calls[1].includeContent)
}

func TestPollerRetriesFailedFilesystemBatchBeforeConsumingNewPages(t *testing.T) {
	dir := t.TempDir()
	mtimePath := filepath.Join(dir, "a-mtime.go")
	deletedPath := filepath.Join(dir, "b-deleted.go")
	contentPath := filepath.Join(dir, "c-content.go")
	require.NoError(t, os.WriteFile(mtimePath, []byte("mtime"), 0o644))
	require.NoError(t, os.WriteFile(contentPath, []byte("new-content"), 0o644))
	mtimeInfo, err := os.Stat(mtimePath)
	require.NoError(t, err)
	contentInfo, err := os.Stat(contentPath)
	require.NoError(t, err)

	store := &pollerReceiptStore{Graph: graph.New(), rows: []graph.FileReceipt{
		{FilePath: "a-mtime.go", MtimeNS: mtimeInfo.ModTime().UnixNano() - 1},
		{FilePath: "b-deleted.go", MtimeNS: 1},
		{
			FilePath: "c-content.go", MtimeNS: contentInfo.ModTime().UnixNano(),
			ContentHash: contentHashForSource([]byte("old-content")), Size: int64(len("old-content")),
		},
	}}
	idx := newTestIndexer(store)
	idx.SetRootPath(dir)
	idx.SetFileMtimes(map[string]int64{
		"a-mtime.go":   mtimeInfo.ModTime().UnixNano() - 1,
		"b-deleted.go": 1,
		"c-content.go": contentInfo.ModTime().UnixNano(),
	})
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	p := newPoller(w, idx, zap.NewNop())

	want := []string{mtimePath, deletedPath, contentPath}
	batchCalls := 0
	w.batchReindex = func(paths []string) (*IndexResult, error) {
		batchCalls++
		require.Equal(t, want, paths)
		if batchCalls == 1 {
			return nil, errors.New("transient reconcile failure")
		}
		return &IndexResult{}, nil
	}

	p.poll()
	require.Len(t, p.pendingFilesystemPaths, len(want))
	pageCalls := len(store.calls)
	// Remove every durable row: the second submission can only come from the
	// bounded pending set, not by rediscovering through an advanced cursor.
	store.rows = nil
	store.highWater = ""
	p.poll()
	require.Equal(t, 2, batchCalls)
	require.Empty(t, p.pendingFilesystemPaths)
	require.Equal(t, pageCalls, len(store.calls),
		"retry must not consume another mtime or content page")
}

func TestPollerFallsBackWhenMtimeSidecarWriteDiverges(t *testing.T) {
	dir := t.TempDir()
	durablePath := filepath.Join(dir, "durable.go")
	missingPath := filepath.Join(dir, "missing.go")
	require.NoError(t, os.WriteFile(durablePath, []byte("durable"), 0o644))
	require.NoError(t, os.WriteFile(missingPath, []byte("changed"), 0o644))
	durableInfo, err := os.Stat(durablePath)
	require.NoError(t, err)
	missingInfo, err := os.Stat(missingPath)
	require.NoError(t, err)

	store := &pollerReceiptStore{
		Graph: graph.New(),
		rows: []graph.FileReceipt{{
			FilePath: "durable.go", MtimeNS: durableInfo.ModTime().UnixNano(),
		}},
		mtimeWriteErr: errors.New("sidecar write failed"),
	}
	idx := newTestIndexer(store)
	idx.SetRootPath(dir)
	idx.SetFileMtimes(map[string]int64{"durable.go": durableInfo.ModTime().UnixNano()})
	idx.recordFileMtimeValue("missing.go", missingInfo.ModTime().UnixNano()-1)
	require.False(t, idx.fileReceiptPagingReliable())

	p := newPoller(nil, idx, zap.NewNop())
	p.mtimeRotation = fileReceiptRotation{afterPath: "old.go", highWaterPath: "z.go"}
	p.contentRotation = fileReceiptRotation{afterPath: "old.go", highWaterPath: "z.go"}
	require.Equal(t, []string{missingPath}, p.pollFilesystem(),
		"a partial durable keyset must fall back to the complete immutable snapshot")
	require.Empty(t, store.calls, "degraded persistence must bypass receipt paging entirely")
	require.Equal(t, fileReceiptRotation{}, p.mtimeRotation)
	require.Equal(t, fileReceiptRotation{}, p.contentRotation,
		"a later authoritative repair must begin a fresh durable rotation")
}

func TestSetFileMtimesDoesNotClaimDurableSidecarRepair(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.markFileMtimePersistenceDirty()

	idx.SetFileMtimes(map[string]int64{"main.go": 1})

	require.False(t, idx.fileReceiptPagingReliable(),
		"restoring an in-memory snapshot does not authoritatively replace durable rows")
}

func TestAuthoritativeFullIndexRepairsMtimeSidecar(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package p\n"), 0o644))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := newTestIndexer(store)
	idx.SetRootPath(dir)
	idx.markFileMtimePersistenceDirty()

	_, err = idx.Index(dir)
	require.NoError(t, err)
	require.True(t, idx.fileReceiptPagingReliable(),
		"a successful authoritative replacement repairs durable receipt paging")
}
