# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in Gortex, please report it responsibly:

1. **Do not** open a public issue.
2. Use GitHub's [private vulnerability reporting](https://github.com/zzet/gortex/security/advisories/new), or email the maintainer directly.
3. Include a description, the affected version or commit, and steps to reproduce.

We aim to acknowledge receipt within 48 hours and will provide a timeline for a fix.

## Overview

Gortex is a code-intelligence engine. It indexes repositories into an in-memory
knowledge graph and exposes that graph over a CLI and an MCP server. Running
locally — on the user's machine, with the user's privileges — is the default and
the assumption of this policy, but Gortex can also be deployed remotely (for
example, a daemon bound to a non-localhost address), where the network-exposure
and authentication considerations below carry more weight.

The MCP tools are typically driven by an LLM agent, so the agent should be
treated as **potentially adversarial**: prompt injection through indexed content
(a crafted README, source comment, test fixture, or dependency) can cause the
agent to invoke tools with attacker-influenced arguments. The boundaries
described below — file-path confinement, opt-in network egress, and explicit
process execution — are the security boundary, not the agent's good behavior.

## Scope

### File system access

- Gortex **reads and writes files within indexed repository roots.** Editing is
  a first-class feature: tools such as `write_file`, `edit_file`, `edit_symbol`,
  `rename_symbol`, `batch_edit`, `move_inline`, `safe_delete_symbol`, and the LSP
  code-action tools modify source files in the repositories Gortex has indexed.
  After a write, the affected file is re-indexed to keep the graph fresh.
- File-path resolution is **confined to indexed repository roots.** A path —
  relative or absolute — is resolved against the roots of the tracked
  repositories, and access outside every indexed root is refused. Symlinks are
  resolved before the check so a link cannot be used to escape a root.
- Gortex does not require, and does not request, access to files outside the
  repositories you index.
- The confinement boundary is the **union of every tracked repository root**,
  not the workspace of the session making the call. A session working in one
  tracked repository can read and write files in another tracked repository by
  absolute path. Workspace and project scoping narrow *graph queries*; they are
  a relevance and context boundary, not a filesystem one. Do not track a
  repository you would not let every agent session on this machine read and
  modify.
- Because the tracked-root set defines that boundary, the tools that extend it
  (`track_repository` / `workspace_admin(operation:"track")`) are part of the
  boundary. Tracking a new root widens file access for every tool and persists
  to your global config; the `force` option, which would allow tracking `/` or
  your home directory, is refused for agent sessions and available only from
  the CLI you run yourself.

### The daemon socket is the trust boundary

The daemon runs as **you**, never with elevated privileges, and its unix socket
is created `0600` inside a `0700` directory. It performs no per-connection
authorization beyond those filesystem permissions: any process that can open
the socket has the daemon's full authority over every tracked repository —
reads, writes, and the control surface (track / untrack / shutdown).

This means Gortex never grants access to a directory you could not already read
yourself. It also means a second local account cannot reach your daemon. What it
does *not* mean is that separate sessions are isolated from one another; they are
not, by design.

Optional HTTP surfaces (`gortex daemon start --http-addr`, `gortex mcp
--server`, `gortex eval-server`) change this: a loopback TCP port is reachable
by **every local process and by any page the user's browser loads**, which the
unix socket's permissions do not cover. `/mcp` therefore refuses cross-origin
browser requests unless you name the origin with `--http-allowed-origin`, and a
non-loopback bind requires an auth token. Prefer the unix socket unless you
specifically need HTTP.

### Network access

- **No telemetry.** Gortex sends no usage data, analytics, or crash reports, and
  performs no update or "phone-home" checks. With no LLM provider, federation, or
  forge tooling configured, Gortex makes **no outbound network requests.**
- Outbound network access happens only through these **opt-in** features:
  - **LLM providers** (`llm.provider`): the `ask` agent and `search_symbols`
    assist modes can call an LLM. The default provider is `local` (in-process,
    no network). When configured for a hosted provider (Anthropic, OpenAI, Azure
    OpenAI, Google Gemini, AWS Bedrock, DeepSeek, or a remote Ollama) or a
    subprocess CLI provider (Claude, Codex, Copilot, Cursor, opencode), prompts
    **derived from your source code** are sent to that endpoint or third-party
    tool. No provider is configured by default, and `ask` / assist stay disabled
    when none is available.
  - **Federation** (`.gortex.yaml` `federation:` / `gortex proxy`): fans
    **read-only** graph queries out to other Gortex daemons you configure. It is
    off unless configured and read-only by default; the `federation.edges`
    cross-daemon edge feature (which fetches remote subgraphs) is off by default.
  - **PR / review tooling** (`gortex prs`, `gortex review --post`, and the
    matching MCP tools): call the GitHub API / the `gh` CLI when you invoke them.
- **Inbound HTTP.** `gortex server` mounts a Streamable-HTTP MCP endpoint at
  `POST /mcp`; the daemon exposes it only when started with `--http-addr`. The
  listener binds to **localhost by default**; binding to a non-localhost address
  requires an authentication token (`--http-auth-token`). The default stdio
  transport communicates only with the parent process.

### Process execution

- Gortex executes external programs only for features you opt into:
  - **Git**, for history-derived features (blame, churn, co-change, diff review).
  - **Language servers** (e.g. `tsserver`), for cross-file resolution and LSP
    code actions, when an LSP is configured and available.
  - **Subprocess LLM providers** and **forge tools** (`claude`, `codex`,
    `copilot`, `cursor-agent`, `opencode`, `gh`), when configured.
- These run with your privileges and may make their own network calls; they are
  invoked only when the corresponding feature is configured or requested.

### Data at rest

- The graph, along with session notes and development memories, is persisted
  locally under `~/.gortex` (and per-repo `.gortex/`). Notes and memories may
  contain excerpts of your source. Nothing is transmitted off the machine except
  through the opt-in network features above.

### Build / supply chain

- **CGO.** Tree-sitter grammars are compiled via CGO from
  `github.com/alexaandru/go-sitter-forest`. The optional in-process LLM (the
  `local` provider) is compiled only with the `llama` build tag.
- SQLite persistence uses the pure-Go `modernc.org/sqlite` driver (no CGO).

## Hardening checklist

The following configuration choices increase Gortex's exposure; review them for
your environment:

- Configuring a **hosted or subprocess LLM provider** sends code-derived prompts
  off the machine.
- Enabling **federation / proxy** sends graph queries to the remote daemons you
  configure.
- Binding the HTTP endpoint to a **non-localhost address** exposes the MCP
  surface to the network — always set `--http-auth-token`, and prefer a
  localhost bind or an SSH tunnel.
- Driving the MCP tools with an agent that ingests **untrusted repository
  content** widens the prompt-injection surface; keep Gortex pointed at
  repositories you trust.
