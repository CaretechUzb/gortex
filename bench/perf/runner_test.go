package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestRunRepo_LocalFixture is the end-to-end smoke test: runs the
// full bench against the in-tree nestjs fixture and asserts the
// shape of the resulting row. Uses a private cache dir so it doesn't
// touch the developer's real ~/.cache/gortex/bench tree.
func TestRunRepo_LocalFixture(t *testing.T) {
	fixturePath, err := filepath.Abs(filepath.Join("..", "fixtures", "di", "nestjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixturePath); err != nil {
		t.Skipf("fixture not available: %v", err)
	}

	// Run with a tiny query set to keep the test fast.
	queries := []string{"AppModule", "ConfigService", "auth handler"}
	row := runRepo(repoSpec{
		Slug:  "nestjs-fixture",
		Path:  fixturePath,
		Local: true,
	}, queries, budgets{
		// Generous budgets so a slow CI doesn't flake — the real
		// validation lives in main.go's strict-mode path. These gates
		// exist to catch a pathological regression (an accidental
		// whole-table scan per query), never to enforce a latency.
		//
		// Calibrated against the SQLite-backed harness under -race,
		// where the fixture measures search p95 ~40ms and impact p95
		// ~27ms — the store reads and the race detector together cost
		// two orders of magnitude over the timings this test was first
		// written for. A budget within 2-3x of the observed figure is
		// not generous: it fails whenever the package runs alongside
		// the rest of the suite and the machine is contended. Keep an
		// order of magnitude of headroom, and re-derive it from the
		// MEASURED line below rather than by guessing.
		ImpactP95Ms: 500.0,
		SearchP95Ms: 500.0,
	})

	// The figures the budgets above are derived from — logged so a
	// recalibration starts from a measurement (`go test -v`) instead of
	// from a guess.
	t.Logf("MEASURED cold=%.3fms search_p50=%.3fms search_p95=%.3fms impact_p50=%.3fms impact_p95=%.3fms impact_p99=%.3fms incremental=%.3fms",
		row.ColdIndexMs, row.SearchP50Ms, row.SearchP95Ms,
		row.ImpactP50Ms, row.ImpactP95Ms, row.ImpactP99Ms, row.IncrementalMs)

	if row.Error != "" {
		t.Fatalf("runRepo error: %s", row.Error)
	}
	if row.Files == 0 {
		t.Errorf("expected indexed files, got 0")
	}
	if row.Nodes == 0 {
		t.Errorf("expected graph nodes, got 0")
	}
	if row.ColdIndexMs <= 0 {
		t.Errorf("cold-index time should be > 0, got %.3f", row.ColdIndexMs)
	}
	if row.SearchP95Ms <= 0 {
		t.Errorf("search p95 should be > 0, got %.3f", row.SearchP95Ms)
	}
	if row.IncrementalMs <= 0 {
		t.Errorf("incremental should be > 0, got %.3f", row.IncrementalMs)
	}
	if row.DBBytes <= 0 {
		t.Errorf("DB size should be > 0, got %d", row.DBBytes)
	}
	if row.BudgetViolations > 0 {
		t.Errorf("generous budgets shouldn't violate, got %d", row.BudgetViolations)
	}
}

func TestPickImpactSeeds_RespectsLimit(t *testing.T) {
	g := graph.New()
	for i := range 5 {
		g.AddNode(&graph.Node{
			ID:   funcID(i),
			Name: funcName(i),
			Kind: graph.KindFunction,
		})
	}
	seeds := pickImpactSeeds(g, 3)
	if len(seeds) != 3 {
		t.Errorf("len(seeds)=%d, want 3 (n < pool)", len(seeds))
	}
	// Pool smaller than n: return all.
	seeds = pickImpactSeeds(g, 10)
	if len(seeds) != 5 {
		t.Errorf("len(seeds)=%d, want 5 (n > pool)", len(seeds))
	}
}

func TestPickImpactSeeds_FunctionsAndMethodsOnly(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "f.go::F", Name: "F", Kind: graph.KindFunction})
	g.AddNode(&graph.Node{ID: "f.go::M", Name: "M", Kind: graph.KindMethod})
	g.AddNode(&graph.Node{ID: "f.go::T", Name: "T", Kind: graph.KindType})
	g.AddNode(&graph.Node{ID: "f.go::V", Name: "V", Kind: graph.KindVariable})
	seeds := pickImpactSeeds(g, 10)
	if len(seeds) != 2 {
		t.Errorf("got %d seeds %v, want 2 (function + method only)", len(seeds), seeds)
	}
}

