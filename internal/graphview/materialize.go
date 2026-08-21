package graphview

import (
	"context"
	"fmt"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Materializer turns a checkout's route into a readable view.
//
// It owns none of the three things it needs and holds them by
// reference: the store the payload lives in, the catalog that says which
// generations a checkout is currently routed to, and the lease manager
// that keeps those generations alive while a reader still holds them.
type Materializer struct {
	// Store is any handle on the database. Materialization reads the
	// indexed corpus through it and derives one pinned handle per routed
	// generation, so which generation the handle it was given happens to
	// be on does not matter.
	Store *store_sqlite.Store
	// Catalog answers what the checkout is routed to.
	Catalog *store_sqlite.Catalog
	// Leases pins the generations a materialized view reads. Retirement
	// consults the same manager, so a generation under a live view
	// cannot be swept.
	Leases *LeaseManager
}

// RepoView is one materialized repository view: the identity that names
// its content, the reader that serves it, what it can answer, and the
// lease that keeps it readable.
//
// Close is mandatory and idempotent. Until it runs, every generation the
// view reads is pinned and retirement of any of them is refused.
type RepoView struct {
	// ID names the exact content this view reads.
	ID RepoViewID
	// Reader is the composed graph: the indexed corpus with the
	// checkout's routed generations stacked on it.
	Reader graph.Reader
	// Completeness is what this view can currently answer.
	Completeness Completeness

	generations []int64
	lease       *Lease
	closeOnce   sync.Once
}

// Generations lists the payload generations this view holds a lease on,
// bottom first: the commit generation, then the working-tree one when
// the route named it.
func (v *RepoView) Generations() []int64 {
	if v == nil {
		return nil
	}
	out := make([]int64, len(v.generations))
	copy(out, v.generations)
	return out
}

// Close releases the view's lease. Calling it twice, or on a nil view,
// does nothing.
func (v *RepoView) Close() {
	if v == nil {
		return
	}
	v.closeOnce.Do(func() { v.lease.Release() })
}

// MaterializeCheckout builds the view a checkout's queries currently
// land on.
//
// The route carries two generation slots. The commit slot is the
// checkout's committed content and is the base of its stack — a checkout
// with no published commit generation has no view to serve, so that is
// where the view_building error comes from rather than from a thinner
// stack. It is the base in the identity's sense too: the view is named
// by that generation, and the fact that reading it costs one overlay
// level over the indexed corpus is a storage detail, not part of what
// the view is. The dirty slot is the working tree, and it is the one
// layer a materialized checkout view stacks on top; buffer layers are
// not materialized here at all, because they belong to a session rather
// than to a checkout and the MCP overlay composes them on top of this
// reader at request time exactly as it does today.
//
// Every slot the route names must be servable or the whole call fails.
// A stack that quietly dropped a layer whose generation was not ready
// would answer with content from the wrong state of the world and look
// exactly like a successful answer while doing it.
//
// The lease is taken before the generations are inspected, not after.
// Retirement refuses a leased generation, so a generation that is still
// ready once the lease is held stays ready; checking first and leasing
// afterwards would leave a window in which the sweep runs between the
// two.
func (m *Materializer) MaterializeCheckout(ctx context.Context, checkoutID string) (*RepoView, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, NewViewError(CodeInvalidViewSelector, "materialization needs a context")
	}
	if checkoutID == "" {
		return nil, NewViewError(CodeInvalidViewSelector, "materialization needs a checkout id")
	}

	route, err := m.route(ctx, checkoutID)
	if err != nil {
		return nil, err
	}
	repoPrefix, err := m.repoPrefix(ctx, route.GraphID)
	if err != nil {
		return nil, err
	}
	if route.CommitGenerationID <= 0 {
		return nil, NewViewError(CodeViewBuilding,
			fmt.Sprintf("checkout %q has no published commit generation", checkoutID))
	}

	generations := []int64{route.CommitGenerationID}
	if route.DirtyGenerationID > 0 {
		generations = append(generations, route.DirtyGenerationID)
	}
	lease := m.Leases.Acquire(generations...)
	view, err := m.assemble(ctx, route, repoPrefix, generations, lease)
	if err != nil {
		lease.Release()
		return nil, err
	}
	return view, nil
}

