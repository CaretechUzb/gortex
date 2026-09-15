// Shared fixtures for the extension suites. The per-suite choreography stays
// in the test files: for the barrier, when each event fires relative to the
// others IS the thing under test.

export const ORIENTATION = "GORTEX-ORIENTATION: prefer graph tools over native reads.";

// `read` and `edit` collide with Pi's builtins; `search` does not.
export const TOOLS = [
  { name: "read", description: "Read indexed source.", inputSchema: { type: "object", properties: {} } },
  { name: "edit", description: "Apply guarded edits.", inputSchema: { type: "object", properties: {} } },
  { name: "search", description: "Search the graph.", inputSchema: { type: "object", properties: {} } },
];

export const delay = (ms) => new Promise((r) => setTimeout(r, ms));

export function freshSession(pi, reason = "startup") {
  return pi.emit("session_start", { type: "session_start", reason });
}

export function firstTurn(pi) {
  return pi.emit("before_agent_start", { type: "before_agent_start", prompt: "go" });
}
