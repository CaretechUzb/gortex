# TODOS

## Scoping

### Fail closed when the cwd is an untracked worktree under a tracked root

**What:** Make a Gortex session refuse, or loudly warn, when its working
directory sits inside a git worktree that is not itself tracked but whose
parent directory is a tracked repository — instead of silently binding to the
enclosing repository.

**Why:** It is a silent wrong-answer path, which is the worst failure shape the
graph can have. Measured 2026-08-27 with a real cwd-bound MCP session:

| cwd | `search symbols "res.partner"` |
|---|---|
| `docker-env/src/local` (tracked) | `local/his_tg/models/res_partner.py::ResPartner` — correct |
| `docker-env/src/local.worktrees` (untracked, **under** a tracked root) | binds to prefix `docker-env`; returns `docker-env/tasks.py::odoomap_dump` |
| `~/conductor/workspaces/local/lyon` (untracked, outside every root) | hard `repo_not_tracked`, session reports INACTIVE |

Nothing errors in the middle row. Locate-intent tools default to "current repo",
so they answer confidently out of a 63-file graph while the user believes they
are querying a 120k-node one.

**Reproduced 2026-08-30 on this repository, in three commands** — the original
table's workspace no longer exists (`docker-env` is not tracked today), and this
shape needs no Odoo checkout at all:

```bash
git worktree add --no-checkout --detach .probe-wt HEAD   # untracked worktree
cd .probe-wt && gortex call get_active_project           # {"bound": true, "project": "gortex"}
gortex call search_symbols --arg query=readyVerdict      # answers, from the PARENT checkout
```

The cwd holds **zero** `.go` files, and `search_symbols` still returns
`gortex/cmd/gortex/repos_ready.go::readyVerdict` with an `absolute_file_path`
pointing outside the cwd — so an agent that follows the path silently reads the
parent's file instead of failing. `bound: true`, no warning. The control is the
asymmetry that makes this a bug rather than a policy: an *empty directory*
outside every root is refused outright (`the gortex daemon does not track …`),
while a genuine git worktree at a different commit is accepted as its parent.

Note the readiness note (`internal/mcp/readiness_note.go`) does **not** cover
this. It qualifies answers from a repo whose derived passes are behind; here the
repo is fully derived and the answer is complete — it is simply about a different
checkout than the one the caller is standing in.

**Context:** `MultiIndexer.ScopeForCWD` (`internal/indexer/workspace_resolve.go:97-138`)
selects the longest tracked `RootPath` containing the cwd, which is what makes
the nested case bind to the parent. The safe behaviour already exists for the
non-nested case: `mcpDispatcher.cwdReachable` (`cmd/gortex/daemon_mcp.go:324-366`)
permits only bootstrap calls and `rewriteUntrackedResponse` restates that in the
`initialize` instructions. The gap is that the guard never fires when a tracked
ancestor exists. `ResolveWorktree` (`internal/indexer/worktree.go`) already
distinguishes a git worktree from an ordinary subdirectory — it is what
`checkoutGroups` keys on — so the probe need not fire on every subdirectory.

Worth deciding explicitly: refuse (consistent with the outside-every-root case)
versus warn and proceed. Refusing is the consistent choice, but it changes
behaviour for anyone who currently relies on the parent binding.

**Effort:** M
**Priority:** P2
**Depends on:** None

## Worktrees

### Detect tracked worktrees whose work is finished

**What:** Surface tracked worktrees whose branch is merged, whose MR is closed,
or which have been untouched for N days, so they get untracked rather than
accumulating.

**Why:** "Track a worktree per unit of work, untrack when done" makes the
untrack step the one a human forgets. Each stale entry costs ~120-140k nodes /
~95-110 MiB and adds a repository to every warm-restart census. Gortex already
handles the adjacent case well — a deleted checkout shows `MISSING` in three
views with the exact `gortex untrack` command to run — so the pattern and the UI
slot exist; a merged-but-still-present worktree is simply not detected.

**Context:** A live example sat on disk while this was written: the worktree at
`~/conductor/workspaces/local/lyon`, last touched 2026-07-15, fully merged into
`16.0` and 1,053 commits behind. `WorktreeRootGone` /
`MultiIndexer.GCVanishedWorktrees` (`internal/indexer/worktree.go:181-190`,
`multi.go`) handle the vanished case and are the natural place to extend —
note they treat only `os.ErrNotExist` as gone, so a flaky filesystem can never
trigger a destructive eviction, and any new predicate should keep that
property. `gortex repos` (`cmd/gortex/repos_cmd.go`) already reports
`head_commit`, `branch`, `indexed_commit`, `last_indexed` and `stale` per entry,
so most of the data a merged-branch check needs is already collected.