// validate refuses a Materializer that cannot do its job, so a missing
// dependency reports itself here instead of as a nil dereference inside
// a read.
func (m *Materializer) validate() error {
	switch {
	case m == nil || m.Store == nil:
		return NewViewError(CodeInvalidViewSelector, "materializer needs a store")
	case m.Catalog == nil:
		return NewViewError(CodeInvalidViewSelector, "materializer needs a catalog")
	case m.Leases == nil:
		return NewViewError(CodeInvalidViewSelector, "materializer needs a lease manager")
	default:
		return nil
	}
}

// route reads the checkout's current route. A route that does not exist
// or has retired is a checkout nothing can be read from — the failure is
// about the checkout, not about the selector that named it.
func (m *Materializer) route(ctx context.Context, checkoutID string) (store_sqlite.CheckoutRoute, error) {
	route, found, err := m.Catalog.GetCheckoutRoute(ctx, checkoutID)
	switch {
	case err != nil:
		return route, WrapViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("read the route of checkout %q", checkoutID), err)
	case !found:
		return route, NewViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("checkout %q is not routed", checkoutID))
	case route.State == store_sqlite.RouteRetired:
		return route, NewViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("the route of checkout %q has retired", checkoutID))
	default:
		return route, nil
	}
}

// repoPrefix resolves the repository the routed graph carries. The
// prefix is part of the view identity, so a graph the catalog has no row
// for cannot be named and cannot be served.
func (m *Materializer) repoPrefix(ctx context.Context, graphID string) (string, error) {
	dedicated, found, err := m.Catalog.GetDedicatedGraph(ctx, graphID)
	switch {
	case err != nil:
		return "", WrapViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("read graph %q", graphID), err)
	case !found:
		return "", NewViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("graph %q has no catalog row", graphID))
	default:
		return dedicated.RepoPrefix, nil
	}
}

// assemble builds the layer stack, the identity and the completeness of
// a leased set of generations. It runs with the lease already held, so
// every generation it reads is safe from the sweep.
//
// generations is the route's slots in order: the commit generation
// first, the working-tree generation after it when the route names one.
// The commit generation is composed onto the indexed corpus here and the
// result is what ComposeRepoView receives as the base, so the identity's
// base generation and the reader's base are the same content.
func (m *Materializer) assemble(
	ctx context.Context,
	route store_sqlite.CheckoutRoute,
	repoPrefix string,
	generations []int64,
	lease *Lease,
) (*RepoView, error) {
	handles := make([]*store_sqlite.Store, 0, len(generations))

	commitHandle, commitLayer, _, err := m.openGeneration(ctx, generations[0])
	if err != nil {
		return nil, err
	}
	handles = append(handles, commitHandle)
	base := graph.NewOverlaidViewWithLayer(m.Store.AtGeneration(0), commitLayer)

	var (
		layers    []graph.OverlayLayerReader
		layerRefs []LayerRef
	)
	for _, generationID := range generations[1:] {
		handle, layer, row, err := m.openGeneration(ctx, generationID)
		if err != nil {
			return nil, err
		}
		ref, err := dirtyLayerRef(row)
		if err != nil {
			return nil, err
		}
		handles = append(handles, handle)
		layers = append(layers, layer)
		layerRefs = append(layerRefs, ref)
	}

	id, err := NewRepoViewID(repoPrefix, route.GraphID, generations[0], layerRefs...)
	if err != nil {
		return nil, err
	}
	reader, id, err := ComposeRepoView(base, layers, id)
	if err != nil {
		return nil, err
	}
	completeness, err := m.completeness(handles)
	if err != nil {
		return nil, err
	}
	return &RepoView{
		ID:           id,
		Reader:       reader,
		Completeness: completeness,
		generations:  generations,
		lease:        lease,
	}, nil
}

// openGeneration checks that one routed generation can be served and
// returns the handle pinned to it, the layer over that handle, and the
// catalog row the identity is built from.
func (m *Materializer) openGeneration(ctx context.Context, generationID int64) (
	*store_sqlite.Store, *GenerationLayer, store_sqlite.ViewGeneration, error,
) {
	row, err := m.servableGeneration(ctx, generationID)
	if err != nil {
		return nil, nil, row, err
	}
	handle := m.Store.AtGeneration(generationID)
	layer, err := NewGenerationLayer(handle)
	if err != nil {
		return nil, nil, row, WrapViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("open generation %d", generationID), err)
	}
	return handle, layer, row, nil
}

