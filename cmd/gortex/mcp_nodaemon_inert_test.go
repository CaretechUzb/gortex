package main

import "testing"

// TestNoDaemonFlag_Inert asserts the legacy --no-daemon flag no longer
// changes the daemon decision: with a daemon up, the decision is daemonReady
// regardless of the flag. Embedded mode is a separate user-global policy,
// never something this compatibility flag can force.
func TestNoDaemonFlag_Inert(t *testing.T) {
	t.Cleanup(restoreSeams)
	isDaemonRunning = func() bool { return true }
	spawnDaemon = func() error { t.Fatal("no spawn expected when the daemon is already up"); return nil }

	for _, noDaemon := range []bool{false, true} {
		mcpNoDaemon = noDaemon
		if got := resolveDaemonDecision(); got != daemonReady {
			t.Errorf("mcpNoDaemon=%v: decision = %v, want daemonReady (the flag must be inert)", noDaemon, got)
		}
	}
	mcpNoDaemon = false
}
