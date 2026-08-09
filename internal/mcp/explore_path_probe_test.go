package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

func pathProbeCandidate(id, file string, textRank int) *rerank.Candidate {
	return &rerank.Candidate{
		Node: &graph.Node{
			ID: id, Name: id, QualName: id,
			Kind: graph.KindFunction, FilePath: file,
		},
		TextRank:   textRank,
		VectorRank: -1,
	}
}

func pathProbeIDs(candidates []*rerank.Candidate) []string {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil || candidate.Node == nil {
			ids = append(ids, "")
			continue
		}
		ids = append(ids, candidate.Node.ID)
	}
	return ids
}

func TestExplorePathProbesScoreOnlyCorroboratedPaths(t *testing.T) {
	probes := newExplorePathProbes(
		"Coverage reports are empty after `CollectCoverage` is enabled and COVERAGE_FORMAT=cobertura",
	)
	if probes.empty() {
		t.Fatal("task with a quoted identifier and an environment probe derived no probes")
	}
	if len(probes.probes) > explorePathProbeLimit {
		t.Fatalf("derived %d probes, want at most %d", len(probes.probes), explorePathProbeLimit)
	}

	corroborated := probes.scorePath("internal/report/collect_coverage.go")
	if corroborated <= 0 {
		t.Fatal("a path naming both probe words scored nothing")
	}
	if partial := probes.scorePath("internal/report/collect_symbols.go"); partial >= corroborated {
		t.Fatalf("partial path match scored %d, want less than %d", partial, corroborated)
	}
	if unrelated := probes.scorePath("internal/graph/store.go"); unrelated != 0 {
		t.Fatalf("unrelated path scored %d, want 0", unrelated)
	}
}

func TestExplorePathProbesCapCorroborationPerPath(t *testing.T) {
	probes := newExplorePathProbes(
		"`RenderTable` and `ParseHeader` and `WriteFooter` and `ScanBorder` disagree",
	)
	if len(probes.probes) < explorePathProbeMatchCap+1 {
		t.Fatalf("fixture derived %d probes, want more than the match cap", len(probes.probes))
	}
	stuffed := probes.scorePath("render_table/parse_header/write_footer/scan_border.go")
	best := 0
	for index := 0; index < explorePathProbeMatchCap; index++ {
		best += probes.probes[index].weight
	}
	if stuffed > best {
		t.Fatalf("term-stuffed path scored %d, want at most %d", stuffed, best)
	}
}

func TestExplorePathProbesIgnoreGenericTaskWords(t *testing.T) {
	probes := newExplorePathProbes("Fix the `issue` in \"the file\" so `code` works")
	for _, path := range []string{"internal/issue.go", "internal/code/file.go"} {
		if score := probes.scorePath(path); score != 0 {
			t.Fatalf("generic probe corroborated %q with %d", path, score)
		}
	}
}

func TestExplorePathProbeLiftAdmitsCorroboratedCandidateBelowCut(t *testing.T) {
	probes := newExplorePathProbes("`CollectCoverage` returns nothing")
	prod := []*rerank.Candidate{
		pathProbeCandidate("head", "internal/a/head.go", 0),
		pathProbeCandidate("second", "internal/b/second.go", 1),
		pathProbeCandidate("weakest", "internal/c/weakest.go", 2),
		pathProbeCandidate("corroborated", "internal/report/collect_coverage.go", 3),
	}
	stampExplorePathProbeScores(prod, probes)
	if explorePathProbeScore(prod[3]) <= 0 {
		t.Fatal("fixture candidate carries no path corroboration")
	}

	lifted := liftExplorePathProbeCandidates(prod, 3, nil)
	selected := selectFinalExploreCandidates(lifted, nil, 3)

	if len(selected) != 3 {
		t.Fatalf("page width = %d, want 3", len(selected))
	}
	if selected[0].Node.ID != "head" {
		t.Fatalf("head row = %q, want head", selected[0].Node.ID)
	}
	if candidateByID(selected, "corroborated") == nil {
		t.Fatalf("corroborated candidate stayed below the cut: %v", pathProbeIDs(selected))
	}
	if candidateByID(selected, "weakest") != nil {
		t.Fatalf("the lift traded a row other than the weakest: %v", pathProbeIDs(selected))
	}
	if candidateByID(selected, "second") == nil {
		t.Fatalf("the lift displaced a stronger row: %v", pathProbeIDs(selected))
	}
	again := liftExplorePathProbeCandidates(prod, 3, nil)
	if fmt.Sprint(pathProbeIDs(lifted)) != fmt.Sprint(pathProbeIDs(again)) {
		t.Fatal("the lift is not deterministic")
	}
}

func TestExplorePathProbeLiftNeverDisplacesReservedRows(t *testing.T) {
	probes := newExplorePathProbes("`CollectCoverage` returns nothing")
	prod := []*rerank.Candidate{
		pathProbeCandidate("head", "internal/a/head.go", 0),
		pathProbeCandidate("anchor", "internal/b/anchor.go", 1),
		pathProbeCandidate("literal", "internal/c/literal.go", 2),
		pathProbeCandidate("corroborated", "internal/report/collect_coverage.go", 3),
	}
	prod[2].Signals = map[string]float64{exploreSourceLiteralSignal: 1}
	stampExplorePathProbeScores(prod, probes)
	reserved := exploreFinalReservedCandidateIDs(prod, map[int]string{0: "anchor"}, "")

	lifted := liftExplorePathProbeCandidates(prod, 3, reserved)

	if fmt.Sprint(pathProbeIDs(lifted)) != fmt.Sprint(pathProbeIDs(prod)) {
		t.Fatalf("path corroboration evicted reserved evidence: %v", pathProbeIDs(lifted))
	}
}