**Effort:** M
**Priority:** P3
**Depends on:** Per-worktree tracking being the adopted workflow — without it
this barely matters.

### Persist per-path dirty provenance so a dirty checkout can still be copied from

**What:** Record WHICH paths were dirty when a repository was indexed, not just
that some were, so `worktreeCopySource` can reconcile them instead of refusing
the whole candidate.

**Why:** `copySourceCommit` (`internal/indexer/track_worktree_copy.go`) now
declines any source with `RepoIndexState.Dirty`, because the field is a bool and
no diff can reconstruct the file set. That is correct but blunt: three of the six
repos in the `docker-env` workspace are dirty at any moment, so a large share of
worktree tracks lose the copy path and pay a full cold index (~667 s) instead of
copy plus reconcile (~200 s).

**Context:** The unsound alternative is worth writing down because it looks
obviously right: unioning the source's CURRENT `git status` into `changed` does
not work, since a source dirty at index time and committed or reverted since
reports clean while its graph still holds the uncommitted content. Measured
consequence of copying a dirty source before the refusal landed: the
destination's indexed `content_hash` matched its dirty source at 15,642 bytes
while its own checkout held the committed file at 15,596. The fix is a new column
or sidecar alongside `RepoIndexState` (`internal/graph/index_state.go`) written by
`persistRepoIndexState` (`internal/indexer/index_state.go`).

**Effort:** L
**Priority:** P3
**Depends on:** A schema version bump — and schema version is kept in lockstep
with main here, so this cannot be renumbered independently.

### Check the copy source's extractor versions before trusting its subgraph

**What:** Compare a copy candidate's `RepoIndexState.ExtractorVersions` against
the daemon's current ones, and decline or reindex when they differ.

**Why:** The copy path reads `IndexedSHA` and `Dirty` from `RepoIndexState` but
never `ExtractorVersions`. Copying from a sibling indexed by an OLDER extractor
installs that extractor's output — and because `restatWorktreeMtimes` writes the
destination's real mtimes, the reconcile finds everything current and never
re-parses. The graph is then silently a version behind, with no signal anywhere.

**Context:** Surfaced by a cross-model review, not by a measurement — nobody has
observed it firing, and it only bites when an extractor version bumps between
indexing the source and copying from it. The machinery already exists:
`extractorVersionStaleLangSet` (`internal/indexer/extractor_version.go:224`)
performs exactly this comparison on the normal index path and reads
`GetRepoIndexState` to do it. This is the third freshness field on that struct
and the only one the copy path still ignores.

**Effort:** S
**Priority:** P3
**Depends on:** Nothing.

### Re-establish fsnotify watches created after `GitWatcher.Start`

**What:** Recompute, or incrementally extend, the watcher's fsnotify watch set
after `Start` has run — so a `packed-refs` file created later by `git gc`, and a
`refs/heads/<prefix>/` directory created by the first branch under a new prefix,
do not stay unwatched until the next daemon restart.

**Why:** Both are the residual of the linked-worktree freshness fix, and both are
the same failure shape as the bug that fix closed: a subscription that silently
is not there. `logs/HEAD` covers both cases unless a repository has
`core.logAllRefUpdates=false`, so this is narrow — but the combination is exactly
what `TestGitWatcher_SlashBranchWithReflogDisabledRestamps` exists to pin, and
that test only covers prefixes that exist at `Start` time.

**Context:** This closes out the investigation that used to sit here, *"Find out
why a tracked worktree can sit stale for a day with a GitWatcher running"*. The
answer was not `seedSHA`: no ref event ever arrived. `GitWatcher.Start` watched
`HEAD`, `packed-refs` and `refs/heads` relative to the **worktree** gitdir, where
the last two do not exist and `HEAD` is a symref a commit never rewrites — so a
linked worktree's watch set was effectively empty and `indexed_sha` froze at
track time while the file watcher kept the graph itself current. `Start` now
watches `HEAD` + `logs/HEAD` from the worktree gitdir and `packed-refs` +
every directory under `refs/heads` from the common dir (`gitCommonDir`, which
reuses `ResolveWorktree`), and warns when no ref-side subscription is installed.
`Start` still runs exactly once per repository at warmup; making the watch set
mutable afterwards adds state to a component that is currently fixed after
`Start`, which is the cost to weigh.

