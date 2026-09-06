package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

// UntrackPlan names what an untrack of one checkout will actually do. Untrack
// is one verb over four quite different transactions, and which one it is
// depends on what else the family holds — so the plan is decided once, by the
// preview, and the confirm executes it rather than deciding again.
type UntrackPlan string

const (
	// UntrackPlanEvict has no catalog identity to reason about: a store with
	// no catalog, or a directory git does not administer. The repository
	// simply leaves the corpus.
	UntrackPlanEvict UntrackPlan = "evict"
	// UntrackPlanDemote hands a checkout to the family's automatic lane. Its
	// corpus goes; its identity, and the view a session gets for it, stay.
	UntrackPlanDemote UntrackPlan = "demote"
	// UntrackPlanForget removes the checkout and everything referencing it.
	// It is what an inaccessible checkout takes: there is no working tree to
	// build automatic layers from, so there is nothing to demote it to.
	UntrackPlanForget UntrackPlan = "forget"
	// UntrackPlanPrimaryClosure retires a primary graph and everything that
	// could only be served because of it.
	UntrackPlanPrimaryClosure UntrackPlan = "primary_closure"
	// UntrackPlanBlocked is an untrack that would leave the family with
	// nothing to serve the checkout from. It names the ways forward instead.
	UntrackPlanBlocked UntrackPlan = "blocked"
)

// ErrUntrackBlocked reports an untrack that cannot be carried out as asked.
// The error names the paths that can.
var ErrUntrackBlocked = errors.New("indexer: this checkout cannot be untracked on its own")

// UntrackPreview is what an untrack of one path or prefix would do.
//
// It is the payload the CLI and the tool surface render before asking, and the
// same value the confirm runs off, so what a user is shown and what happens
// cannot drift apart.
type UntrackPreview struct {
	Prefix      string
	CheckoutID  string
	Incarnation string
	FamilyID    string
	GraphID     string
	// Plan is the transaction the confirm will run.
	Plan UntrackPlan
	// Accessible reports whether the checkout's root answered when the
	// catalog last looked.
	Accessible bool
	// IsPrimary reports that the checkout owns its family's base corpus.
	IsPrimary bool
	// SolePrimary reports that no dedicated graph in the family survives the
	// retirement, so the closure carries on into a family teardown. It is the
	// difference between "this repository stops being served from here" and
	// "the daemon forgets this repository", which is the whole of what a
	// caller has to be shown before it confirms.
	SolePrimary bool
	// PrimaryEpoch is the family's compare-and-set token, carried into the
	// confirm so a primary that moved between the two is refused.
	PrimaryEpoch int64
	// Closure is everything the confirm removes, for a plan that removes
	// anything beyond the checkout itself.
	Closure []reconcile.Dependent
	// Preserved is what survives it.
	Preserved []reconcile.Dependent
	// Blockers explains a blocked plan: what is missing, and what to do
	// instead.
	Blockers []string
}

// resolveDestructivePrefix resolves only identities the caller named
// explicitly. Unlike ResolvePrefix, it never interprets a bare, unknown token
// relative to the daemon's working directory: doing that in a destructive
// flow can turn a typo into a path inside an unrelated tracked repository.
//
// Exact live prefixes and exact durable graph prefixes are accepted. Filesystem
// containment is accepted only for absolute paths, including paths whose
// checkout is currently absent from the in-memory index but still has a
// catalog identity.
func (l *CheckoutLifecycle) resolveDestructivePrefix(ctx context.Context, pathOrPrefix string) (string, error) {
	if l == nil || l.mi == nil || pathOrPrefix == "" {
		return "", nil
	}
	if meta := l.mi.GetMetadata(pathOrPrefix); meta != nil {
		return pathOrPrefix, nil
	}
	if l.catalog != nil {
		graph, ok, err := l.catalog.GetDedicatedGraph(ctx, GraphIDFor(pathOrPrefix))
		if err != nil {
			return "", err
		}
		if ok && graph.RepoPrefix == pathOrPrefix {
			return pathOrPrefix, nil
		}
	}
	if !filepath.IsAbs(pathOrPrefix) {
		return "", nil
	}
	if prefix := l.ResolvePrefix(pathOrPrefix); prefix != "" {
		return prefix, nil
	}
	if l.catalog == nil {
		return "", nil
	}
	checkout, found, err := l.checkoutForPath(ctx, pathOrPrefix)
	if err != nil || !found {
		return "", err
	}
	graphs, err := l.catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
	if err != nil {
		return "", err
	}
	for _, graph := range graphs {
		if graph.OwnerCheckoutID == checkout.CheckoutID && graph.RepoPrefix != "" {
			return graph.RepoPrefix, nil
		}
	}
	return "", nil
}

