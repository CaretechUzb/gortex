package store_sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The defect that motivates the whole completion-vs-start rule, written as a
// test so it cannot come back.
//
// The derived passes are graph writers: every edge they emit advances the
// anchor they are being measured against. A stamp holding the generation read
// at derive START is therefore behind the moment the derive ends, and nothing
// later repairs it — a second derive over unchanged content inserts no rows,
// so it moves nothing and the gap is permanent. Every repo would read partial
// forever after its first derive, which is precisely the false alarm that
// makes a readiness column worth ignoring.
func TestStampDeriveStateSurvivesTheDerivesOwnWrites(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)

	// The index lands the repo's content.
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	atDeriveStart := readGen(t, store, "repoA")
	require.Positive(t, atDeriveStart)

	// The derive runs. Its passes emit edges of their own, which advance the
	// very anchor it is about to stamp.
	store.AddBatch([]*graph.Node{genNode("repoA", "Derived")}, []*graph.Edge{{
		From: "repoA/a.go::A", To: "repoA/a.go::Derived", Kind: graph.EdgeCalls,
	}})
	require.Greater(t, readGen(t, store, "repoA"), atDeriveStart,
		"the derive's own writes must move the anchor — otherwise this test proves nothing")

	require.NoError(t, store.StampDeriveState(
		[]graph.DeriveCompletion{{RepoPrefix: "repoA", PassVersion: 3}}, 1700))

	st, found, err := store.GetDeriveState("repoA")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, readGen(t, store, "repoA"), st.DerivedGen,
		"the stamp must record the generation the derive LEFT, not the one it found")
	require.Greater(t, st.DerivedGen, atDeriveStart,
		"stamping the derive-start generation would leave this repo partial forever")
}

// After a stamp, only a genuinely new mutation may reopen the gap. This is the
// pair of the test above: completion-stamping must not have bought its way out
// of the false-partial by becoming blind to real staleness.
func TestDeriveStateGoesStaleOnlyOnAMutationAfterTheStamp(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)

	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	require.NoError(t, store.StampDeriveState(
		[]graph.DeriveCompletion{{RepoPrefix: "repoA"}}, 1700))

	st, _, err := store.GetDeriveState("repoA")
	require.NoError(t, err)
	require.Equal(t, readGen(t, store, "repoA"), st.DerivedGen, "current right after the stamp")

	// A no-op batch changes nothing, so the repo stays current.
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	require.Equal(t, st.DerivedGen, readGen(t, store, "repoA"),
		"an idle write must not decay a ready repo")

	// A real edit does move it, and that is the staleness signal — reached
	// with no commit and no HEAD transition, which is exactly the case
	// indexed_at could never see.
	store.AddBatch([]*graph.Node{genNode("repoA", "B")}, nil)
	require.Greater(t, readGen(t, store, "repoA"), st.DerivedGen,
		"an incremental edit must make the repo read partial")
}

// The stamp is provenance, not graph. If writing it advanced the anchor it
// records, the repo could never converge on ready: each stamp would create the
// staleness the next stamp had to chase.
func TestStampDeriveStateDoesNotAdvanceTheAnchorItRecords(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	before := readGen(t, store, "repoA")

	require.NoError(t, store.StampDeriveState(
		[]graph.DeriveCompletion{{RepoPrefix: "repoA"}}, 1700))
	require.Equal(t, before, readGen(t, store, "repoA"),
		"stamping must not be a graph mutation")

	// And stamping twice is idempotent for the same reason.
	require.NoError(t, store.StampDeriveState(
		[]graph.DeriveCompletion{{RepoPrefix: "repoA"}}, 1800))
	require.Equal(t, before, readGen(t, store, "repoA"))
}

func TestDeriveStateRoundTripsEveryField(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)

	require.NoError(t, store.StampDeriveState([]graph.DeriveCompletion{{
		RepoPrefix:  "repoA",
		DerivedSHA:  "abc123",
		PassVersion: 7,
		ConfigHash:  "cfg-9",
		Scoped:      true,
	}}, 1712345678))

	st, found, err := store.GetDeriveState("repoA")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "abc123", st.DerivedSHA)
	require.Equal(t, int64(1712345678), st.DerivedAt)
	require.Equal(t, int64(7), st.PassVersion)
	require.Equal(t, "cfg-9", st.ConfigHash)
	require.True(t, st.Scoped)
	require.False(t, st.Legacy)
}

// Absence is a reportable state, not a gap. A repo tracked during daemon
// warmup is never derived at all — permanently — and until this row existed
// nothing could say so: it answered queries with a silent subset and read
// exactly like a repo derived a second ago.
func TestGetDeriveStateReportsANeverDerivedRepo(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)

	st, found, err := store.GetDeriveState("repoA")
	require.NoError(t, err)
	require.False(t, found, "a tracked but underived repo must report no row, not a zero one")
	require.Equal(t, "repoA", st.RepoPrefix)
}

// A real completion must clear the migration's legacy sentinel. Otherwise a
// pre-v13 repo would render "unknown" forever, no matter how many times the
// daemon actually derived it.
func TestStampDeriveStateClearsTheLegacySentinel(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)

	_, err := store.writerDB.Exec(
		`INSERT INTO derive_state (repo_prefix, legacy) VALUES ('repoA', 1)`)
	require.NoError(t, err)
	st, found, err := store.GetDeriveState("repoA")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, st.Legacy)

	require.NoError(t, store.StampDeriveState(
		[]graph.DeriveCompletion{{RepoPrefix: "repoA"}}, 1700))
	st, _, err = store.GetDeriveState("repoA")
	require.NoError(t, err)
	require.False(t, st.Legacy, "a recorded derive must retire the unknown-state sentinel")
	require.Equal(t, readGen(t, store, "repoA"), st.DerivedGen)
}

