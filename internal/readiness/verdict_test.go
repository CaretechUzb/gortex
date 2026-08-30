package readiness

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

// derivedRepo is the shape of a repo that has been indexed, derived and
// enriched and has nothing outstanding — the baseline every case below varies
// one field of.
func derivedRepo() (RepoState, Inputs) {
	entry := RepoState{Path: "/tmp/r", Indexed: true}
	in := Inputs{
		DeriveTable: true,
		EnrichTable: true,
		PassVersion: indexer.DerivePassVersion,
		Repo: store_sqlite.RepoReadiness{
			ContentGen:  7,
			DeriveFound: true,
			Derive: graph.DeriveState{
				DerivedContentGen: 7,
				PassVersion:       indexer.DerivePassVersion,
			},
			EnrichProviders:     2,
			EnrichMinContentGen: 7,
			EnrichRows:          2,
		},
	}
	return entry, in
}

// The verdict ladder, one case per rung, in the order they outrank each other.
// Keeping it pure is what makes this table possible: no git, no sqlite, no
// daemon, and every state reachable from a struct literal.
func TestReadyVerdictLadder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*RepoState, *Inputs)
		want   string
	}{
		{"a settled repo", func(*RepoState, *Inputs) {}, LabelReady},

		{"a deleted checkout outranks everything — it can never be ready again",
			func(e *RepoState, _ *Inputs) { e.Missing = true }, LabelMissing},

		{"never indexed outranks a derive verdict; there is nothing to derive from",
			func(e *RepoState, _ *Inputs) { e.Indexed = false }, LabelNotIndexed},

		{"a stale index outranks a stale derive: fix the first and the second follows",
			func(e *RepoState, _ *Inputs) { e.Stale = true }, LabelStale},

		{"work in flight is named, not accused",
			func(_ *RepoState, in *Inputs) {
				in.Deriving = true
				in.Repo.DeriveFound = false
			}, LabelDeriving},

		{"work the daemon owes but has not opened is named too — the same rows " +
			"that read never-derived without the marker",
			func(_ *RepoState, in *Inputs) {
				in.DerivePending = true
				in.Repo.DeriveFound = false
			}, LabelDeriving},

		{"enrichment in flight, same",
			func(_ *RepoState, in *Inputs) {
				in.Enriching = true
				in.Repo.EnrichMinContentGen = 0
			}, LabelEnriching},

		{"a store older than this binary is unknown, not a missing derive",
			func(_ *RepoState, in *Inputs) { in.DeriveTable = false }, LabelUnknown},

		{"THE bug: a repo the derived passes never ran for",
			func(_ *RepoState, in *Inputs) { in.Repo.DeriveFound = false },
			LabelNeverDerived},

		{"a v13 legacy row asserts nothing and must not be laundered into ready",
			func(_ *RepoState, in *Inputs) { in.Repo.Derive.Legacy = true }, LabelUnknown},

		{"content moved since the derive",
			func(_ *RepoState, in *Inputs) { in.Repo.ContentGen = 8 }, LabelPartial},

		{"derived by an older synthesis version",
			func(_ *RepoState, in *Inputs) { in.Repo.Derive.PassVersion = 0 }, LabelPartial},

		{"derive-relevant config changed under it",
			func(_ *RepoState, in *Inputs) {
				in.ConfigHash = "beef"
				in.Repo.Derive.ConfigHash = "cafe"
			}, LabelPartial},

		{"one enrichment provider is behind",
			func(_ *RepoState, in *Inputs) { in.Repo.EnrichMinContentGen = 6 },
			LabelPartial},

		{"no provider applies — n/a never blocks ready",
			func(_ *RepoState, in *Inputs) {
				in.Repo.EnrichProviders = 0
				in.Repo.EnrichMinContentGen = 0
				in.Repo.EnrichNoneDeclared = true
				in.Repo.EnrichRows = 1
			}, LabelReady},

		{"no config hash published means no comparison, not a failed one",
			func(_ *RepoState, in *Inputs) {
				in.ConfigHash = ""
				in.Repo.Derive.ConfigHash = "whatever"
			}, LabelReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entry, in := derivedRepo()
			tc.mutate(&entry, &in)
			label, reason := Verdict(entry, in)
			require.Equal(t, tc.want, label)
			if label == LabelReady {
				require.Empty(t, reason, "a ready repo needs no remediation")
			} else {
				require.NotEmpty(t, reason, "every not-ready state must say what to do")
			}
		})
	}
}

