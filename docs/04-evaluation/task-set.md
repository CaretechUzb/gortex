# Seed task set

15 tasks (3 per category) the published methodology runs against
on each release. Each task carries a **canonical answer** the
judge uses as ground truth — written by a human expert before any
agent runs. Adding tasks: append to the appropriate section,
write the canonical answer first, only THEN run the harness.

The task corpus is the gortex repo itself (eats its own dog
food). Extending to other corpora means writing a new
`task-set-<repo>.md` with the same structure and pointing the
harness at it via `--task-set`.

---

## Category 1: Architectural explanation (3 tasks)

### 1.1 Indexer pipeline walkthrough

**Prompt**: "Walk me through how the gortex indexer processes a
single new source file. Name the packages it crosses and the
order of operations."

**Canonical answer (~150 words)**:

1. `indexer.Indexer.IndexCtx(root)` walks `root` via
   `internal/indexer/scan.go::scanFiles`, producing one
   `parseJob` per matching file.
2. Each job is dispatched to `parser.Registry` (the language
   plugin matching the file extension) via
   `internal/parser/treesitter.go`.
3. The parser extracts symbols / edges as
   `parser.ExtractionResult`; `Indexer.processExtraction` writes
   them into the `graph.Graph` and accumulates incoming-edge
   tracking for the next phase.
4. The backing store owns the symbol FTS corpus: shadow drains bulk-publish it,
   direct-store passes rebuild it, and incremental mutations upsert or delete
   affected rows. The vector pipeline prepares an immutable plan with
   `Indexer.prepareSearchIndexForPublication`, then publishes it through
   `Indexer.installVectorPlan` after the durable shadow drain succeeds.
5. Semantic enrichment (`internal/semantic`) runs LSP / SCIP
   providers in parallel; resolved edges get
   `Origin=lsp_resolved` for tier filtering.
6. Returns `IndexResult` with file / node counts + duration.

### 1.2 Community detection role

**Prompt**: "What does the `internal/analysis/communities.go`
package do, and how does it integrate with `smart_context`?"

**Canonical answer (~80 words)**:

Implements Leiden community detection on the graph. Output is a
`CommunityResult` mapping node ID → community ID + per-community
cohesion score. `smart_context` reads it via the Context.CommunityOf
hook in the rerank pipeline: candidates sharing the session's
home community get a locality boost. Recompute is triggered on
graph re-warm; the result is cached in `Server.analysis`.

### 1.3 Daemon dispatch path

**Prompt**: "How does a MCP request hit the daemon and get
routed back to a per-session response?"

**Canonical answer (~120 words)**:

A `gortex mcp` client opens a stdio JSON-RPC pipe; the daemon
dispatcher (`internal/daemon/dispatcher.go::MCPDispatcher`)
parses frames, looks up or creates a per-session `*mcp.Server`
via `Sessions.GetOrCreate`, and forwards. The Server holds a
shared `*graph.Graph` + per-session `tokenStats` +
`sessionState` (notes, frecency, etc.). Responses go back the
same pipe with the matching JSON-RPC id. Cross-session memory
(notes / memories / feedback) is workspace-scoped via the
session's resolved cwd → workspace ID.

---

## Category 2: Refactor safety (3 tasks)

### 2.1 Rename a public method

**Prompt**: "I'm renaming `Indexer.Index` to `Indexer.IndexRoot`.
List every caller that needs updating."

**Canonical answer** (verified via `gortex find_usages
gortex/internal/indexer/indexer.go::Indexer.Index`):

- `gortex/internal/indexer/multi.go::MultiIndexer.IndexRepo`
- `gortex/cmd/gortex/eval_recall.go::runEvalRecall`
- `gortex/bench/perf/runner.go::runRepo` (introduced in the L5
  bench commit)
- `gortex/bench/token-efficiency/runner.go::indexRepoForBench`
- All `*_test.go` files calling `idx.Index(...)` (count: see
  `find_usages` output)

### 2.2 Change a signature

**Prompt**: "I want to add a `context.Context` to `Engine.SearchSymbols`.
List every caller and what they'll need to change."

**Canonical answer** (verified via `gortex verify_change`): see
`find_usages` output; ~12 callers across `cmd/gortex/`,
`internal/mcp/`, `bench/perf/`, `bench/token-efficiency/`. Each
needs to pass through the request's context (most have one
available; some need to use `context.Background()` for now).

### 2.3 Remove a deprecated field

**Prompt**: "I want to remove the `Edge.LegacyConfidence` field.
What breaks?"

**Canonical answer**: the field doesn't exist; the canonical
answer is "no such field; nothing breaks". A passing agent
should say so explicitly, not hallucinate impact.

---

## Category 3: Bug localization (3 tasks)

### 3.1 Panic trace

**Prompt**: "We have a panic in `internal/savings/store.go` at
`flushLocked`. What conditions could cause it?"

