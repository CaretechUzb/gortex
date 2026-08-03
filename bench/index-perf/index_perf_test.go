// Package indexperf is a cold-index performance regression harness.
//
// It repeatedly cold-indexes a small, fixed Go fixture through the real
// indexer pipeline (the same indexer.New / Index path the daemon uses) and
// records three headline numbers per run: wall-clock time, bytes allocated,
// and the GC CPU fraction. The best (minimum) wall-clock across runs is
// compared against a committed baseline, and the harness reports a regression
// when it exceeds the baseline by more than regressionTolerance.
//
// Run it:
//
//	go test ./bench/index-perf/ // skipped: wall-clock gates need isolation
//	GORTEX_BENCH_INDEX_GATE=1 go test -count=1 -run '^TestColdIndexNoWallClockRegression$' -v ./bench/index-perf/
//
// Regenerate the committed baseline on the target machine:
//
//	GORTEX_BENCH_INDEX_GATE=1 GORTEX_BENCH_INDEX_UPDATE_BASELINE=1 go test -count=1 -run '^TestColdIndexNoWallClockRegression$' -v ./bench/index-perf/
//
// Other knobs:
//
//	GORTEX_BENCH_INDEX_FIXTURE=/abs/dir   point the harness at another tree
//	GORTEX_BENCH_INDEX_RUNS=16            number of timed cold-index passes
//
// Only wall-clock is gated; the allocation and GC-fraction figures are
// recorded and printed so later work on GC tuning and bulk persistence can
// watch them move.
package indexperf

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// regressionTolerance is the fraction by which a fresh cold-index wall-clock
// may exceed the committed baseline before the harness reports a regression.
const regressionTolerance = 0.15

// defaultRuns is how many cold-index passes the isolated gate times. The
// minimum across passes is the representative figure: scheduler and allocator
// jitter only ever add time, so the minimum is the most stable estimator of
// the true cost.
const defaultRuns = 16

const (
	gateEnv           = "GORTEX_BENCH_INDEX_GATE"
	updateBaselineEnv = "GORTEX_BENCH_INDEX_UPDATE_BASELINE"
	fixtureDirEnv     = "GORTEX_BENCH_INDEX_FIXTURE"
	runsEnv           = "GORTEX_BENCH_INDEX_RUNS"
)

// measurement holds the metrics from a single cold-index pass.
type measurement struct {
	WallClockNs    int64
	AllocBytes     uint64
	GCCPUFraction  float64
	NumGC          uint32
	GCPauseTotalNs uint64
	Nodes          int
	Edges          int
	Files          int
}

// baseline is the committed reference recorded on a representative machine.
type baseline struct {
	WallClockNs    int64   `json:"wall_clock_ns"`
	WallClockMs    float64 `json:"wall_clock_ms"`
	AllocBytes     uint64  `json:"alloc_bytes"`
	GCCPUFraction  float64 `json:"gc_cpu_fraction"`
	NumGC          uint32  `json:"num_gc"`
	GCPauseTotalNs uint64  `json:"gc_pause_total_ns"`
	Nodes          int     `json:"nodes"`
	Edges          int     `json:"edges"`
	Files          int     `json:"files"`
	Runs           int     `json:"runs"`
	GoVersion      string  `json:"go_version"`
	GOOS           string  `json:"goos"`
	GOARCH         string  `json:"goarch"`
	Note           string  `json:"note"`
}

// TestColdIndexNoWallClockRegression cold-indexes the fixture, prints the
// three headline numbers, and fails when the best wall-clock regresses past
// the committed baseline by more than regressionTolerance.
func TestColdIndexNoWallClockRegression(t *testing.T) {
	if os.Getenv(gateEnv) != "1" {
		t.Skipf("wall-clock gate requires an isolated run; use %s=1 go test -count=1 -run '^TestColdIndexNoWallClockRegression$' -v ./bench/index-perf/", gateEnv)
	}

	fixture := fixtureDir(t)
	runs := runCount()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	best := measureColdIndexes(t, fixture, runs, reg)

	wallMs := float64(best.WallClockNs) / 1e6
	t.Logf("cold-index wall-clock:      %.2f ms (best of %d runs)", wallMs, runs)
	t.Logf("cold-index allocated bytes: %d (%.1f MiB)", best.AllocBytes, float64(best.AllocBytes)/(1024*1024))
	t.Logf("cold-index GC CPU fraction: %.6f (num_gc=%d, pause_total=%d ns)", best.GCCPUFraction, best.NumGC, best.GCPauseTotalNs)
	t.Logf("indexed graph:              %d nodes, %d edges, %d files", best.Nodes, best.Edges, best.Files)

	// Defend against a future config/exclude change silently turning the
	// fixture cold index into a no-op (which would make the bench meaningless
	// and the wall-clock ~0).
	if best.Files == 0 || best.Nodes == 0 {
		t.Fatalf("fixture indexed nothing (files=%d nodes=%d) — fixture missing or skipped by the indexer", best.Files, best.Nodes)
	}

	baselinePath := baselineFile(t)

	if os.Getenv(updateBaselineEnv) == "1" {
		writeBaseline(t, baselinePath, best, runs)
		t.Logf("baseline written to %s", baselinePath)
		return
	}

	base, err := readBaseline(baselinePath)
	if err != nil {
		t.Skipf("no usable baseline at %s (%v); regenerate with %s=1", baselinePath, err, updateBaselineEnv)
	}

	// The committed baseline is recorded without the race detector. Race
	// instrumentation inflates time and allocation several-fold, so a
	// race-mode wall-clock is not comparable to it: print the numbers but do
	// not gate on them under -race (keeps `go test -race ./...` green).
	if raceDetector {
		t.Logf("race detector active: skipping the +%.0f%% wall-clock regression gate (race timing is not comparable to the baseline)", regressionTolerance*100)
		return
	}

	limitNs := float64(base.WallClockNs) * (1 + regressionTolerance)
	t.Logf("baseline wall-clock:        %.2f ms; regression limit (+%.0f%%): %.2f ms",
		float64(base.WallClockNs)/1e6, regressionTolerance*100, limitNs/1e6)

	if float64(best.WallClockNs) > limitNs {
		t.Errorf("cold-index wall-clock regression: %.2f ms exceeds the baseline %.2f ms by more than %.0f%% (limit %.2f ms). Investigate the slowdown, or refresh the baseline with %s=1 if the change is intentional.",
			wallMs, float64(base.WallClockNs)/1e6, regressionTolerance*100, limitNs/1e6, updateBaselineEnv)
	}
}

