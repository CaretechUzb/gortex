package indexer

import (
	"sync/atomic"
	"testing"
)

func TestNativeParsePressureReliefThresholdAndFlush(t *testing.T) {
	var releases atomic.Int64
	r := &nativeParsePressureRelief{
		release: func() uintptr {
			releases.Add(1)
			return 17
		},
	}

	r.afterParse("go", nativeParsePressureThresholdBytes*2)
	r.afterParse("c", nativeParsePressureThresholdBytes-1)
	if got := releases.Load(); got != 0 {
		t.Fatalf("releases before C-family threshold = %d, want 0", got)
	}

	r.afterParse("cpp", 1)
	if got := releases.Load(); got != 1 {
		t.Fatalf("releases at threshold = %d, want 1", got)
	}
	stats := r.stats()
	if stats.calls != 1 || stats.releasedBytes != 17 {
		t.Fatalf("stats after threshold = %+v, want one call and 17 released bytes", stats)
	}

	r.flush()
	if got := releases.Load(); got != 1 {
		t.Fatalf("empty flush releases = %d, want 1", got)
	}

	r.afterParse("objc", 1024)
	r.flush()
	if got := releases.Load(); got != 2 {
		t.Fatalf("tail flush releases = %d, want 2", got)
	}
	stats = r.stats()
	if stats.calls != 2 || stats.releasedBytes != 34 {
		t.Fatalf("stats after tail flush = %+v, want two calls and 34 released bytes", stats)
	}
}

func TestNativeParsePressureReliefIgnoresInvalidInputs(t *testing.T) {
	var releases atomic.Int64
	r := &nativeParsePressureRelief{
		release: func() uintptr {
			releases.Add(1)
			return 0
		},
	}

	r.afterParse("c", 0)
	r.afterParse("c", -1)
	r.afterParse("typescript", nativeParsePressureThresholdBytes)
	r.flush()
	if got := releases.Load(); got != 0 {
		t.Fatalf("invalid inputs triggered %d releases, want 0", got)
	}
}