// One derive covering several repos stamps each against its OWN generation.
// A shared value would mark a quiet repo stale the moment a busy sibling
// pulled the number past it.
func TestStampDeriveStateUsesEachReposOwnGeneration(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.AddBatch([]*graph.Node{genNode("repoA", "A"), genNode("repoB", "B")}, nil)
	// Make the two generations differ.
	store.AddBatch([]*graph.Node{genNode("repoA", "A2")}, nil)
	genA, genB := readGen(t, store, "repoA"), readGen(t, store, "repoB")
	require.NotEqual(t, genA, genB)

	require.NoError(t, store.StampDeriveState([]graph.DeriveCompletion{
		{RepoPrefix: "repoA"}, {RepoPrefix: "repoB"},
	}, 1700))

	stA, _, err := store.GetDeriveState("repoA")
	require.NoError(t, err)
	stB, _, err := store.GetDeriveState("repoB")
	require.NoError(t, err)
	require.Equal(t, genA, stA.DerivedGen)
	require.Equal(t, genB, stB.DerivedGen)
}

// A repo with no anchor row yet stamps at zero rather than failing. Zero is the
// honest answer — nothing has been recorded — and the first real mutation
// moves the anchor to 1, which correctly reads partial.
func TestStampDeriveStateHandlesARepoWithNoAnchorRow(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)

	require.NoError(t, store.StampDeriveState(
		[]graph.DeriveCompletion{{RepoPrefix: "unseen"}}, 1700))
	st, found, err := store.GetDeriveState("unseen")
	require.NoError(t, err)
	require.True(t, found)
	require.Zero(t, st.DerivedGen)

	gen, contentGen, found, err := store.GetRepoGraphGen("unseen")
	require.NoError(t, err)
	require.False(t, found)
	require.Zero(t, gen)
	require.Zero(t, contentGen)

	store.AddBatch([]*graph.Node{genNode("unseen", "A")}, nil)
	require.Greater(t, readGen(t, store, "unseen"), st.DerivedGen)
}

// The bug this method exists to prevent, pinned.
//
// A repo tracked during daemon warmup is never globally derived -- permanently.
// It correctly reads "never derived" because it has no row. If one saved file
// could write that row, the column would promote it to "ready" while implements
// inference, framework synthesis and cross-repo detection had still never run
// over it: the readiness feature blessing the exact silent-subset case it was
// built to catch.
func TestRefreshDeriveStateNeverPromotesANeverDerivedRepo(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.AddBatch([]*graph.Node{genNode("warmup", "A")}, nil)

	refreshed, err := store.RefreshDeriveState([]string{"warmup"}, 1700)
	require.NoError(t, err)
	require.Zero(t, refreshed, "there is no completion to refresh")

	_, found, err := store.GetDeriveState("warmup")
	require.NoError(t, err)
	require.False(t, found, "an incremental derive must not manufacture a completion")
}

// The other half: a repo that HAS been derived must be kept current by the
// incremental passes, or every ordinary file save would leave it reading
// partial forever -- the reindex moves the anchor, and no whole-graph derive is
// ever scheduled for a single edit.
func TestRefreshDeriveStateRenewsARealCompletionWithoutRewritingItsProvenance(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	require.NoError(t, store.StampDeriveState([]graph.DeriveCompletion{{
		RepoPrefix: "repoA", DerivedSHA: "abc123", PassVersion: 4, ConfigHash: "cfg-1",
	}}, 1700))

	// A save lands, so the repo now reads partial.
	store.AddBatch([]*graph.Node{genNode("repoA", "B")}, nil)
	stale, _, err := store.GetDeriveState("repoA")
	require.NoError(t, err)
	require.Less(t, stale.DerivedGen, readGen(t, store, "repoA"))

	refreshed, err := store.RefreshDeriveState([]string{"repoA"}, 1800)
	require.NoError(t, err)
	require.Equal(t, 1, refreshed)

	st, _, err := store.GetDeriveState("repoA")
	require.NoError(t, err)
	require.Equal(t, readGen(t, store, "repoA"), st.DerivedGen, "the repo is current again")
	require.Equal(t, int64(1800), st.DerivedAt)
	// The incremental pass did not produce this completion, so it must not
	// claim its provenance -- notably not its pass_version, which would
	// otherwise launder a stale build's output into the current one.
	require.Equal(t, int64(4), st.PassVersion)
	require.Equal(t, "cfg-1", st.ConfigHash)
	require.Equal(t, "abc123", st.DerivedSHA)
}

// A legacy row's true state is unknowable, so an unrelated save must not
// launder it into a recorded completion.
func TestRefreshDeriveStateLeavesTheLegacySentinelAlone(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	_, err := store.writerDB.Exec(
		`INSERT INTO derive_state (repo_prefix, legacy) VALUES ('repoA', 1)`)
	require.NoError(t, err)

	refreshed, err := store.RefreshDeriveState([]string{"repoA"}, 1800)
	require.NoError(t, err)
	require.Zero(t, refreshed)

	st, _, err := store.GetDeriveState("repoA")
	require.NoError(t, err)
	require.True(t, st.Legacy, "a pre-v13 row stays unknown until a real derive runs")
	require.Zero(t, st.DerivedAt)
}

func TestStampDeriveStateIgnoresEmptyInput(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	require.NoError(t, store.StampDeriveState(nil, 1700))
	require.NoError(t, store.StampDeriveState([]graph.DeriveCompletion{{RepoPrefix: ""}}, 1700))

	_, found, err := store.GetDeriveState("")
	require.NoError(t, err)
	require.False(t, found, "the unprefixed sentinel has no per-repo state row")
}
