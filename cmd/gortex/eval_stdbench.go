package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/eval/stdbench"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

var (
	evalStdbenchBench   string
	evalStdbenchDataset string
	evalStdbenchFormat  string
	evalStdbenchOut     string
)

var evalStdbenchCmd = &cobra.Command{
	Use:   "stdbench",
	Short: "Run a standardized retrieval benchmark (CoIR / SWE-ContextBench / ContextBench)",
	Long: `Runs Gortex's shipping text retrieval — the store-native symbol FTS
search_symbols queries — against a standardized code-retrieval benchmark
and reports Recall@K, Precision@K, NDCG@10, and MRR.

Benchmarks (--bench):
  coir              CoIR (Code Information Retrieval, ACL 2025). --dataset is
                    a BEIR-layout directory: corpus.jsonl + queries.jsonl +
                    qrels/<split>.tsv.
  swe-contextbench  SWE-ContextBench (arXiv 2602.08316). --dataset is a JSONL
                    file, one context-retrieval task per line.
  contextbench      ContextBench (arXiv 2602.05892). Same JSONL task layout.

The JSONL task line schema is {id, query, relevant:[ids], candidates:[{id,
text}]}; per-task candidate pools are merged into one corpus. Field-name
aliases (question / problem_statement, gold / context, documents / pool)
are accepted.

Typical use:

  gortex eval stdbench --bench coir --dataset datasets/coir/codesearchnet
  gortex eval stdbench --bench contextbench --dataset datasets/contextbench.jsonl --format json`,
	RunE: runEvalStdbench,
}

func init() {
	evalStdbenchCmd.Flags().StringVar(&evalStdbenchBench, "bench", "", "benchmark: coir | swe-contextbench | contextbench")
	evalStdbenchCmd.Flags().StringVar(&evalStdbenchDataset, "dataset", "", "dataset path: a BEIR directory for coir, a .jsonl file for the others")
	evalStdbenchCmd.Flags().StringVar(&evalStdbenchFormat, "format", "markdown", "output format: markdown or json")
	evalStdbenchCmd.Flags().StringVar(&evalStdbenchOut, "out", "", "output file (default: stdout)")
	evalCmd.AddCommand(evalStdbenchCmd)
}

func runEvalStdbench(_ *cobra.Command, _ []string) error {
	if evalStdbenchDataset == "" {
		return fmt.Errorf("--dataset is required")
	}

	var (
		ds  stdbench.Dataset
		err error
	)
	switch strings.ToLower(strings.TrimSpace(evalStdbenchBench)) {
	case "coir":
		ds, err = stdbench.LoadCoIR(evalStdbenchDataset)
	case "swe-contextbench", "swe_contextbench", "swecontextbench":
		ds, err = stdbench.LoadSWEContextBench(evalStdbenchDataset)
	case "contextbench":
		ds, err = stdbench.LoadContextBench(evalStdbenchDataset)
	default:
		return fmt.Errorf("unknown --bench %q (want coir, swe-contextbench, or contextbench)", evalStdbenchBench)
	}
	if err != nil {
		return fmt.Errorf("loading %s: %w", evalStdbenchBench, err)
	}

	if len(ds.Corpus) == 0 {
		return fmt.Errorf("benchmark %q has an empty corpus — CoIR ships corpus.jsonl; "+
			"for the JSONL task benchmarks each task must carry a `candidates` pool", evalStdbenchBench)
	}
	if ds.RelevantCount() == 0 {
		return fmt.Errorf("benchmark %q carries no relevance judgements to score against", evalStdbenchBench)
	}

	// Index the corpus into a throwaway SQLite store's native symbol FTS —
	// the text retrieval search_symbols actually runs — and rank doc IDs
	// per query. Note the corpus goes in through the store's FTS writer
	// directly, never through search.Backend.Add: the shipping backend is
	// an adapter over this same FTS whose Add is a no-op, so routing the
	// corpus through it would index nothing and report a silent 0.0.
	st, closeStore, err := newEvalStore("stdbench")
	if err != nil {
		return fmt.Errorf("opening the eval store: %w", err)
	}
	defer closeStore()

	searcher, ok := st.(graph.SymbolSearcher)
	if !ok {
		return fmt.Errorf("the eval store does not expose native symbol search")
	}
	counter, ok := st.(graph.SymbolFTSCounter)
	if !ok {
		return fmt.Errorf("the eval store does not report its symbol FTS document count")
	}

	items := make([]graph.SymbolFTSItem, 0, len(ds.Corpus))
	for _, d := range ds.Corpus {
		// Mirrors the indexer's ftsTokensFor: the write side splits with
		// search.Tokenize so the read side's query tokenisation lands on
		// the same terms.
		items = append(items, graph.SymbolFTSItem{
			NodeID: d.ID,
			Tokens: strings.Join(search.Tokenize(d.Text), " "),
		})
	}
	// One call, not a chunked loop: BulkUpsertSymbolFTS wipes the prefix
	// before inserting, so a second call under the same prefix would erase
	// the first. It bounds its own INSERT statements internally.
	if err := searcher.BulkUpsertSymbolFTS("", items); err != nil {
		return fmt.Errorf("indexing the %s corpus: %w", ds.Name, err)
	}
	if err := searcher.BuildSymbolIndex(); err != nil {
		return fmt.Errorf("building the symbol index: %w", err)
	}
	indexed, err := counter.SymbolFTSCount()
	if err != nil {
		return fmt.Errorf("counting the indexed corpus: %w", err)
	}
	if indexed != len(ds.Corpus) {
		return fmt.Errorf("indexed %d of %d corpus documents — scoring a partial corpus "+
			"would report a retrieval number no user can reproduce", indexed, len(ds.Corpus))
	}

	var retrieveErr error
	retrieve := func(query string, k int) []string {
		hits, err := searcher.SearchSymbols(query, k)
		if err != nil {
			if retrieveErr == nil {
				retrieveErr = err
			}
			return nil
		}
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.NodeID)
		}
		return ids
	}

	metrics := stdbench.Evaluate(ds, retrieve, nil)
	if retrieveErr != nil {
		return fmt.Errorf("searching the corpus: %w", retrieveErr)
	}

	var rendered string
	if strings.EqualFold(evalStdbenchFormat, "json") {
		b, err := json.MarshalIndent(metrics, "", "  ")
		if err != nil {
			return err
		}
		rendered = string(b)
	} else {
		rendered = metrics.Markdown()
	}

	if evalStdbenchOut != "" {
		if err := os.WriteFile(evalStdbenchOut, []byte(rendered+"\n"), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", evalStdbenchOut)
		return nil
	}
	fmt.Println(rendered)
	return nil
}
