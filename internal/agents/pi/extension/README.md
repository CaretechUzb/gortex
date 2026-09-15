# Pi extension suite

Behavioural coverage for `index.ts` — the readiness barrier, tool-name
aliasing, and the per-invocation resolver.

```sh
node --test                                   # from this directory
go test ./internal/agents/pi/                 # same suite, via the Go wrapper
GORTEX_PI_INDEX_TS=/path/to/other/index.ts node --test
```

Node 24 or newer: the suite imports `index.ts` directly under type stripping,
so there is no build step, no dependencies, and no `package.json`.
`TestPiExtensionHarness` skips when the toolchain is missing;
`GORTEX_REQUIRE_NODE=1` makes that a failure instead, which is what CI sets.

`go:embed extension/index.ts` names a single file, so nothing here ships to a
user's `.pi/extensions/gortex/`.

## What it rests on

`testsupport/pi-stub.mjs`'s `emit()` runs listeners sequentially and awaits
each one, mirroring `ExtensionRunner.emit` (`runner.js:623`) and
`emitBeforeAgentStart` (`runner.js:881`) in Pi 0.85.1. **The barrier is a no-op
if that stops being true.** Pi is not vendored here, so this stub's fidelity is
the only thing in the tree that would notice.

Two further fidelity notes: `registerTool` is a `Map.set` that throws only
after `invalidate()` (`loader.js:238`), which is the case `safeRegister`'s
`catch` actually covers; and the child-process mock answers `initialize`,
`tools/list` and `tools/call` without modelling frame splitting, the 64 MiB
frame cap, or `notifications/tools/list_changed` re-sync — `tools_search`
promotion is not covered.

## Drift fences

`testsupport/subject.mjs` builds the subject from the live source, and fences
every assumption it makes about that source so drift fails the suite loudly.
Each installer sentinel asserts it matched, and a final sweep rejects any
`{{...}}` left behind — so a sentinel removed from `index.ts` and one newly
added to `adapter.go` both fail here.
The `node:child_process` seam and the `READY_WAIT_MS` declaration are fenced
the same way.
