# Multi-repo workspaces

Gortex can index multiple repositories into a single shared graph, enabling cross-repo symbol resolution, impact analysis, and navigation.

## Workspace boundary

Every node and contract is keyed on a **workspace slug**, which is the hard graph boundary for cross-repo work. Two repos that should pair their contracts (an HTTP server and the client that calls it, a Kafka producer and its consumer, etc.) must declare the same `workspace:` in their `.gortex.yaml` — otherwise contract matching stops at the boundary and they look like orphans.

Slug resolution precedence (first match wins):

1. `RepoEntry.workspace` in `~/.gortex/config.yaml` — overrides everything, ideal for OSS / read-only repos where you don't want to leave an artifact in the tree
2. `workspace:` in the repo's own `.gortex.yaml` — the default for first-party repos
3. The repo prefix — fallback when neither is set, so each unconfigured repo gets its own isolated workspace

The same chain applies to the optional `project:` slug (a sub-bucket inside a workspace). The daemon loads every tracked repo into one shared graph; you scope a query to a single workspace or project at request time rather than at startup. Over the HTTP surface (`gortex daemon start --http-addr ...`) the `/v1/graph` route accepts `?project=` and `?repo=` to narrow the dump, so a typo'd value returns an empty result for that request instead of bringing the whole index up empty.

## Configuration

Two-tier config hierarchy:

- **Global config** (`~/.gortex/config.yaml`) — projects, repo lists, active project, reference tags
- **Workspace config** (`.gortex.yaml` per repo) — guards, excludes, local overrides

