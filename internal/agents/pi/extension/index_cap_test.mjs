// READY_WAIT_MS expiry: the barrier gives up once the cap passes, and the
// turn it lets through says so.
//
// Separate file because it needs its own subject — READY_WAIT_MS is rewritten
// at build time, so this suite cannot share the module the others use.

import { describe, it, before } from "node:test";
import assert from "node:assert/strict";

import { buildSubject, loadFactory } from "./testsupport/subject.mjs";
import { control, resetInitializeAttempts } from "./testsupport/child-process-mock.mjs";
import { createPi } from "./testsupport/pi-stub.mjs";
import { ORIENTATION, TOOLS, freshSession, firstTurn } from "./testsupport/fixtures.mjs";

// A 20ms cap against a 1000ms registration. The gap is wide on purpose: the
// bounded-wait assertion is the suite's only upper bound, so it needs room to
// stay honest on a loaded CI runner without ever passing on an unbounded wait.
const CAP_MS = 20;
const HANDSHAKE_MS = 500;
const TOOLS_LIST_MS = 500;

describe("the cap expires before registration finishes", () => {
  let pi, held, toolsAtRelease, turn1, laterTurn;

  before(async () => {
    const subject = await buildSubject({ readyWaitMs: CAP_MS });

    resetInitializeAttempts();
    control.reset({
      handshakeDelayMs: HANDSHAKE_MS,
      toolsListDelayMs: TOOLS_LIST_MS,
      tools: TOOLS,
      hookDecision: { orientation: ORIENTATION },
    });

    const factory = await loadFactory(subject, 0);
    pi = createPi();
    factory(pi);

    const started = freshSession(pi);

    const t0 = Date.now();
    await firstTurn(pi);
    held = Date.now() - t0;
    toolsAtRelease = pi.registered.size;

    turn1 = await pi.pushContext();

    await started;
    laterTurn = await pi.pushContext();
  });

  it("bounds the wait by the cap", () => {
    assert.ok(
      held < 400,
      `held ${held}ms against a ${CAP_MS}ms cap and a ${HANDSHAKE_MS + TOOLS_LIST_MS}ms registration`,
    );
  });

  it("releases the turn before the tools are live", () => {
    assert.equal(toolsAtRelease, 0);
  });

  it("still carries the orientation", () => {
    assert.equal(turn1.length, 1);
    assert.ok(turn1[0].content.includes(ORIENTATION));
  });

  it("warns that the tools are not callable yet", () => {
    assert.match(turn1[0].content, /still registering/i);
  });

  it("names /reload as the recovery", () => {
    assert.ok(turn1[0].content.includes("/reload"));
  });

  it("lands the registration after the cap anyway", () => {
    assert.equal(pi.registered.size, 3);
  });

  it("still explains the rename on a late-registered tool", () => {
    assert.ok((pi.registered.get("gortex_read")?.description ?? "").includes("`gortex_read`"));
  });

  it("injects nothing on later turns", () => {
    assert.equal(laterTurn.length, 0);
  });
});