// PreviewUntrack decides what untracking one path or prefix would do, and
// enumerates what it would take with it.
//
// Nothing is written. Every branch below is a property of the catalog rows, so
// a preview and the confirm that follows it read the same evidence — the only
// thing that can come between them is another actor moving the rows, which is
// what the incarnation and epoch guards on the confirm are for.
func (l *CheckoutLifecycle) PreviewUntrack(ctx context.Context, pathOrPrefix string) (UntrackPreview, error) {
	if l == nil || l.mi == nil {
		return UntrackPreview{}, errors.New("indexer: checkout lifecycle is not wired")
	}
	prefix, err := l.resolveDestructivePrefix(ctx, pathOrPrefix)
	if err != nil {
		return UntrackPreview{}, err
	}
	if prefix == "" {
		return UntrackPreview{}, fmt.Errorf("%w: %s", ErrCheckoutNotTracked, pathOrPrefix)
	}
	out := UntrackPreview{Prefix: prefix, Plan: UntrackPlanEvict}

	checkout, err := l.checkoutForPrefix(ctx, prefix)
	if err != nil || checkout == nil {
		return out, err
	}
	out.CheckoutID, out.Incarnation = checkout.CheckoutID, checkout.Incarnation
	out.FamilyID = checkout.FamilyID
	out.Accessible = checkout.State == store_sqlite.CheckoutStateReady

	graphs, err := l.catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
	if err != nil {
		return out, err
	}
	var owned, primary *store_sqlite.DedicatedGraph
	for i := range graphs {
		if graphs[i].OwnerCheckoutID == checkout.CheckoutID {
			owned = &graphs[i]
		}
		if graphs[i].IsPrimaryBase {
			primary = &graphs[i]
		}
	}
	if owned != nil {
		out.GraphID = owned.GraphID
		out.IsPrimary = owned.IsPrimaryBase
	}

	if out.IsPrimary {
		closure, err := l.rec.PrimaryClosure(ctx, owned.GraphID)
		if err != nil {
			return out, err
		}
		out.Plan = UntrackPlanPrimaryClosure
		out.PrimaryEpoch = closure.PrimaryEpoch
		out.Closure, out.Preserved = closure.Dependents, closure.Preserved
		out.SolePrimary = closure.SoleGraph
		return out, nil
	}

	if !out.Accessible {
		// A root that cannot be read cannot be rebuilt into automatic layers
		// either, so the only thing left to do with it is to let it go.
		dependents, err := l.rec.Dependents(ctx, checkout.CheckoutID)
		if err != nil {
			return out, err
		}
		out.Plan, out.Closure = UntrackPlanForget, dependents
		return out, nil
	}

	servable := primary != nil &&
		primary.OwnerCheckoutID != checkout.CheckoutID &&
		primary.State == reconcile.GraphStateReady
	if !servable {
		out.Plan = UntrackPlanBlocked
		out.Blockers = []string{
			"family " + checkout.FamilyID + " has no other ready primary corpus to serve this checkout from",
			"set another checkout's graph as the family primary, then untrack this one",
			"or preview a forget, which removes the checkout and its corpus outright",
		}
		return out, nil
	}
	family, found, err := l.catalog.GetRepositoryFamily(ctx, checkout.FamilyID)
	if err != nil {
		return out, err
	}
	if !found {
		return out, fmt.Errorf("%w: family %s", store_sqlite.ErrCatalogNotFound, checkout.FamilyID)
	}
	out.Plan = UntrackPlanDemote
	out.PrimaryEpoch = family.PrimaryEpoch
	if owned != nil {
		out.Closure = append(out.Closure, reconcile.Dependent{
			Kind:   reconcile.DependentGraph,
			ID:     owned.GraphID,
			Detail: "corpus " + owned.RepoPrefix + " is retired; the checkout is served from the family primary",
		})
	}
	views, err := l.catalog.ListRefViews(ctx, out.GraphID)
	if err != nil {
		return out, err
	}
	for _, view := range views {
		out.Closure = append(out.Closure, reconcile.Dependent{
			Kind:   reconcile.DependentRefView,
			ID:     view.RefViewID,
			Detail: "view " + view.SelectorValue + " is rooted in this graph",
		})
	}
	out.Preserved = append(out.Preserved, reconcile.Dependent{
		Kind:   reconcile.DependentCheckout,
		ID:     checkout.CheckoutID,
		Detail: "checkout " + checkout.AdminName + " keeps its identity and is served from graph " + primary.GraphID,
	})
	return out, nil
}

