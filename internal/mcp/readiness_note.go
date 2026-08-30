package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/readiness"
)

// readinessTTL bounds how often one session re-reads readiness from the store.
//
// Not a once-per-session latch. A repo goes `partial` the moment a file changes
// under it, so a session that resolved "ready" on its first call and never
// looked again would fall silent exactly when the warning became true — the
// same silent-wrong-answer shape this note exists to remove, reintroduced by
// the cache. A few seconds is short enough that no realistic session misses a
// flip and long enough that a burst of tool calls pays for one read.
const readinessTTL = 3 * time.Second

// readinessShadowDepth caps the walk to a shadow's owner. Shadows nest; a cycle
// would not, but a bounded loop cannot hang the request path either way.
const readinessShadowDepth = 8

// readinessStateReader is the optional store capability this note needs. Only
// *store_sqlite.Store implements it; an in-memory graph or a test double does
// not, and gets no note rather than a fabricated one.
type readinessStateReader interface {
	ReadinessStates() (store_sqlite.ReadinessStates, error)
}

// readinessNoteApplies reports whether a tool's answer is drawn from the graph
// and therefore worth qualifying.
//
// It reuses the momentum read set and adds analyze: a partial derive skews an
// impact or architecture rollup exactly as it skews "who uses this". Edit and
// verify tools stay out for the same reason momentum excludes them — the note
// is about the trustworthiness of an answer, and those do not return one.
func readinessNoteApplies(tool string) bool {
	return momentumReadTools[tool] || tool == "analyze"
}

// durableReadinessStore walks a shadow graph to the durable store underneath.
//
// Readiness lives only in SQLite — it has no in-memory representation — so an
// overlay session's shadow cannot answer for it and the owner must. This is the
// same rule that the enrichment writers learned the hard way: anything with no
// in-memory form resolves through the owner before the capability assertion, or
// the assertion fails silently and the feature quietly does nothing.
func durableReadinessStore(g graph.Store) graph.Store {
	for i := 0; i < readinessShadowDepth && g != nil; i++ {
		shadow, ok := g.(graph.ShadowedStore)
		if !ok {
			return g
		}
		owner := shadow.ShadowOwner()
		if owner == nil {
			return g
		}
		g = owner
	}
	return g
}

// sessionReadinessStates returns this session's cached readiness view, reading
// through to the store when the cache is cold or older than readinessTTL.
func (s *Server) sessionReadinessStates(sess *sessionState) (store_sqlite.ReadinessStates, bool) {
	sess.mu.Lock()
	if !sess.readinessAt.IsZero() && time.Since(sess.readinessAt) < readinessTTL {
		cached := sess.readinessCache
		sess.mu.Unlock()
		return cached, true
	}
	sess.mu.Unlock()

	reader, ok := durableReadinessStore(s.graph).(readinessStateReader)
	if !ok {
		return store_sqlite.ReadinessStates{}, false
	}
	states, err := reader.ReadinessStates()
	if err != nil {
		// A readiness read that fails says nothing about the repos. Staying
		// quiet is the only honest option: inventing "not ready" here would
		// report a failure to look as a fact about the graph.
		return store_sqlite.ReadinessStates{}, false
	}

	sess.mu.Lock()
	sess.readinessCache = states
	sess.readinessAt = time.Now()
	sess.mu.Unlock()
	return states, true
}

// readinessNote renders the caveat for one repo.
func readinessNote(repo, label, reason string) string {
	return fmt.Sprintf(
		"(Readiness note: repo %q is %s — %s Answers drawn from it may be missing "+
			"edges, so treat an empty or short result as unproven rather than as "+
			"evidence of absence. This note appears once per repo.)",
		repo, label, strings.TrimSuffix(reason, ".")+".")
}

