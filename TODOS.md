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
