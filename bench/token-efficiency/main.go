// Command token-efficiency runs the 3-pipeline token-economy
// benchmark: ripgrep+full-read vs ripgrep+context vs gortex
// (search_symbols + get_symbol_source). See bench/token-efficiency/
// README.md for the contract and the methodology footnote it places
// in BENCHMARK.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// benchRow captures one query's outcome across all three pipelines.
type benchRow struct {
	Query       string         `json:"query"`
	Expected    []string       `json:"expected"`
	RipgrepFull pipelineResult `json:"ripgrep_full"`
	RipgrepCtx  pipelineResult `json:"ripgrep_context"`
	Gortex      pipelineResult `json:"gortex"`
	// Recall@k by token budget. 2k is the "agent reads two pages of
	// context" frame; 10k is "agent reads a quarter of a window".
	RecallAt2kFull    float64 `json:"recall_at_2k_full"`
	RecallAt2kCtx     float64 `json:"recall_at_2k_context"`
	RecallAt2kGortex  float64 `json:"recall_at_2k_gortex"`
	RecallAt10kFull   float64 `json:"recall_at_10k_full"`
	RecallAt10kCtx    float64 `json:"recall_at_10k_context"`
	RecallAt10kGortex float64 `json:"recall_at_10k_gortex"`
}

func main() {
	repo := flag.String("repo", ".", "indexed repository path (queries run against this corpus)")
	queriesPath := flag.String("queries", "bench/token-efficiency/queries.json", "JSON query set")
	truthPath := flag.String("groundtruth", "bench/token-efficiency/groundtruth.json", "JSON per-query expected file paths")
	out := flag.String("out", "", "markdown output path (default stdout)")
	jsonOut := flag.String("json", "", "optional JSON metrics output")
	format := flag.String("format", "markdown", "markdown | json")
	topK := flag.Int("top-k", 5, "candidates per query for the gortex pipeline")
	budgetRatio := flag.Float64("budget-ratio", 0.5, "fail when gortex median tokens > budget-ratio × ripgrep+full-read median (0 disables)")
	strict := flag.Bool("strict", false, "exit 1 when the budget gate trips")
	skipRipgrep := flag.Bool("skip-ripgrep", false, "skip ripgrep pipelines (e.g. when rg isn't installed)")
	flag.Parse()

	absRepo, err := filepath.Abs(*repo)
	if err != nil {
		die("repo path: %v", err)
	}
	if _, err := os.Stat(absRepo); err != nil {
		die("repo path: %v", err)
	}

	queries, err := loadQueries(*queriesPath)
	if err != nil {
		die("queries: %v", err)
	}
	truth, err := loadGroundTruth(*truthPath)
	if err != nil {
		die("groundtruth: %v", err)
	}

	// Detect ripgrep up front so we can short-circuit cleanly.
	rgAvailable := !*skipRipgrep
	if rgAvailable {
		if _, err := exec.LookPath("rg"); err != nil {
			fmt.Fprintf(os.Stderr, "[token-eff] ripgrep not on PATH; --skip-ripgrep implied\n")
			rgAvailable = false
		}
	}

	// Index the repo once for the gortex pipeline.
	fmt.Fprintf(os.Stderr, "[token-eff] indexing %s...\n", absRepo)
	indexed, err := indexRepoForBench(absRepo)
	if err != nil {
		die("index: %v", err)
	}
	defer indexed.close()
	fmt.Fprintf(os.Stderr, "[token-eff] indexed %d nodes\n", indexed.graph.NodeCount())

	// Run each query across the available pipelines.
	rows := make([]benchRow, 0, len(queries))
	for _, q := range queries {
		row := benchRow{Query: q, Expected: truth[q]}
		if rgAvailable {
			row.RipgrepFull = runRipgrepFullRead(absRepo, q)
			row.RipgrepCtx = runRipgrepContext(absRepo, q)
		}
		row.Gortex = runGortex(absRepo, q, indexed, *topK)
		row.RecallAt2kFull = recallAtBudget(row.RipgrepFull, row.Expected, 2_000)
		row.RecallAt2kCtx = recallAtBudget(row.RipgrepCtx, row.Expected, 2_000)
		row.RecallAt2kGortex = recallAtBudget(row.Gortex, row.Expected, 2_000)
		row.RecallAt10kFull = recallAtBudget(row.RipgrepFull, row.Expected, 10_000)
		row.RecallAt10kCtx = recallAtBudget(row.RipgrepCtx, row.Expected, 10_000)
		row.RecallAt10kGortex = recallAtBudget(row.Gortex, row.Expected, 10_000)
		rows = append(rows, row)
		fmt.Fprintf(os.Stderr, "[token-eff] %-45s  full=%-7d ctx=%-7d gortex=%d\n",
			q, row.RipgrepFull.Tokens, row.RipgrepCtx.Tokens, row.Gortex.Tokens)
	}

	// Render primary output.
	var primary []byte
	switch strings.ToLower(*format) {
	case "markdown", "md":
		primary = []byte(renderMarkdown(rows, rgAvailable))
	case "json":
		primary = mustMarshalJSON(rows)
	default:
		die("unknown --format %q", *format)
	}
	if err := writeOutput(*out, primary); err != nil {
		die("write primary: %v", err)
	}

	// Optional companion JSON.
	if *jsonOut != "" {
		if err := writeOutput(*jsonOut, mustMarshalJSON(rows)); err != nil {
			die("write json: %v", err)
		}
	}

	// Budget gate.
	if *strict && *budgetRatio > 0 && rgAvailable {
		medianFull := medianTokensFn(rows, func(r benchRow) int { return r.RipgrepFull.Tokens })
		medianGortex := medianTokensFn(rows, func(r benchRow) int { return r.Gortex.Tokens })
		if medianFull > 0 {
			ratio := float64(medianGortex) / float64(medianFull)
			if ratio > *budgetRatio {
				die("budget gate: gortex median tokens (%d) / ripgrep+full-read median (%d) = %.2f > limit %.2f",
					medianGortex, medianFull, ratio, *budgetRatio)
			}
		}
	}
}