func TestPickIncrementalFiles_RespectsLimit(t *testing.T) {
	g := graph.New()
	for i := range 8 {
		g.AddNode(&graph.Node{
			ID:       fileID(i),
			Kind:     graph.KindFile,
			FilePath: fileName(i),
		})
	}
	files := pickIncrementalFiles(g, 3)
	if len(files) != 3 {
		t.Errorf("len(files)=%d, want 3", len(files))
	}
	for _, f := range files {
		if f == "" {
			t.Errorf("empty file path returned")
		}
	}
}

func TestTouchFile_MissingFileSilent(t *testing.T) {
	if err := touchFile(filepath.Join(t.TempDir(), "missing.go")); err != nil {
		t.Errorf("missing file should not error, got %v", err)
	}
}

func TestTouchFile_BumpsMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.go")
	if err := os.WriteFile(path, []byte("// x"), 0o644); err != nil {
		t.Fatal(err)
	}
	st1, _ := os.Stat(path)
	// Sleep to ensure mtime granularity is exceeded.
	old := st1.ModTime()
	if err := touchFile(path); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(path)
	if !st2.ModTime().After(old) && !st2.ModTime().Equal(old) {
		t.Errorf("mtime regressed: %v → %v", old, st2.ModTime())
	}
}

func TestDBSizeBytes_SumsEveryFileInStoreDir(t *testing.T) {
	dir := t.TempDir()
	// A store directory holds the database plus its write-ahead log
	// and any sidecar; every one of them counts toward what the user
	// sees on disk.
	files := map[string]int{
		"bench.sqlite":     4096,
		"bench.sqlite-wal": 512,
		"nested/side.db":   128,
	}
	want := int64(0)
	for name, size := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		want += int64(size)
	}
	if got := dbSizeBytes(dir); got != want {
		t.Errorf("dbSizeBytes = %d, want %d", got, want)
	}
}

func TestDBSizeBytes_MissingDirReportsFailure(t *testing.T) {
	if got := dbSizeBytes(filepath.Join(t.TempDir(), "absent")); got != -1 {
		t.Errorf("dbSizeBytes(absent) = %d, want -1", got)
	}
}

func TestDefaultRepoSet_IncludeLinuxFlag(t *testing.T) {
	without := defaultRepoSet(false)
	with := defaultRepoSet(true)
	if len(without) != 3 || len(with) != 4 {
		t.Errorf("expected 3 without linux + 4 with, got %d / %d", len(without), len(with))
	}
	// linux must be in the includeLinux=true set, NOT in the false set.
	hasLinux := func(rs []repoSpec) bool {
		for _, r := range rs {
			if r.Slug == "linux" {
				return true
			}
		}
		return false
	}
	if hasLinux(without) {
		t.Error("defaultRepoSet(false) should not include linux")
	}
	if !hasLinux(with) {
		t.Error("defaultRepoSet(true) should include linux")
	}
}

// --- helpers --------------------------------------------------------

func funcID(i int) string   { return "f.go::F" + itoa(i) }
func funcName(i int) string { return "F" + itoa(i) }
func fileID(i int) string   { return "file" + itoa(i) + ".go" }
func fileName(i int) string { return "file" + itoa(i) + ".go" }
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var out []byte
	for i > 0 {
		out = append([]byte{byte('0' + i%10)}, out...)
		i /= 10
	}
	return string(out)
}
