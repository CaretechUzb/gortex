package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/readiness"
)

// derivedStates is a store view in which every listed repo is indexed, derived
// and enriched — the baseline each case below breaks one way.
func derivedStates(prefixes ...string) store_sqlite.ReadinessStates {
	out := store_sqlite.ReadinessStates{
		StoreFound:  true,
		DeriveTable: true,
		EnrichTable: true,
		Index:       map[string]graph.RepoIndexState{},
		Repos:       map[string]store_sqlite.RepoReadiness{},
	}
	for _, p := range prefixes {
		out.Index[p] = graph.RepoIndexState{}
		out.Repos[p] = store_sqlite.RepoReadiness{
			ContentGen:  7,
			DeriveFound: true,
			Derive: graph.DeriveState{
				DerivedContentGen: 7,
				PassVersion:       indexer.DerivePassVersion,
			},
			EnrichProviders:     1,
			EnrichMinContentGen: 7,
			EnrichRows:          1,
		}
	}
	return out
}

func repoSet(prefixes ...string) map[string]bool {
	out := map[string]bool{}
	for _, p := range prefixes {
		out[p] = true
	}
	return out
}

// The decision itself. Only the two states where an answer is incomplete RIGHT
// NOW and nothing is already fixing it earn a note — the rest would be noise,
// and an agent that learns to skip this note skips the one that mattered.
func TestReadinessWarningsFireOnlyForAnIncompleteAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		mutate  func(*store_sqlite.ReadinessStates)
		pending bool
		want    string // "" means no note
	}{
		{"a settled repo says nothing", func(*store_sqlite.ReadinessStates) {}, false, ""},

		{"THE case: the derived passes never ran",
			func(st *store_sqlite.ReadinessStates) {
				r := st.Repos["odoo"]
				r.DeriveFound = false
				st.Repos["odoo"] = r
			}, false, readiness.LabelNeverDerived},

		{"content moved since the derive",
			func(st *store_sqlite.ReadinessStates) {
				r := st.Repos["odoo"]
				r.ContentGen = 9
				st.Repos["odoo"] = r
			}, false, readiness.LabelPartial},

		{"enrichment behind the indexed content",
			func(st *store_sqlite.ReadinessStates) {
				r := st.Repos["odoo"]
				r.EnrichMinContentGen = 2
				st.Repos["odoo"] = r
			}, false, readiness.LabelPartial},

		{"a repo with no index row is not indexed, which is not an incomplete answer",
			func(st *store_sqlite.ReadinessStates) { delete(st.Index, "odoo") }, false, ""},

		{"a store predating derive tracking is unknown, not a missing derive",
			func(st *store_sqlite.ReadinessStates) { st.DeriveTable = false }, false, ""},

		{"work the daemon owes suppresses the note — it is being fixed as it is read",
			func(st *store_sqlite.ReadinessStates) {
				r := st.Repos["odoo"]
				r.DeriveFound = false
				st.Repos["odoo"] = r
			}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := derivedStates("odoo")
			tc.mutate(&st)
			got := readinessWarnings(st, repoSet("odoo"), tc.pending)
			if tc.want == "" {
				require.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			require.Equal(t, "odoo", got[0].repo)
			require.Equal(t, tc.want, got[0].label)
			require.NotEmpty(t, got[0].reason, "a note that names no remedy is half a feature")
		})
	}
}

// The session scope is the ceiling: a broken repo the session never answers
// from is not this session's problem, and naming it would send someone to fix
// something unrelated to the answer in front of them.
func TestReadinessWarningsRespectTheSessionScope(t *testing.T) {
	t.Parallel()
	st := derivedStates("odoo", "addons")
	broken := st.Repos["addons"]
	broken.DeriveFound = false
	st.Repos["addons"] = broken

	require.Empty(t, readinessWarnings(st, repoSet("odoo"), false),
		"addons is broken but out of scope")

	got := readinessWarnings(st, repoSet("odoo", "addons"), false)
	require.Len(t, got, 1)
	require.Equal(t, "addons", got[0].repo)
}

// Several broken repos come out sorted, so a multi-repo session's notes do not
// reorder between calls.
func TestReadinessWarningsAreStablyOrdered(t *testing.T) {
	t.Parallel()
	st := derivedStates("odoo", "addons", "local")
	for _, p := range []string{"odoo", "addons", "local"} {
		r := st.Repos[p]
		r.DeriveFound = false
		st.Repos[p] = r
	}
	got := readinessWarnings(st, repoSet("odoo", "addons", "local"), false)
	require.Len(t, got, 3)
	require.Equal(t, []string{"addons", "local", "odoo"},
		[]string{got[0].repo, got[1].repo, got[2].repo})
}

// Once per repo, not once per call. A caveat repeated on every answer becomes
// wallpaper; per repo rather than per session because a warning about one repo
// says nothing about its neighbour.
func TestReadinessNoteFiresOncePerRepoNotOncePerCall(t *testing.T) {
	t.Parallel()
	sess := &sessionState{}
	warn := []readinessWarning{{"odoo", readiness.LabelNeverDerived, "because"}}

	first := sess.latchReadinessWarnings(warn)
	require.Len(t, first, 1, "the first call must warn")

	second := sess.latchReadinessWarnings(warn)
	require.Empty(t, second, "the same repo must not warn twice")

	other := []readinessWarning{{"addons", readiness.LabelPartial, "because"}}
	require.Len(t, sess.latchReadinessWarnings(other), 1,
		"a different repo has its own latch")
}