// --- rendering ------------------------------------------------------

// renderMarkdown produces the per-query table plus a summary line.
// When ripgrep is unavailable the ripgrep columns render as "—" so a
// CI without rg still gets a meaningful gortex-only table.
func renderMarkdown(rows []benchRow, rgAvailable bool) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Token-efficiency benchmark")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "_Tokens per response (lower is better) and recall@k-by-token-budget for three retrieval pipelines: ripgrep+full-read (naive agent baseline), ripgrep+context (±50 lines around hit), and gortex (search_symbols + get_symbol_source)._")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| query | tokens (rg+full) | tokens (rg+ctx) | tokens (gortex) | recall@2k rg+full / rg+ctx / gortex | recall@10k rg+full / rg+ctx / gortex |")
	fmt.Fprintln(&b, "|-------|----------------:|----------------:|---------------:|------------------------------------|--------------------------------------|")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %s / %s / %.2f | %s / %s / %.2f |\n",
			truncate(r.Query, 38),
			tokensCell(r.RipgrepFull, rgAvailable),
			tokensCell(r.RipgrepCtx, rgAvailable),
			r.Gortex.Tokens,
			recallCell(r.RecallAt2kFull, rgAvailable),
			recallCell(r.RecallAt2kCtx, rgAvailable),
			r.RecallAt2kGortex,
			recallCell(r.RecallAt10kFull, rgAvailable),
			recallCell(r.RecallAt10kCtx, rgAvailable),
			r.RecallAt10kGortex,
		)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, summaryLine(rows, rgAvailable))
	return b.String()
}

func tokensCell(p pipelineResult, available bool) string {
	if !available {
		return "—"
	}
	if p.Error != "" {
		return "ERR"
	}
	return fmt.Sprintf("%d", p.Tokens)
}

func recallCell(v float64, available bool) string {
	if !available {
		return "—"
	}
	return fmt.Sprintf("%.2f", v)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// summaryLine emits the headline median-tokens + recall figure that
// real readers care about. Format:
//
//	"Median tokens: rg+full=X / rg+ctx=Y / gortex=Z (gortex −P%).
//	 Median recall@2k: rg+full=A / rg+ctx=B / gortex=C."
func summaryLine(rows []benchRow, rgAvailable bool) string {
	if len(rows) == 0 {
		return "_no rows_"
	}
	mt := func(f func(benchRow) int) int { return medianTokensFn(rows, f) }
	mr := func(f func(benchRow) float64) float64 { return medianFloatFn(rows, f) }
	mFull := mt(func(r benchRow) int { return r.RipgrepFull.Tokens })
	mCtx := mt(func(r benchRow) int { return r.RipgrepCtx.Tokens })
	mGortex := mt(func(r benchRow) int { return r.Gortex.Tokens })

	rec2Full := mr(func(r benchRow) float64 { return r.RecallAt2kFull })
	rec2Ctx := mr(func(r benchRow) float64 { return r.RecallAt2kCtx })
	rec2Gortex := mr(func(r benchRow) float64 { return r.RecallAt2kGortex })

	if !rgAvailable {
		return fmt.Sprintf("**Summary (gortex only — rg not installed):** Median tokens %d. Median recall@2k %.2f.",
			mGortex, rec2Gortex)
	}
	ratio := ""
	if mFull > 0 {
		delta := (1.0 - float64(mGortex)/float64(mFull)) * 100
		ratio = fmt.Sprintf(" gortex saves %.1f%% vs rg+full.", delta)
	}
	return fmt.Sprintf("**Summary:** Median tokens — rg+full=%d / rg+ctx=%d / gortex=%d.%s Median recall@2k — rg+full=%.2f / rg+ctx=%.2f / gortex=%.2f.",
		mFull, mCtx, mGortex, ratio,
		rec2Full, rec2Ctx, rec2Gortex,
	)
}

// medianTokensFn / medianFloatFn condense per-pipeline values across
// rows into the median. Used by both the summary line and the budget
// gate so the two stay in lock-step.
func medianTokensFn(rows []benchRow, pick func(benchRow) int) int {
	if len(rows) == 0 {
		return 0
	}
	vs := make([]int, 0, len(rows))
	for _, r := range rows {
		vs = append(vs, pick(r))
	}
	sort.Ints(vs)
	return vs[len(vs)/2]
}

func medianFloatFn(rows []benchRow, pick func(benchRow) float64) float64 {
	if len(rows) == 0 {
		return 0
	}
	vs := make([]float64, 0, len(rows))
	for _, r := range rows {
		vs = append(vs, pick(r))
	}
	sort.Float64s(vs)
	return vs[len(vs)/2]
}

// --- helpers --------------------------------------------------------

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "token-eff: "+format+"\n", args...)
	os.Exit(1)
}

func writeOutput(path string, body []byte) error {
	if path == "" {
		_, err := os.Stdout.Write(body)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

func mustMarshalJSON(rows []benchRow) []byte {
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		die("marshal json: %v", err)
	}
	return append(b, '\n')
}

func loadQueries(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Queries []string `json:"queries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc.Queries, nil
}

func loadGroundTruth(path string) (map[string][]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Queries map[string][]string `json:"queries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Queries == nil {
		doc.Queries = map[string][]string{}
	}
	return doc.Queries, nil
}