// servableGeneration reads one routed generation and reports whether it
// can be served.
//
// ready and superseded both serve: superseded says only that a newer
// generation exists, and the route — not the newness of a generation —
// decides what a checkout reads. building is the retryable case a caller
// polls. Anything else is content that is going away or never arrived,
// and serving from it would mean answering out of a payload the sweep
// may already be deleting.
func (m *Materializer) servableGeneration(ctx context.Context, generationID int64) (store_sqlite.ViewGeneration, error) {
	row, found, err := m.Catalog.GetViewGeneration(ctx, generationID)
	if err != nil {
		return row, WrapViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("read generation %d", generationID), err)
	}
	if !found {
		return row, NewViewError(CodeViewBuilding,
			fmt.Sprintf("generation %d is not in the catalog", generationID))
	}
	switch row.State {
	case store_sqlite.ViewGenerationReady, store_sqlite.ViewGenerationSuperseded:
		return row, nil
	case store_sqlite.ViewGenerationBuilding:
		return row, NewViewError(CodeViewBuilding,
			fmt.Sprintf("generation %d is still building", generationID))
	default:
		return row, NewViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("generation %d is %s", generationID, string(row.State)))
	}
}

// dirtyLayerRef names the working-tree layer of a checkout's stack. A
// generation that names no layer cannot be identified, and an identity
// that cannot be built is a view that cannot be cached or compared.
func dirtyLayerRef(row store_sqlite.ViewGeneration) (LayerRef, error) {
	if row.LayerID == "" {
		return LayerRef{}, NewViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("generation %d names no layer", row.GenerationID))
	}
	ref := LayerRef{Kind: LayerDirty, LayerID: row.LayerID, Generation: row.GenerationID}
	if err := ref.Validate(); err != nil {
		return LayerRef{}, err
	}
	return ref, nil
}

// completeness unions the producer states of the whole stack, bottom
// generation first, so the topmost generation that declares a producer
// decides the view's state for it.
//
// The union starts from the base corpus, which declares nothing and is
// complete for everything. That is not an omission: a producer row is
// written per derived generation, and the corpus underneath them is a
// plain whole index, so its contribution to any capability is complete
// by construction and there is no row to read — SetProducerState refuses
// a base handle for exactly that reason. Only a generation stacked on it
// can narrow a capability, which is the direction the union runs, and a
// generation that leaves a capability out is saying it did not touch it
// rather than that it withdrew it.
//
// A producer that does not name a known capability is skipped rather
// than recorded: producer names are a build-side vocabulary and the ones
// that do not correspond to something a caller can require are build
// stages, not answers a view offers.
func (m *Materializer) completeness(generations []*store_sqlite.Store) (Completeness, error) {
	known := KnownCapabilities()
	out := make(Completeness, len(known))
	for _, id := range known {
		out[id] = StateComplete
	}
	for _, handle := range generations {
		rows, err := handle.ProducerStates()
		if err != nil {
			return nil, WrapViewError(CodeCheckoutInaccessible,
				fmt.Sprintf("read producer states of generation %d", handle.ViewGeneration()), err)
		}
		for _, row := range rows {
			id := CapabilityID(row.Producer)
			state := capabilityStateOf(row.State)
			if !id.Valid() || !state.Valid() {
				continue
			}
			out[id] = state
		}
	}
	return out, nil
}

// capabilityStateOf maps a producer's contribution state onto what a
// caller sees. The two vocabularies are one-to-one: a producer is the
// thing that populates a capability, so how far along it is IS how far
// along the capability is.
func capabilityStateOf(state store_sqlite.ProducerState) CapabilityState {
	switch state {
	case store_sqlite.ProducerStateComplete:
		return StateComplete
	case store_sqlite.ProducerStateIncomplete:
		return StateIncomplete
	case store_sqlite.ProducerStateBuilding:
		return StateBuilding
	case store_sqlite.ProducerStateUnavailable:
		return StateUnavailable
	case store_sqlite.ProducerStateDisabledByConfig:
		return StateDisabledByConfig
	default:
		return CapabilityState("")
	}
}
