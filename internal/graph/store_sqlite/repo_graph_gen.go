package store_sqlite

import (
	"context"
	"database/sql"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// The durable per-repo mutation anchor.
//
// Readiness asks "has every stage caught up with the graph as it is now?", so
// it needs a value that moves whenever the graph moves. repo_index_state's
// indexed_at cannot serve: it has exactly two writers (a full reindex, and the
// git watcher on a HEAD transition), so the most common mutation of all -- an
// incremental single-file reindex -- leaves it untouched. A stage compared
// against it would read "current" forever after an ordinary edit.
//
//	  write path                       repo_graph_gen         derive_state
//	  ----------                       --------------         ------------
//	  derive runs; its own edges land  bump ->  41                    0
//	  derive completes, stamps                  41    read -> derived_gen 41
//	  file saved, batch commits        bump ->  42                   41
//	  readiness compares                        42       41 <  42 -> PARTIAL
//	  derive re-runs and completes     bump ->  43    read -> derived_gen 43
//	  readiness compares                        43       43 == 43 -> READY
//
// Note which end of the derive is stamped. The passes are graph writers
// themselves, so they advance the anchor as they run; a stamp taken at derive
// START would be behind the moment the derive ended, and no later derive would
// close the gap, because a re-derive over unchanged content inserts nothing and
// so moves nothing. StampDeriveState therefore reads the anchor at completion,
// in the transaction that writes the row.
//
// Two properties make this the anchor rather than a timestamp: it advances on
// every committed mutation regardless of which writer caused it, and two
// distinct integers cannot collide the way two same-second timestamps can.
//
// The bump is written INSIDE the mutating transaction (see
// commitGraphMutation), never after it. Bumping after the commit would leave a
// crash window in which the graph has moved but the anchor has not, and a
// stage stamped at the old value would read "current" against a graph it no
// longer describes -- the exact silent wrong answer readiness exists to catch.

// bumpRepoGensTx advances the anchor for each named repo inside the caller's
// transaction. The upsert seeds a repo whose row does not exist yet (tracked
// after the v13 migration ran), so no caller has to pre-create one.
//
// Duplicate prefixes are collapsed first: one transaction is one mutation, and
// a batch naming a repo twice must advance it once, not once per mention.
func bumpRepoGensTx(tx *sql.Tx, prefixes []string) error {
	for _, prefix := range dedupePrefixes(prefixes) {
		if _, err := tx.Exec(`INSERT INTO repo_graph_gen (repo_prefix, gen) VALUES (?, 1)
			ON CONFLICT(repo_prefix) DO UPDATE SET gen = repo_graph_gen.gen + 1`, prefix); err != nil {
			return err
		}
	}
	return nil
}

// bumpAllRepoGensTx is the conservative fallback for a mutation whose blast
// radius genuinely spans every repository, or that cannot name the repos it
// touched (an edge-kind eviction deletes by kind across the whole store).
//
// Over-bumping is deliberately the safe direction. A repo advanced without
// having really changed reads "partial" -- a false alarm that the next derive
// clears. Under-bumping produces the opposite and unacceptable error: a repo
// that really did change still reading "ready".
func bumpAllRepoGensTx(tx *sql.Tx) error {
	// The WHERE is not a filter: SQLite cannot parse an upsert directly after
	// a SELECT (the parser cannot tell ON CONFLICT from a join constraint), and
	// a trailing WHERE clause is the documented disambiguator.
	_, err := tx.Exec(`INSERT INTO repo_graph_gen (repo_prefix, gen)
		SELECT repo_prefix, 1 FROM repo_index_state WHERE true
		ON CONFLICT(repo_prefix) DO UPDATE SET gen = repo_graph_gen.gen + 1`)
	if err != nil {
		return err
	}
	// A prefix can hold graph rows before it has a repo_index_state row (the
	// index stamps that at the END of a run). Those rows are still real graph
	// mutations, so advance every anchor the store already knows about too.
	_, err = tx.Exec(`UPDATE repo_graph_gen SET gen = gen + 1
		WHERE repo_prefix NOT IN (SELECT repo_prefix FROM repo_index_state)`)
	return err
}

// commitGraphMutation is the commit path for every graph-mutating store
// method that owns its transaction directly. It exists so the durable anchor
// bump cannot be forgotten: a new mutation family that commits its own
// transaction and skips this helper also skips the in-process invalidation
// token, which its own tests notice immediately.
//
// Two families -- reindexEdgesSetTransactionLocked and
// reindexUnresolvedEdgeTargetsTransactionLocked -- hand their transaction to
// an inner helper and only learn the outcome after it commits. Those call
// bumpRepoGensTx directly inside that helper, where the transaction still
// exists, and keep their outer finishAnalysisMutationLocked call for the
// in-memory token. Anchor durability is identical; only the seam differs.
//
// changed carries the caller's own "did any row actually move" verdict --
// a no-op batch must not advance any anchor, or every readiness verdict decays
// on idle writes. prefixes names the repos the mutation touched; pass nil with
// all=true for a genuinely store-wide mutation. writeMu must be held.
func (s *Store) commitGraphMutation(tx *sql.Tx, changed bool, prefixes []string, all bool) error {
	if changed {
		var err error
		if all {
			err = bumpAllRepoGensTx(tx)
		} else {
			err = bumpRepoGensTx(tx, prefixes)
		}
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.finishAnalysisMutationLocked(changed)
	return nil
}

// noteGraphMutation is commitGraphMutation's counterpart for the single-
// statement mutation paths that never open a transaction -- a lone edge
// update or delete executed straight on the writer connection.
//
// Here the bump is a second statement rather than part of the caller's, so a
// crash landing between the two loses it. That window is one statement wide
// and is accepted deliberately: wrapping these hot single-edge resolver paths
// in an explicit transaction would cost more on every write than the residual
// risk. The batched paths -- which is where whole repositories actually change
// -- go through commitGraphMutation and have no such window. writeMu is held
// by every caller.
func (s *Store) noteGraphMutation(ctx context.Context, changed bool, prefixes []string) error {
	if changed {
		for _, prefix := range dedupePrefixes(prefixes) {
			if _, err := s.execActiveWriteLocked(ctx,
				`INSERT INTO repo_graph_gen (repo_prefix, gen) VALUES (?, 1)
				ON CONFLICT(repo_prefix) DO UPDATE SET gen = repo_graph_gen.gen + 1`,
				prefix); err != nil {
				return err
			}
		}
	}
	s.finishAnalysisMutationLocked(changed)
	return nil
}

// addPrefix appends prefix unless it is empty or already present.
//
// The linear scan is deliberate. A batch may carry tens of thousands of nodes
// and edges, but the DISTINCT repos behind them are almost always one and
// never more than a handful, so the scan is over a 1-3 element slice while a
// map would allocate buckets sized for the input. Collecting through this
// helper also avoids materialising the full per-row prefix list first: on the
// primary write path that intermediate slice measured ~17 KB/op of garbage for
// a result of one string.
func addPrefix(out []string, prefix string) []string {
	if prefix == "" {
		return out
	}
	for _, existing := range out {
		if existing == prefix {
			return out
		}
	}
	return append(out, prefix)
}

// dedupePrefixes returns the distinct non-empty prefixes in sorted order.
// Sorting keeps the per-transaction write order deterministic, which stops two
// concurrent transactions touching the same repo set from deadlocking on the
// upsert in opposite orders.
func dedupePrefixes(prefixes []string) []string {
	if len(prefixes) == 0 {
		return nil
	}
	var out []string
	for _, p := range prefixes {
		out = addPrefix(out, p)
	}
	sort.Strings(out)
	return out
}

// toRepoExpr mirrors graph.RepoPrefixOfID for an edge's target, matching
// fromRepoColumnDDL's expression. edges has a generated from_repo column but
// no to_repo counterpart, so the target side is computed inline.
const toRepoExpr = `CASE WHEN instr(to_id, '/') > 1 THEN substr(to_id, 1, instr(to_id, '/') - 1) ELSE '' END`

// evictionTouchedPrefixesTx asks SQLite which repos a scope eviction actually
// changes. It MUST run before the DELETEs, because the only record of that
// ownership is the rows about to be removed.
//
// Node ownership alone is not enough. Evicting one repo's file also deletes
// every edge incident to its nodes, including cross-repo edges owned by a
// DIFFERENT repo -- whose "who uses this" answer just changed while nothing in
// its own file set moved. Missing that is precisely the fail-open case this
// anchor exists to prevent, so both edge directions are collected too.
//
// Three separate indexed queries rather than one OR: edges_by_from and
// edges_by_to each seek, whereas an OR over both columns degenerates into a
// scan -- the same reason evictByPredicateResult issues two deletes instead of
// one. The predicate is always a package constant, never caller SQL.
func evictionTouchedPrefixesTx(ctx context.Context, tx *sql.Tx, predicate string, arg any) ([]string, error) {
	scoped := `SELECT id FROM nodes WHERE ` + predicate
	var out []string
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT DISTINCT repo_prefix FROM nodes WHERE ` + predicate, []any{arg}},
		{`SELECT DISTINCT ` + toRepoExpr + ` FROM edges WHERE from_id IN (` + scoped + `)`, []any{arg}},
		{`SELECT DISTINCT from_repo FROM edges WHERE to_id IN (` + scoped + `)`, []any{arg}},
	} {
		rows, err := tx.QueryContext(ctx, q.sql, q.args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var prefix string
			if err := rows.Scan(&prefix); err != nil {
				rows.Close() //nolint:errcheck // returning the scan error
				return nil, err
			}
			out = addPrefix(out, prefix)
		}
		err = rows.Err()
		rows.Close() //nolint:errcheck // value already captured
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// repoPrefixesOfIDs derives the touched repos from graph node IDs. Every
// repo-owned ID carries its prefix ahead of the first '/', which is what makes
// per-site prefix discovery mechanical rather than a new bookkeeping burden.
// A bare global sentinel (dep::, external::, unresolved::) yields "" and is
// dropped by dedupePrefixes -- it belongs to no repository.
func repoPrefixesOfIDs(ids []string) []string {
	var out []string
	for _, id := range ids {
		out = addPrefix(out, graph.RepoPrefixOfID(id))
	}
	return out
}

// repoPrefixesOfEdges derives the touched repos from both endpoints. An edge
// is a mutation of the graph at each end, so a cross-repo edge advances both
// anchors: a caller asking "who uses this" against either repo sees an answer
// that changed.
func repoPrefixesOfEdges(edges []*graph.Edge) []string {
	var out []string
	for _, e := range edges {
		if e == nil {
			continue
		}
		out = addPrefix(out, graph.RepoPrefixOfID(e.From))
		out = addPrefix(out, graph.RepoPrefixOfID(e.To))
	}
	return out
}

// repoPrefixesOfStamps derives the touched repos from semantic node stamps.
func repoPrefixesOfStamps(stamps []graph.SemanticNodeStamp) []string {
	var out []string
	for _, stamp := range stamps {
		out = addPrefix(out, graph.RepoPrefixOfID(stamp.NodeID))
	}
	return out
}

// repoPrefixesOfReindex covers both ends of an edge reindex AND its previous
// identity: moving an edge off a node mutates the repo it left, not only the
// one it lands in.
func repoPrefixesOfReindex(batch []graph.EdgeReindex) []string {
	var out []string
	for _, r := range batch {
		if r.Edge != nil {
			out = addPrefix(out, graph.RepoPrefixOfID(r.Edge.From))
			out = addPrefix(out, graph.RepoPrefixOfID(r.Edge.To))
		}
		out = addPrefix(out, graph.RepoPrefixOfID(r.OldFrom))
		out = addPrefix(out, graph.RepoPrefixOfID(r.OldTo))
	}
	return out
}

// repoPrefixesOfProvenance derives the touched repos from provenance updates.
func repoPrefixesOfProvenance(batch []graph.EdgeProvenanceUpdate) []string {
	var out []string
	for _, u := range batch {
		if u.Edge == nil {
			continue
		}
		out = addPrefix(out, graph.RepoPrefixOfID(u.Edge.From))
		out = addPrefix(out, graph.RepoPrefixOfID(u.Edge.To))
	}
	return out
}

// repoPrefixesOfSlugs reads the repo each workspace slug names outright.
func repoPrefixesOfSlugs(slugs []graph.WorkspaceSlug) []string {
	var out []string
	for _, slug := range slugs {
		out = addPrefix(out, slug.RepoPrefix)
	}
	return out
}

// repoPrefixesOfNodes prefers the node's declared RepoPrefix and falls back to
// its ID. Synthetic nodes are written with an explicit prefix that their ID
// shape does not always encode.
func repoPrefixesOfNodes(nodes []*graph.Node) []string {
	var out []string
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.RepoPrefix != "" {
			out = addPrefix(out, n.RepoPrefix)
			continue
		}
		out = addPrefix(out, graph.RepoPrefixOfID(n.ID))
	}
	return out
}
