# Pull-request actions

The daemon exposes a small set of data-only MCP tools over your repository's
pull requests:

- **`list_prs`** — list a repo's PRs with a one-shot review-state
  classification (DRAFT / BASE_MISMATCH / CHANGES_REQUESTED / APPROVED /
  STALE / READY), a normalized CI rollup, and per-PR merge blockers.
- **`get_pr_impact`** — map a PR's changed files to the symbols they define,
  score PR-level risk across five axes, and group the affected surface by
  community and by caller/test file. Set `receipt: true` for a small,
  privacy-safe review receipt.
- **`triage_prs`** — rank a repo's open PRs by graph-derived review priority
  (highest risk first, deterministic).

These tools are **read-only** — none of them edits code or posts to GitHub.

## Providing a GitHub token to the daemon

The daemon **self-serves** PR data: it pairs a GitHub token with the repo
identity it already indexed, so there is no CLI-versus-daemon auth split and
no dependency on a `gh` CLI login. All it needs is a token in **the daemon's
own environment**.

The token resolves from, in order:

1. `GH_TOKEN`
2. `GITHUB_TOKEN`

Set one of these in the environment the daemon process runs under — not in
your interactive shell, unless the daemon inherits it. For example, when
starting the daemon manually:

```bash
GH_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxx gortex daemon start --detach
```

For a long-running daemon managed by a service supervisor, set the variable in
that unit's environment (e.g. a systemd `Environment=` line, a launchd
`EnvironmentVariables` entry, or your process manager's env file) so it is
present every time the daemon starts.

GitHub Enterprise: when `GITHUB_API_URL` or `GH_HOST` names a non-`github.com`
host, the forge client targets that Enterprise API base automatically. The
same `GH_TOKEN` / `GITHUB_TOKEN` resolution applies.

In CI, a per-PR Action's `GITHUB_TOKEN` is picked up automatically — no extra
configuration is needed.

## When no token is available

If no token is resolvable **and** you did not supply already-fetched data,
each tool degrades gracefully instead of failing:

```json
{ "error": "forge unavailable",
  "hint": "set GH_TOKEN (or GITHUB_TOKEN) in the daemon environment" }
```

The hint names the token for the forge that actually serves the repository,
derived from its `origin` remote. On a GitLab remote it instead reads:

```json
{ "error": "forge unavailable",
  "hint": "set GITLAB_TOKEN (or run `glab auth login --hostname gitlab.example.com`) in the daemon environment" }
```

## Declaring a host the router cannot recognise

Routing needs no configuration for github.com, GitHub Enterprise, gitlab.com, a
host with a `gitlab` label, or any host you have already run `glab auth login`
against. A GitLab or GHE behind a vanity domain carries none of those signals,
so declare it in the **global** config (`~/.gortex/config.yaml`):

```yaml
forge:
  hosts:
    - host: code.internal.corp
      provider: gitlab                                    # github | gitlab
      api_base: https://code.internal.corp/gitlab/api/v4  # optional
      token_env: CORP_GITLAB_TOKEN                        # optional
```

`hosts` is a list rather than a map keyed by hostname because the config loader
treats `.` as a key-path delimiter — a map key of `code.internal.corp` would be
parsed as the nested path `code` → `internal` → `corp`.

`token_env` names the environment **variable**, never the token itself, so no
credential lands in a file that gets committed. It is read before the generic
`GITLAB_TOKEN`, which is what lets two instances each use their own credential.

Only the global config is consulted — never a repo's own `.gortex.yaml`. A
repo-local file is attacker-supplied content in any repo you did not write, and
letting it name the API base and token variable for its own host would let a
cloned repo redirect where a credential is sent. `GORTEX_FORGE_PROVIDER`,
`GITLAB_API_URL`, and `GITLAB_TOKEN` still override a declaration for one run.

A GitHub rate-limit is surfaced as a typed degradation carrying the
Retry-After hint:

```json
{ "error": "rate limited", "retry_after_s": 42 }
```

## Skipping the network with caller-supplied data

Every tool accepts an optional caller-supplied data path so an agent (or a CLI
front-end) that already fetched the PR data can avoid a refetch:

- `list_prs` accepts `prs` — a JSON array of already-fetched PR objects.
- `get_pr_impact` accepts `files` — a JSON array of changed file paths.
- `triage_prs` accepts `prs` and/or `files` (a JSON object mapping a PR
  number to its changed file paths).

When supplied data is present, the tool classifies / scores it directly and
makes no network call. Triage additionally caches each fetched PR for a short
window so a re-run within the window does not refetch the same PR.

## Per-PR reviewer graph bundle

`gortex prs bundle <number>` writes a self-contained, reviewer-focused slice of
the knowledge graph to a JSON file (`--out`, default `pr-<number>-bundle.json`):

- the PR's **changed files**,
- the graph-joined **impact** — the blast radius, the five-axis PR-risk score,
  and a small privacy-safe **review receipt** (risk tier + next-safe-action +
  merge-blocker verdict) — taken verbatim from `get_pr_impact`,
- the ranked **reviewer suggestions** from `suggest_reviewers` (CODEOWNERS +
  recent authorship + co-change experts).

The bundle is deterministic for an unchanged PR (the changed-file list is
sorted and the JSON is stably indented), so it can be uploaded as a CI artifact
and diffed across runs. The command is daemon-first: the forge supplies the
changed-file set and the daemon joins it against the indexed graph — no second
in-process index. A failing `suggest_reviewers` (missing token / CODEOWNERS)
does not sink the bundle; the reviewers section is simply omitted.

### Wiring it into CI

A ready-to-use GitHub Action template lives at
[`.github/workflows/gortex-pr-review.yml.example`](../../.github/workflows/gortex-pr-review.yml.example).
The `.yml.example` suffix means GitHub does **not** run it as-is — copy it to
`.github/workflows/gortex-pr-review.yml` in your repository to enable it. On
each `pull_request` it builds gortex, starts the daemon, indexes the checked-out
repo, runs `gortex prs bundle <N>`, and uploads the bundle with
`actions/upload-artifact`. It maps the Action-provided `GITHUB_TOKEN` to
`GH_TOKEN` so the daemon self-serves the PR's changed files with no extra secret
configuration.
