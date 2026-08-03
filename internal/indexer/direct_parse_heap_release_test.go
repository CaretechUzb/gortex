package indexer

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLargeDirectParseNeedsHeapRelease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		directStore bool
		files       int
		inputBytes  int64
		want        bool
	}{
		{name: "shadow is excluded", files: defaultShadowMaxFileCount, inputBytes: defaultShadowMaxBytes},
		{name: "ordinary direct repo", directStore: true, files: defaultShadowMaxFileCount - 1, inputBytes: defaultShadowMaxBytes - 1},
		{name: "large by files", directStore: true, files: defaultShadowMaxFileCount, want: true},
		{name: "large by bytes", directStore: true, inputBytes: defaultShadowMaxBytes, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := largeDirectParseNeedsHeapRelease(tt.directStore, tt.files, tt.inputBytes); got != tt.want {
				t.Fatalf("largeDirectParseNeedsHeapRelease() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDirectParseHeapReleaserGuardsOrdinaryAndLowRetainedHeap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		req        directParseHeapReleaseRequest
		retained   uint64
		wantRun    bool
		wantReads  int32
		wantRelief int32
	}{
		{
			name: "shadow path",
			req: directParseHeapReleaseRequest{
				files: defaultShadowMaxFileCount, inputBytes: defaultShadowMaxBytes,
			},
		},
		{
			name: "ordinary direct repo",
			req: directParseHeapReleaseRequest{
				directStore: true, files: defaultShadowMaxFileCount - 1, inputBytes: defaultShadowMaxBytes - 1,
			},
		},
		{
			name: "large direct repo below retained threshold",
			req: directParseHeapReleaseRequest{
				directStore: true, inputBytes: defaultShadowMaxBytes,
			},
			retained:  99,
			wantReads: 1,
		},
		{
			name: "large direct repo at retained threshold",
			req: directParseHeapReleaseRequest{
				directStore: true, files: defaultShadowMaxFileCount,
			},
			retained:   100,
			wantRun:    true,
			wantReads:  2,
			wantRelief: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var reads atomic.Int32
			var releases atomic.Int32
			r := &directParseHeapReleaser{
				minRetained: 100,
				cooldown:    time.Minute,
				now:         func() time.Time { return time.Unix(100, 0) },
				readStats: func(ms *runtime.MemStats) {
					reads.Add(1)
					*ms = runtime.MemStats{HeapSys: tt.retained}
				},
				release: func() { releases.Add(1) },
			}
			if got := r.maybeRelease(tt.req); got != tt.wantRun {
				t.Fatalf("maybeRelease() = %v, want %v", got, tt.wantRun)
			}
			if got := reads.Load(); got != tt.wantReads {
				t.Fatalf("ReadMemStats calls = %d, want %d", got, tt.wantReads)
			}
			if got := releases.Load(); got != tt.wantRelief {
				t.Fatalf("heap releases = %d, want %d", got, tt.wantRelief)
			}
		})
	}
}

func TestDirectParseHeapReleaserProductionRetainedBoundary(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		retained uint64
		wantRun  bool
	}{
		{name: "one byte below", retained: directParseHeapReleaseMinRetained - 1},
		{name: "at boundary", retained: directParseHeapReleaseMinRetained, wantRun: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var releases atomic.Int32
			r := &directParseHeapReleaser{
				minRetained: directParseHeapReleaseMinRetained,
				cooldown:    directParseHeapReleaseCooldown,
				now:         func() time.Time { return time.Unix(100, 0) },
				readStats: func(ms *runtime.MemStats) {
					*ms = runtime.MemStats{HeapSys: tt.retained}
				},
				release: func() { releases.Add(1) },
			}
			got := r.maybeRelease(directParseHeapReleaseRequest{
				directStore: true,
				inputBytes:  defaultShadowMaxBytes,
			})
			if got != tt.wantRun {
				t.Fatalf("maybeRelease() = %v, want %v", got, tt.wantRun)
			}
			wantReleases := int32(0)
			if tt.wantRun {
				wantReleases = 1
			}
			if got := releases.Load(); got != wantReleases {
				t.Fatalf("heap releases = %d, want %d", got, wantReleases)
			}
		})
	}
}

func TestDirectParseHeapReleaserCooldown(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)
	var releases int
	r := &directParseHeapReleaser{
		minRetained: 100,
		cooldown:    30 * time.Second,
		now:         func() time.Time { return now },
		readStats: func(ms *runtime.MemStats) {
			*ms = runtime.MemStats{HeapSys: 100}
		},
		release: func() { releases++ },
	}
	req := directParseHeapReleaseRequest{directStore: true, inputBytes: defaultShadowMaxBytes}

	if !r.maybeRelease(req) {
		t.Fatal("first eligible completion did not release")
	}
	if r.maybeRelease(req) {
		t.Fatal("completion inside cooldown released again")
	}
	now = now.Add(30 * time.Second)
	if !r.maybeRelease(req) {
		t.Fatal("completion at cooldown boundary did not release")
	}
	if releases != 2 {
		t.Fatalf("heap releases = %d, want 2", releases)
	}
}

func TestDirectParseHeapReleaserSerializesConcurrentCompletions(t *testing.T) {
	t.Parallel()

	const callers = 24
	var retained atomic.Uint64
	retained.Store(200)
	var releases atomic.Int32
	firstStarted := make(chan struct{})
	allowFirst := make(chan struct{})
	r := &directParseHeapReleaser{
		minRetained: 100,
		cooldown:    0,
		now:         time.Now,
		readStats: func(ms *runtime.MemStats) {
			*ms = runtime.MemStats{HeapSys: retained.Load()}
		},
		release: func() {
			if releases.Add(1) == 1 {
				close(firstStarted)
				<-allowFirst
			}
			retained.Store(0)
		},
	}
	req := directParseHeapReleaseRequest{directStore: true, inputBytes: defaultShadowMaxBytes}

	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			r.maybeRelease(req)
		}()
	}
	<-firstStarted
	close(allowFirst)
	wg.Wait()

	if got := releases.Load(); got != 1 {
		t.Fatalf("concurrent heap releases = %d, want 1", got)
	}
}
