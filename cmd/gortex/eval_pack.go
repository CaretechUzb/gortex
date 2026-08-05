package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/eval/packeval"
	"github.com/zzet/gortex/internal/eval/recall"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/packstrategy"
	"github.com/zzet/gortex/internal/search/rerank"
	"github.com/zzet/gortex/internal/tokens"
)

var (
	evalPackFixture     string
	evalPackIndex       string
	evalPackFormat      string
	evalPackOut         string
	evalPackStrategies  string
	evalPackK           int
	evalPackTokenBudget int
	evalPackFetchLimit  int
	evalPackAblate      bool
	evalPackComprModel  string
)

var evalPackCmd = &cobra.Command{
	Use:   "pack",
	Short: "Precision@K / Recall@K / MRR over context-packing strategies",
	Long: `Held-out retrieval-precision harness for context packing.

Indexes a repository, runs each fixture query through the real retrieval
stack (BM25 + the rerank pipeline, including RWR/PPR centrality), then
A/Bs the pluggable pack strategies (top-k / density / file-grouped) under
a fixed token budget — scoring the delivered top-K against hand-curated
gold on Precision@K, Recall@K, and MRR.

With --ablate the sweep runs twice (centrality on / off) so the RWR
ranker's contribution to P@K is measured directly. With
--comprehension-model an LLM is asked grounded questions about the packed
context rendered in each wire format (requires ANTHROPIC_API_KEY).

Examples:
  gortex eval pack --fixture bench/fixtures/retrieval.yaml
  gortex eval pack --strategies density,top-k --token-budget 4000 --ablate
  gortex eval pack --comprehension-model claude-haiku-4-5`,
	RunE: runEvalPack,
}

func init() {
	evalPackCmd.Flags().StringVar(&evalPackFixture, "fixture", "bench/fixtures/retrieval.yaml", "fixture YAML path (reuses the recall fixture format)")
	evalPackCmd.Flags().StringVar(&evalPackIndex, "index", ".", "repository path to index before running queries")
	evalPackCmd.Flags().StringVar(&evalPackFormat, "format", "markdown", "output format: markdown or json")
	evalPackCmd.Flags().StringVar(&evalPackOut, "out", "", "output file (default: stdout)")
	evalPackCmd.Flags().StringVar(&evalPackStrategies, "strategies", "", "comma-separated strategies (default: all — top-k,density,file-grouped)")
	evalPackCmd.Flags().IntVar(&evalPackK, "k", 10, "precision/recall cutoff K")
	evalPackCmd.Flags().IntVar(&evalPackTokenBudget, "token-budget", 8000, "pack token budget")
	evalPackCmd.Flags().IntVar(&evalPackFetchLimit, "fetch-limit", 50, "candidates gathered before packing")
	evalPackCmd.Flags().BoolVar(&evalPackAblate, "ablate", false, "also run with RWR/PPR centrality disabled, to measure its contribution")
	evalPackCmd.Flags().StringVar(&evalPackComprModel, "comprehension-model", "", "LLM model for the format-comprehension probe (e.g. claude-haiku-4-5); requires ANTHROPIC_API_KEY")
	evalCmd.AddCommand(evalPackCmd)
}

