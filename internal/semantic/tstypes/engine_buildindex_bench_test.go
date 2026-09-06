package tstypes

// Regression-measurement benchmark for the addons python-types
// whole-repo enrichment budget cut (>1,080s since 2026-08-31, was
// 20-194s). The candidate change is buildIndex's added stub-snapshot
// loop (b5b2e6ca / 9aca4faf): for every non-file node in a file it now
// walks a.outEdges(n.ID) to build idx.stubsByLine. This benchmark runs
// the FULL Enrich pipeline (not buildIndex in isolation — preload/
// adjacency wiring makes an isolated call unrepresentative) over a
// synthetic corpus shaped like addons: one class + five methods + one
// module function per file (7 nodes/file, matching the ~6.9
// nodes/file measured on addons), with each method calling its
// later siblings (0-4 calls) and the module function calling every
// method (5 calls) — so several nodes carry 3-5 outgoing EdgeCalls
// edges, the shape the new loop filters on.
//
// Compare against the identical benchmark run from a checkout of
// b5b2e6ca~1 (pre-regression, before either commit) to see whether
// buildIndex's current cost explains the reported regression.

import (
	"fmt"
	"testing"

	"go.uber.org/zap"
)

// addonsShapeFile renders one synthetic python file: a class with 5
// methods that call their later siblings, plus a module function that
// calls every method. 7 nodes/file; several nodes with 3-5 outgoing
// calls edges.
func addonsShapeFile(i int) string {
	src := fmt.Sprintf("class Worker%d:\n", i)
	steps := 5
	for s := 1; s <= steps; s++ {
		src += fmt.Sprintf("    def step%d(self):\n", s)
		any := false
		for t := s + 1; t <= steps; t++ {
			src += fmt.Sprintf("        self.step%d()\n", t)
			any = true
		}
		if !any {
			src += "        pass\n"
		}
		src += "\n"
	}
	src += fmt.Sprintf("\ndef run%d(w: Worker%d):\n", i, i)
	for s := 1; s <= steps; s++ {
		src += fmt.Sprintf("    w.step%d()\n", s)
	}
	src += "\n"
	return src
}

func addonsShapeCorpus(n int) map[string]string {
	files := make(map[string]string, n)
	for i := 0; i < n; i++ {
		files[fmt.Sprintf("addons/mod%d/models.py", i)] = addonsShapeFile(i)
	}
	return files
}

// BenchmarkBuildIndexAddonsShape times the full python-types Enrich
// pass (which invokes buildIndex once per file per applicable apply
// phase via preparePage) over an addons-shaped corpus, at increasing
// file counts. Run with -benchtime=1x — each iteration re-extracts
// the whole corpus from disk (StopTimer'd) and the enrich pass itself
// is the only timed work; the corpus is big enough that repeat
// iterations within a short benchtime add little precision for a lot
// of wall time.
func BenchmarkBuildIndexAddonsShape(b *testing.B) {
	for _, n := range []int{200, 500, 2000, 4000, 8000, 12000} {
		b.Run(fmt.Sprintf("files=%d", n), func(b *testing.B) {
			files := addonsShapeCorpus(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				g, dir := buildFixture(b, files)
				p := NewProvider(PythonSpec(), zap.NewNop())
				b.StartTimer()
				if _, err := p.Enrich(g, dir); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(n), "files")
			b.ReportMetric(b.Elapsed().Seconds()/float64(b.N)/float64(n)*1e9, "ns/file")
		})
	}
}
