package mcp

import (
	"context"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// creditFileConsumption is the tool-call observer: when the agent opens
// a file (get_editing_context / read_file) shortly after a search, every
// symbol in that file the search surfaced is credited to the search's
// query — the same implicit (query → symbol) signal get_symbol_source
// records, extended to the "I'm about to work here" file-open tools.
// Cheap-gated on a fresh search so a file open with no recent search
// pays nothing.
func (s *Server) creditFileConsumption(ctx context.Context, filePath string) {
	if s == nil || s.combo == nil || ctx == nil || ctx.Err() != nil || filePath == "" {
		return
	}
	sess := s.sessionFor(ctx)
	if sess == nil || ctx.Err() != nil {
		return
	}
	query, matched := sess.attributedFileConsumption(filePath)
	if query != "" && len(matched) > 0 {
		s.combo.RecordBatch(query, matched)
	}
}

// forceInjectCap bounds how many learned-but-unfetched symbols a single
// search may force-inject. A small cap nudges the result set toward
// symbols the agent has reached for before on this query without letting
// the learned channel flood out fresh BM25 hits.
const forceInjectCap = 3

// forceInjectLearnedCandidates surfaces symbols that the implicit-
// feedback loop associates with this query but that BM25 never fetched —
// the "force-inject bypass-RRF" path. Without it, a symbol the agent
// repeatedly picks for a query can never re-appear once it drops out of
// the BM25 candidate set; the learned boost only reorders what BM25
// already returned.
//
// Sources, in priority order: the exact-query combo index, the
// per-keyword combo index, and the task-scoped feedback "missing" list.
// Injected candidates are deduped against the existing pool, fetched
// from the graph, then run through the same post-filter the BM25 results
// passed (repo / kind / lang / path / corpus / session scope) so an
// injected symbol can never escape the caller's scope. They are appended
// before the rerank so the combo / feedback signals rank them in
// context rather than stapling them to the tail.
func (s *Server) forceInjectLearnedCandidates(ctx context.Context, query string, nodes []*graph.Node, postFilter func([]*graph.Node) []*graph.Node) []*graph.Node {
	if s == nil || query == "" {
		return nodes
	}
	seen := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		if n != nil {
			seen[n.ID] = struct{}{}
		}
	}

	// Gather learned candidate IDs in priority order, strongest boost
	// first within each source.
	var ids []string
	appendBoosted := func(m map[string]float64) {
		if len(m) == 0 {
			return
		}
		type kv struct {
			id string
			v  float64
		}
		kvs := make([]kv, 0, len(m))
		for id, v := range m {
			kvs = append(kvs, kv{id, v})
		}
		sort.Slice(kvs, func(i, j int) bool {
			if kvs[i].v != kvs[j].v {
				return kvs[i].v > kvs[j].v
			}
			return kvs[i].id < kvs[j].id
		})
		for _, k := range kvs {
			ids = append(ids, k.id)
		}
	}
	if s.combo != nil {
		appendBoosted(s.combo.BoostMap(query))
		appendBoosted(s.combo.KeywordBoostMap(query))
	}
	if s.feedback != nil && s.feedback.HasData() {
		ids = append(ids, s.feedback.MissedSymbolsForQuery(query, 2)...)
	}
	if len(ids) == 0 {
		return nodes
	}

	reader := s.readerFor(ctx)
	var fetched []*graph.Node
	for _, id := range ids {
		if len(fetched) >= forceInjectCap {
			break
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if n := reader.GetNode(id); n != nil {
			fetched = append(fetched, n)
		}
	}
	if len(fetched) == 0 {
		return nodes
	}
	if postFilter != nil {
		fetched = postFilter(fetched)
	}
	if len(fetched) == 0 {
		return nodes
	}
	return append(nodes, fetched...)
}
