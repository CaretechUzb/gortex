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

func TestNativeParseAdmissionWeight(t *testing.T) {
	const (
		sourceBytes int64 = 1024
		budget      int64 = 512 << 20
	)
	for _, language := range []string{"c", "cpp", "cuda", "objc"} {
		if got, want := nativeParseAdmissionWeight(language, sourceBytes, budget), budget/2; got != want {
			t.Errorf("nativeParseAdmissionWeight(%q) = %d, want half-budget floor %d", language, got, want)
		}
	}
	for _, language := range []string{"go", "typescript", ""} {
		if got := nativeParseAdmissionWeight(language, sourceBytes, budget); got != sourceBytes {
			t.Errorf("nativeParseAdmissionWeight(%q) = %d, want unchanged %d", language, got, sourceBytes)
		}
	}
	const largeSource int64 = 100 << 20
	if got, want := nativeParseAdmissionWeight("c", largeSource, budget), largeSource*nativeParseAdmissionMultiplier; got != want {
		t.Errorf("large native admission weight = %d, want %d", got, want)
	}
	if got, want := nativeParseAdmissionWeight("c", sourceBytes, 0), sourceBytes*nativeParseAdmissionMultiplier; got != want {
		t.Errorf("unbudgeted native admission weight = %d, want %d", got, want)
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if got := nativeParseAdmissionWeight("c", maxInt64, budget); got != maxInt64 {
		t.Fatalf("overflowing admission weight = %d, want saturation at %d", got, maxInt64)
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