// measureColdIndexes runs sequential cold-index passes through one registered
// parser registry and returns the pass with the smallest wall-clock. Production
// likewise shares one registry across indexers; keeping it live here prevents
// repeated native query construction and finalizers from perturbing later runs.
func measureColdIndexes(t *testing.T, fixture string, runs int, reg *parser.Registry) measurement {
	t.Helper()
	var best, expected measurement
	for i := 0; i < runs; i++ {
		m := measureOnce(t, fixture, reg)
		if i == 0 {
			best = m
			expected = m
			continue
		}
		if m.Nodes != expected.Nodes || m.Edges != expected.Edges || m.Files != expected.Files {
			t.Fatalf("cold index run %d graph mismatch: got %d nodes, %d edges, %d files; first run had %d nodes, %d edges, %d files",
				i+1, m.Nodes, m.Edges, m.Files, expected.Nodes, expected.Edges, expected.Files)
		}
		if m.WallClockNs < best.WallClockNs {
			best = m
		}
	}
	return best
}

// measureOnce performs one full cold index over fixture against a fresh graph
// and indexer, timing the Index call and capturing allocation and GC deltas
// across it.
func measureOnce(t *testing.T, fixture string, reg *parser.Registry) measurement {
	t.Helper()

	g := graph.New()
	idx := indexer.New(g, reg, config.Config{}.Index, zap.NewNop())

	// Settle the heap so the deltas attribute allocation and pauses to the
	// index pass alone, then snapshot the starting counters.
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()
	res, err := idx.Index(fixture)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("cold index of %s failed: %v", fixture, err)
	}

	// Read counters without forcing a GC first: NumGC and PauseTotalNs deltas
	// should reflect only the collections that happened during indexing.
	runtime.ReadMemStats(&after)

	return measurement{
		WallClockNs:    elapsed.Nanoseconds(),
		AllocBytes:     after.TotalAlloc - before.TotalAlloc,
		GCCPUFraction:  after.GCCPUFraction,
		NumGC:          after.NumGC - before.NumGC,
		GCPauseTotalNs: after.PauseTotalNs - before.PauseTotalNs,
		Nodes:          res.NodeCount,
		Edges:          res.EdgeCount,
		Files:          res.FileCount,
	}
}

// fixtureDir returns the directory to cold-index: the env override when set,
// otherwise the committed Go fixture under testdata.
func fixtureDir(t *testing.T) string {
	t.Helper()
	if v := os.Getenv(fixtureDirEnv); v != "" {
		return v
	}
	abs, err := filepath.Abs(filepath.Join("testdata", "fixture"))
	if err != nil {
		t.Fatalf("resolve fixture dir: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("fixture dir %s missing: %v", abs, err)
	}
	return abs
}

// baselineFile returns the path of the committed baseline JSON.
func baselineFile(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "baseline.json"))
	if err != nil {
		t.Fatalf("resolve baseline path: %v", err)
	}
	return abs
}

// runCount resolves the number of timed passes from the environment, falling
// back to defaultRuns.
func runCount() int {
	if v := os.Getenv(runsEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultRuns
}

// readBaseline loads and validates the committed baseline.
func readBaseline(path string) (baseline, error) {
	var b baseline
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, err
	}
	if b.WallClockNs <= 0 {
		return b, fmt.Errorf("baseline has non-positive wall_clock_ns (%d)", b.WallClockNs)
	}
	return b, nil
}

// writeBaseline records the measurement as the new committed baseline.
func writeBaseline(t *testing.T, path string, m measurement, runs int) {
	t.Helper()
	b := baseline{
		WallClockNs:    m.WallClockNs,
		WallClockMs:    float64(m.WallClockNs) / 1e6,
		AllocBytes:     m.AllocBytes,
		GCCPUFraction:  m.GCCPUFraction,
		NumGC:          m.NumGC,
		GCPauseTotalNs: m.GCPauseTotalNs,
		Nodes:          m.Nodes,
		Edges:          m.Edges,
		Files:          m.Files,
		Runs:           runs,
		GoVersion:      runtime.Version(),
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		Note:           "best-of-runs cold index of testdata/fixture; the gate is wall_clock_ns + 15%; regenerate with " + updateBaselineEnv + "=1",
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write baseline %s: %v", path, err)
	}
}
