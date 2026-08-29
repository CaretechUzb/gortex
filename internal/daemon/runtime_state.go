package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/zzet/gortex/internal/platform"
)

// RuntimeState is what a running daemon records about the choices it resolved
// at startup, for the out-of-band CLI commands that have to read the same
// files it does. The PID file says only that a daemon exists; a command like
// `gortex repos` reads the daemon's store directly and needs to know WHICH
// store, which is otherwise knowable only from the argv of a process it cannot
// see. Everything here is a resolved absolute value, never a flag as typed.
type RuntimeState struct {
	// PID is the daemon process that wrote this file. Readers use it to tell
	// a live record from one a killed daemon left behind.
	PID int `json:"pid"`
	// BackendPath is the resolved graph store file the daemon opened —
	// --backend-path expanded to an absolute path, or the platform default
	// when the flag was not given.
	BackendPath string `json:"backend_path"`

	// DerivingSince and DerivingScope name a derived-pass run in flight, so a
	// reader can say "deriving…" for a repo whose stamp has not landed yet
	// instead of "never derived". A whole-workspace derive takes tens of
	// minutes on a large workspace, and reporting every repo as underived for
	// its duration would train the reader to ignore the column.
	//
	// DerivingScope is the covered set the passes reported, NOT "all repos".
	// A scoped post-track derive covers only the newly-tracked sibling
	// checkouts; marking the other six repos as deriving would hide their real
	// verdict behind a run that has nothing to do with them.
	DerivingSince int64    `json:"deriving_since,omitempty"`
	DerivingScope []string `json:"deriving_scope,omitempty"`

	// EnrichingRepos names the repos with a semantic-enrichment pass running.
	// Symmetric with the derive markers, and needed for the same reason:
	// enrichment counts toward readiness and is long-running.
	EnrichingRepos []string `json:"enriching_repos,omitempty"`
}

// IsDeriving reports whether a derived-pass run currently covers repoPrefix.
// An in-flight run with no recorded scope is a whole-workspace one and covers
// everything; that is the historical unscoped shape, not a missing value.
func (st RuntimeState) IsDeriving(repoPrefix string) bool {
	if st.DerivingSince == 0 {
		return false
	}
	if len(st.DerivingScope) == 0 {
		return true
	}
	return containsPrefix(st.DerivingScope, repoPrefix)
}

// IsEnriching reports whether a semantic-enrichment pass is running for
// repoPrefix. Unlike a derive, an absent list means nothing is enriching —
// enrichment is always per-repo, so it has no whole-workspace form.
func (st RuntimeState) IsEnriching(repoPrefix string) bool {
	return containsPrefix(st.EnrichingRepos, repoPrefix)
}

func containsPrefix(list []string, want string) bool {
	for _, p := range list {
		if p == want {
			return true
		}
	}
	return false
}

// RuntimeStatePath returns the file the daemon records its resolved runtime
// state in. It sits beside the PID file so the two share a lifetime, and is
// overridable for tests and custom deployments.
func RuntimeStatePath() string {
	if override := os.Getenv("GORTEX_DAEMON_STATEFILE"); override != "" {
		return override
	}
	if dir, ok := stateDir(); ok {
		return filepath.Join(dir, "daemon.state.json")
	}
	return filepath.Join(os.TempDir(), "gortex-daemon.state.json")
}

// WriteRuntimeState records st for the lifetime of this daemon. The PID is
// stamped from the calling process, so a caller cannot publish a record that
// claims to belong to some other daemon.
func WriteRuntimeState(st RuntimeState) error {
	st.PID = os.Getpid()
	path := RuntimeStatePath()
	if err := EnsureParentDir(path); err != nil {
		return err
	}
	blob, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(path, blob, 0o600)
}

// UpdateRuntimeState rewrites the running daemon's record through mutate,
// preserving every field mutate does not touch. It is how the in-flight derive
// and enrichment markers reach a reader that cannot see this process.
//
// This is what makes the file mutable mid-run rather than write-once at daemon
// start, and it is the accepted cost of the markers: `gortex repos` reads the
// store directly precisely because the control socket is unavailable when the
// daemon is busy, which is exactly when a derive is in flight and readiness
// matters most. A lost update is cosmetic and self-correcting — the record is
// discarded wholesale when the PID dies, and the next transition rewrites it.
//
// Serialised process-wide: derive and enrichment transitions arrive from
// different goroutines and would otherwise interleave read-modify-write.
func UpdateRuntimeState(mutate func(*RuntimeState)) error {
	if mutate == nil {
		return nil
	}
	runtimeStateMu.Lock()
	defer runtimeStateMu.Unlock()
	st, _ := readRuntimeStateFile()
	mutate(&st)
	return WriteRuntimeState(st)
}

var runtimeStateMu sync.Mutex

// RemoveRuntimeState deletes the record. Called on the shutdown path alongside
// the PID file; an already-absent file is not an error.
func RemoveRuntimeState() {
	_ = os.Remove(RuntimeStatePath())
}

// ReadRuntimeState returns the running daemon's recorded state. It reports
// false when there is no record, when the record cannot be parsed, or when the
// process that wrote it is gone — a record a killed daemon left behind names a
// store nothing is using, and routing a reader there is worse than falling
// back to the default.
func ReadRuntimeState() (RuntimeState, bool) {
	st, ok := readRuntimeStateFile()
	if !ok {
		return RuntimeState{}, false
	}
	// The liveness gate is also what makes the in-flight markers crash-safe: a
	// daemon killed mid-derive leaves DerivingSince set forever, and discarding
	// the whole record here is what stops a reader believing it.
	if st.PID <= 0 || !platform.ProcessAlive(st.PID) {
		return RuntimeState{}, false
	}
	return st, true
}

// readRuntimeStateFile parses the record without the liveness gate. Only the
// read-modify-write path uses it: a daemon updating its own record must not
// have to prove to itself that it is alive.
func readRuntimeStateFile() (RuntimeState, bool) {
	var st RuntimeState
	blob, err := os.ReadFile(RuntimeStatePath())
	if err != nil {
		return RuntimeState{}, false
	}
	if err := json.Unmarshal(blob, &st); err != nil {
		return RuntimeState{}, false
	}
	return st, true
}