func runEvalPack(_ *cobra.Command, _ []string) error {
	fixtureBytes, err := os.ReadFile(evalPackFixture)
	if err != nil {
		return fmt.Errorf("reading fixture: %w", err)
	}
	var fixture recall.Fixture
	if err := yaml.Unmarshal(fixtureBytes, &fixture); err != nil {
		return fmt.Errorf("parsing fixture: %w", err)
	}
	if len(fixture.Cases) == 0 {
		return fmt.Errorf("fixture %s has no cases", evalPackFixture)
	}
	if fixture.Name == "" {
		fixture.Name = filepath.Base(evalPackFixture)
	}

	absIndex, err := filepath.Abs(evalPackIndex)
	if err != nil {
		return fmt.Errorf("resolving index path: %w", err)
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	g, closeStore, err := newEvalStore("pack")
	if err != nil {
		return fmt.Errorf("opening eval store: %w", err)
	}
	defer closeStore()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, cfg.Index, newRecallLogger())

	fmt.Fprintf(os.Stderr, "[gortex eval pack] indexing %s...\n", absIndex)
	res, err := idx.Index(absIndex)
	if err != nil {
		return fmt.Errorf("indexing: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[gortex eval pack] indexed %d files, %d nodes in %dms\n",
		res.FileCount, res.NodeCount, res.DurationMs)

	eng := query.NewEngine(g)
	eng.SetSearch(idx.Search())
	pipeline := rerank.NewDefault()
	snap := analysis.BuildAdjacencySnapshot(g)
	fmt.Fprintf(os.Stderr, "[gortex eval pack] adjacency snapshot: %d nodes, %d edges, %d package roots\n",
		snap.NodeCount(), snap.EdgeCount(), snap.PackageRootCount())

	strategies := parsePackStrategies(evalPackStrategies)
	opts := packeval.Options{
		Strategies:  strategies,
		K:           evalPackK,
		TokenBudget: evalPackTokenBudget,
		FetchLimit:  evalPackFetchLimit,
	}

	var out strings.Builder

	runSweep := func(label string, centrality bool) {
		provider := buildPackProvider(g, eng, pipeline, snap, centrality)
		rep := packeval.Run(fixture, provider, opts)
		if evalPackFormat == "json" {
			b, _ := json.MarshalIndent(map[string]any{"label": label, "report": rep}, "", "  ")
			out.Write(b)
			out.WriteByte('\n')
			return
		}
		fmt.Fprintf(&out, "## Variant: %s\n\n", label)
		out.WriteString(packeval.Markdown(rep))
		out.WriteString("\n")
	}

	if evalPackAblate {
		runSweep("centrality ON (RWR/PPR ranker)", true)
		runSweep("centrality OFF (ablation)", false)
	} else {
		runSweep("centrality ON (RWR/PPR ranker)", true)
	}

	// Optional LLM format-comprehension probe.
	if evalPackComprModel != "" {
		out.WriteString(runPackComprehension(g, eng, pipeline, snap, fixture))
	}

	output := out.String()
	if evalPackOut != "" {
		if err := os.WriteFile(evalPackOut, []byte(output), 0o644); err != nil {
			return fmt.Errorf("writing output: %w", err)
		}
		fmt.Fprintf(os.Stderr, "[gortex eval pack] wrote %s\n", evalPackOut)
		return nil
	}
	fmt.Print(output)
	return nil
}

// buildPackProvider returns a RankedProvider backed by the live engine +
// rerank pipeline. When centrality is true the rerank Context carries a
// PersonalizedPageRank closure so ProximitySignal fires — exactly the
// production handler path, minus session-only signals.
func buildPackProvider(g graph.Store, eng *query.Engine, pipeline *rerank.Pipeline, snap *analysis.AdjacencySnapshot, centrality bool) packeval.RankedProvider {
	return func(queryStr string, limit int) []packstrategy.Item {
		nodes := eng.SearchSymbols(queryStr, limit)
		if len(nodes) == 0 {
			return nil
		}
		cands := make([]*rerank.Candidate, 0, len(nodes))
		for i, n := range nodes {
			cands = append(cands, &rerank.Candidate{Node: n, TextRank: i, VectorRank: -1})
		}
		rctx := &rerank.Context{Graph: g}
		if centrality && snap != nil {
			rctx.Centrality = func(seeds []string) map[string]float64 {
				return snap.PersonalizedPageRank(seeds, 0)
			}
		}
		ranked := pipeline.Rerank(queryStr, cands, rctx)
		items := make([]packstrategy.Item, 0, len(ranked))
		for _, c := range ranked {
			if c == nil || c.Node == nil {
				continue
			}
			items = append(items, packstrategy.Item{
				ID:       c.Node.ID,
				FilePath: c.Node.FilePath,
				Score:    c.Score,
				Tokens:   estimateSymbolTokens(c.Node),
			})
		}
		return items
	}
}

// estimateSymbolTokens approximates a symbol's packed token cost from its
// line span (the pack would carry roughly the function body), with a
// floor so a one-line symbol still has a non-trivial cost.
func estimateSymbolTokens(n *graph.Node) int {
	if n == nil {
		return 12
	}
	if sig, ok := n.Meta["signature"].(string); ok && sig != "" && n.EndLine <= n.StartLine {
		return tokens.Count(sig) + 8
	}
	lines := n.EndLine - n.StartLine + 1
	if lines < 1 {
		lines = 1
	}
	const tokensPerLine = 11
	return lines*tokensPerLine + 8
}

// runPackComprehension renders a small packed context per format and asks
// an LLM grounded questions, reporting per-format comprehension. The
// pack is built from the first concept fixture case's top reranked
// symbols. Skips cleanly when ANTHROPIC_API_KEY is unset.
func runPackComprehension(g graph.Store, eng *query.Engine, pipeline *rerank.Pipeline, snap *analysis.AdjacencySnapshot, fixture recall.Fixture) string {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	var ask packeval.Asker
	if apiKey != "" {
		ask = anthropicAsker(apiKey, evalPackComprModel)
	}

	// Build entries from the first case's top reranked symbols.
	provider := buildPackProvider(g, eng, pipeline, snap, true)
	var entries []packeval.ContextEntry
	var firstQuery string
	for _, c := range fixture.Cases {
		items := provider(c.Query, 12)
		if len(items) == 0 {
			continue
		}
		firstQuery = c.Query
		for i, it := range items {
			if i >= 6 {
				break
			}
			n := g.GetNode(it.ID)
			if n == nil {
				continue
			}
			sig, _ := n.Meta["signature"].(string)
			entries = append(entries, packeval.ContextEntry{
				ID: n.ID, Name: n.Name, FilePath: n.FilePath, Signature: sig,
			})
		}
		break
	}
	if len(entries) == 0 {
		return "\n## Format comprehension\n\n_no candidates to pack_\n"
	}

	questions := []packeval.ComprehensionQuestion{
		{Question: fmt.Sprintf("Which symbol in the pack is most relevant to: %q? Reply with just its name.", firstQuery), Accept: []string{entries[0].Name}},
		{Question: "Name any one file path that appears in the pack.", Accept: filePathsOf(entries)},
	}
	renderers := map[string]packeval.FormatRenderer{
		"json":     renderEntriesJSON,
		"markdown": renderEntriesMarkdown,
		"toon":     renderEntriesTOON,
	}
	rep := packeval.RunFormatComprehension(entries, questions, renderers, tokens.Count, ask)
	return "\n" + packeval.ComprehensionMarkdown(rep)
}

func filePathsOf(entries []packeval.ContextEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.FilePath != "" {
			out = append(out, e.FilePath)
		}
	}
	return out
}

