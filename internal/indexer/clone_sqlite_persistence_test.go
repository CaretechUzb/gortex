package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type cloneProjectionCountingStore struct {
	graph.Store
	pager  graph.CloneCorpusPager
	writer graph.CloneCorpusWriter

	pageCalls  int
	writeCalls int
	writeRows  int
}

func newCloneProjectionCountingStore(store graph.Store) *cloneProjectionCountingStore {
	return &cloneProjectionCountingStore{
		Store:  store,
		pager:  store.(graph.CloneCorpusPager),
		writer: store.(graph.CloneCorpusWriter),
	}
}

func (s *cloneProjectionCountingStore) CloneCorpusPage(repoPrefix, after string, limit int) ([]graph.CloneCorpusRow, error) {
	s.pageCalls++
	return s.pager.CloneCorpusPage(repoPrefix, after, limit)
}

func (s *cloneProjectionCountingStore) BulkSetCloneCorpus(repoPrefix string, rows []graph.CloneCorpusRow) error {
	s.writeCalls++
	s.writeRows += len(rows)
	return s.writer.BulkSetCloneCorpus(repoPrefix, rows)
}

func TestSQLiteCloneCorpusSurvivesReloadAndWarmReplay(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0") // exercise the direct SQLite cold path
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "main.go"), cloneRepoSource)
	dbPath := filepath.Join(t.TempDir(), "clone.sqlite")

	store, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	idx := newTestIndexer(store)
	_, err = idx.Index(repo)
	require.NoError(t, err)
	require.Len(t, similarToEdges(store), 2, "cold SQLite index must emit the symmetric clone pair")
	require.NoError(t, store.Close())

	reopened, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.Len(t, similarToEdges(reopened), 2, "clone edges must survive SQLite reopen")

	for _, id := range []string{"main.go::processItems", "main.go::processRecords", "main.go::openAndScan"} {
		node := reopened.GetNode(id)
		require.NotNil(t, node)
		require.NotEmpty(t, node.Meta[cloneSigMetaKey], "flat clone_sig must hydrate into Meta after reopen")
		_, hasRawShingles := node.Meta[cloneShinglesMetaKey]
		require.False(t, hasRawShingles, "raw shingles must live only in the compact sidecar")
	}

	counted := newCloneProjectionCountingStore(reopened)
	page, err := counted.CloneCorpusPage("", "", cloneCorpusFinalizeBatch)
	require.NoError(t, err)
	require.Len(t, page, 3)
	for _, row := range page {
		require.True(t, row.Finalized)
		require.NotEmpty(t, row.Shingles)
		require.NotEmpty(t, row.Signature)
	}

	maintained := newIncrementalCloneIndex()
	maintained.Rebuild(counted, "")
	require.True(t, maintained.built)
	require.Equal(t, len(page), maintained.corpus)
	require.Len(t, maintained.shingles, len(page))

	assertEquivalent := func(actual *incrementalCloneIndex) {
		t.Helper()
		require.True(t, actual.built)
		require.Equal(t, maintained.corpus, actual.corpus)
		require.Equal(t, maintained.shingles, actual.shingles)
		seededPair := false
		for _, row := range page {
			probe, ok := clones.DecodeSignature(row.Signature)
			require.True(t, ok)
			item := clones.Item{ID: row.NodeID, Sig: probe, TokenCount: row.TokenCount}
			expectedPairs := maintained.lsh.QueryPairs(item, clones.DefaultThreshold)
			seededPair = seededPair || len(expectedPairs) > 0
			require.ElementsMatch(t,
				expectedPairs,
				actual.lsh.QueryPairs(item, clones.DefaultThreshold),
			)
			for _, shingle := range row.Shingles {
				require.Equal(t, maintained.cms.Count(shingle), actual.cms.Count(shingle))
			}
		}
		require.True(t, seededPair, "incremental indexes must retain the persisted clone pair")
	}

	counted.pageCalls = 0
	counted.writeCalls = 0
	counted.writeRows = 0
	beforeEdges := reopened.EdgeCount()
	replayed := newTestIndexer(counted)
	replayed.RunGlobalGraphPasses(context.Background())
	require.Equal(t, 1, counted.pageCalls, "warm global pass must scan the finalized corpus exactly once")
	require.Zero(t, counted.writeCalls, "warm finalized corpus must not rewrite signatures")
	require.Zero(t, counted.writeRows)
	require.Equal(t, beforeEdges, reopened.EdgeCount(), "warm replay must be graph-idempotent")
	assertEquivalent(replayed.cloneIndex)

	counted.pageCalls = 0
	fallback := newIncrementalCloneIndex()
	fallback.AdoptBaselineOrRebuild(counted, "", nil)
	require.Equal(t, 1, counted.pageCalls, "missing baseline must retain the bounded rebuild fallback")
	assertEquivalent(fallback)
}