func TestExplorePathProbeLiftIsNoOpWithoutCorroboration(t *testing.T) {
	probes := newExplorePathProbes("`CollectCoverage` returns nothing")
	prod := []*rerank.Candidate{
		pathProbeCandidate("head", "internal/a/head.go", 0),
		pathProbeCandidate("second", "internal/b/second.go", 1),
		pathProbeCandidate("weakest", "internal/c/weakest.go", 2),
		pathProbeCandidate("unrelated", "internal/d/unrelated.go", 3),
	}
	stampExplorePathProbeScores(prod, probes)
	for _, candidate := range prod {
		if explorePathProbeScore(candidate) != 0 {
			t.Fatalf("candidate %q was stamped without a matching probe", candidate.Node.ID)
		}
	}

	lifted := liftExplorePathProbeCandidates(prod, 3, nil)

	if fmt.Sprint(pathProbeIDs(lifted)) != fmt.Sprint(pathProbeIDs(prod)) {
		t.Fatalf("uncorroborated page was reordered: %v", pathProbeIDs(lifted))
	}
}

func TestExplorePathProbeLiftKeepsBetterCorroborationInsideTheCut(t *testing.T) {
	probes := newExplorePathProbes("`CollectCoverage` returns nothing")
	prod := []*rerank.Candidate{
		pathProbeCandidate("head", "internal/a/head.go", 0),
		pathProbeCandidate("second", "internal/b/second.go", 1),
		pathProbeCandidate("inside", "internal/report/collect_coverage.go", 2),
		pathProbeCandidate("below", "cmd/collect_coverage/main.go", 3),
	}
	stampExplorePathProbeScores(prod, probes)

	lifted := liftExplorePathProbeCandidates(prod, 3, nil)

	if fmt.Sprint(pathProbeIDs(lifted)) != fmt.Sprint(pathProbeIDs(prod)) {
		t.Fatalf("an equally corroborated row was traded away: %v", pathProbeIDs(lifted))
	}
}

func TestLeadingRankedFilePrefersProbeCorroboratedFileOnTiedRanking(t *testing.T) {
	probes := newExplorePathProbes("`CollectCoverage` returns nothing")
	pool := []*rerank.Candidate{
		pathProbeCandidate("a", "internal/a/first.go", 0),
		pathProbeCandidate("b", "internal/b/second.go", 1),
		pathProbeCandidate("c", "internal/report/collect_coverage.go", 2),
		pathProbeCandidate("d", "internal/d/fourth.go", 3),
		pathProbeCandidate("e", "internal/e/fifth.go", 4),
	}
	if file := exploreLeadingRankedFile(pool); file != "" {
		t.Fatalf("scattered ranking elected %q before corroboration", file)
	}

	stampExplorePathProbeScores(pool, probes)

	if file := exploreLeadingRankedFile(pool); file != "internal/report/collect_coverage.go" {
		t.Fatalf("leading file = %q, want the probe-corroborated file", file)
	}
}

func TestLeadingRankedFileKeepsSettledRankingOverProbes(t *testing.T) {
	probes := newExplorePathProbes("`CollectCoverage` returns nothing")
	pool := []*rerank.Candidate{
		pathProbeCandidate("a", "internal/a/first.go", 0),
		pathProbeCandidate("b", "internal/report/collect_coverage.go", 1),
		pathProbeCandidate("c", "internal/a/first.go", 2),
		pathProbeCandidate("d", "internal/d/fourth.go", 3),
		pathProbeCandidate("e", "internal/e/fifth.go", 4),
	}
	stampExplorePathProbeScores(pool, probes)

	if file := exploreLeadingRankedFile(pool); file != "internal/a/first.go" {
		t.Fatalf("leading file = %q, want the file ranking already settled on", file)
	}
}

func TestLeadingRankedFileStaysUnelectedWhenProbesTie(t *testing.T) {
	probes := newExplorePathProbes("`CollectCoverage` returns nothing")
	pool := []*rerank.Candidate{
		pathProbeCandidate("a", "internal/a/first.go", 0),
		pathProbeCandidate("b", "internal/report/collect_coverage.go", 1),
		pathProbeCandidate("c", "cmd/collect_coverage/main.go", 2),
		pathProbeCandidate("d", "internal/d/fourth.go", 3),
		pathProbeCandidate("e", "internal/e/fifth.go", 4),
	}
	stampExplorePathProbeScores(pool, probes)

	if file := exploreLeadingRankedFile(pool); file != "" {
		t.Fatalf("leading file = %q, want no election while corroboration is tied", file)
	}
}

// The source lane derives a wider probe set from the same ranking; the artifact
// lane's own bound is unchanged.
func TestRankedExploreArtifactProbesKeepTheirOwnBound(t *testing.T) {
	task := "`RenderTable` and `ParseHeader` and `WriteFooter` and `ScanBorder` disagree in build config"
	seen := make(map[string]struct{})

	artifact := rankedExploreArtifactProbes(task, seen)

	if len(artifact) != exploreArtifactProbeLimit {
		t.Fatalf("artifact probes = %d, want %d", len(artifact), exploreArtifactProbeLimit)
	}
	for _, probe := range artifact {
		if _, marked := seen[strings.ToLower(probe)]; !marked {
			t.Fatalf("emitted probe %q did not mark the consumed term set", probe)
		}
	}
	if source := newExplorePathProbes(task); len(source.probes) <= exploreArtifactProbeLimit {
		t.Fatalf("source probes = %d, want more than the artifact bound", len(source.probes))
	}
}
