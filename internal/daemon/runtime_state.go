package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

// RuntimeState is what a running daemon records about the choices it resolved
// at startup, for the out-of-band CLI commands that have to read the same
// files it does. The PID file says only that a daemon exists; a command like
// `gortex repos` reads the daemon's store directly and needs to know WHICH
// store, which is otherwise knowable only from the argv of a process it cannot
// see. Everything here is a resolved absolute value, never a flag as typed.
// StartupPhase is the daemon's pre-socket lifecycle state. The real socket is
// still the authoritative ready signal; these phases only explain why a live
// child has not opened it yet.
type StartupPhase string

const (
	StartupOpeningStore StartupPhase = "opening_store"
	StartupMigrating    StartupPhase = "migrating"
	StartupServing      StartupPhase = "serving"
	StartupFailed       StartupPhase = "failed"
)

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

	// DerivePending names the repos a derived-pass run is OWED but has not
	// opened yet. DerivingSince covers the run itself; this covers everything
	// before it, and that gap is not small: the scheduler debounces for two
	// seconds, republishes the checkout grouping, runs a whole-workspace
	// cross-repo resolve and then waits on the batch-mutation gate, all before
	// the passes call DeriveBegan. A repo tracked into that window has no
	// derive_state row yet and no in-flight marker either, so it reads "never
	// derived" — the one verdict that tells a reader the graph is silently
	// wrong — while `gortex daemon status`, which asks the scheduler directly,
	// says a derive is pending for it. Two surfaces disagreeing about repo
	// health is worse than one of them being incomplete.
	//
	// Only the owed repos are listed, never the whole workspace. The set a
	// pending pass will actually cover is not decided until it starts
	// (rederiveScope reads a checkout grouping that is republished on the way
	// in), and the repos this exists to rescue are precisely the newly tracked
	// ones. Marking the others would hide their real verdict behind a run that
	// has nothing to say about them.
	DerivePending []string `json:"derive_pending,omitempty"`

	// EnrichingRepos names the repos with a semantic-enrichment pass running.
	// Symmetric with the derive markers, and needed for the same reason:
	// enrichment counts toward readiness and is long-running.
	EnrichingRepos []string `json:"enriching_repos,omitempty"`

	// DeriveConfigHash is what this daemon's derive-relevant configuration
	// hashes to right now — the framework allow-list and the external-call
	// synthesis flags, which change derived output with no code change.
	//
	// Published here rather than recomputed by the reader because resolving it
	// means merging the global config with every repo's own .gortex.yaml, and a
	// second implementation of that merge would drift. A reader with no live
	// daemon sees an empty string and skips the comparison instead of guessing.
	DeriveConfigHash string `json:"derive_config_hash,omitempty"`

	// Startup fields are optional for backward compatibility with runtime
	// records written by older binaries.
	StartupPhase     StartupPhase `json:"startup_phase,omitempty"`
	StartupStartedAt int64        `json:"startup_started_at,omitempty"`
	StartupUpdatedAt int64        `json:"startup_updated_at,omitempty"`
	MigrationVersion int          `json:"migration_version,omitempty"`
	MigrationName    string       `json:"migration_name,omitempty"`
	StartupError     string       `json:"startup_error,omitempty"`
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

// IsDerivePending reports whether a derived-pass run is owed to repoPrefix but
// has not opened yet. Unlike IsDeriving, an empty list means nothing is owed:
// the set is always explicit here, because a pending pass has not yet decided
// what it will cover and an absent value must not be read as "everything".
func (st RuntimeState) IsDerivePending(repoPrefix string) bool {
	return containsPrefix(st.DerivePending, repoPrefix)
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

// StartupProgressFresh reports whether st is a live pre-socket progress record
// updated recently enough for a supervising CLI to keep waiting.
func (st RuntimeState) StartupProgressFresh(now time.Time, maxAge time.Duration) bool {
	if st.StartupPhase != StartupOpeningStore && st.StartupPhase != StartupMigrating {
		return false
	}
	if st.StartupUpdatedAt <= 0 || maxAge <= 0 {
		return false
	}
	updated := time.UnixMilli(st.StartupUpdatedAt)
	return !updated.After(now.Add(maxAge)) && now.Sub(updated) <= maxAge
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
	st.StartupUpdatedAt = time.Now().UnixMilli()
	path := RuntimeStatePath()
	if err := EnsureParentDir(path); err != nil {
		return err
	}
	blob, err := json.Marshal(st)
	if err != nil {
		return err
	}
	// Heartbeats and migration callbacks can update this file while the
	// detached parent reads it. Publish by same-directory atomic replacement so
	// a reader never observes a truncated JSON document.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".daemon-state-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(blob); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
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
