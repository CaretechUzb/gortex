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

## Forge

### Paginate both forge backends, and batch GitLab's aggregate fan-out

**What:** Neither `ghClient.ListPRs` nor `glClient.ListPRs` paginates: both set
`per_page`/`PerPage` to 100, issue one request, and ignore GitHub's
`Link: rel="next"` and GitLab's `X-Next-Page`. A repository with more than 100
open PRs/MRs silently triages a truncated queue. Separately, GitLab's
`fillAggregates` costs up to two extra round-trips per MR (single-MR GET for
`head_pipeline`, `/approvals` for the decision) under an errgroup of 8.

**Why:** The truncation is silent, which is the failure shape this repo treats
as worst: `triage_prs` and `conflicts_prs` rank a partial population and report
it with full confidence. It was left out of the GitLab-backend change on purpose
— GitHub has the same limit today, so the two are at parity, and fixing one side
alone would make two implementations of one interface disagree about what "the
first N" means. Fix them together or not at all.

For the fan-out, GitLab GraphQL returns both aggregates for a whole page in one
query:

```graphql
project(fullPath: $p) {
  mergeRequests(state: opened, first: $n) {
    nodes { iid headPipeline { status } approvalState { approved } }
  }
}
```

That replaces up to 2N REST calls with 1. The REST fan-out was kept because it
mirrors the existing GitHub shape; the GraphQL path is a redesign, not a fix.

**Also deferred with it:** `glabHosts` and `declaredHosts` are process-lifetime
memos, so a `glab auth login --hostname X` performed AFTER the daemon started
never takes effect — the host stays untrusted for a shared token and keeps
reporting `errNoGitLabToken`, which reads as a broken credential rather than a
stale cache. `MissingTokenHint` actively tells the user to run that command.
Either give both memos a TTL, invalidate them on config reload, or say so in the
hint.


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

### Elect the family's primary base from mainline, and seed the submodule's main checkout

**What:** `CheckoutLifecycle.bindDedicatedGraph` elects `IsPrimaryBase: len(graphs) == 0` —
the FIRST dedicated graph bound in a family, permanently. On docker-env that is
`local@aurora-redesign`, a feature branch 1,382 files off mainline, so every automatic
checkout of `local` composes its commit layer over it: 1,667 masked paths, a closure that
hits the 200-file cap every time, 477 files re-parsed and 14–20 min of resolve + derived
tail + FTS rebuild per build (7 builds on 2026-09-05, ~83 min, 5 of 7 discarded).
`local`'s own mainline checkout cannot compete because the seeder rejects it at every
start: `daemon: seeding the checkout catalog was incomplete: seed …/src/local: git does not
list …/src/local as a worktree of …/.git/modules/src/local` (a submodule's main worktree,
listed by `git worktree list` from the module gitdir under its gitdir-file root).

**What shipped instead (2026-09-06, decision D6-A):** reuse across restarts
(`Catalog.FindViewGenerationByIdentity`; the boot sweep now runs after coordinators exist
and keeps the newest `defaultRetainedCommitLayers` commit layers per checkout out of the
retirement cohort), containment (no coordinator for a checkout that owns a dedicated
graph; a new automatic checkout's first build is demand-only — `FirstBuildDelay` 0 =
build when a read routes to it, >0 = also after that long, <0 = never defer; env
`GORTEX_CHECKOUT_FIRST_BUILD_DELAY`; the untrack demotion registers the route and does not
build), and honesty (`view_generations.completeness = closure_truncated` persisted, counted
in `ViewsHealth.Incomplete`). Reuse is sound only because everything that decides
truncation rides `config_hash`; a time- or budget-based truncation must also require an
empty completeness.

**Still open:** the election itself and the seeder. Elect only the family's base checkout
(`@main`), leave a family primary-less until `set-primary` otherwise, fall back to the base
checkout's own corpus when no dedicated primary exists, and make the seeder accept a
submodule main worktree (compare after `EvalSymlinks`, accept the gitdir-file root). This
re-bases every existing automatic checkout's layers on this workspace, so it needs its own
live verification. Raising `defaultAffectedByMax` (200) would make each build slower, not
cheaper — the cap is a symptom of the wrong base.

