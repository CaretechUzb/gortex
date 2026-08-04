package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// The contract tier is committed all-at-once by the tail of a full index,
// while re-index admission is per-file mtime. Those policies disagree: a run
// whose tail never completes leaves the tier EMPTY, and every later warm
// restart sees unchanged mtimes, re-extracts nothing, and keeps it empty for
// the life of the index. The graph is otherwise complete, the daemon is past
// warmup, so `analyze kind=routes` answers a clean `rows=0` that reads exactly
// like "this repository has no routes".
//
// graph.ContractState is the durable record of a completed whole-repo contract
// pass. The helpers here read it so a zero-row contract / route answer can say
// which zero it is instead of silently failing open.

// contractTierCaveatKey is the stable token callers can match on. It names the
// condition, not the presentation, so a client can branch on it without
// parsing prose.
const contractTierCaveatKey = "contract_tier_unbuilt"

const contractTierDetail = "no contract-tier completion marker is recorded for these repositories, so this empty result is not evidence that they declare no routes or contracts: a full index whose contract tail was lost commits nothing to the tier, and per-file mtime admission then re-extracts nothing on every later restart. (An index written before this marker existed also has no row.)"

const contractTierRemedy = "rebuild the tier with a whole-repo re-parse — untrack_repository then track_repository, or restart the daemon with GORTEX_WARMUP_FULL_RETRACK=1. reindex_repository will NOT rebuild it: it admits files by mtime, and an unbuilt tier leaves every mtime unchanged."

// contractTierScopeRepos returns the repo prefixes an empty contract / route
// answer covered, narrowed the same way the answer itself was: an explicit
// allow-set first, then the session workspace, then every tracked repo. The
// single-repo daemon reports the one empty prefix it indexes under.
func (s *Server) contractTierScopeRepos(ctx context.Context, allow map[string]bool) []string {
	if len(allow) > 0 {
		out := make([]string, 0, len(allow))
		for p, ok := range allow {
			if ok {
				out = append(out, p)
			}
		}
		sort.Strings(out)
		return out
	}
	if s.multiIndexer == nil {
		return []string{""}
	}
	if ws, _, bound := s.sessionScope(ctx); bound {
		inWorkspace := s.multiIndexer.ReposInWorkspace(ws)
		out := make([]string, 0, len(inWorkspace))
		for p := range inWorkspace {
			out = append(out, p)
		}
		sort.Strings(out)
		return out
	}
	out := s.multiIndexer.RepoPrefixes()
	sort.Strings(out)
	return out
}

// contractTierUnbuiltRepos returns the in-scope repositories whose contract
// tier carries no completion marker, sorted.
//
// Empty — no caveat — when the backend does not persist contract state. The
// in-memory graph is the only such backend and it rebuilds its contract tier
// from scratch on every start, so the lost-tail hole this reports cannot
// survive there and claiming otherwise would be a false alarm.
func (s *Server) contractTierUnbuiltRepos(ctx context.Context, allow map[string]bool) []string {
	store, ok := s.graph.(graph.ContractStateStore)
	if !ok {
		return nil
	}
	var unbuilt []string
	for _, prefix := range s.contractTierScopeRepos(ctx, allow) {
		_, found, err := store.GetContractState(prefix)
		if err != nil || found {
			continue
		}
		unbuilt = append(unbuilt, prefix)
	}
	return unbuilt
}

// contractTierCaveat is the structured caveat attached to a zero-row contract
// or route answer whose scope holds a repository with no completion marker.
func contractTierCaveat(repos []string) map[string]any {
	return map[string]any{
		"caveat": contractTierCaveatKey,
		"repos":  displayRepoPrefixes(repos),
		"detail": contractTierDetail,
		"remedy": contractTierRemedy,
	}
}

// contractTierCaveatLine renders the caveat for the compact text encodings.
func contractTierCaveatLine(repos []string) string {
	return contractTierCaveatKey + ": " + strings.Join(displayRepoPrefixes(repos), ", ") +
		" — " + contractTierDetail + " " + contractTierRemedy + "\n"
}

// stampContractTierCaveat records the caveat on a response whose body encoding
// carries no room for it (GCX). Mirrors stampScopeNote: the token lands in the
// result meta, where a client that dispatches on it can still see it.
func stampContractTierCaveat(res *mcp.CallToolResult, repos []string) {
	if res == nil || len(repos) == 0 {
		return
	}
	note := contractTierCaveatLine(repos)
	if res.Meta == nil {
		res.Meta = mcp.NewMetaFromMap(map[string]any{contractTierCaveatKey: note})
		return
	}
	if res.Meta.AdditionalFields == nil {
		res.Meta.AdditionalFields = map[string]any{}
	}
	res.Meta.AdditionalFields[contractTierCaveatKey] = note
}

// displayRepoPrefixes renders the empty prefix (the single-repo daemon's sole
// repo, and the shared-externals bucket in multi-repo mode) as a readable
// placeholder instead of an empty string in the middle of a list.
func displayRepoPrefixes(repos []string) []string {
	out := make([]string, 0, len(repos))
	for _, p := range repos {
		if p == "" {
			p = "(default)"
		}
		out = append(out, p)
	}
	return out
}
