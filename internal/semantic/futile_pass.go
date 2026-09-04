package semantic

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// A pass that cannot finish, and how not to run it twice.
//
// Some provider/repo pairs exhaust their whole budget and land nothing.
// Measured on the `addons` repository (106,431 nodes), python-types:
//
//	2026-09-01 23:44  1,495s pass, 1,217s of it lock wait, partial, coverage 0
//	2026-09-02 18:04  1,495s pass,   407s of it lock wait, partial, coverage 0
//
// Both times the apply itself ran ~1,088s and produced nothing, and both times
// it held the store-wide resolve mutex for the duration. That is a fixed cost
// paid for a result that is known in advance, and it is charged to every OTHER
// pass in the process rather than to the one making it.
//
// So the outcome is remembered. A pass that was futile at a given revision is
// futile at that revision again: the content is identical and so is the budget.
//
// # Scope, and why it is not durable
//
// This record lives for one daemon life. It is not persisted, and the choice is
// forced rather than preferred:
//
//   - enrichment_state has no column for an ATTEMPT. Its rows record
//     completions, and the schema is pinned in lockstep with upstream, so a
//     column cannot be added here. Writing a futile attempt into the existing
//     columns is worse than not recording it: RefreshEnrichmentProviders keys
//     on indexed_sha <> '' and would launder the row into "a pass really
//     finished", destroying the EnrichmentNeverRan re-arm that is the whole
//     reason a repo like this ever gets another chance.
//   - daemon.state.json is written fresh at startup and deleted at shutdown by
//     design — it exists to be discarded when the PID dies, which is exactly
//     the property a cross-restart ledger must not have.
//
// Within one daemon life the record still bites: the deferred-enrichment pool
// runs on warmup, after a copy-track, and from the janitor sweep, and a repo
// touched by two of those used to pay the futile pass twice. Across restarts it
// does not, and the resolve-mutex yield in the tstypes apply is what keeps that
// from mattering — the pass still burns its budget, but no longer blocks
// anything else while it does. See TODOS.md for the durable-ledger follow-up.

// futilePass is one recorded futile outcome.
type futilePass struct {
	// Budget is what the pass was given, so the log can say what was spent.
	Budget time.Duration
	// At is when the outcome was recorded.
	At time.Time
}

// futilePassKey identifies a (repo, provider, revision) attempt. The revision is
// part of the key: new content deserves a new attempt.
func futilePassKey(repoPrefix, provider, sha string) string {
	return repoPrefix + "\x00" + provider + "\x00" + sha
}

// recordFutilePass remembers that this provider spent its whole budget on this
// repo at this revision and covered nothing.
//
// Only for a pass bounded by its BUDGET. A pass cut short by daemon shutdown or
// by a cancelled parent has not shown that the work is impossible, only that it
// was interrupted, and recording it would suppress a pass that might well have
// succeeded. An empty sha records nothing at all: without a revision to key on,
// "the same attempt" is not a statement that can be made.
func (m *Manager) recordFutilePass(repoPrefix, provider, sha string, budget time.Duration) {
	if m == nil || sha == "" || provider == "" {
		return
	}
	m.mu.Lock()
	if m.futilePasses == nil {
		m.futilePasses = make(map[string]futilePass)
	}
	m.futilePasses[futilePassKey(repoPrefix, provider, sha)] = futilePass{
		Budget: budget,
		At:     time.Now(),
	}
	m.mu.Unlock()
}

// futilePassRecorded reports a previously recorded futile outcome for this
// exact (repo, provider, revision).
func (m *Manager) futilePassRecorded(repoPrefix, provider, sha string) (futilePass, bool) {
	if m == nil || sha == "" || provider == "" {
		return futilePass{}, false
	}
	m.mu.RLock()
	rec, ok := m.futilePasses[futilePassKey(repoPrefix, provider, sha)]
	m.mu.RUnlock()
	return rec, ok
}

// enrichResultFutile classifies a finished pass as futile: it ran out its own
// deadline and landed nothing at all.
//
// Every clause is load-bearing.
//
// Cut by its DEADLINE, not by an interruption. BoundReason is not the test —
// enrichBoundReason maps every partial to EnrichBoundBudget, cancellations
// included — so the abort reason is read directly. A pass stopped by a closing
// manager or a cancelled parent says nothing about whether the work is
// possible, and suppressing it would suppress a pass that might well succeed.
//
// It had work and did none of it. A partial pass WITH coverage, or with edges
// landed, made progress and deserves another run to make more; only a pass that
// moved nothing at all is known to be unable to.
func enrichResultFutile(res *EnrichResult) bool {
	if res == nil || !res.Partial {
		return false
	}
	if res.AbortReason != context.DeadlineExceeded.Error() {
		return false
	}
	return res.SymbolsTotal > 0 && res.SymbolsCovered == 0 &&
		res.EdgesAdded == 0 && res.EdgesConfirmed == 0 && res.EdgesRebound == 0
}

// logFutileSkip explains a skip once per occurrence. Deliberately a warning:
// the repo is being left less enriched than it should be, and the operator has
// to be able to find out why without reading source.
func (m *Manager) logFutileSkip(repoName, provider, lang, sha string, rec futilePass) {
	if m == nil || m.logger == nil {
		return
	}
	m.logger.Warn("semantic enrichment skipped: previous pass exhausted its budget with zero coverage",
		zap.String("provider", provider),
		zap.String("language", lang),
		zap.String("repo", repoName),
		zap.String("sha", shortSHA(sha)),
		zap.Duration("previous_budget", rec.Budget),
		zap.String("remedy", "raise the per-repo enrichment budget, or exclude this language for this repo"),
	)
}