**Effort:** M **Priority:** P2 **Depends on:** nothing.

### Corpus retirement holds the batch-mutation gate for the whole eviction

**What:** untracking (demoting) a 125k-node / 1M-edge copied worktree took 28 min on
2026-09-06 02:46 and 55 min on 2026-09-06 06:24. Measured on the second run: everything up
to the file purge and the config removal was done within 20 min; the remaining ~35 min was
ONE call, `untrackRepoChecked` → `ReconcileContractEdgesForFrontier`
(`internal/indexer/repository_untrack.go`, `incremental_contract_reconcile.go`), which
holds `g.ResolveMutex()` for its entire body — `InlineWrappersForFiles`,
`persistScopedInlinedContracts`, `BindProviderSymbols`, `contracts.Match` over the MERGED
registry of all eight tracked repos (the "ForFrontier" name notwithstanding), the
incident-edge scan and `ReplaceDerivedContracts` — plus `mi.reconcileMu`, under the
caller's `batchMutationGate.RLock()` and the reachability topology writer, and logs nothing
but a failure warning. It competed with two back-to-back post-warmup analysis suites
(30 min, then 47.5 min); in the 28-min control the suite's publications were abandoned
after concurrent mutations and released early. While it ran, control requests exceeded
their 30 s budgets, `edit` tool calls were abandoned at 59 s, and one watcher patch took
8.7 min. `demote` also used to call `evictRepoChecked` a second time after the saga had
released the graph, re-paying the vector republish and a topology mutation on an empty
prefix — that second call is now skipped when the owned binding is already gone.

**Why:** the untrack path is now asynchronous and honest about this, but the cost itself is
the same as before and lands on every session on the daemon.

**Context:** three fixes, in order of value. (1) Narrow the resolve-lane hold in
`ReconcileContractEdgesForFrontier` to the incident-edge scan and the replace (the
function's own comment says the lock exists for the scan), with a page yield in the scan —
the same remedy the tstypes apply uses. (2) Give the analysis suite a yield point or extend
`preemptWorkspaceRederive`'s stand-down to it, so a teardown does not queue behind a
47-minute pass. (3) Page the purge itself and yield the gate between pages. Note that
`intent_transitions.last_progress` used to be stamped only at worker entry and on deferral,
so a frozen `last_progress` was NORMAL for a healthy demotion — do not read it as a stall
(stamps per saga phase and a start/finish log around the contract reconcile were added on
2026-09-06). Recovery if a demotion really stalls: a daemon restart resumes the transition
(`resumeModeTransitions`) and re-entering `demote` is idempotent; no catalog surgery.

**Effort:** M **Priority:** P3 **Depends on:** nothing.

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

### Decide whether the watch poller should default on

**What:** Decide whether `Watch.Enabled` should default to `true`. Nothing else
about the flag is open — see the correction below.

**Why:** The poller is the only *generic* backstop against a watch-topology
surprise. `observeGitHead` -> `finalizeGitHead` (`internal/indexer/poller.go`)
restamps `indexed_sha` on its own within one interval, so with it running the
linked-worktree freshness defect fixed above would have been at most ten minutes
of staleness rather than permanent. Its cost is one `git rev-parse` plus a
bounded receipt sweep per repository per interval, the interval scaling from
15 s to 10 min by node count (`pollInterval`).

**Correction, 2026-09-01 — the other half of this entry was wrong.** It
originally proposed splitting the poller out from the flag, on the premise that
`watch.enabled: false` failing to disable watching was an accident. It is not:
upstream settled it deliberately, in both directions. `969c26b4` made `Enabled`
gate all of `Start()` and was reverted by `30878fbe` because
`config.Default()` ships `Enabled: false`, so every repo without an explicit
`watch.enabled: true` silently got no live indexing at all. The flag now has
exactly ONE reader — `internal/indexer/watcher.go:616` — gating only the poller
that runs *alongside a live fsnotify backend*. The two degraded-path pollers
(slow-mount, inotify/FD exhaustion) start unconditionally, because fsnotify is
already dead there and declining the poller would leave the repo stale with no
fallback at all. `TestWatcher_ShippedDefaultStillWatches` pins the shipped
default; upstream noted every other watcher test hardcodes `Enabled: true`,
which is why the regression shipped uncaught.

**Context:** The evidence that raised this — six days of
`~/.gortex/cache/daemon.log` carrying zero poller lines while carrying live
content-watcher lines for the same repositories — is consistent with the design
above rather than anomalous: fsnotify was healthy throughout, so the only poller
in play was the opt-in one, and no repo opts in (the global config sets no
`watch:` block, `gortex/.gortex.yaml` sets `enabled: false` explicitly, and
`config.Default()` is `false`).

The argument against defaulting on is now stronger than when this was written:
the GitWatcher fix restored the designed ref path for linked worktrees, so the
poller is redundant in every case currently known. It earns its cost only
against the *next* topology surprise. That is a real but speculative benefit,
which is why this is a judgement call rather than a bug.

**Effort:** S
**Priority:** P3 — downgraded from P2 once the GitWatcher fix closed the
concrete case that motivated it.
**Depends on:** Nothing.

### A checkout's HEAD is now verified against its own repository before it is written anywhere

**What:** closed 2026-09-05. `git -C <root> rev-parse HEAD` is directory-scoped: when a
linked worktree's `.git` link is already unlinked but the directory still exists (the
window every `git worktree remove` and `rm -rf` passes through), git walks up to the
enclosing repository and answers ITS HEAD with exit 0. That is how `local@MR6410`'s
ref-change reconcile adopted the docker-env superproject's `038bba933ed0` on 2026-09-04
(docker-env and `src/local` share history, so the cross-repository diff was even valid),
re-rooted 13,791 paths under the vanishing worktree, evicted 2,845 files and published the
foreign commit as its baseline. `src/addons` has no `.git` at all (it is gitignored by
docker-env), so its `gortex repos` HEAD was the superproject's too.

