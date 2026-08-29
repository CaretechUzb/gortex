package main

import (
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
)

// runtimeStateMarker publishes the daemon's in-flight derive and enrichment
// work into its runtime-state file.
//
// That file is the only channel available. `gortex repos` deliberately issues
// no control request — it reads the store directly — precisely because the
// socket is unavailable when the daemon is busy, which is exactly when a
// derive is in flight and readiness matters most. Without these markers a
// whole-workspace derive would make every repo report itself underived for the
// tens of minutes it runs, which is the false alarm that teaches a reader to
// stop trusting the column.
type runtimeStateMarker struct {
	logger *zap.Logger
}

func (m runtimeStateMarker) DeriveBegan(scope []string, configHash string) {
	m.update(func(st *daemon.RuntimeState) {
		st.DerivingSince = time.Now().Unix()
		st.DerivingScope = scope
		// Refreshed on every run, not written once at startup: tracking or
		// untracking a repository changes the workspace-wide allow-list union,
		// and a reader comparing against a stale hash would report a config
		// drift that no longer exists.
		st.DeriveConfigHash = configHash
	})
}

// DeriveEnded clears the in-flight fields and deliberately leaves
// DeriveConfigHash alone: it describes the configuration, not the run, and is
// what a reader compares a stamped completion against long after the run ends.
func (m runtimeStateMarker) DeriveEnded() {
	m.update(func(st *daemon.RuntimeState) {
		st.DerivingSince = 0
		st.DerivingScope = nil
	})
}

func (m runtimeStateMarker) EnrichingChanged(repos []string) {
	m.update(func(st *daemon.RuntimeState) { st.EnrichingRepos = repos })
}

// update is advisory, exactly like the initial record: a daemon that cannot
// write its state still serves. A lost marker costs a repo a better label, not
// a correct one — it falls back to the verdict the store alone supports.
func (m runtimeStateMarker) update(mutate func(*daemon.RuntimeState)) {
	if err := daemon.UpdateRuntimeState(mutate); err != nil && m.logger != nil {
		m.logger.Debug("daemon: could not publish runtime marker", zap.Error(err))
	}
}
