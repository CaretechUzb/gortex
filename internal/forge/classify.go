package forge

import (
	"strings"
	"time"
)

// staleAfterDays is the age past which an open PR is classified STALE
// (when nothing more pressing applies).
const staleAfterDays = 30

// Status is the pure-Go classification of a PR against a default base.
// It carries no network dependency and is table-testable.
type Status struct {
	State        string
	BaseMismatch bool
	Draft        bool
	AgeDays      int
	Blockers     []string
}

// ParseBases splits an accepted-base specification into branch names.
//
// A repository often has more than one legitimate long-lived target: a release
// line and a redesign branch, say. Flagging every PR that does not target the
// single "default" branch as BASE_MISMATCH makes the state useless on such a
// repo — whichever branch you pick, the other group is mislabelled. Callers
// therefore accept a comma-separated list wherever a single base used to go.
//
// Separators are commas; surrounding whitespace is trimmed and empty entries
// dropped. Branch names are compared exactly, as git treats them.
func ParseBases(spec string) []string {
	parts := strings.Split(spec, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ClassifyStatus reduces a PR to a single review-state label against the
// repo's default base. defaultBase may name several accepted bases,
// comma-separated (see ParseBases).
func ClassifyStatus(pr PR, defaultBase string) Status {
	return ClassifyStatusAgainst(pr, ParseBases(defaultBase))
}

// ClassifyStatusAgainst reduces a PR to a single review-state label, computed
// purely from its already-fetched fields against the set of accepted bases.
// State precedence: DRAFT → BASE_MISMATCH → CHANGES_REQUESTED → APPROVED
// → STALE → READY. Blockers accumulates every condition that would hold
// a merge.
//
// An empty bases set disables the base check rather than flagging everything:
// "we do not know the accepted bases" must not read as "every PR targets the
// wrong branch".
func ClassifyStatusAgainst(pr PR, bases []string) Status {
	s := Status{
		Draft:   pr.IsDraft,
		AgeDays: ageDays(pr.UpdatedAt),
	}
	if len(bases) > 0 && pr.BaseRef != "" && !matchesBase(pr.BaseRef, bases) {
		s.BaseMismatch = true
	}

	if pr.IsDraft {
		s.Blockers = append(s.Blockers, "draft")
	}
	if s.BaseMismatch {
		s.Blockers = append(s.Blockers, "base-mismatch")
	}
	if pr.ReviewDecision == "CHANGES_REQUESTED" {
		s.Blockers = append(s.Blockers, "changes-requested")
	}
	if RollupCI(pr) == "FAILURE" {
		s.Blockers = append(s.Blockers, "ci-failure")
	}

	switch {
	case pr.IsDraft:
		s.State = "DRAFT"
	case s.BaseMismatch:
		s.State = "BASE_MISMATCH"
	case pr.ReviewDecision == "CHANGES_REQUESTED":
		s.State = "CHANGES_REQUESTED"
	case pr.ReviewDecision == "APPROVED":
		s.State = "APPROVED"
	case s.AgeDays >= staleAfterDays:
		s.State = "STALE"
	default:
		s.State = "READY"
	}
	return s
}

// matchesBase reports whether ref is one of the accepted bases. Git branch
// names are case-sensitive, so the comparison is exact.
func matchesBase(ref string, bases []string) bool {
	for _, b := range bases {
		if ref == b {
			return true
		}
	}
	return false
}

// RollupCI echoes a PR's reconstructed CI rollup, normalized to one of
// NONE / FAILURE / PENDING / SUCCESS. An empty rollup reads as NONE.
func RollupCI(pr PR) string {
	switch pr.CIRollup {
	case "FAILURE", "PENDING", "SUCCESS":
		return pr.CIRollup
	default:
		return "NONE"
	}
}

// ageDays returns the whole-day age of t relative to now, clamped at 0
// for a zero or future timestamp.
func ageDays(t time.Time) int {
	if t.IsZero() {
		return 0
	}
	d := time.Since(t)
	if d < 0 {
		return 0
	}
	return int(d.Hours() / 24)
}