**Fix:** one shared guard, `checkoutHeadSHA` (`internal/indexer/checkout_head_identity.go`),
resolves the common dir and the commit in ONE `git rev-parse --path-format=absolute
--git-common-dir HEAD` and refuses when the root is gone, `<root>/.git` cannot be Lstat'd,
the common dir differs from the admitted baseline, or the output is unparseable (three
sentinels, each with its own log line). All four HEAD-stamping sites go through it:
`GitWatcher.Start`/`reconcile`, `repoHeadAndDirty`/`repoHead`, `pollerHeadSHA`, and the
copy-source probe `gitHeadSHA` (30 s bound, because the gitcmd semaphore wait counts against
it and a refusal would silently push a copy onto the 30-min cold index; refusals are now
logged). Every caller already treated an empty HEAD as "skip". Tests:
`TestGitWatcher_RefusesForeignRepositoryHeadDuringWorktreeRemoval`,
`TestRepoHeadAndDirty_RefusesForeignRepositoryHead`,
`TestCheckoutHeadIdentity_RefusesRelinkedGitDir`, `…SameGitPathThroughSymlink`.
Visible consequence: `addons`' HEAD column is now empty, which is honest — the poller
would otherwise have diffed docker-env's history against the addons graph.

**Residual:** subdirectory tracking is not a supported shape (track accepts any directory;
a non-git root already takes the "no git head" path), so the guard makes no allowance for
it. If that ever changes, derive the expected common dir from the root admitted at track
time rather than requiring `<root>/.git`.

**Effort:** — **Priority:** done

### `gortex untrack` is asynchronous; `--wait` polls to the true end state

**What:** closed 2026-09-06. `untrack_repository` blocked on `ApplyUntrack`, whose demote
plan ran a synchronous commit-layer build for the demoted checkout (14–20 min on this
workspace, see the coordinator entry below), so every untrack of a worktree was abandoned
at the 59 s tool deadline while the work kept running detached. The handler now calls
`StartApplyUntrack` and answers `pending` with the `transition_id` and `checkout_id`;
`gortex untrack --wait [--wait-timeout 30m]` polls the read-only `list_checkouts` until
`effective_mode=automatic` AND no transition is in flight (the transition row is deleted by
`CompleteIntentTransition`, after the dedicated corpus is retired and the config entry
removed), fails fast on a `failed` transition or a checkout that disappears, gives up after
three consecutive poll errors, and shows "mode flipped; retiring the dedicated corpus"
in between. A demotion no longer fails after its commit when no coordinator can be started.