Excludes are layered — builtin → the repo's `.gitignore` chain → global → per-repo entry → workspace — with gitignore semantics. `.gitignore` is respected by default so you don't have to re-declare entries already curated for git; opt out per-workspace with `respect_gitignore: false` in `.gortex.yaml`. Use `!pattern` in a later layer to re-include something an earlier layer excluded. Beyond `.gitignore`, the index walk also honors per-directory `.gortexignore` files (Gortex's own ignore file, a sibling to `.gitignore`) and ripgrep's `.ignore` / `.rgignore` — each scoped to the directory that contains it.

When a tracked root sits below its git root — the monorepo case, `gortex track repo/projects/App` with the repository at `repo/` — every `.gitignore` from the git root down to the tracked root applies, as it would for git, with ancestor patterns re-anchored onto the tracked root. A deeper file overrides a shallower one, so a `!pattern` next to your code still wins over the repository-wide rule. Ancestor patterns that describe a sibling subtree are dropped, and so is one that would ignore the tracked root itself: you asked Gortex to index that directory explicitly.

```yaml
# ~/.gortex/config.yaml
active_project: my-saas

exclude:                            # Applies to every tracked repo
  - "**/*.generated.*"
  - "node_modules/"                 # Already in the builtin baseline

repos:
  - path: /home/user/projects/gortex
    name: gortex
    exclude:                        # Extra patterns just for this repo
      - "results/**"

projects:
  my-saas:
    repos:
      - path: /home/user/projects/frontend
        name: frontend
        ref: work
      - path: /home/user/projects/backend
        name: backend
        ref: work
      - path: /home/user/projects/shared-lib
        name: shared-lib
        ref: opensource
```

`synthesize_external_calls: true` (opt-in, default off — set in `.gortex.yaml` or the global config) makes the resolver synthesize placeholder nodes for calls into un-indexed external packages or sibling services, so call-chains keep the external hop instead of terminating at the indexed boundary.

## Daemon tuning (optional)

The daemon's defaults handle typical workflows without configuration. These knobs exist for monorepos, branch-heavy workflows, or filesystems without fsnotify support.

```yaml
# ~/.gortex/config.yaml (or per-repo .gortex.yaml)
watch:
  debounce_ms: 150            # per-file patch debounce (default 150)

  # Storm mode — when more than N events land within the window,
  # switch from per-file debounced patching to a batched reconcile
  # that defers cross-file resolver + search work until a quiet
  # period has passed. Amortises the cost of bulk operations
  # (rsync, npm install, branch checkout, bulk format-on-save,
  # find-and-replace). Zero = disabled (default).
  storm_threshold: 0          # 0 disables; try 50 on monorepos
  storm_window_ms: 500
  storm_quiet_period_ms: 500
```

Environment variables:

- `GORTEX_RECONCILE_INTERVAL` — janitor tick that walks every tracked repo and runs the full-tree `IncrementalReindexPaths` pipeline against disk. Insurance against fsnotify gaps on NFS/SMB mounts, inotify watch-limit exhaustion, or daemon downtime where edits happened offline. Default `1h`; `"0"` or `"off"` disables; otherwise any Go duration string (e.g., `15m`).
- The daemon also watches each tracked repo's `.git/HEAD`, so branch switches and rebases reconcile incrementally (via `git diff --name-status`) rather than by re-indexing every changed file individually — no configuration needed.
- `GORTEX_WARMUP_FULL_RETRACK=1` — force every repo through a whole-repo re-track (evict + re-parse every file) on the next warm restart instead of the default scoped reconcile. An escape hatch for when the on-disk change census itself is suspect.
- `GORTEX_WARMUP_FULL_RESOLVE=1` — force the warm-restart master resolve to re-examine the whole graph instead of scoping to changed repos; also makes the resolver ignore the durable terminal-edge stamp and re-attempt every previously-given-up-on edge. Use when a scoped resolve is suspected of missing edges.
- `GORTEX_WARMUP_FORCE_ENRICH=1` — bypass the persisted per-repo enrichment-completion markers and re-run semantic enrichment for every repo on warm restart, even ones whose marker already matches HEAD on a clean tree.
- `GORTEX_DAEMON_MEMLIMIT` — standing soft memory limit installed at daemon boot, as a human size (`4GiB`, `2048MiB`, `2G`) or `off` / `0` to disable. The daemon is a long-lived background service; a soft limit makes the GC pace against a ceiling and resist heap balloon growth rather than letting the high-water climb toward machine RAM. Overrides the `daemon.memory_limit` config value; an explicit `GOMEMLIMIT` overrides both (the runtime already honors it). Unset applies the default policy: a quarter of host RAM, clamped to `[1GiB, 8GiB]`. The cold-index window temporarily raises this to a larger budget and restores it afterward.
- `GORTEX_DAEMON_MEMRELEASE=0` — disable the post-burst heap-to-OS release. By default the daemon calls `debug.FreeOSMemory()` at allocation-burst boundaries (warmup completion, a reconcile-janitor tick that reindexed something, the close of a cold-index window, and a whole-graph analysis pass) so a burst's high-water footprint is returned to the OS promptly instead of pinning resident memory at the peak. It only ever fires at those boundaries, never on a timer.

## CLI

```bash
gortex track /path/to/repo          # Add a repo to the workspace
gortex untrack /path/to/repo        # Remove a repo from the workspace
gortex mcp --track /path/to/repo    # Track additional repos on startup
gortex mcp --project my-saas        # Set active project scope
gortex status                       # Per-repo and per-project stats
gortex repos                        # List tracked repos — head-commit SHA, last-indexed time, staleness flag
gortex repos --json                 # Same, machine-readable (for scripts / CI)

# Stamp workspace / project slugs across tracked repos (migration helper)
gortex workspace list                                       # Show what each tracked repo currently declares
gortex workspace list --json                                # Same, machine-readable
gortex workspace set backend api                            # Write workspace=api to backend's .gortex.yaml
gortex workspace set upstream-lib api --global              # OSS-friendly: pin to api in ~/.gortex/config.yaml
gortex workspace set-all api --root ~/projects/work --yes   # Bulk: stamp every tracked repo under a prefix

# Manage the effective ignore list used by indexing + watching
gortex config exclude list                          # Show all layers (builtin, global, repo entry, workspace)
gortex config exclude add pkg/generated             # Default target: workspace .gortex.yaml
gortex config exclude add '**/*.bak' --global       # Write to ~/.gortex/config.yaml
gortex config exclude add testdata/ --repo backend  # Write to a RepoEntry
gortex config exclude remove pkg/generated          # Remove from the same target
```

## MCP tools

Agents can manage repos at runtime without CLI access:

| Tool | Description |
|------|-------------|
| `track_repository` | Add a repo, index immediately, persist to config |
| `untrack_repository` | Remove a repo, evict nodes/edges, persist to config |
| `set_active_project` | Switch project scope for all subsequent queries |
| `get_active_project` | Return current project name and repo list |

Locate, reach, and analyze query tools uniformly accept `repo`, `project`, `workspace`, and `scope` parameters for scoping (plus `ref` where reference tags apply). All are clamped to the session workspace — the hard isolation boundary. Default breadth now follows **tool intent** when `scope.intent_defaults` is enabled (the default); see [Tool scoping by intent](#tool-scoping-by-intent) below.

For `analyze`, the overrides genuinely narrow its **graph-node** kinds — `dead_code`, `hotspots`, `cycles`, `health_score`, `todos`, `stale_code`, `ownership`, `coverage_gaps`, `coverage_summary`, `impact`, `bottlenecks`, `role`, `k8s_resources`, `images`, `kustomize`, `dbt_models`, `external_calls`, and the like — and, since v1, its **edge-walk / graph-algorithm / framework / file-AST-scan** kinds too (`channel_ops`, `pubsub`, `routes`, `models`, `pagerank`, `kcore`, `edge_audit`, `tests_as_edges`, `sast`, `review`, …), which prune their rows / re-tally their counts against the same workspace + repo allow-set. The narrowing also resolves the two kind-specific collisions: `kind=cross_repo` keeps `repo` as its boundary filter and `kind=cycles` keeps `scope` as a file-path / package prefix (both are stripped from the uniform scope-resolution view). **v1 caveat:** the remaining long-tail kinds — community detection (`clusters`, `concepts`, `suggest_boundaries`), git/disk-mining (`blame`, `coverage`, `fixes_history`, `retrieval_log`, `temporal_verify`), per-id (`would_create_cycle`, `def_use`), `synthesizers` / `resolution_outcomes`, and `sql_rebuild` — remain workspace-bound but are **not** repo-narrowed — passing a narrowing arg on such a kind stamps a `scope_note` on the response disclosing the no-op.

Some `analyze` operation names are not dispatcher kinds at all: the facade maps them to a separate legacy tool, which the dispatcher — and so `resolveScope` — never sees. Those tools carry the clamp themselves, and they split the same two ways:

- **Workspace-clamped and repo-narrowed:** `health` (`audit_health`), `clones` (`find_clones`), `inspections` (`run_inspections`), `processes` (`get_processes`), `recent_changes` (`get_recent_changes`). Their rows are per-node, per-pair, or per-file, so a `repo` / `project` / `scope` selector narrows them for real.
- **Workspace-clamped only:** `communities` (`get_communities`), for the same reason as the `clusters` / `concepts` kinds — one partition is computed over the whole index, so a narrowing arg is resolved for `scope_applied` and then widened to the workspace, with the response disclosing it.

Their by-id lookups (`get_communities id:`, `get_processes id:`) resolve against the clamped set, so an out-of-scope id reports the same miss as one that never existed.

## Tool scoping by intent

Tools are split by intent — each group has a different default scope:

| Intent | Tools | Default scope |
|--------|-------|---------------|
| **Locate** ("where is X defined") | `search_symbols`, `search_text`, `find_files` | current repo |
| **Reach** ("who consumes X") | `find_usages`, `get_callers`, `get_call_chain`, `contracts` | workspace |
| **Analyze** | `analyze`, `review`, sast | workspace (graph-node + edge-walk / algorithm / framework / scan kinds narrow to `repo`/`project`/`scope`; community / git-mining / per-id kinds stay workspace-bound — see the caveat above) |

Other query tools (`get_symbol`, `get_file_summary`, `smart_context`, etc.) keep their existing per-tool scope classification; the intent defaults above apply to the locate/reach/analyze groups listed in the table.

### `scope.intent_defaults` config flag

- Controls the intent-based default scoping described above
- **Defaults ON** (enabled out of the box — this is the new behavior after upgrade)
- **Narrow-only invariant:** the intent defaults only ever *narrow* within the session workspace (the hard isolation boundary); they never widen past it, and an explicit `repo` / `project` / `workspace` / `scope` arg always overrides the default
- Opt out: set `scope.intent_defaults: false` in `.gortex.yaml`, or set env var `GORTEX_SCOPE_INTENT_DEFAULTS=0`

**⚠ Upgrade note (behavior change):** When upgrading to this version:

- Locate tools narrow their default: project → repo (you now need `repo:"*"` to search the whole workspace)
- Reach tools widen their default: project → workspace (cross-repo callers surface automatically)
- Restore the old behavior with `scope.intent_defaults: false` or `GORTEX_SCOPE_INTENT_DEFAULTS=0`

### Widen sentinels

When intent defaults are on, you can still widen or narrow explicitly:

- `repo:"*"` — widen a locate tool back to the whole workspace
- `project:<name>` — select the middle rung (explicit project scope)
- `scope:<name>` — select a named saved scope

### Uniform parameter set

Every locate/reach/analyze tool now uniformly accepts `repo`, `project`, `workspace`, and `scope` parameters — including the legacy tools the `analyze` facade forwards to (`audit_health`, `find_clones`, `run_inspections`, `get_communities`, `get_processes`, `get_recent_changes`). All are clamped to the session workspace (the hard isolation boundary). For `analyze` this narrows the graph-node, edge-walk, graph-algorithm, framework, and file/AST-scan kinds; the remaining community / git-mining / per-id / synthesizer kinds are workspace-bound but not repo-narrowed in v1 (see the [MCP tools](#mcp-tools) caveat above).

### Response metadata

Scoped tool responses carry a `scope_applied` meta field plus a one-line widen hint naming an explicit override that re-broadens the result (e.g. `repo:"*"` for the whole workspace, or `project:<name>` / `scope:<name>` to re-scope to a deliberate rung). `analyze` additionally stamps a `scope_note` when a narrowing arg is passed to a kind that does not repo-narrow its rows in v1, so the no-op is self-documenting rather than silent.

## How it works

- **Qualified node IDs** — IDs are always `<repo_prefix>/<path>::<Symbol>` (e.g., `frontend/src/app.ts::App`), including for a workspace tracking a single repo: a lone repo is simply the first tracked repo. Tools still accept an unqualified path (`src/app.ts`) when exactly one repo is tracked, since there is then nothing to be ambiguous about.
- **Cross-repo edges** — the resolver links symbols across repo boundaries with same-repo preference. Cross-repo edges carry a `cross_repo: true` flag.
- **Impact analysis** — `explain_change_impact`, `verify_change`, and `get_test_targets` follow cross-repo edges automatically, grouping results by repository.
- **Shared repos** — the same repo can appear in multiple projects with different reference tags. It's indexed once and shared across projects.
- **Auto-detection** — set `workspace.auto_detect: true` in `.gortex.yaml` to auto-discover Git repos in a parent directory.
