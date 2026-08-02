package indexer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/parser"
)

type nativeAdmissionTestExtractor struct {
	calls   atomic.Int32
	entered chan struct{}
	release <-chan struct{}
	exited  chan struct{}
}

func (ext *nativeAdmissionTestExtractor) Language() string     { return "c" }
func (ext *nativeAdmissionTestExtractor) Extensions() []string { return []string{".c"} }

func (ext *nativeAdmissionTestExtractor) Extract(string, []byte) (*parser.ExtractionResult, error) {
	ext.calls.Add(1)
	if ext.entered != nil {
		select {
		case ext.entered <- struct{}{}:
		default:
		}
	}
	if ext.release != nil {
		<-ext.release
	}
	if ext.exited != nil {
		close(ext.exited)
	}
	return &parser.ExtractionResult{}, nil
}

func TestNativeParseAdmissionWeightOnlyNative(t *testing.T) {
	const budget = int64(512 << 20)
	if got := nativeParseAdmissionWeight("go", 1<<20, budget); got != 1<<20 {
		t.Fatalf("non-native helper weight = %d, want unchanged source size", got)
	}
	admission := newNativeParseExtractionAdmission(0, nil, newParseAdmissionBudget(budget))
	lease, err := admission.acquire(context.Background(), "go", 1<<20)
	if err != nil || lease != nil {
		t.Fatalf("non-native admission = (%v, %v), want no lease", lease, err)
	}
	if got := nativeParseAdmissionWeight("c", 1, budget); got != budget/2 {
		t.Fatalf("small C source weight = %d, want %d", got, budget/2)
	}
	if got := nativeParseAdmissionWeight("cpp", budget/4, budget); got != budget {
		t.Fatalf("large C++ source weight = %d, want %d", got, budget)
	}
}

func TestNativeParseAdmissionCapsConcurrentTrees(t *testing.T) {
	shared := newParseAdmissionBudget(8)
	admission := newNativeParseExtractionAdmission(0, nil, shared)

	first, err := admission.acquire(context.Background(), "c", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := admission.acquire(context.Background(), "c", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if lease, err := admission.acquire(ctx, "c", 1); !errors.Is(err, context.DeadlineExceeded) {
		lease.Release()
		t.Fatalf("third admission error = %v, want deadline exceeded", err)
	}

	first.Release()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	third, err := admission.acquire(ctx, "c", 1)
	if err != nil {
		t.Fatalf("admission after release: %v", err)
	}
	third.Release()
}

func TestGeneratedParserProjectionBypassesNativeAdmission(t *testing.T) {
	shared := newParseAdmissionBudget(8)
	if err := shared.acquire(context.Background(), shared.capacity); err != nil {
		t.Fatal(err)
	}
	defer shared.release(shared.capacity)
	admission := newNativeParseExtractionAdmission(0, nil, shared)

	idx := &Indexer{logger: zap.NewNop()}
	ext := &nativeAdmissionTestExtractor{}
	src := generatedParserAdmissionFixture()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, skipped, err := idx.extractFileCtx(
		ctx, admission, nil, nil,
		"/tmp/parser.c", "parser.c", "c", ext, src,
	)
	if err != nil {
		t.Fatalf("projected parser: %v", err)
	}
	if skipped || result == nil {
		t.Fatalf("projected parser result = (%v, skipped=%v)", result, skipped)
	}
	if got := ext.calls.Load(); got != 0 {
		t.Fatalf("extractor calls = %d, want 0", got)
	}
}

func TestTimedOutNativeExtractionRetainsAdmission(t *testing.T) {
	shared := newParseAdmissionBudget(4)
	admission := newNativeParseExtractionAdmission(0, nil, shared)
	idx := &Indexer{
		config: config.IndexConfig{MaxExtractMillis: 20},
		logger: zap.NewNop(),
	}

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	exited := make(chan struct{})
	blocking := &nativeAdmissionTestExtractor{
		entered: entered,
		release: release,
		exited:  exited,
	}
	type extractionOutcome struct {
		result  *parser.ExtractionResult
		skipped bool
		err     error
	}
	outcome := make(chan extractionOutcome, 1)
	go func() {
		result, skipped, err := idx.extractFileCtx(
			context.Background(), admission, nil, nil,
			"/tmp/blocked.c", "blocked.c", "c", blocking, []byte("int x;"),
		)
		outcome <- extractionOutcome{result: result, skipped: skipped, err: err}
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("blocking extractor did not enter")
	}
	first := <-outcome
	if first.result == nil || !first.skipped || first.err == nil {
		t.Fatalf("timeout result = (%v, skipped=%v, err=%v)", first.result, first.skipped, first.err)
	}

	second := &nativeAdmissionTestExtractor{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, _, err := idx.extractFileCtx(
		ctx, admission, nil, nil,
		"/tmp/second.c", "second.c", "c", second, []byte("int y;"),
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second extraction error = %v, want deadline exceeded", err)
	}
	if got := second.calls.Load(); got != 0 {
		t.Fatalf("second extractor calls = %d while timed-out parse was live, want 0", got)
	}

	close(release)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("timed-out extractor did not exit")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, skipped, err := idx.extractFileCtx(
		ctx, admission, nil, nil,
		"/tmp/third.c", "third.c", "c", second, []byte("int z;"),
	)
	if err != nil || skipped {
		t.Fatalf("extraction after native release = (skipped=%v, err=%v)", skipped, err)
	}
	if got := second.calls.Load(); got != 1 {
		t.Fatalf("extractor calls after release = %d, want 1", got)
	}
}

func TestSetSharedParseMemoryBudgetSeparatesNativeAdmission(t *testing.T) {
	idx := &Indexer{}
	mi := &MultiIndexer{indexers: map[string]*Indexer{"repo": idx}}
	mi.SetSharedParseMemoryBudget(64)

	raw := mi.parseAdmission.Load()
	native := mi.nativeParseAdmission.Load()
	if raw == nil || native == nil {
		t.Fatal("expected both shared admission budgets")
	}
	if raw == native {
		t.Fatal("raw and native admission unexpectedly share accounting")
	}
	if idx.parseAdmission.Load() != raw || idx.nativeParseAdmission.Load() != native {
		t.Fatal("live Indexer did not receive both shared budgets")
	}
}

func generatedParserAdmissionFixture() []byte {
	return []byte(
		"#include \"tree_sitter/parser.h\"\n" +
			"#define LANGUAGE_VERSION 14\n" +
			"#define STATE_COUNT 1\n" +
			"TS_PUBLIC const TSLanguage *tree_sitter_fixture(void) { return &language; }\n",
	)
}
