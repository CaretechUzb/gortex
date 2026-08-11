package store_sqlite

import (
	"os"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// BenchmarkGuardSiteProbes replays the resolver guard's candidate-probe
// workload against a REAL store copy, isolating the probe path from cold-run
// confounds (a production guard measured ~176 jobs/s with 41% of CPU in
// pread syscalls; a healthy run does thousands/s). Point GORTEX_BENCH_STORE
// at a copied store file — never a live daemon's — and vary
// GORTEX_SQLITE_MMAP_MB across runs to measure the window's effect.
//
// Sub-benchmarks:
//   - raw_file_order:  any-site probe SQL in file-clustered discovery order
//     (the pre-round-8 access pattern).
//   - raw_from_sorted: the same probes globally (from,line)-sorted — the
//     round-8 "monotonic" order.
//   - api_get_candidates: the full GetEdgeCandidates path (internal sort,
//     row decode, canonicalization) — what the guard actually pays.
func BenchmarkGuardSiteProbes(b *testing.B) {
	path := os.Getenv("GORTEX_BENCH_STORE")
	if path == "" {
		b.Skip("set GORTEX_BENCH_STORE to a copied store.sqlite to run")
	}
	s, err := Open(path)
	if err != nil {
		b.Fatalf("open bench store: %v", err)
	}
	defer s.Close()

	rows, err := s.db.Query(`SELECT from_id, line, kind FROM edges
WHERE kind IN ('calls','references') AND origin IN ('ast_inferred','speculative','')
ORDER BY file_path, line LIMIT 120000`)
	if err != nil {
		b.Fatalf("harvest probe sites: %v", err)
	}
	var fileOrdered []graph.EdgeSite
	for rows.Next() {
		var site graph.EdgeSite
		var kind string
		if err := rows.Scan(&site.From, &site.Line, &kind); err != nil {
			_ = rows.Close()
			b.Fatalf("scan probe site: %v", err)
		}
		site.Kind = graph.EdgeKind(kind)
		fileOrdered = append(fileOrdered, site)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		b.Fatalf("harvest probe sites: %v", err)
	}
	if err := rows.Close(); err != nil {
		b.Fatalf("close harvest: %v", err)
	}
	if len(fileOrdered) < 1024 {
		b.Skipf("store yielded only %d probe sites", len(fileOrdered))
	}
	fromSorted := append([]graph.EdgeSite(nil), fileOrdered...)
	sort.Slice(fromSorted, func(i, j int) bool {
		if fromSorted[i].From != fromSorted[j].From {
			return fromSorted[i].From < fromSorted[j].From
		}
		return fromSorted[i].Line < fromSorted[j].Line
	})

	const chunk = 512
	rawProbe := func(sites []graph.EdgeSite) {
		args := make([]any, 0, chunk*2)
		for _, site := range sites {
			args = append(args, site.From, site.Line)
		}
		edges, err := s.queryEdgeCandidatesSQL(edgeCandidatesAnySiteQuery(len(sites)), args...)
		if err != nil {
			b.Fatalf("raw probe: %v", err)
		}
		_ = edges
	}
	runRaw := func(name string, sites []graph.EdgeSite) {
		b.Run(name, func(b *testing.B) {
			probed := 0
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				lo := (i * chunk) % (len(sites) - chunk)
				rawProbe(sites[lo : lo+chunk])
				probed += chunk
			}
			b.ReportMetric(float64(probed)/b.Elapsed().Seconds(), "sites/s")
		})
	}
	runRaw("raw_file_order", fileOrdered)
	runRaw("raw_from_sorted", fromSorted)

	b.Run("api_get_candidates", func(b *testing.B) {
		probed := 0
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			lo := (i * chunk) % (len(fileOrdered) - chunk)
			_ = s.GetEdgeCandidates(nil, fileOrdered[lo:lo+chunk])
			probed += chunk
		}
		b.ReportMetric(float64(probed)/b.Elapsed().Seconds(), "sites/s")
	})
}