**Measured:** probe copy 125k nodes: `--wait` returned "landed" in 3.6 s on the mode flip
alone (the bug the end-state predicate fixes); the real teardown took 28 min because
`purgeRepoChecked` holds the batch-mutation gate and the reachability writer for the whole
eviction (`watcher: mutation admission timed out; deferring patch` during it). That cost is
pre-existing and unchanged — a 30-minute `--wait-timeout` is the honest default here.

**Effort:** — **Priority:** done. Follow-up (P3): the corpus retirement's gate hold time.

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

**Update 2026-09-01:** the diverged branch no longer schedules that repo-wide
derive when the reconcile's scoped tail already repaired the divergence
(`copiedDivergenceRepaired`) — it restamps directly, like the identical branch.
So this exposure now covers BOTH copy branches: any copied worktree that reaches
`ready` by restamp is stranded by its first subsequent save until something
re-stamps. Same fix candidates as below; the armed-but-idle derive would cover
both branches at once.

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

### The master resolve binds across sibling checkouts — the checkout gate covers only the framework passes

**What:** `ast_inferred` resolver output (references / extends / composes) crosses
checkout-group boundaries. Measured 2026-09-01 on the live docker-env store:
`local@aurora-redesign` — a PLAIN tracked worktree, no copy involved — holds
1,206 `references` (origin=ast_inferred) into `local`, and `local` holds 584
into aurora. A freshly copy-tracked worktree (`rederive-probe`, 70-file
divergence) accumulated 5,320 such edges into its two siblings from 221 source
files during one reconcile — its `ResolveFilesAndIncoming` (files=168) ran
BEFORE the new prefix was registered, so `publishCheckoutGroups` could not yet
name it a sibling even if the resolver consulted the grouping. Cross-REPO
parity is untouched (probe→odoo and probe→addons matched `local` exactly:
99,462 / 131,641).

**Why it matters:** two checkouts of one repository are in contact — "who uses
this symbol" on `local` returns the sibling checkout's callers. Same disease as
the ~190k-edge checkout-grouping incident, small dose, and it grows with every
resolve over sibling-heavy frontiers.

**Context:** the checkout gate exists for the framework passes
(`frameworkRepoGateStore`) and the copy's inbound pass; the master resolve's
candidate selection has no equivalent. Two fix shapes: (a) gate candidate
selection by `graph.SiblingCheckouts` in the resolver, (b) for the copy path
specifically, make the destination prefix part of the published grouping before
`ReconcileRepoCtx`'s resolve tail runs. (a) also fixes the plain-worktree
baseline; (b) alone does not.

**Effort:** M
**Priority:** P2
**Depends on:** Nothing.

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

**The harness now exists.** `copyTrackHarness` in
`internal/indexer/copy_deferred_tail_test.go` is the sqlite store + real git
worktree + `ReconcileRepoCtx` setup this entry asked for, and it drives
`trackWorktreeByCopy` end to end (note `copySourceRepo`'s 12 files: fewer and the
reconcile routes to a full retrack on churn). What is still missing is an
assertion on the RESTAMP specifically — the existing tests observe the log lines
around it, so moving `RestampCopiedReadiness` back above `ReconcileRepoCtx` would
still leave them green. Add a `CopiedReadinessRestamper` spy that records call
order against the reconcile.

**Effort:** M
**Priority:** P2
**Depends on:** None.


### Verify whether enrichment providers write edges outside the changed file set

**What:** Determine whether go-types / python-types / typescript-types can add or
evict edges on files that were NOT in the frontier they were handed. If they can,
both the per-file ledger discharge and the new scoped content stamp are optimistic
together.

