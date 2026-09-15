// Overlapping session_start invocations on ONE extension instance: the turn
// parked on the newest invocation must not be released by an older one
// finishing. This is the review finding on PR #787 — a single mutable
// `settleSessionReady` slot, reassigned by each arm, settles whichever promise
// is current when the OLD handler's finally runs.
//
// Not reachable through Pi 0.85.1's own modes (every path that emits
// session_start constructs a fresh instance first), so it drives the closure
// directly: bindExtensions() emits with no idempotence guard, and
// core/sdk.js:69 lets an embedder share one resourceLoader across sessions.

import { describe, it, before } from "node:test";
import assert from "node:assert/strict";

import { buildSubject, loadFactory } from "./testsupport/subject.mjs";
import { control, resetInitializeAttempts } from "./testsupport/child-process-mock.mjs";
import { createPi } from "./testsupport/pi-stub.mjs";
import { ORIENTATION, TOOLS, freshSession, firstTurn } from "./testsupport/fixtures.mjs";

// C is slow enough that A's post-backoff retry (~1s: start() backs off 1s and
// retries once superseded) settles with a wide margin on either side of the
// assertions below. Both bounds sit ~1s away from the value they fence.
const SLOW_HANDSHAKE_MS = 2500;
const SETTLED_BY_MS = 2000;

describe("a stale session_start settling mid-turn", () => {
  let pi, aFinished, releasedWhenAFinished, released;

  before(async () => {
    const subject = await buildSubject();

    resetInitializeAttempts();
    control.reset({
      handshakeDelayMs: 0,
      toolsListDelayMs: 0,
      tools: TOOLS,
      hookDecision: { orientation: ORIENTATION },
    });

    const factory = await loadFactory(subject, 0);
    pi = createPi();
    factory(pi);

    // A: superseded by B, so its start() rejects, backs off 1s and retries —
    // it settles roughly a second from now, long after B is done.
    const a = freshSession(pi, "startup");

    // B: fast, and overlaps A.
    await freshSession(pi, "new");

    // C: starts after B settled and is still running when A finally finishes.
    // The child reads the latency when the extension writes `initialize`, so
    // dropping the global back to 0 immediately afterwards leaves C slow while
    // letting A's post-backoff retry complete quickly.
    control.handshakeDelayMs = SLOW_HANDSHAKE_MS;
    const c = freshSession(pi, "reload");
    control.handshakeDelayMs = 0;

    const t0 = Date.now();
    released = 0;
    const turn = firstTurn(pi).then(() => {
      released = Date.now() - t0;
    });

    await a;
    aFinished = Date.now() - t0;
    releasedWhenAFinished = released;

    await turn;
    await c;
  });

  it("settles the stale invocation first", () => {
    // Precondition: without this the rest proves nothing.
    assert.ok(aFinished < SETTLED_BY_MS, `A settled at ${aFinished}ms`);
  });

  it("does not release the parked turn when the stale invocation settles", () => {
    assert.equal(releasedWhenAFinished, 0, `released at ${releasedWhenAFinished}ms, when A settled at ${aFinished}ms`);
  });

  it("holds the turn until the invocation it parked on finishes", () => {
    assert.ok(released >= SETTLED_BY_MS, `released at ${released}ms`);
  });

  it("leaves the newest session's tools live", () => {
    assert.equal(pi.registered.size, 3);
  });
});