// A failed call must not collect a second, unrelated complaint, and a tool that
// returns no graph answer has nothing to qualify.
func TestReadinessNoteSkipsErrorsAndNonGraphTools(t *testing.T) {
	t.Parallel()
	s := &Server{session: &sessionState{}}

	errRes := mcp.NewToolResultError("boom")
	require.Same(t, errRes, s.maybeAttachReadinessNote(t.Context(), "search", errRes))

	require.Nil(t, s.maybeAttachReadinessNote(t.Context(), "search", nil))

	ok := mcp.NewToolResultText("fine")
	require.Same(t, ok, s.maybeAttachReadinessNote(t.Context(), "edit_file", ok),
		"an edit tool returns no graph answer to qualify")
}

// The tool set: graph-answering tools are qualified, acting tools are not.
func TestReadinessNoteAppliesToGraphAnsweringToolsOnly(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"search", "read", "relations", "trace", "explore", "analyze"} {
		require.True(t, readinessNoteApplies(tool), tool)
	}
	for _, tool := range []string{"edit_file", "write_file", "rename_symbol", "verify_change"} {
		require.False(t, readinessNoteApplies(tool), tool)
	}
}

// The rendered note has to name the repo, the verdict and the remedy — a note
// that says "something is incomplete" and stops is one an agent cannot act on.
func TestReadinessNoteNamesTheRepoTheVerdictAndTheRemedy(t *testing.T) {
	t.Parallel()
	note := readinessNote("odoo", readiness.LabelNeverDerived,
		"the derived passes have never run for this repo, so cross-repo and interface queries return a subset; restart the daemon")
	require.Contains(t, note, "odoo")
	require.Contains(t, note, readiness.LabelNeverDerived)
	require.Contains(t, note, "restart the daemon")
	require.True(t, strings.HasPrefix(note, "(Readiness note:"),
		"the momentum notes' shape, so an agent reads it the same way")
}

// shadowStore is a graph.Store that reports an owner, standing in for the
// overlay shadow an editor session pushes.
type shadowStore struct {
	graph.Store
	owner graph.Store
}

func (s shadowStore) ShadowOwner() graph.Store { return s.owner }

// Readiness lives only in SQLite, so a shadow cannot answer for it. Walking to
// the owner is what keeps an overlay session from silently losing the note —
// the same rule the enrichment writers learned by losing their state to a
// shadow they never resolved through.
func TestReadinessResolvesThroughAShadowToItsOwner(t *testing.T) {
	t.Parallel()
	owner := graph.New()

	require.Same(t, owner, durableReadinessStore(owner),
		"a store that shadows nothing is its own owner")

	nested := shadowStore{owner: shadowStore{owner: owner}}
	require.Same(t, owner, durableReadinessStore(nested), "shadows nest")

	require.NotNil(t, durableReadinessStore(shadowStore{owner: nil}),
		"a shadow with no owner still yields a usable store, not nil")
}

// fakeReadinessStore is a graph.Store that can answer the readiness capability
// and counts how often it is asked.
type fakeReadinessStore struct {
	graph.Store
	states store_sqlite.ReadinessStates
	reads  int
}

func (f *fakeReadinessStore) ReadinessStates() (store_sqlite.ReadinessStates, error) {
	f.reads++
	return f.states, nil
}

// The cache must expire. A repo goes partial the moment a file changes under
// it, so a session that resolved "ready" on its first call and latched that
// answer forever would go silent exactly when the warning became true — this
// note's own failure mode, reintroduced by its cache. Bounded staleness is the
// trade; permanent staleness is not.
func TestReadinessReReadsAfterTheCacheExpires(t *testing.T) {
	t.Parallel()
	fake := &fakeReadinessStore{states: derivedStates("odoo")}
	s := &Server{session: &sessionState{}, graph: fake}
	sess := s.session

	_, ok := s.sessionReadinessStates(sess)
	require.True(t, ok)
	require.Equal(t, 1, fake.reads, "a cold cache reads through")

	_, ok = s.sessionReadinessStates(sess)
	require.True(t, ok)
	require.Equal(t, 1, fake.reads, "a warm cache does not re-read")

	// The repo goes partial, and the session has been alive past the TTL.
	broken := fake.states.Repos["odoo"]
	broken.ContentGen = 99
	fake.states.Repos["odoo"] = broken
	sess.mu.Lock()
	sess.readinessAt = time.Now().Add(-2 * readinessTTL)
	sess.mu.Unlock()

	got, ok := s.sessionReadinessStates(sess)
	require.True(t, ok)
	require.Equal(t, 2, fake.reads, "an expired cache reads through again")
	require.Len(t, readinessWarnings(got, repoSet("odoo"), false), 1,
		"the flip to partial must become visible mid-session")
}

// A store that cannot answer the capability yields no note rather than a
// fabricated verdict, and a read that fails says nothing about the repos —
// reporting a failure to look as a fact about the graph is the very move this
// change exists to stop.
func TestReadinessStaysQuietWhenItCannotLook(t *testing.T) {
	t.Parallel()
	s := &Server{session: &sessionState{}, graph: graph.New()}
	_, ok := s.sessionReadinessStates(s.session)
	require.False(t, ok, "a plain in-memory graph cannot answer readiness")
}
