# `internal/daemon`

Process-local daemon plumbing — Unix socket transport, MCP dispatch
session state, runtime state paths, plus the iteration-1 multi-server
roster and editor-overlay machinery.

## Surface

| File | Role |
|------|------|
| `paths.go` | Default file paths (socket, PID, log) |
| `proto.go` | Wire types for the daemon's internal MCP protocol |
| `client.go` | Connect-and-talk client used by the proxy CLI |
| `servers.go` | `~/.gortex/servers.toml` loader + `ServerClient` (HTTP/unix) + `WorkspaceRosterCache` + `RouteForCwd` |
| `overlay.go` | `OverlayManager` — register / push / delete editor buffer overrides |
| `router.go` | `Router` — hybrid-read query router |

## Multi-server routing

`~/.gortex/servers.toml` declares the set of Gortex servers reachable
from this daemon. The schema:

```toml
[[server]]
slug = "main"
url = "unix:///run/gortex/main.sock"
default = true

[[server]]
slug = "tuck"
url = "https://tuck.gortex.example/v1"
auth_token_env = "GORTEX_TOKEN_TUCK"
workspaces = ["tuck"]
```

`url` accepts `http(s)://...` (TCP) or `unix:///path/to.sock`
(Unix-domain socket). Auth tokens come from `auth_token` (literal,
discouraged) or `auth_token_env` (env-var name resolved per-call so
rotation lands without a daemon restart).

`Router.RouteToolCall` walks the priority chain on every MCP tool
invocation:

1. Caller-supplied scope override (e.g. `workspace: "tuck"` on the
   tool args).
2. Walking up from cwd to find a `.gortex.yaml::workspace` declaration.
3. Workspace roster — pre-declared `workspaces = [...]` lists in
   `servers.toml`, then the cached `GET /v1/workspaces/<ws>/repos`
   roster from each configured server.
4. The `default = true` entry, if any.

Resolved server == `LocalSlug` → run in-process via `LocalExecute`.
Resolved server != `LocalSlug` → proxy via `ServerClient.ProxyTool`
(POST `/v1/tools/<name>` with bearer auth).

## Editor overlays

`OverlayManager` holds per-session in-flight file overrides so MCP
clients can ask "what would `find_usages` look like with my unsaved
buffer?" The HTTP front door at `/v1/overlay/sessions/...` is wired
in `internal/server`. `BaseSHA` drift detection refuses to merge a
stale overlay so wrong-line-number errors don't surface as graph bugs.

## Caveat: pre-workspace-slug stores

A graph store written by a daemon that predates the workspace-slug
schema carries empty `WorkspaceID`/`ProjectID` columns on its nodes.
The daemon's warmup path
(`MultiIndexer.BackfillWorkspaceSlugs` invoked from
`cmd/gortex/daemon_state.go::warmupDaemonState`) re-stamps them from
the per-repo `.gortex.yaml`. Without that pass, the matcher's
`EffectiveWorkspace` falls back to `RepoPrefix` and explicit
shared-workspace setups silently lose identity until every file is
touched.
