# Daemon-mode MCP-tool latency

Per-tool p50 / p95 / p99 latency for the production MCP dispatch
path. Builds an in-process MCP server against a target corpus,
fires N `Handler.CallToolStrict` invocations per tool, aggregates
latencies into a published table.

## What it measures

- **Handler-end-to-end latency** for each MCP tool: JSON arg parse
  → tool dispatch → handler logic → response encode. Same code
  path the production stdio / HTTP / daemon-socket front-ends use.
- **Per-tool spread**: cheap tools (`graph_stats`, `get_callers`)
  separate from heavy ones (`smart_context`, `get_repo_outline`)
  so the published table shows realistic operating envelope.

## What it does NOT measure

- **Stdio framing** (gortex mcp's pipe overhead)
- **Daemon socket dispatch** (gortex daemon's UNIX socket / HTTP
  ingress overhead)
- **Network RTT** (if reaching the daemon remotely)

Each adds a roughly constant ~0.1-1 ms per call on a warm pipe;
the handler latency below dominates user-perceived response time.

## Running

```sh
# Default: index `.` and fire 200 iters per tool
go run ./bench/daemon-latency

# Higher iter count for tighter percentiles
go run ./bench/daemon-latency -iter 500

# Specific subset of tools (useful for tuning one signal)
go run ./bench/daemon-latency -tools graph_stats,search_symbols

# CSV / JSON outputs for downstream tooling
go run ./bench/daemon-latency -csv bench/results/dl.csv -json bench/results/dl.json
```

Flags:

- `-repo PATH` — corpus to index (default `.`)
- `-iter N` — iterations per tool (default 200; warm-up of N/10
  is added on top)
- `-tools LIST` — comma-separated subset
- `-out PATH` — primary output (default stdout)
- `-csv PATH` / `-json PATH` — companion outputs
- `-format markdown|csv|json` — primary format

Or via the CLI surface:

```sh
gortex bench daemon-latency --out-dir bench/results
```

## Tools benchmarked

| tool | shape |
|------|-------|
| `graph_stats` | no-arg snapshot; cheap |
| `search_symbols` | 1 query arg; rotated through 10 fixtures so a per-query cache doesn't trivially hit |
| `get_symbol_source` | 1 id arg; pinned to a sampled function from the indexed graph |
| `get_callers` | 1 id arg + limit |
| `find_usages` | 1 id arg |
| `get_file_summary` | 1 path arg; pinned to a sampled file |
| `smart_context` | 1 task arg; expensive, fewer iters per cycle |
| `get_repo_outline` | no-arg; walks whole graph |

Sampled targets are picked once at start so each tool sees the
same target across iterations — the per-call latency reflects
handler arithmetic, not target lookup.

## Methodology

- Warm-up of `iter/10` (min 5) per tool primes any lazy
  initialisation in the handler / graph before the measured loop
  starts.
- Per-iteration latency captured via `time.Since(start)` with
  μs precision.
- Percentiles computed via the nearest-rank method:
  `idx = (pct × n) / 100`. For N=200 → p95=sorted[190].
- Errors are counted in `error_rate` but their latencies are
  still measured (an error path that takes 3× the happy-path
  time is itself a signal).

## Honest caveats

- Numbers are operator-machine-specific. Absolute values vary 2-5×
  across hardware classes; the **relative spread** between tools
  (cheap vs heavy) is what publishes reproducibly.
- Cold-cache effects show up most in `search_symbols` (BM25
  re-ranks under load) and `smart_context` (assembles fresh
  context each call). Warm-up reduces but doesn't eliminate them.
- The smoke run below was measured on the retired in-memory backend;
  the harness now builds a SQLite store, so the numbers are pending
  re-measurement against it.
- Smoke run on the gortex repo (71k nodes, Apple M3 Max):
  - `graph_stats` p50 4.2ms · p95 5.5ms
  - `search_symbols` p50 1.2ms · p95 22.4ms
  - `get_symbol_source` p50 0.19ms · p95 0.9ms
  - `get_callers` / `find_usages` p50 < 0.02ms (graph lookup)
  - `smart_context` p50 1.5ms · p95 24ms
  - `get_repo_outline` p50 60ms · p95 217ms

  Median p95 across tools: 5.5 ms. Median p99: 5.9 ms.