**Effort:** M
**Priority:** P3
**Depends on:** Nothing.

### Decide what `watch.enabled` should gate, and whether it should default on

**What:** Decide whether `Watch.Enabled` should default to `true`, or whether the
poller's HEAD check should be split out from the flag that currently gates it.

**Why:** `watch.enabled: false` does **not** disable watching.
`MultiWatcher.Start` starts every repository's fsnotify watcher and its
`GitWatcher` unconditionally; the flag gates only the slow-mount degradation
probe and the poller (`internal/indexer/watcher.go`, `config.Default()`). The
consequence is not cosmetic: with the poller running, the linked-worktree
freshness bug above would have been at most ten minutes of staleness, because
`observeGitHead` → `finalizeGitHead` (`internal/indexer/poller.go`) restamps
`indexed_sha` on its own. With the flag off — the default — the `GitWatcher` is
the *only* freshness path, so a watch-topology surprise is permanent rather than
transient.

**Context:** Measured 2026-09-01: six days of `~/.gortex/cache/daemon.log`
contained zero poller lines while carrying live content-watcher lines for the
same repositories, and no repository on that machine had a poller — the global
config sets no `watch:` block, `gortex/.gortex.yaml` sets `enabled: false`
explicitly, and `config.Default()` is `false`. The cost of defaulting on is one
`git rev-parse` plus a bounded receipt sweep per repository per interval, with
the interval scaling from 15 s to 10 min by node count (`pollInterval`). The
cheaper half of this is renaming or splitting the flag so its name matches what
it gates; the default is the part that affects every user.

**Effort:** S
**Priority:** P2
**Depends on:** Nothing.

## Readiness

### Refuse, not just warn, on queries against a not-ready repo

**What:** An opt-in strict mode that makes a query against a repo whose derived
passes have not finished fail rather than answer with a caveat.

**Why:** The warning shipped — `internal/mcp/readiness_note.go` attaches a
readiness note to the answer itself, so an agent no longer has to know to run
`gortex repos` first. Warning was deliberately chosen over refusing because it
is strictly additive and cannot break a working session. Refusing is a real
behaviour change for everyone currently querying a partial repo, so it wants to
be opt-in and decided on its own.

**Context:** The verdict now lives in `internal/readiness` (moved out of
`cmd/gortex/repos_ready.go`, which keeps thin aliases) so both surfaces share
one ladder. `readiness.BlocksQueries` is the trigger, deliberately narrow —
`never derived` and `partial` only. The MCP note reads readiness through
`(*store_sqlite.Store).ReadinessStates`, cached per session behind a short TTL.

Two known coarsenesses a strict mode would have to tighten first: the note
leaves `Missing` and `Stale` unset (both need a filesystem or git probe the
request path does not pay for), and it uses the workspace-wide
`WorkspaceRederivePending()` as a blanket suppressor rather than the CLI's
per-repo deriving / pending markers, which live on the daemon runtime record.
Both err toward silence, which is right for a warning and wrong for a refusal.

**Effort:** S
**Priority:** P3
**Depends on:** the readiness note, which shipped.

### Surface readiness in `gortex daemon status`

**What:** Add derive / enrichment fields to `TrackedRepoStatus`
(`internal/daemon/proto.go`, today `Files, Nodes, Edges, LastIndex, Memory,
Missing, Unloaded`) and render them in `renderDaemonRepos`
(`cmd/gortex/daemon.go`, labels from `repoStateLabel`).

**Why:** Two commands disagreeing about repo health is worse than one command
being incomplete. `daemon status` is where a user looks first.

**Context:** `StatusResponse` already carries a workspace-wide
`DerivingWorkspace bool` (`proto.go`, from `WorkspaceRederivePending()`); the
per-repo dimension is what is missing. Caveat: this path goes over the control
socket and fails when the daemon is busy — which is exactly when readiness
matters, and is why `gortex repos` reads the store directly and was the right
first home. This complements that rather than replacing it.

**Effort:** M
**Priority:** P3
**Depends on:** the `READY` column, for the verdict and its vocabulary.

### Make the derive config hash describe the pass set that actually ran

**What:** Move `DeriveConfigHash` from one daemon-wide value to a per-workspace
(or per-repo) one, and migrate the four consumers that assume a single hash.

**Why:** The global pass now narrows framework execution to the covered
workspaces (`allowedFrameworksForScope`), but the fingerprint stamped against
each repo is still the daemon-wide union. So a repo's recorded hash names
patterns that never executed for it, and editing an allow-list in an unrelated
workspace marks every repo `partial` and re-derives it — every time.