// Running and owed share a label — a reader needs "the daemon is on it", and a
// second label would ripple through readyCell, the summary buckets and the
// JSON contract for no gain. They must not share a reason: the two differ by
// minutes of cross-repo resolve, and "running now" against a pass that has not
// opened is the kind of small lie that makes someone stop believing the column.
func TestAnOwedDeriveIsNamedAsQueuedNotAsRunning(t *testing.T) {
	t.Parallel()
	entry, in := derivedRepo()
	in.Repo.DeriveFound = false

	in.Deriving, in.DerivePending = true, false
	runningLabel, runningReason := Verdict(entry, in)

	in.Deriving, in.DerivePending = false, true
	owedLabel, owedReason := Verdict(entry, in)

	require.Equal(t, LabelDeriving, runningLabel)
	require.Equal(t, LabelDeriving, owedLabel)
	require.NotEqual(t, runningReason, owedReason)
	require.Contains(t, owedReason, "queued")

	// The control: the same store rows with no marker at all are the verdict
	// this fix exists to stop being reported.
	in.DerivePending = false
	bareLabel, _ := Verdict(entry, in)
	require.Equal(t, LabelNeverDerived, bareLabel)
}

// The enrichment sub-verdict, and the distinction the __none__ sentinel exists
// to draw. "Nothing applies" is an answer; "nothing recorded" is the absence of
// one, and only the first can be trusted to let a repo through.
func TestEnrichVerdict(t *testing.T) {
	t.Parallel()
	base := store_sqlite.RepoReadiness{ContentGen: 5}

	for _, tc := range []struct {
		name  string
		table bool
		repo  store_sqlite.RepoReadiness
		want  string
	}{
		{"table absent", false, base, EnrichLabelUnknown},
		{"no rows at all — nobody has looked", true, base, EnrichLabelUnknown},
		{"only the __none__ sentinel — looked, nothing applies", true,
			store_sqlite.RepoReadiness{ContentGen: 5, EnrichRows: 1, EnrichNoneDeclared: true},
			EnrichLabelNA},
		{"every provider current", true,
			store_sqlite.RepoReadiness{ContentGen: 5, EnrichRows: 2, EnrichProviders: 2, EnrichMinContentGen: 5},
			EnrichLabelCurrent},
		{"the MINIMUM decides: one fresh provider cannot speak for a sibling that never ran", true,
			store_sqlite.RepoReadiness{ContentGen: 5, EnrichRows: 2, EnrichProviders: 2, EnrichMinContentGen: 0},
			EnrichLabelStale},
		{"a provider ahead of the counter is still current", true,
			store_sqlite.RepoReadiness{ContentGen: 5, EnrichRows: 1, EnrichProviders: 1, EnrichMinContentGen: 9},
			EnrichLabelCurrent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want,
				EnrichVerdict(Inputs{EnrichTable: tc.table, Repo: tc.repo}))
		})
	}
}

// The remediation hint is deliberately narrow: only the states where an answer
// is incomplete right now and nothing is already fixing it. Listing the
// self-resolving ones too would bury the actionable ones — and, on the MCP
// surface, would train an agent to skip the note entirely.
func TestOnlyIncompleteAnswersEarnARemediationHint(t *testing.T) {
	t.Parallel()
	require.True(t, BlocksQueries(LabelNeverDerived))
	require.True(t, BlocksQueries(LabelPartial))
	for _, label := range []string{
		LabelReady, LabelStale, LabelUnknown,
		LabelDeriving, LabelEnriching, LabelMissing, LabelNotIndexed,
	} {
		require.False(t, BlocksQueries(label), label)
	}
}