**Why:** the file-scoped enrichment stamp records repo-level `content_gen`
coverage after a pass over an exact frontier, on the grounds that
`dischargePendingEnrichFrontier` already makes the same frontier-sufficiency
assumption and has done so since it was written. That is a real assumption, not a
proof, and it was accepted knowingly rather than verified. If a provider reaches
outside its batch, a repo can read `ready` while carrying edges no pass has
refreshed — the silent-subset failure the readiness column exists to catch, and
one the stamp would now hide rather than surface.

**Context:** raised as an outside-voice finding during the review of the scoped
stamp (decision 10A), 2026-09-02. The stamp itself is
`Store.AdvanceContentGenForCompletedProviders`; the frontier is built by
`Indexer.deferredEnrichFrontiers`. Checking one provider is enough to settle it:
enrich a two-file repo over a one-file frontier and diff the edge set of the file
that was not on it.

**Effort:** S
**Priority:** P2
**Depends on:** Nothing.


### `TestAgentsRenderIsHermetic` fails whenever Claude Code is running

**What:** the hermeticity guard at `cmd/gortex/agents_render_test.go:186` asserts
that no user-level manifest path under the real `$HOME` has an mtime newer than
`renderGuardStart`. One of those paths is `~/.claude.json` — a file Claude Code
rewrites continuously while a session is open. Compare content (or a hash taken
before the render) instead of mtime, or skip a path whose mtime moved without its
bytes changing.

**Why:** it makes `go test ./cmd/gortex/` non-deterministic for anyone running the
suite from inside a Claude Code session, which is now a normal way to work on this
repo. A test that fails for reasons unrelated to the change under test is worse
than no test: it trains people to re-run until green, and the next real leak gets
re-run away with it.

**Context:** measured 2026-09-02 — 2 failures in 4 consecutive `-race` runs of the
package, both:

    agents_render_test.go:186: claude-code: /Users/commeta/.claude.json was
    modified during the render — the sandbox leaked into the real home

