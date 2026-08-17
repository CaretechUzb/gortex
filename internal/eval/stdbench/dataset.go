// Package stdbench loads standardized code-retrieval benchmarks —
// CoIR, SWE-ContextBench, and ContextBench — into a single normalized
// {corpus, queries, qrels} model and scores a retriever against them
// with the textbook Recall@K / Precision@K / NDCG@K / MRR metrics.
//
// The loaders parse the on-disk formats; the actual retrieval is left
// to the caller (the `gortex eval stdbench` verb wires in the
// store-native symbol FTS search_symbols runs), so the same harness
// measures whatever retriever is handed to Evaluate.
package stdbench

// Doc is one corpus document — a code snippet, file, or symbol the
// retriever ranks. ID is the identifier the benchmark's relevance
// judgements reference.
type Doc struct {
	ID   string
	Text string
}

// Query is one benchmark query plus its graded relevance judgements.
// Relevant maps a corpus Doc.ID to its relevance grade: 1 means
// relevant, higher grades mean more relevant (CoIR / BEIR qrels carry
// graded labels; the JSONL task benchmarks default every gold ID to
// grade 1).
type Query struct {
	ID       string
	Text     string
	Relevant map[string]int
}

// Dataset is a loaded benchmark: a corpus to index plus the queries to
// run against it.
type Dataset struct {
	Name    string
	Corpus  []Doc
	Queries []Query
}

// RelevantCount returns the number of queries that carry at least one
// relevance judgement — the denominator Evaluate averages over.
func (d Dataset) RelevantCount() int {
	n := 0
	for _, q := range d.Queries {
		if len(q.Relevant) > 0 {
			n++
		}
	}
	return n
}
