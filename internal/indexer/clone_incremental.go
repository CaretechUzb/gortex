package indexer

import (
	"sync"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
)

// incrementalCloneIndex maintains the clone-detection state (CMS +
// length-stratified LSH) live across single-file edits so a (re)index of
// one file updates EdgeSimilarTo edges in O(edited file) instead of the
// whole-graph detectClonesAndEmitEdges recompute. It is the steady-state
// counterpart of the batch pass: the batch pass re-baselines (corrects CMS
// drift) and runs diffusion; this index keeps the direct similar_to edges
// in step between batch passes.
//
// Source of truth in-session is the in-memory shingles cache; the durable
// copy lives in the CloneShingle* sidecar so Rebuild can reseed the CMS
// after a warm restart without re-parsing. Signatures are computed through
// the same kernel the batch pass uses (computeCloneSigFromShingles), so at
// a given corpus the incremental and batch edge sets are identical.
//
// It is NOT goroutine-safe beyond its own mutex — every method takes the
// lock — and is driven under the indexer's write path (one goroutine at a
// time), the same single-writer discipline the underlying clones.CMS /
// clones.StratifiedIndex assume.
type incrementalCloneIndex struct {
	mu       sync.Mutex
	cms      *clones.CMS
	lsh      *clones.StratifiedIndex
	shingles map[string][]uint64 // node id -> raw shingle set (cache)
	corpus   int
	built    bool
	// pending is true when an edit touched clone-capable functions while
	// the in-memory index was unbuilt. It is observable and stays true
	// until an explicit global/clone-consuming rebuild completes.
	pending bool
}

// newIncrementalCloneIndex returns an empty, un-built index. built stays
// false until a batch pass or Rebuild seeds it from the graph / sidecar;
// while un-built the indexer falls back to the whole-graph clone pass.
func newIncrementalCloneIndex() *incrementalCloneIndex {
	// Rebuild and AdoptBaselineOrRebuild initialize the heavy CMS/LSH state
	// before setting built. Incremental mutation callers check Ready first, so
	// an index that has not completed a global clone pass needs only this small
	// state holder.
	return &incrementalCloneIndex{}
}

// tokensFromMeta reads a node's stamped normalised-token count, tolerating
// the int / int64 / float64 shapes a backend round-trip may produce.
// Mirrors the switch in detectClonesAndEmitEdgesCtx so the LSH length
// classes match the batch pass.
func tokensFromMeta(n *graph.Node) int {
	if n == nil || n.Meta == nil {
		return 0
	}
	switch v := n.Meta[cloneTokensMetaKey].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// cloneFuncNodes filters a node slice to the function/method nodes that
// participate in clone detection.
func cloneFuncNodes(nodes []*graph.Node) []*graph.Node {
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			out = append(out, n)
		}
	}
	return out
}

