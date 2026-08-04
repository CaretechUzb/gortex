package indexer

import (
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublishedFileMtimesDetachOnFirstMutation(t *testing.T) {
	idx := &Indexer{fileMtimes: map[string]int64{
		"a.go": 1,
		"b.go": 2,
	}}

	published := idx.publishFileMtimes()
	require.True(t, idx.fileMtimesShared)

	idx.recordFileMtimeValue("a.go", 10)
	assert.Equal(t, int64(1), published["a.go"], "published snapshot must remain immutable")
	assert.Equal(t, int64(10), idx.fileMtimes["a.go"])
	assert.False(t, idx.fileMtimesShared, "the first write must detach the working map")

	idx.recordFileMtimeValue("c.go", 3)
	assert.Equal(t, int64(3), idx.fileMtimes["c.go"])
	assert.NotContains(t, published, "c.go")
	assert.False(t, idx.fileMtimesShared, "later writes reuse the detached working map")

	republished := idx.publishFileMtimes()
	idx.mtimeMu.Lock()
	idx.ensureFileMtimesWritableLocked()
	delete(idx.fileMtimes, "b.go")
	idx.mtimeMu.Unlock()
	assert.Contains(t, republished, "b.go", "deletion must detach from a republished snapshot")
	assert.NotContains(t, idx.fileMtimes, "b.go")
}

func TestPublishedFileMtimesRemainStableDuringConcurrentWrites(t *testing.T) {
	const files = 2048
	mtimes := make(map[string]int64, files)
	for i := 0; i < files; i++ {
		mtimes[fmt.Sprintf("file-%04d.go", i)] = int64(i)
	}
	idx := &Indexer{fileMtimes: mtimes}
	published := idx.publishFileMtimes()

	var readers sync.WaitGroup
	for reader := 0; reader < 8; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for pass := 0; pass < 25; pass++ {
				seen := 0
				for range published {
					seen++
				}
				assert.Equal(t, files, seen)
			}
		}()
	}

	for i := 0; i < 128; i++ {
		idx.recordFileMtimeValue(fmt.Sprintf("new-%04d.go", i), int64(i))
	}
	readers.Wait()

	assert.Len(t, published, files)
	assert.Len(t, idx.fileMtimes, files+128)
}

func TestSetFileMtimesOwnsRestoredMap(t *testing.T) {
	restored := map[string]int64{"a.go": 1}
	idx := &Indexer{fileMtimes: map[string]int64{"old.go": 2}, fileMtimesShared: true}

	idx.SetFileMtimes(restored)
	restored["a.go"] = 99

	assert.Equal(t, int64(1), idx.fileMtimes["a.go"])
	assert.False(t, idx.fileMtimesShared)
}

func TestMultiIndexerFileMtimesReturnsPublishedSnapshot(t *testing.T) {
	idx := &Indexer{fileMtimes: map[string]int64{"a.go": 1}}
	published := idx.publishFileMtimes()
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{
		"repo": {FileMtimes: published},
	}}

	assert.Equal(t, published, mi.FileMtimes("repo"))
	assert.Nil(t, mi.FileMtimes("missing"))

	idx.recordFileMtimeValue("a.go", 2)
	assert.Equal(t, int64(1), mi.FileMtimes("repo")["a.go"])
}

func BenchmarkFileMtimeSnapshot(b *testing.B) {
	const files = 50_000
	mtimes := make(map[string]int64, files)
	for i := 0; i < files; i++ {
		mtimes[fmt.Sprintf("path/to/file-%05d.go", i)] = int64(i)
	}
	idx := &Indexer{fileMtimes: mtimes}

	b.Run("copy", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			snapshot := idx.FileMtimes()
			runtime.KeepAlive(snapshot)
		}
	})
	b.Run("published", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			snapshot := idx.publishFileMtimes()
			runtime.KeepAlive(snapshot)
		}
	})
}