// PreviewForget decides what forgetting one path or prefix would do.
//
// Forget is the deliberate removal untrack refuses to be: where untrack looks
// for a way to keep the checkout — demoting it into the family's automatic
// lane, or refusing when it cannot — forget says the identity and its corpus
// are to go. The primary branch is unchanged, because retiring a primary is
// already the removal it looks like.
func (l *CheckoutLifecycle) PreviewForget(ctx context.Context, pathOrPrefix string) (UntrackPreview, error) {
	preview, err := l.PreviewUntrack(ctx, pathOrPrefix)
	if err != nil {
		return preview, err
	}
	switch preview.Plan {
	case UntrackPlanEvict, UntrackPlanForget, UntrackPlanPrimaryClosure:
		return preview, nil
	}
	// A demote or a blocked untrack both describe a checkout that could have
	// kept its identity. The closure is re-read as the removal it is instead.
	dependents, err := l.rec.Dependents(ctx, preview.CheckoutID)
	if err != nil {
		return preview, err
	}
	preview.Plan = UntrackPlanForget
	preview.Preserved, preview.Blockers = nil, nil
	preview.Closure = dependents
	if preview.GraphID != "" {
		preview.Closure = append([]reconcile.Dependent{{
			Kind:   reconcile.DependentGraph,
			ID:     preview.GraphID,
			Detail: "corpus " + preview.Prefix + " is retired with the checkout",
		}}, preview.Closure...)
	}
	preview.Closure = append(preview.Closure, reconcile.Dependent{
		Kind:   reconcile.DependentCheckout,
		ID:     preview.CheckoutID,
		Detail: "the checkout identity is removed rather than demoted to the automatic lane",
	})
	return preview, nil
}