func renderEntriesJSON(entries []packeval.ContextEntry) string {
	b, _ := json.Marshal(entries)
	return string(b)
}

func renderEntriesMarkdown(entries []packeval.ContextEntry) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "- **%s** (`%s`) — %s\n", e.Name, e.FilePath, e.Signature)
	}
	return b.String()
}

func renderEntriesTOON(entries []packeval.ContextEntry) string {
	var b strings.Builder
	b.WriteString("name\tfile\tsignature\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", e.Name, e.FilePath, e.Signature)
	}
	return b.String()
}

// anthropicAsker returns an Asker that calls the Anthropic Messages API,
// mirroring the recall judge's minimal HTTP client.
func anthropicAsker(apiKey, model string) packeval.Asker {
	client := &http.Client{Timeout: 30 * time.Second}
	return func(prompt string) (string, error) {
		payload := map[string]any{
			"model":      model,
			"max_tokens": 64,
			"messages":   []map[string]any{{"role": "user", "content": prompt}},
		}
		body, _ := json.Marshal(payload)
		req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("content-type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			return "", fmt.Errorf("anthropic http %d: %s", resp.StatusCode, string(respBody))
		}
		var parsed struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return "", err
		}
		var text string
		for _, c := range parsed.Content {
			if c.Type == "text" {
				text += c.Text
			}
		}
		return text, nil
	}
}

// parsePackStrategies parses the --strategies flag; empty means all.
func parsePackStrategies(s string) []packstrategy.Strategy {
	s = strings.TrimSpace(s)
	if s == "" {
		return packstrategy.All()
	}
	var out []packstrategy.Strategy
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, packstrategy.Normalize(part))
		}
	}
	if len(out) == 0 {
		return packstrategy.All()
	}
	return out
}