**Context:** Deliberately shipped broad, and the rationale is written at
`DeriveConfigHash` (`internal/indexer/derive_state.go`) so nobody narrows it
piecemeal. Broad is the safe direction: it can only over-report `partial`, never
under-report it. The cost of fixing it is that this is a persistence and
runtime-state migration, not a signature change — `runDaemonStart`
(`cmd/gortex/daemon.go:509`) stores one hash in runtime state, `stampDeriveState`
(`derive_state.go:181`) stamps one for ALL covered repos,
`ScheduleDeriveForConfigDrift` (`:235`) compares every repo against one "current"
hash, and `applyReadiness` (`cmd/gortex/repos_cmd.go:237`) reads one per CLI row.
Changing the digest input also forces a one-time re-derive of every tracked repo
(~36 min measured for the six-repo `docker-env` workspace). Migrate all four
together or leave it broad; narrowing this function alone reports `ready` over a
derive that never happened.

**Effort:** L
**Priority:** P3
**Depends on:** Nothing, but it only matters now that the gate is workspace-scoped.

### Reconcile the enrichment declared-provider set with the started set

**What:** Find out which of the two provider sets is wrong when semantic
enrichment declares one provider and then starts four.

**Why:** Same shape as the framework allow-list leak — a gate computes a narrowed
set and something downstream ignores it. Enrichment was 359 s of a 29-minute
worktree track, so if the declared set is the correct one and three providers run
needlessly, that is a real cut on every enrichment, not just copies.

**Context:** Measured on the `local@test` track, 2026-08-31:

    semantic: applicable enrichment providers declared  {"providers":["go-types"]}
    semantic enrichment starting  {"provider":"go-types"}
    semantic enrichment starting  {"provider":"python-types"}
    semantic enrichment starting  {"provider":"typescript-types"}
    semantic enrichment starting  {"provider":"go-ast-types"}

One declared, four started — and both Go providers reported `confirmed:0 added:0`
on an Odoo repo with no Go, so at least one of the two sets is wrong. It may be
the declared half rather than the started half: `python-types` did real work
(`confirmed:1082`), so declaring only `go-types` for that repo looks incorrect
too. `semantic/manager.go:738` emits the declared line, `:1399` emits each start;
the gap between them holds the answer.

**Effort:** S
**Priority:** P3
**Depends on:** Nothing.

### An identical copy schedules no derive, so the next file save strands it forever

**What:** `trackWorktreeByCopy` guards both follow-up stages on `len(changed) > 0`:
a copy whose source was indexed at this checkout's HEAD gets `RestampCopiedReadiness`
and nothing else. That is right at the instant it commits — the carried stamps
describe the carried graph exactly. It stops being right on the first write to the
new checkout: `content_gen` advances past the stamp and no derive is queued to
re-stamp it, so the repo reads `partial: files changed since the derived passes
last ran` until an unrelated repo's workspace rederive happens to cover it, or the
daemon restarts.

**Why:** observed live on 2026-08-31. `local@fix_tier_validation`, an identical
copy of `local@test` (`reconciled_files=0`), reached `ready` and then flipped to
`partial`. This is not the ordering bug fixed in `trackWorktreeByCopy` — that one
is closed, and `TestARestampWrittenBeforeTheRegisteringWriteIsDestroyedByIt`
(`internal/graph/store_sqlite/repo_subgraph_copy_readiness_test.go`) now pins the
premise it rests on. It is the same interaction recorded for ordinary saves,
reaching the copy path through a door the copy path leaves open: the diverged copy
arms a derive that would re-stamp, the identical copy arms nothing.

**Context:** the fix is not "always schedule a derive" — that reintroduces the full
post-track derive the copy exists to avoid, for a repo that genuinely owes no
derivation yet. The candidates are an armed-but-idle derive that only fires once
`content_gen` moves, or letting the existing watcher-driven incremental derive
stamp `derive_state` (it already recomputes the frontier; it just does not claim
completion). Check `readiness.Verdict`'s ladder order first: `Deriving` and
`DerivePending` both outrank the `DerivedContentGen` clause, so arming is
sufficient to stop the false alarm even before the pass runs.

**Effort:** M
**Priority:** P2
**Depends on:** None.

### Cover `trackWorktreeByCopy`'s call order with an integration test