// demote hands one dedicated checkout to the family's automatic lane.
//
// It registers the automatic route and builds nothing. The layers a demoted
// checkout will be served through are exactly the ones its coordinator builds
// on any other day, and building them here would put a whole commit layer plus
// a working-tree layer inside the untrack — for a checkout the caller has just
// said it does not want tracked, and which a burst of track/untrack may move
// again before anything reads it. The route therefore lands in the pending
// state: it names the primary graph and no generations, which is the honest
// description of a checkout that is served by the base corpus with exact:false
// until its first layer exists. A read that routes here demands that layer and
// the coordinator builds it; nothing that reads is left waiting on a build
// nobody asked for.
//
// The corpus it is leaving goes last, after the flip and through the guarded
// retirement path, so a view materialized just before the flip keeps its lease
// on what it is reading.
//
// The order of what remains is what an observer keys on. The mode flip is
// published FIRST and everything expensive happens behind it — the dedicated
// graph retired, the repository's rows evicted, its entry removed from the
// global config — with CompleteIntentTransition last. So the transition row
// disappearing, not the mode column, is what says a demotion is done; a
// caller that waits on the mode alone (gortex untrack --wait) declares success
// seconds into a teardown that has minutes left to run.
func (l *CheckoutLifecycle) demote(
	ctx context.Context,
	checkout store_sqlite.Checkout,
	owned *store_sqlite.DedicatedGraph,
	authorization reconcile.DemotionAuthorization,
) error {
	// The pre-flight the synchronous rehome used to perform as a side effect of
	// constructing a coordinator. Nothing has been written yet, so a primary
	// that cannot serve this checkout refuses the demotion here rather than
	// leaving it half-done: without it a demotion completes with the corpus
	// retired, a pending route and no coordinator, and the checkout is unserved
	// until the hourly janitor notices.
	if err := l.primaryCanServe(ctx, authorization.PrimaryGraphID, checkout); err != nil {
		return err
	}
	if err := l.registerAutomaticRoute(ctx, checkout.CheckoutID, authorization.PrimaryGraphID); err != nil {
		return err
	}

	// Offer the private graph's payload before the catalog transaction journals
	// its retirement. The journal is the crash-recovery authority; this in-memory
	// backlog merely lets the current process reclaim generations promptly.
	if owned != nil {
		l.oweRetirement(l.graphGenerations(ctx, owned.GraphID)...)
	}
	commit, commitErr := l.rec.CommitAuthorizedDemotion(ctx, checkout, authorization)
	if commitErr != nil && !commit.Committed {
		// The route just registered describes a checkout that is still
		// dedicated, so it is withdrawn again. Nothing was built under it, so
		// there is no payload to hand back. Intent revocation is not lost: the
		// durable transition remains and a retry adopts it.
		if deleteErr := l.catalog.DeleteCheckoutRoute(ctx, checkout.CheckoutID); deleteErr != nil &&
			!errors.Is(deleteErr, store_sqlite.ErrCatalogNotFound) {
			l.logger.Warn("checkout lifecycle: could not withdraw a rolled-back route",
				zap.String("checkout", checkout.CheckoutID), zap.Error(deleteErr))
		}
		l.sweepRetirements(ctx)
		return commitErr
	}
	if l.demoteBarrier != nil {
		l.demoteBarrier()
	}
	// The commit is through, and with it the retirement saga that ran inside
	// it: the mode flip, the repository purge and DeleteDedicatedGraph are all
	// durable now. Stamped on this side of the barrier rather than immediately
	// after the commit call, so the barrier stays a true mid-teardown
	// observation point — everything a fixture reads there was stamped from
	// inside the saga, which is the part that can run for tens of minutes.
	l.noteTransitionProgress(ctx, checkout.CheckoutID, "graph_retired")
	// The binding is gone now, so the automatic lane will accept the checkout —
	// and where the commit journalled but its cleanup did not, the row it left
	// standing is named as retiring so it cannot veto the coordinator forever.
	l.ensureCoordinatorDespite(ctx, authorization.PrimaryGraphID, checkout, authorization.OwnedGraphID)
	if !l.coordinatorRegistered(checkout.CheckoutID) {
		// The checkout is automatic with a pending route and nothing building
		// for it. That is a SERVING gap, not an unfinished demotion: the mode
		// flip, the corpus retirement and the repository eviction have all
		// committed, and the only thing left below is the durable finish. So it
		// is reported and the finish still runs — failing here instead left the
		// transition standing with everything already torn down, which is a
		// demotion that reads as half-done to every observer (the config entry
		// gone, the corpus gone, the transition still in flight) until the
		// hourly sweep resumed it. The coordinator is what the automatic lane
		// brings up on its own: the first read of this checkout activates one,
		// and the family reconciliation installs one regardless.
		l.logger.Warn("checkout lifecycle: demoted checkout has no coordinator yet",
			zap.String("checkout", checkout.CheckoutID),
			zap.String("primary", authorization.PrimaryGraphID))
	}
	// And it is deliberately NOT demanded. The coordinator is registered so the
	// checkout is watched and a read can wake it, but the first commit layer
	// over the family base costs the same whole-tree build here as anywhere
	// else — and untracking a worktree is, more often than not, the step before
	// deleting it. Demanding here bought that build for a checkout nobody was
	// going to read. The read seam demands instead (ActivateCheckout, and
	// activateCheckout for a cold one), so the only thing not demanding costs
	// is one exact:false answer to whoever reads this checkout first.

	if commitErr != nil {
		// The mode flip and graph-retirement journal committed together.
		// Queries already use the primary; Resume or the next retry finishes the
		// graph cleanup without replaying the demotion.
		l.logger.Warn("checkout lifecycle: demoted graph cleanup remains journalled",
			zap.String("checkout", checkout.CheckoutID),
			zap.String("graph", authorization.OwnedGraphID), zap.Error(commitErr))
	}

	prefix := ""
	if owned != nil {
		prefix = owned.RepoPrefix
	}
	// Only when the retirement did not get to it. The eviction is one call
	// deep inside the saga above — releaseGraph runs cleanupHooks.ReleaseGraph,
	// which is this exact evictRepoChecked on this exact prefix and root path,
	// and DELETES THE GRAPH ROW ON THE NEXT LINE. So a binding that is gone is
	// proof the eviction ran: nothing else in the daemon deletes a demotion's
	// owned binding (the promotion path's DeleteDedicatedGraph unwinds a graph
	// its own in-flight promotion just minted, and a checkout cannot hold a
	// promotion and a demotion transition at once). Running it anyway paid a
	// second whole-repository teardown — a fresh mutation lane, the reachability
	// topology writer and a republish of the aggregate vector corpus — for a
	// prefix with nothing left in it.
	//
	// A binding that is still standing is the partial-commit and resume path,
	// where the saga journalled the retirement but did not execute it, and this
	// call is the retry that persists the external cleanup. An existence check
	// that errors is answered the same way, because skipping an eviction on a
	// catalog hiccup is the worse of the two mistakes.
	if l.ownedBindingStanding(ctx, authorization.OwnedGraphID) {
		if _, _, err := l.evictRepoChecked(ctx, prefix, checkout.RootPath); err != nil {
			// Publication already committed. Keep the automatic route live and
			// the transition standing so restart can retry only the external
			// cleanup.
			l.sweepRetirements(ctx)
			return fmt.Errorf("indexer: persist demoted checkout configuration: %w", err)
		}
	}
	if err := l.catalog.CompleteIntentTransition(ctx, checkout.CheckoutID,
		authorization.Transition.TransitionID); err != nil {
		l.sweepRetirements(ctx)
		return fmt.Errorf("indexer: complete demotion transition: %w", err)
	}
	l.sweepRetirements(ctx)
	return nil
}