// maybeAttachReadinessNote appends a caveat when this session's answers come
// from a repo whose derived passes have not finished.
//
// `gortex repos` has carried this verdict for a while, but nothing else did:
// a user had to know to run it first, and an agent asking the graph a question
// had no reason to. The answer itself is the only place the caveat reliably
// reaches whoever is about to act on it, which is why this rides in the result
// body as prose rather than as a structured field — the momentum notes are the
// in-repo precedent for a signal an agent actually reads.
//
// Nil-safe pass-through for error results and sessionless contexts, matching
// maybeAttachMomentumNote: a failed call must not collect a second, unrelated
// complaint.
func (s *Server) maybeAttachReadinessNote(ctx context.Context, toolName string, res *mcp.CallToolResult) *mcp.CallToolResult {
	if res == nil || res.IsError || !readinessNoteApplies(toolName) {
		return res
	}
	sess := s.sessionFor(ctx)
	if sess == nil {
		return res
	}
	repos, bound := s.sessionWorkspaceRepoSet(ctx)
	if !bound || len(repos) == 0 {
		return res
	}
	states, ok := s.sessionReadinessStates(sess)
	if !ok {
		return res
	}

	// A workspace rederive the daemon owes suppresses the note wholesale.
	//
	// This is the coarse stand-in for the CLI's per-repo deriving / pending
	// markers, which come from the daemon's runtime record and are not on this
	// side. It errs toward silence on purpose: BlocksQueries is deliberately
	// narrow so the note stays believable, and warning about work already
	// underway is exactly the noise that teaches an agent to skip it.
	pending := s.multiIndexer != nil && s.multiIndexer.WorkspaceRederivePending()

	pendingNotes := readinessWarnings(states, repos, pending)
	if len(pendingNotes) == 0 {
		return res
	}

	for _, w := range sess.latchReadinessWarnings(pendingNotes) {
		res.Content = append(res.Content, mcp.NewTextContent(readinessNote(w.repo, w.label, w.reason)))
	}
	return res
}

// readinessWarning is one repo's blocking verdict, ready to render.
type readinessWarning struct{ repo, label, reason string }

// readinessWarnings is the decision, kept pure so every case is reachable from
// a struct literal — the same property that makes the verdict ladder itself
// table-testable. Results are sorted by repo so a multi-repo session emits its
// notes in a stable order.
func readinessWarnings(
	states store_sqlite.ReadinessStates, repos map[string]bool, pending bool,
) []readinessWarning {
	sorted := make([]string, 0, len(repos))
	for prefix := range repos {
		sorted = append(sorted, prefix)
	}
	sort.Strings(sorted)

	var out []readinessWarning
	for _, prefix := range sorted {
		_, indexed := states.Index[prefix]
		// Missing and Stale are left false: both need a filesystem or git
		// probe this path will not pay for per call. Neither can hide a
		// warning — they sit ABOVE the two blocking rungs in the ladder, so
		// omitting them can only let a note through, never suppress one, and
		// a repo that is genuinely never-derived returns a subset whether or
		// not its index also trails HEAD.
		label, reason := readiness.Verdict(
			readiness.RepoState{Indexed: indexed},
			readiness.Inputs{
				DeriveTable:   states.DeriveTable,
				EnrichTable:   states.EnrichTable,
				Repo:          states.Repos[prefix],
				PassVersion:   indexer.DerivePassVersion,
				DerivePending: pending,
			})
		if !readiness.BlocksQueries(label) {
			continue
		}
		out = append(out, readinessWarning{prefix, label, reason})
	}
	return out
}

// latchReadinessWarnings returns the subset of warnings this session has not
// already emitted, marking them emitted.
//
// Once per repo, not once per call: repeating the caveat on every subsequent
// answer would make it wallpaper, and an agent that learns to skip one note
// skips the one that mattered. Per repo rather than per session because a
// workspace can have several, and a warning about `odoo` says nothing about
// `addons`.
func (ss *sessionState) latchReadinessWarnings(all []readinessWarning) []readinessWarning {
	if ss == nil || len(all) == 0 {
		return nil
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.readinessNoted == nil {
		ss.readinessNoted = map[string]bool{}
	}
	var fire []readinessWarning
	for _, w := range all {
		if ss.readinessNoted[w.repo] {
			continue
		}
		ss.readinessNoted[w.repo] = true
		fire = append(fire, w)
	}
	return fire
}