// Rebuild resets the index and reseeds it from the graph's current
// signatures plus the persisted shingle sidecar. It is the warmup /
// post-batch / warm-restart path: after the whole-graph clone pass has
// stamped clone_sig on the surviving bodies (and finaliseCloneSignatures
// has persisted clone_shingles for EVERY eligible body — survivors and
// boilerplate-dropped alike — to the sidecar), Rebuild walks this repo's
// bodies, rebuilds the CMS + corpus from the persisted shingles, banks
// each surviving signature into the live LSH index, and marks built=true
// so subsequent edits go incremental.
//
// The CMS and corpus MUST mirror finaliseCloneSignatures' bodies set: that
// pass builds its CMS and useFilter/threshold from ALL eligible bodies
// (every func/method node that had clone_shingles), including the ones it
// then drops as boilerplate-dominated (no clone_sig). Seeding the CMS /
// corpus only from survivors (clone_sig present) would under-count the
// sketch and shrink the corpus, so the incremental path would filter
// against a different threshold than the batch finalise and stamp
// different signatures on the edited file. We therefore seed CMS + corpus
// from every body with persisted shingles and gate ONLY the LSH Add on a
// decodable clone_sig (survivors). This makes Rebuild's CMS/corpus
// byte-match what the batch finalise produced.
//
// Repo-scoped: it walks the repo's nodes (via cloneRepoNodes) filtered to
// n.RepoPrefix == repoPrefix so each per-repo index's corpus counts only that
// repo's bodies — matching its repo-scoped LoadCloneShingles seed. An
// unfiltered walk would count every repo's bodies into a single repo's corpus
// and skew its threshold. cloneRepoNodes always uses the exact repo projection;
// for the empty-prefix single-repo case Graph scans its shard maps directly
// without maintaining a duplicate byRepo bucket or taking a global snapshot.
//
// Tolerant of a missing/partial sidecar: a body with a clone_sig but no
// persisted shingle row still enters the LSH index (so its edges are
// maintained) — that body just contributes nothing to the CMS / corpus,
// which at the re-baseline corpus is corrected at the next batch pass.
func (ci *incrementalCloneIndex) Rebuild(g graph.Store, repoPrefix string) {
	if ci == nil || g == nil {
		return
	}
	ci.mu.Lock()
	defer ci.mu.Unlock()

	ci.cms = clones.NewCMS(65536, 4)
	ci.lsh = clones.NewStratifiedIndex()
	ci.shingles = make(map[string][]uint64)
	ci.corpus = 0
	if pager, ok := g.(graph.CloneCorpusPager); ok {
		after := ""
		loaded := false
		pageFailed := false
		for {
			page, err := pager.CloneCorpusPage(repoPrefix, after, cloneCorpusFinalizeBatch)
			if err != nil {
				pageFailed = true
				break
			}
			if len(page) == 0 {
				break
			}
			loaded = true
			for _, row := range page {
				if len(row.Shingles) > 0 {
					for _, shingle := range row.Shingles {
						ci.cms.Add(shingle)
					}
					ci.shingles[row.NodeID] = row.Shingles
				}
				ci.corpus++
				if !row.Finalized || row.Signature == "" {
					continue
				}
				sig, ok := clones.DecodeSignature(row.Signature)
				if ok {
					ci.lsh.Add(clones.Item{ID: row.NodeID, Sig: sig, TokenCount: row.TokenCount})
				}
			}
			after = page[len(page)-1].NodeID
			if len(page) < cloneCorpusFinalizeBatch {
				break
			}
		}
		if loaded && !pageFailed {
			ci.built = true
			ci.pending = false
			return
		}
		// Compatibility/error fallback starts from a clean state before the
		// legacy scoped node projection below.
		ci.cms = clones.NewCMS(65536, 4)
		ci.lsh = clones.NewStratifiedIndex()
		ci.shingles = make(map[string][]uint64)
		ci.corpus = 0
	}

	var load map[string][]uint64
	if r, ok := g.(graph.CloneShingleReader); ok {
		if rows, err := r.LoadCloneShingles(repoPrefix); err == nil {
			load = rows
		}
	}

	for _, n := range cloneRepoNodes(g, repoPrefix) {
		if n == nil {
			continue
		}
		if n.RepoPrefix != repoPrefix {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		// Seed CMS + corpus from every eligible body that has persisted
		// shingles — survivors AND boilerplate-dropped bodies — so the
		// sketch and corpus mirror finaliseCloneSignatures' bodies set.
		sh := load[n.ID]
		if len(sh) > 0 {
			for _, s := range sh {
				ci.cms.Add(s)
			}
			ci.shingles[n.ID] = sh
			ci.corpus++
		}
		// Only survivors (a decodable clone_sig) enter the LSH index —
		// dropped bodies have no signature and never produce edges.
		if n.Meta == nil {
			continue
		}
		enc, ok := n.Meta[cloneSigMetaKey].(string)
		if !ok || enc == "" {
			continue
		}
		sig, ok := clones.DecodeSignature(enc)
		if !ok {
			continue
		}
		ci.lsh.Add(clones.Item{ID: n.ID, Sig: sig, TokenCount: tokensFromMeta(n)})
	}
	ci.built = true
	ci.pending = false
}

// AdoptBaselineOrRebuild seeds the steady-state clone index from the complete
// compact corpus that the immediately preceding global detector already read.
// This transfers the CMS, retained LSH and raw-shingle cache without a second
// CloneCorpusPage scan or LSH build. Legacy stores, incomplete/error or
// cancelled finalization, prefix mismatches, and malformed baselines retain the
// exact Rebuild fallback.
func (ci *incrementalCloneIndex) AdoptBaselineOrRebuild(g graph.Store, repoPrefix string, baseline *cloneCorpusBaseline) {
	if ci == nil {
		return
	}
	if baseline == nil || !baseline.complete || baseline.repoPrefix != repoPrefix ||
		baseline.cms == nil || baseline.lsh == nil || baseline.shingles == nil ||
		baseline.corpus < 0 || baseline.itemCount < 0 || baseline.corpus < baseline.itemCount ||
		(baseline.corpus == 0 && len(baseline.shingles) != 0) {
		ci.Rebuild(g, repoPrefix)
		return
	}

	ci.mu.Lock()
	ci.cms = baseline.cms
	ci.lsh = baseline.lsh
	ci.shingles = baseline.shingles
	ci.corpus = baseline.corpus
	ci.built = true
	ci.pending = false
	// A baseline is a single-consumer handoff. Clearing the large fields both
	// documents that ownership transfer and prevents accidental double use.
	baseline.cms = nil
	baseline.lsh = nil
	baseline.shingles = nil
	baseline.items = nil
	baseline.itemCount = 0
	baseline.corpus = 0
	baseline.complete = false
	ci.mu.Unlock()
}

func (ci *incrementalCloneIndex) readyLocked() bool {
	return ci.built && ci.cms != nil && ci.lsh != nil && ci.shingles != nil
}

// Ready reports whether one-file clone maintenance can run without a
// graph-wide seed.
func (ci *incrementalCloneIndex) Ready() bool {
	if ci == nil {
		return false
	}
	ci.mu.Lock()
	defer ci.mu.Unlock()
	return ci.readyLocked()
}

// MarkPending records that clone edges may be incomplete after a bounded
// edit. It intentionally schedules no background work: an automatic rebuild
// would merely move whole-graph starvation off the watcher goroutine.
func (ci *incrementalCloneIndex) MarkPending() {
	if ci == nil {
		return
	}
	ci.mu.Lock()
	ci.pending = true
	ci.mu.Unlock()
}

func (ci *incrementalCloneIndex) Pending() bool {
	if ci == nil {
		return false
	}
	ci.mu.Lock()
	defer ci.mu.Unlock()
	return ci.pending
}

// CloneIndexPending reports whether clone-derived edges are awaiting an
// explicit global/clone-consuming rebuild.
func (idx *Indexer) CloneIndexPending() bool {
	return idx != nil && idx.cloneIndex != nil && idx.cloneIndex.Pending()
}

// EvictFuncs removes a set of function/method nodes from the index: it
// decrements their shingles out of the CMS, drops them from the LSH index
// and the in-memory cache, and deletes their rows from the persisted
// sidecar. Called with the OLD function ids of a file just before that
// file's fresh nodes are added (UpdateFuncs), so a re-index is an
// evict-then-add of only the edited file's bodies.
func (ci *incrementalCloneIndex) EvictFuncs(g graph.Store, ids []string) {
	if ci == nil || len(ids) == 0 {
		return
	}
	ci.mu.Lock()
	defer ci.mu.Unlock()
	if !ci.readyLocked() {
		ci.pending = true
		return
	}
	for _, id := range ids {
		sh, ok := ci.shingles[id]
		if !ok {
			// Not a tracked clone body (no signature / never added) —
			// still remove from the LSH index in case it was banked,
			// then move on.
			ci.lsh.Remove(id)
			continue
		}
		for _, s := range sh {
			ci.cms.Decrement(s)
		}
		delete(ci.shingles, id)
		ci.lsh.Remove(id)
		ci.corpus--
	}
	if w, ok := g.(graph.CloneShingleWriter); ok {
		_ = w.DeleteCloneShingles(ids)
	}
}

// UpdateFuncs banks the freshly-parsed function/method nodes of one file
// into the index and emits the EdgeSimilarTo edges their signatures imply.
// funcNodes carry the raw shingle set on Meta (cloneShinglesMetaKey,
// stamped by applyCloneSignatures during parse) — this method computes
// their signatures through the same kernel the batch pass uses, so the two
// paths agree exactly.
//
// Two phases. First every new body's shingles are folded into the CMS,
// cached, persisted, and the corpus count bumped — so the boilerplate
// threshold the signature kernel sees reflects the new corpus, matching
// finaliseCloneSignatures. Then each body's signature is computed, stamped
// on the node, banked into the LSH index, and queried for clone pairs;
// surviving pairs are materialised as symmetric EdgeSimilarTo edges (both
// directions, mirroring detectClonesAndEmitEdgesCtx).
func (ci *incrementalCloneIndex) UpdateFuncs(g graph.Store, repoPrefix string, funcNodes []*graph.Node, threshold float64) {
	if ci == nil || g == nil {
		return
	}
	ci.mu.Lock()
	defer ci.mu.Unlock()
	if !ci.readyLocked() {
		if len(funcNodes) > 0 {
			ci.pending = true
		}
		return
	}

	// Phase 1: fold every new body into the CMS + cache + sidecar and
	// bump the corpus count, so the boilerplate gate below sees the same
	// corpus the batch finalise would.
	type pending struct {
		node     *graph.Node
		shingles []uint64
	}
	var todo []pending
	for _, n := range funcNodes {
		if n == nil || n.Meta == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		sh, ok := n.Meta[cloneShinglesMetaKey].([]uint64)
		if !ok {
			continue
		}
		for _, s := range sh {
			ci.cms.Add(s)
		}
		ci.shingles[n.ID] = sh
		ci.corpus++
		todo = append(todo, pending{node: n, shingles: sh})
	}

	// Corpus-based gate, matching finaliseCloneSignatures exactly.
	useFilter := ci.corpus >= cmsMinCorpus
	var thr uint32
	if useFilter {
		thr = uint32(float64(ci.corpus) * cmsBoilerplateRatio)
		if thr < 1 {
			thr = 1
		}
	}

	// Phase 2: compute each signature, stamp it, bank it into the LSH
	// index, and remember the banked Item so we can query for pairs once
	// every new body is in the index. clone_shingles is removed from Meta
	// (the sidecar holds the durable copy) — mirrors finalise.
	added := make([]clones.Item, 0, len(todo))
	projection := make([]graph.CloneCorpusRow, 0, len(todo))
	for _, p := range todo {
		n := p.node
		sig, ok := computeCloneSigFromShingles(ci.cms, thr, useFilter, p.shingles)
		delete(n.Meta, cloneShinglesMetaKey)
		row := graph.CloneCorpusRow{
			NodeID: n.ID, RepoPrefix: n.RepoPrefix, Shingles: p.shingles,
			TokenCount: tokensFromMeta(n), Finalized: true,
		}
		if !ok {
			delete(n.Meta, cloneSigMetaKey)
			projection = append(projection, row)
			continue
		}
		row.Signature = clones.EncodeSignature(sig)
		n.Meta[cloneSigMetaKey] = row.Signature
		item := clones.Item{ID: n.ID, Sig: sig, TokenCount: row.TokenCount}
		ci.lsh.Add(item)
		added = append(added, item)
		projection = append(projection, row)
	}
	if writer, ok := g.(graph.CloneCorpusWriter); ok && len(projection) > 0 {
		_ = writer.BulkSetCloneCorpus(repoPrefix, projection)
	} else if writer, ok := g.(graph.CloneShingleWriter); ok && len(projection) > 0 {
		rows := make(map[string][]uint64, len(projection))
		for _, row := range projection {
			rows[row.NodeID] = row.Shingles
		}
		_ = writer.BulkSetCloneShingles(repoPrefix, rows)
	}

	// Gather clone pairs touching newly-added bodies into one bounded staging
	// slice at a time, then materialise each slice through the same endpoint-
	// prefetch + AddBatch path as the cold pass. QueryPairs can surface a pair
	// from both newly-added endpoints; the persistent seen set dedupes it across
	// flushes without retaining every Pair payload for a large edited file.
	pairs := make([]clones.Pair, 0, clonePairBatchSize)
	seen := make(map[[2]string]struct{})
	flush := func() {
		materializeClonePairs(g, pairs, graph.EdgeSimilarTo, seen)
		pairs = pairs[:0]
	}
	for _, item := range added {
		for _, pair := range ci.lsh.QueryPairs(item, threshold) {
			pairs = append(pairs, pair)
			if len(pairs) == clonePairBatchSize {
				flush()
			}
		}
	}
	if len(pairs) > 0 {
		flush()
	}
}
