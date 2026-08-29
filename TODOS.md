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

### Warn on queries against a not-ready repo

**What:** MCP and CLI query paths consult the readiness verdict and warn, or
refuse under a flag.

**Why:** "who uses this" returns a silent subset against an underived repo
today. The `READY` column on `gortex repos` makes that detectable for the first
time, but the column is advisory — a user has to know to run `repos` first, and
an agent asking the graph a question has no reason to. The real fix is for the
answer itself to carry the caveat.

**Context:** The verdict is `readyVerdict` in `cmd/gortex/repos_ready.go`. It is
pure, and its inputs come from `store_sqlite.ReadReadinessStates` plus the
daemon runtime record — one read-only open, no control socket — so the same
inputs are available to any surface. Open design questions: warn always, or
refuse under a flag; per-tool or global; and how an agent surfaces the warning
through the response envelope without it being ignored like every other
advisory field.

**Effort:** M
**Priority:** P2
**Depends on:** the `READY` column, which supplies the verdict.

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