type cloneCorpusBenchmarkStore struct {
	graph.Store
	rows      []graph.CloneCorpusRow
	pageCalls int
	failAfter int
}

func (s *cloneCorpusBenchmarkStore) CloneCorpusPage(_ string, after string, limit int) ([]graph.CloneCorpusRow, error) {
	s.pageCalls++
	if s.failAfter > 0 && s.pageCalls > s.failAfter {
		return nil, errors.New("injected clone corpus page failure")
	}
	start := sort.Search(len(s.rows), func(i int) bool { return s.rows[i].NodeID > after })
	if start == len(s.rows) {
		return nil, nil
	}
	end := min(start+limit, len(s.rows))
	return s.rows[start:end], nil
}

func (s *cloneCorpusBenchmarkStore) BulkSetCloneCorpus(_ string, _ []graph.CloneCorpusRow) error {
	return nil
}

func benchmarkCloneCorpus(size int) (*cloneCorpusBaseline, []graph.CloneCorpusRow) {
	baseline := &cloneCorpusBaseline{
		cms:       clones.NewCMS(65536, 4),
		lsh:       clones.NewStratifiedIndex(),
		shingles:  make(map[string][]uint64, size),
		itemCount: size,
		corpus:    size,
		complete:  true,
	}
	rows := make([]graph.CloneCorpusRow, 0, size)
	for i := 0; i < size; i++ {
		id := fmt.Sprintf("node-%06d", i)
		shingles := make([]uint64, 12)
		for j := range shingles {
			shingles[j] = uint64(i*32 + j + 1)
			baseline.cms.Add(shingles[j])
		}
		sig, ok := clones.SignatureFromShingles(shingles, 0)
		if !ok {
			panic("benchmark corpus unexpectedly produced no signature")
		}
		item := clones.Item{ID: id, Sig: sig, TokenCount: 100}
		baseline.shingles[id] = shingles
		baseline.lsh.Add(item)
		rows = append(rows, graph.CloneCorpusRow{
			NodeID: id, Shingles: shingles, TokenCount: item.TokenCount,
			Signature: clones.EncodeSignature(sig), Finalized: true,
		})
	}
	return baseline, rows
}

func TestFinaliseCloneCorpusAbortReleasesBaseline(t *testing.T) {
	_, rows := benchmarkCloneCorpus(cloneCorpusFinalizeBatch)
	store := &cloneCorpusBenchmarkStore{rows: rows, failAfter: 1}
	baseline := finaliseCloneCorpusCtx(context.Background(), store, "")

	require.False(t, baseline.complete)
	require.Nil(t, baseline.cms)
	require.Nil(t, baseline.lsh)
	require.Nil(t, baseline.shingles)
	require.Nil(t, baseline.items)
	require.Zero(t, baseline.itemCount)
	require.Zero(t, baseline.corpus)
}

func BenchmarkIncrementalCloneIndexBaselineReuse(b *testing.B) {
	baseline, rows := benchmarkCloneCorpus(2 * cloneCorpusFinalizeBatch)
	store := &cloneCorpusBenchmarkStore{rows: rows}

	b.Run("adopt_finalized_baseline", func(b *testing.B) {
		b.ReportAllocs()
		store.pageCalls = 0
		for i := 0; i < b.N; i++ {
			candidate := *baseline
			ci := &incrementalCloneIndex{}
			ci.AdoptBaselineOrRebuild(store, "", &candidate)
			if !ci.Ready() {
				b.Fatal("adopted index is not ready")
			}
		}
		b.ReportMetric(float64(store.pageCalls)/float64(b.N), "corpus_pages/op")
	})

	b.Run("rebuild_from_pager", func(b *testing.B) {
		b.ReportAllocs()
		store.pageCalls = 0
		for i := 0; i < b.N; i++ {
			ci := &incrementalCloneIndex{}
			ci.Rebuild(store, "")
			if !ci.Ready() {
				b.Fatal("rebuilt index is not ready")
			}
		}
		b.ReportMetric(float64(store.pageCalls)/float64(b.N), "corpus_pages/op")
	})
}