The test already anticipates the cause in its own message ("false positive only if
another process wrote that path during this test"), so this is closing a known
hole rather than diagnosing a new one. Everything else in the run passed, and the
same package passes solo.

**Effort:** S
**Priority:** P3
**Depends on:** Nothing.

### `TestConcurrencyCapNeverExceeded` samples for in-flight git processes and can see none

**What:** `internal/gitcmd/gitcmd_test.go:118` fails with `expected at least one
in-flight git process, saw 0` under a loaded `go test ./...` sweep, and passes
3/3 in isolation on the same machine.

**Why:** the assertion is a sample of a race it does not control. Under a full
`-race` sweep the scheduler can run every spawned git process to completion
between two samples, so the test reports a concurrency-cap violation for a run
that never violated the cap. A flake that fires only when CI is busy is the
worst kind: it looks like a real concurrency finding.

**Context:** observed 2026-09-02 in a `go test -race -timeout=30m ./...` sweep;
the only failing package, and `internal/gitcmd` was unmodified in that tree. The
fix is probably to make the workers observable (a barrier the test releases)
rather than to sample harder.

**Effort:** S
**Priority:** P3
**Depends on:** Nothing.

### The tstypes whole-repo pass was O(N²) on Odoo-shaped corpora — fixed twice over

**What:** record of the 2026-09-05/06 investigation; residuals below. Three compounding
causes, all measured:
1. `tstypes.Provider` had no file-batch entry, so every "scoped" copy repair was the
   whole-repo pass (fixed 2026-09-05: frontier-scoped `EnrichFilesContext`).
2. The enforced per-pass budget is `enrichRepoTimeout(len(files))` — a python FILE count
   fed to a formula calibrated for symbol-NODE counts (10 min + 40 ms × N): 11,970 files →
   1,079 s, and the 5,400 s `deadline` logged at start is the outer ceiling, not the budget.
   The budget and the pass progress (`budget_s`, `file_count`, `phase`, `pages_applied`,
   `staging_ms`, `apply_ms`, `symbols_covered`) now ride on `semantic enrichment complete`.
   The formula itself is deliberately unchanged: with the fixes below the pass fits.
3. Per-page preload was repo-sized twice: (a) every call's METHOD name was hydrated
   repo-wide and seeded the frontier walk (2M+1 nodes per page for M classes sharing
   `create`/`write`); (b) `loadAdjacency` fetched EVERY inbound edge of every frontier node
   while the applier reads only `member_of`/`param_of` — 860,270 inbound edges on page 0 of
   `addons` for 6,809 used (0.8%), `AccountMove.create` alone 10,255 in-edges. Fixed by the
   `nameRole`-scoped preload (synthetic 12k files 165 s → 17 s) and by pushing the kind
   filter into SQL on the existing `edges_by_to(to_id, kind)` index
   (`InEdgesByKindFinder`; conformance-tested on both backends). On a copy of the live
   store the `addons` pass went from cut at 1,079 s in phase `supers` to complete in 88 s
   at 93.7% coverage with an identical resolution digest.

**Residual checks:** (1) confirm the live `addons` pass completes and writes its marker on
the next restart; (2) three sibling readers still take the unfiltered inbound projection
and filter in Go — `lsp/provider.go::addOverrideEdges`, `store_traversal.go::GetFileSubGraph`,
`query/engine.go::GetFileSymbols` — same hub exposure, untouched; (3) the in-memory
`graph.Graph.NodesInFilesByKind` scans every node × 14 kinds despite `byFile` (a fixture
cost on SQLite, a live O(N²) if that backend ever runs a pass); (4) the 2026-09-01 cliff
(the same pass completed in 194 s on 08-30) is most plausibly hub in-degree growth from the
framework/odoo synthesizer edges, not a tstypes change — the 08-31 `buildIndex` commits
were benchmarked at 5–9% and cleared; (5) the analysis suite (`analysis_persistence.go`)
still contends with enrichment applies for the resolve mutex.

**Effort:** S (residuals) **Priority:** P3

### `addons` python-types: cause found and fixed; verify the marker lands live

**What:** the pass never finished because (a) its enforced budget was a file count in a
per-node formula (1,079 s, never logged) and (b) the per-page preload was O(hub in-degree):
see "The tstypes whole-repo pass was O(N²)…" above for the measurements and the fix. The
table of cut passes (2,308 s / 1,495 s / 1,087–1,924 s, apply ≈ 1,080 s in every one,
four of them with zero lock wait and alone in the pool) is explained by the budget alone;
the "super-linear shape" hypothesis was right but the term was inbound degree, not a
mega-file or a deep hierarchy (largest addons python file: 194 nodes).

**Remaining:** after the next restart, confirm `semantic enrichment complete
provider=python-types repo=addons partial=false` and that `deferred enrichment re-armed`
no longer names `addons`. Note `addons` has no `.git` (it is gitignored by docker-env), so
its re-arm marker reads `no git head` and the whole-repo marker is keyed on that path.

**Effort:** S **Priority:** P3

### Make the futile-enrichment record survive a restart

**What:** `Manager.futilePasses` (`internal/semantic/futile_pass.go`) remembers
a (repo, provider, revision) pass that ran out its deadline and landed nothing,
so the next trigger in the same process skips it. The record dies with the
daemon, and `MaybeSeedPendingEnrich` re-arms the repo on the next warm restart —
which is exactly when the cost is paid.

**Why:** the in-process record only catches the second and later triggers within
one daemon life (warmup, then a copy-track, then the janitor). The first
trigger after every restart still pays the full dead budget.

**Context:** neither existing store has a home for it, and the file explains
both refusals in detail. `enrichment_state` records COMPLETIONS and has no
attempt column; the schema is pinned in lockstep with upstream, and writing an
attempt into `indexed_sha` would make `RefreshEnrichmentProviders` launder the
row into "a pass really finished", destroying the `EnrichmentNeverRan` re-arm.
`daemon.state.json` is written fresh at startup and removed at shutdown by
design. So this needs either an upstream schema addition (an `attempts` /
`last_attempt_sha` column, or a small `enrichment_attempts` table) or a new
daemon-owned file with an explicit lifetime. Decide which before implementing.

**Effort:** M
**Priority:** P3
**Depends on:** the `addons` fix above landed 2026-09-06 and the pass now finishes on a
store copy in 88 s, so on this workspace the record no longer matters; keep the entry for
a corpus that genuinely cannot finish.


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