**Canonical answer**: `flushLocked` runs under `s.mu`. Panic
paths: (a) flock acquisition fails (file system permission /
disk full → wrapped, not panicked); (b) atomic-rename fails on
some filesystems → returns error; (c) gob encode fails on
unexpected map shape → would panic in `encoder.Encode`. Most
likely candidate: corruption of the in-memory `s.file.PerRepo`
map by a goroutine that bypassed the mutex.

### 3.2 Wrong rank order

**Prompt**: "After my recent rerank-signal change, the top
result for `validateToken` is now a test file instead of the
real implementation. What signal probably regressed?"

**Canonical answer**: `path_penalty` (the test-file demotion).
If a path matching the test-file regex stopped getting the ×0.3
multiplier, test files would no longer be demoted. Check
`signals_path_penalty.go::classifyPathPenalty` and the regex
patterns in `pathRETest`.

### 3.3 Cross-repo missing edges

**Prompt**: "After indexing a multi-repo workspace, calls from
`web` to `cloud_web` aren't showing up in `get_callers`. What
gives?"

**Canonical answer**: cross-repo resolution depends on
`internal/resolver/cross_repo.go::CrossRepoResolver`; it only
runs when `MultiIndexer` has indexed all repos in the same
workspace. Most likely cause: one of the repos was indexed in
isolation (not via `gortex track`) so the resolver never saw
the cross-repo `import` edges.

---

## Category 4: Impact analysis (3 tasks)

### 4.1 Touch a hot path

**Prompt**: "I'm about to change the signature of
`tokens.Count`. List the test files that need re-running."

**Canonical answer**: use `gortex get_test_targets
internal/tokens/tokens.go::Count`. Expected hits include
`internal/tokens/tokens_test.go`, every test in
`internal/mcp/` that calls `tokenStatsFor`, the savings tests,
the bench harnesses' test files.

### 4.2 Blast radius

**Prompt**: "If I introduce a bug in `Graph.AllNodes`, what's
the worst-case downstream effect?"

**Canonical answer**: AllNodes is called by basically every
analyzer; impact is "the whole codebase". A canonical answer
quantifies it via `gortex explain_change_impact` (depth=3) and
notes which communities / processes are at risk.

### 4.3 Cycle detection

**Prompt**: "Would adding an `imports` edge from
`internal/search/rerank` to `internal/search` create a cycle?"

**Canonical answer**: yes if `internal/search` already imports
`internal/search/rerank` (parent-child importing child). Verify
via `gortex analyze kind=would_create_cycle from_id=...
to_id=...`. Currently `internal/search/hybrid.go` imports
`internal/search/rerank` for the auto-α blend, so adding the
reverse would form a cycle.

---

## Category 5: Contract extraction (3 tasks)

### 5.1 Public API of a package

**Prompt**: "List every exported function / method / type in
`internal/savings/`, with one-line summaries."

**Canonical answer**: use `gortex contracts list
internal/savings`. Expected ~12 symbols:
`Pricing`, `CostAvoided`, `CostAvoidedAll`, `Store`, `Open`,
`DefaultPath`, `EventsPathFor`, `Store.AddObservation`,
`Store.Snapshot`, `Store.Flush`, `Store.Reset`, `Bucket`,
`Event`, `LoadEvents`, `BarString`, `SavingsPercent`,
`AggregateByTool`, `FilterDay`, `FilterSince`,
`BuildDashboard`. Plus the canonical pricing model constants.

### 5.2 Tool surface of a package

**Prompt**: "What MCP tools does `internal/mcp/tools_savings.go`
register, and what are their parameter contracts?"

**Canonical answer**: pull from
`internal/mcp/tools_savings.go::registerSavingsTools` (or
equivalent). Tool list + per-tool param schema. A passing agent
should produce the exact param names + types.

### 5.3 Configuration surface

**Prompt**: "What `.gortex.yaml` keys does the indexer respect?
Group by required vs optional."

**Canonical answer**: cross-reference `internal/config/config.go`
+ each parser's `RegisterX` call. Required: none (all keys have
defaults). Optional: `index.exclude`, `index.max_file_size`,
`semantic.enable_*`, etc. A passing agent should produce the
list with default values per key.

---

## Curation rules

1. **Canonical answers come first.** Writing them after seeing
   agent outputs is methodology fraud — the temptation to
   "match" the agent's wording corrupts the ground truth.
2. **One topic per task.** A task that asks two questions splits
   the (a)/(b)/(c) signal — judge gives partial credit, which
   makes the headline noisy.
3. **Verify with the tools.** Every task's canonical answer
   should be reproducible by running gortex tools manually; if
   you can't reproduce it, the answer is wrong (or the tool
   has a bug worth filing).
4. **Bias toward realistic prompts.** The seed set is drawn
   from actual user sessions (anonymized). Synthetic prompts
   ("explain this complex graph algorithm") aren't what real
   users ask.