// ownedBindingStanding reports that the dedicated-graph row a demotion is
// retiring is still there — i.e. that the retirement saga has not released it,
// and the repository it names has therefore not been evicted yet.
//
// An empty id is a demotion with no corpus to give up, which needs no
// eviction. A failed read answers true: re-running an eviction that already
// happened costs work, skipping one that did not costs correctness.
func (l *CheckoutLifecycle) ownedBindingStanding(ctx context.Context, ownedGraphID string) bool {
	if ownedGraphID == "" {
		return false
	}
	if l == nil || l.catalog == nil {
		return true
	}
	_, found, err := l.catalog.GetDedicatedGraph(ctx, ownedGraphID)
	if err != nil {
		return true
	}
	return found
}

// primaryCanServe refuses a demotion onto a primary that cannot back a
// coordinator.
//
// It asks the two questions buildCoordinator answers with a silent (nil, nil):
// whether the graph is there with a repo prefix, and whether that prefix is
// actually served by an indexer. Both are properties of the family rather than
// of the checkout, and both leave the automatic lane unable to compose
// anything — so they belong in front of the first write, where a refusal costs
// nothing.
func (l *CheckoutLifecycle) primaryCanServe(
	ctx context.Context, primaryGraphID string, checkout store_sqlite.Checkout,
) error {
	if l.store == nil || l.catalog == nil || l.mi == nil {
		return fmt.Errorf("indexer: this daemon cannot serve checkout %s from a shared corpus",
			checkout.CheckoutID)
	}
	primary, found, err := l.catalog.GetDedicatedGraph(ctx, primaryGraphID)
	if err != nil {
		return err
	}
	if !found || primary.RepoPrefix == "" || l.mi.GetIndexer(primary.RepoPrefix) == nil {
		return fmt.Errorf("indexer: the primary graph %s cannot serve checkout %s yet",
			primaryGraphID, checkout.CheckoutID)
	}
	return nil
}

// registerAutomaticRoute points a checkout at a graph with no layers over it
// yet.
//
// Pending is the state that says exactly that, and it is the same one an
// ordinary cycle leaves while it rebuilds: the reader takes its base-corpus
// fallback and is told the view it asked for was not served. A checkout that
// has never been routed gets its row installed; one that still holds a row
// from an earlier life is repointed under its own epoch, so a coordinator that
// is somehow still writing it loses the compare-and-set rather than being
// overwritten.
func (l *CheckoutLifecycle) registerAutomaticRoute(ctx context.Context, checkoutID, graphID string) error {
	route, routed, err := l.catalog.GetCheckoutRoute(ctx, checkoutID)
	if err != nil {
		return err
	}
	if !routed {
		return l.catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
			CheckoutID: checkoutID,
			GraphID:    graphID,
			State:      store_sqlite.RoutePending,
		})
	}
	previousCommit, previousDirty := route.CommitGenerationID, route.DirtyGenerationID
	if err := l.catalog.FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{
		CheckoutID:         checkoutID,
		ExpectedRouteEpoch: route.RouteEpoch,
		GraphID:            graphID,
		State:              store_sqlite.RoutePending,
	}); err != nil {
		return err
	}
	// Whatever the old route named was built over the corpus the checkout is
	// leaving and composes over nothing here, so it is owed a retirement.
	l.oweRetirement(previousDirty, previousCommit)
	return nil
}

// graphGenerations lists the payload generations built over one graph, so a
// retirement can offer them once nothing routes to them any more.
func (l *CheckoutLifecycle) graphGenerations(ctx context.Context, graphID string) []int64 {
	rows, err := l.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{GraphID: graphID})
	if err != nil {
		l.logger.Debug("checkout lifecycle: could not list a graph's generations",
			zap.String("graph", graphID), zap.Error(err))
		return nil
	}
	out := make([]int64, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.GenerationID)
	}
	return out
}

// blockedUntrack renders a blocked preview as the error the caller gets.
func blockedUntrack(preview UntrackPreview) error {
	return fmt.Errorf("%w: %s: %s", ErrUntrackBlocked, preview.Prefix,
		strings.Join(preview.Blockers, "; "))
}