**What:** the restamp-after-reconcile ordering is guarded at the store layer
(`repo_subgraph_copy_readiness_test.go`: the stranding, its repair, the
never-ran-provider and legacy-row refusals, and now the restamp-before failure
mode) but nothing asserts that `trackWorktreeByCopy` actually calls
`RestampCopiedReadiness` after `ReconcileRepoCtx`, or skips it when the reconcile
errors. Moving the call back above the reconcile leaves every existing test green.

**Why:** that ordering is the W1.3 fix. It is the one change in the worktree-copy
work with no test that can fail if it is reverted.

**Context:** not reachable from the unit fixtures in `track_worktree_copy_test.go`.
`copyGateGraph` wraps `*graph.Graph`, which does not implement `CopyRepoSubgraph`,
so `trackWorktreeByCopy` returns `supported=false` before it reaches either call.
The harness needs a real `store_sqlite` store, a real git worktree on disk (the
source ranking shells out to git), and `ReconcileRepoCtx`. `internal/indexer` has
no sqlite-backed test today, so this establishes that pattern; check for an import
cycle first — `store_sqlite` is a leaf and `internal/readiness` already imports it.

**Effort:** M
**Priority:** P2
**Depends on:** None.


## Analysis

### Report the true cross-repo boundary, not just promoted relations

**What:** Give `analyze kind=cross_repo` a second section counting every edge
whose endpoints sit in different repos, beside the resolver-promoted rows it
reports today. `get_architecture` should read the same rollup.

**Why:** The number it reports is not the boundary, and on a framework-heavy
workspace it is off by an order of magnitude. Measured 2026-08-30 against the
5-repo `docker-env` (Odoo) workspace:

| pair | distinct relations | reported | sees |
|---|---|---|---|
| `local` -> `odoo` | 108,614 | 13,230 | 12.2% |
| `odoo` -> `local` | 108,697 | 1,038 | 1.0% |

`handleAnalyzeCrossRepo` (`internal/mcp/tools_analyze_edges.go:1569-1699`)
enumerates three hardcoded kinds — `EdgeCrossRepoCalls`, `EdgeCrossRepoImplements`,
`EdgeCrossRepoExtends` — and applies no boundary predicate at all; `fromRepo` /
`toRepo` are resolved afterwards and used only as grouping keys. The Odoo
synthesizer emits `references` / `imports` / `composes` / `overrides` / `tests`,
none of which has a `cross_repo_*` mirror, so ~95k real crossings never reach the
rollup. On an Odoo codebase `references via=odoo-model` *is* the dominant coupling
relation — `fields.Many2one('hr.department')` binding a local field to the class
behind the model — so what survives the filter is the plain-Python slice that
would exist even with the framework pass switched off.

The clean proof that the kind list and not the `cross_repo` column is the gate:
2,401 `imports` and 119 `instantiates` on `local` -> `odoo` already carry
`cross_repo = 1` and still do not appear.

The misleading-answer path is already closed — `commandCrossRepoUsage`
(`internal/agents/claudecode/content.go`) demotes the call to step 5 and states
it is not a boundary census — so this entry is about making the real number
*available*, not about stopping a wrong one being believed.

**Context:** Four measurements constrain the implementation:

- The authoritative query (`edges` joined to `nodes` twice, filtering
  `nf.repo_prefix <> nt.repo_prefix`) takes **45s** on the 8 GB store and returns
  only 143 rows. That does not fit the ~59s handler deadline — `get_architecture`
  already times out on this workspace — so it belongs in the generation-keyed
  `PutAnalysisBlob` / `LoadAnalysisBlob` cache
  (`internal/graph/store_sqlite/analysis_generation_{read,write}.go`), which
  `invalidateAnalysisGenerationTx` already invalidates on mutation. Return
  `boundary: {status: "not_computed"}` when cold rather than blocking a caller.
- Deriving the repo prefix from the node id instead of joining is not a
  shortcut: it is both slower (33s) *and* wrong, missing 572 edges whose node
  carries a repo prefix that its id does not.
- The census must skip kinds for which `graph.BaseKindForCrossRepo` returns ok.
  The resolver stores a mirror beside every edge it promotes, so a raw row count
  double-counts exactly the promoted set (121,844 rows -> 108,614 relations).
- `edges.tier` is empty in the column — tier is derived at read time via
  `graph.ResolvedBy(origin)`. Group by `origin` and map it in Go.

**Effort:** L
**Priority:** P3
**Depends on:** None. P3 rather than P2 only because the agent-facing warning
shipped first; without that warning this is a silent wrong answer.
