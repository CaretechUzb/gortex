package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// requestViewCtxKey carries the view decision made for one `tools/call`.
// Unexported so nothing outside this package can smuggle a reader onto an
// unrelated context.
type requestViewCtxKey struct{}

// requestView is what one request reads through, plus what the response says
// about it.
type requestView struct {
	// reader is the composed routed stack. Nil means the base corpus serves,
	// which is what every request read before routed views existed.
	reader graph.Reader
	// materialized is the leased view behind reader, released on request end.
	materialized *graphview.RepoView
	// candidates and content are the stack expressed as search corpora —
	// what reader cannot serve, since no composition carries an index. Both
	// are bound once at materialization; see bindSources.
	candidates []query.ViewLayerSource
	content    *viewContentSearcher
	// rider travels on the response whenever the caller named a view or
	// something other than the base answered.
	rider *graphview.ViewRider
	// files is the committed-tree file surface a view with no working copy
	// reads through. Nil for every view that has one.
	files *refViewFiles
	// viewRoot is the working copy this view's content is on disk at: the
	// routed checkout's root. Empty for a view of a committed tree, which is
	// the whole difference a filesystem-backed capability turns on.
	viewRoot string

	// mu guards the annotations the request collects while it runs. The
	// capability evaluation writes before the handler starts, but a handler
	// may annotate from a goroutine it fans out to.
	mu sync.Mutex
	// degraded lists capabilities this view does not serve completely that
	// the request did not require. They never fail it — they ride back on
	// the rider so a thin answer is legible as one.
	degraded []graphview.CapabilityStatus
	// baseScoped lists capabilities a base-scoped engine answered while
	// this view served the request.
	baseScoped []graphview.CapabilityID
}

// completeness is what the materialized view can answer, nil for a request
// the base corpus serves.
func (v *requestView) completeness() graphview.Completeness {
	if v == nil || v.materialized == nil {
		return nil
	}
	return v.materialized.Completeness
}

// noteDegraded records capabilities that shaped the answer without failing
// the request.
func (v *requestView) noteDegraded(statuses []graphview.CapabilityStatus) {
	if v == nil || len(statuses) == 0 {
		return
	}
	v.mu.Lock()
	v.degraded = append(v.degraded, statuses...)
	v.mu.Unlock()
}

// noteBaseScoped records capabilities a base-scoped engine answered.
func (v *requestView) noteBaseScoped(caps []graphview.CapabilityID) {
	if v == nil || len(caps) == 0 {
		return
	}
	v.mu.Lock()
	v.baseScoped = mergeCapabilities(v.baseScoped, caps)
	v.mu.Unlock()
}

// annotations reports what the request collected, for the rider.
func (v *requestView) annotations() ([]graphview.CapabilityStatus, []graphview.CapabilityID) {
	if v == nil {
		return nil, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.degraded), slices.Clone(v.baseScoped)
}

// routed reports whether a composed checkout view — rather than the base
// corpus — answers this request.
func (v *requestView) routed() bool { return v != nil && v.reader != nil }

// close releases the generations the view leased and the git child its file
// surface holds. Idempotent and nil-safe.
func (v *requestView) close() {
	if v == nil {
		return
	}
	v.files.close()
	v.materialized.Close()
}

func withRequestView(ctx context.Context, v *requestView) context.Context {
	if ctx == nil || v == nil {
		return ctx
	}
	return context.WithValue(ctx, requestViewCtxKey{}, v)
}

func requestViewFromContext(ctx context.Context) *requestView {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(requestViewCtxKey{}).(*requestView)
	return v
}

// SetMaterializer wires the routed-view materializer built over the shared
// store's catalog. Passing nil (or never calling this) leaves every request
// on the base corpus: the view argument then reports the capability as
// unavailable instead of quietly answering from somewhere else.
func (s *Server) SetMaterializer(m *graphview.Materializer) {
	if s == nil {
		return
	}
	s.materializer = m
}

// Materializer returns the routed-view materializer the server reads through,
// nil when the backend carries no view catalog. It is how a caller that also
// owns retirement can check that both sides pin generations with the same
// lease manager.
func (s *Server) Materializer() *graphview.Materializer {
	if s == nil {
		return nil
	}
	return s.materializer
}

// viewArgName is the request-level argument every tool honours. It is read
// and stripped by the request middleware, so no handler sees it and no tool
// schema has to declare it.
const viewArgName = "view"

// viewSelectorFields are the object keys a view argument may carry. Anything
// else is a typo the caller must see, not a field to ignore — an ignored
// selector field would silently answer about a different view.
var viewSelectorFields = map[string]bool{
	"kind": true, "graph_id": true, "checkout_id": true, "value": true,
}

// takeViewSelector pulls the structured view argument off the request and
// removes it from the argument map.
//
// It runs before parameter reconciliation so the alias matcher cannot rewrite
// `view` into some tool's own similarly-named parameter, and before any
// handler runs so every read tool honours the selector without per-tool
// plumbing.
func takeViewSelector(req *mcp.CallToolRequest) (graphview.Selector, error) {
	auto := graphview.Selector{Kind: graphview.SelectorAuto}
	if req == nil {
		return auto, nil
	}
	args, ok := req.Params.Arguments.(map[string]any)
	if !ok {
		return auto, nil
	}
	raw, present := args[viewArgName]
	if !present {
		return auto, nil
	}
	delete(args, viewArgName)
	if raw == nil {
		return auto, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return graphview.Selector{}, graphview.NewViewError(graphview.CodeInvalidViewSelector,
			"the view argument must be an object naming a kind")
	}
	fields := make(map[string]string, len(obj))
	for name, value := range obj {
		if !viewSelectorFields[name] {
			return graphview.Selector{}, graphview.NewViewError(graphview.CodeInvalidViewSelector,
				fmt.Sprintf("the view argument has no field %q", name))
		}
		text, ok := value.(string)
		if !ok {
			return graphview.Selector{}, graphview.NewViewError(graphview.CodeInvalidViewSelector,
				fmt.Sprintf("view field %q must be a string", name))
		}
		fields[name] = text
	}
	return graphview.ParseSelector(fields["kind"], fields["graph_id"], fields["checkout_id"], fields["value"])
}

// resolveRequestView decides what this request reads through.
//
// Precedence is explicit selector, then the session's cwd binding, then the
// base corpus. An explicit selector fails loudly; a cwd binding that cannot
// be served falls back to the base and says so on the response.
//
// Materialization is per request. Caching it across requests needs the route
// epoch as the key — a route flip has to invalidate the cached stack — and
// that is the optimization this deliberately leaves for later.
func (s *Server) resolveRequestView(ctx context.Context, selector graphview.Selector) (*requestView, error) {
	view, err := s.selectRequestView(ctx, selector)
	s.recordRequestView(view, err)
	return view, err
}

// recordRequestView counts what answered this request, and logs the ones that
// did not answer what was asked for.
//
// The counter is by view kind and — for an inexact answer — by the code that
// explains the substitution; neither carries a checkout, a ref or a
// fingerprint. The log line beside a fallback carries all three, because "the
// worktree lane fell back to base 40 times" is only actionable once you know
// which worktree and which generation stack it was trying to reach.
func (s *Server) recordRequestView(view *requestView, err error) {
	if err != nil {
		return
	}
	viewmetrics.Count(viewmetrics.RequestServedTotal, requestViewKind(view))
	if view == nil || view.rider == nil || view.rider.Exact {
		return
	}
	reason := viewmetrics.FallbackReasonCode(view.rider.FallbackReason)
	viewmetrics.Count(viewmetrics.RequestFallbackTotal, reason)
	if s.logger == nil {
		return
	}
	fields := []zap.Field{
		zap.String("requested_view", view.rider.RequestedView),
		zap.String("actual_view", view.rider.ActualView),
		zap.String("reason", reason),
		zap.String("detail", view.rider.FallbackReason),
	}
	if view.rider.GraphID != "" {
		fields = append(fields, zap.String("graph", view.rider.GraphID))
	}
	if view.rider.CheckoutID != "" {
		fields = append(fields, zap.String("checkout", view.rider.CheckoutID))
	}
	if view.rider.ViewFingerprint != "" {
		fields = append(fields, zap.String("view_fingerprint", view.rider.ViewFingerprint))
	}
	if view.materialized != nil {
		fields = append(fields, zap.Int64s("generations", view.materialized.Generations()))
	}
	if view.rider.BuildToken != "" {
		fields = append(fields, zap.String("build_token", view.rider.BuildToken))
	}
	s.logger.Debug("view routing: served a fallback view", fields...)
}

// requestViewKind names the shape of view that answered: the indexed corpus,
// a routed working copy, or a committed tree. The file surface is what tells
// the last two apart — only a view with no working copy reads its bytes out of
// the object store.
func requestViewKind(view *requestView) string {
	switch {
	case view == nil || view.reader == nil:
		return viewmetrics.ViewBase
	case view.files != nil:
		return viewmetrics.ViewRef
	default:
		return viewmetrics.ViewWorktree
	}
}

// selectRequestView is resolveRequestView's decision, split out so the
// recording above wraps every path through it exactly once.
func (s *Server) selectRequestView(ctx context.Context, selector graphview.Selector) (*requestView, error) {
	if s == nil || s.materializer == nil {
		if selector.Kind == graphview.SelectorAuto {
			return nil, nil
		}
		return nil, graphview.NewViewError(graphview.CodeCapabilityUnavailable,
			"this store carries no view catalog, so only the automatic view can be served")
	}
	switch selector.Kind {
	case graphview.SelectorAuto:
		return s.viewForSessionCWD(ctx)
	case graphview.SelectorWorktree:
		return s.viewForWorktreeSelector(ctx, selector)
	case graphview.SelectorBase:
		return s.viewForBaseSelector(ctx, selector)
	case graphview.SelectorGitRef, graphview.SelectorCommit:
		return s.viewForRefSelector(ctx, selector)
	default:
		return nil, graphview.NewViewError(graphview.CodeCapabilityUnavailable,
			fmt.Sprintf("a %s selector names a view no builder produces yet", string(selector.Kind)))
	}
}

// viewForSessionCWD binds the session's working directory to a registered
// checkout and routes the request to that checkout's composed view.
//
// Only an automatic checkout is routed here. A dedicated checkout and the
// family's primary are served from the indexed corpus, which is exactly what
// the base path already does for them.
func (s *Server) viewForSessionCWD(ctx context.Context) (*requestView, error) {
	cwd := SessionCWDFromContext(ctx)
	if cwd == "" {
		return nil, nil
	}
	checkout, found, err := graphview.CheckoutForPath(ctx, s.materializer.Catalog, s.viewFamilies(ctx), cwd)
	if err != nil {
		// The binding is an optimization over the base corpus, not a
		// precondition for answering: a catalog that cannot be read still
		// answers from the base. The cwd may well sit inside a routed
		// checkout, so the degradation rides on the response rather than
		// passing for an exact answer.
		if s.logger != nil {
			s.logger.Debug("view routing: could not bind the session cwd to a checkout", zap.Error(err))
		}
		return viewFallback(false, graphview.NewViewRider(graphview.Selector{Kind: graphview.SelectorAuto}), err)
	}
	if !found || !graphview.ServesAutomaticView(checkout) {
		return nil, nil
	}
	requested := graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: checkout.CheckoutID}
	return s.materializeRequestView(ctx, requested, checkout, false)
}

// viewForWorktreeSelector serves an explicitly named checkout. Every refusal
// is reported with its own code: the caller asked for one specific view and
// must never be handed a different one.
func (s *Server) viewForWorktreeSelector(ctx context.Context, selector graphview.Selector) (*requestView, error) {
	catalog := s.materializer.Catalog
	checkout, found, err := catalog.GetCheckout(ctx, selector.CheckoutID)
	switch {
	case err != nil:
		return nil, graphview.WrapViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("read checkout %q", selector.CheckoutID), err)
	case !found:
		return nil, graphview.NewViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("checkout %q is not registered", selector.CheckoutID))
	}
	if err := s.checkoutInSessionScope(ctx, checkout); err != nil {
		return nil, err
	}
	if checkout.State != store_sqlite.CheckoutStateReady {
		return nil, graphview.NewViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("checkout %q is %s", checkout.CheckoutID, string(checkout.State)))
	}
	if err := s.familyHasPrimary(ctx, checkout.FamilyID); err != nil {
		return nil, err
	}
	return s.materializeRequestView(ctx, selector, checkout, true)
}

// viewForBaseSelector pins the request to a named base graph. A dedicated
// graph is read from the indexed corpus, so the selector's work is proving
// the graph exists and is ready — and naming it on the response.
func (s *Server) viewForBaseSelector(ctx context.Context, selector graphview.Selector) (*requestView, error) {
	dedicated, found, err := s.materializer.Catalog.GetDedicatedGraph(ctx, selector.GraphID)
	switch {
	case err != nil:
		return nil, graphview.WrapViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("read graph %q", selector.GraphID), err)
	case !found:
		return nil, graphview.NewViewError(graphview.CodeInvalidViewSelector,
			fmt.Sprintf("graph %q is not registered", selector.GraphID))
	}
	// The scope ceiling is checked before the state, so a session outside the
	// workspace cannot tell a building graph from a ready one in a sibling
	// workspace. This is the order the worktree selector holds to.
	if err := s.repoPrefixInSessionScope(ctx, dedicated.RepoPrefix, selector.GraphID); err != nil {
		return nil, err
	}
	if dedicated.State != reconcile.GraphStateReady {
		return nil, graphview.NewViewError(graphview.CodeViewBuilding,
			fmt.Sprintf("graph %q is %s", selector.GraphID, dedicated.State))
	}
	rider := graphview.NewViewRider(selector)
	rider.MarkExact(selector.String())
	rider.GraphID = dedicated.GraphID
	return &requestView{rider: rider}, nil
}

// materializeRequestView turns a routed checkout into the reader that answers
// the request.
//
// strict separates the two callers. An explicit selector must fail rather than
// answer about something else; a cwd binding falls back to the base corpus and
// records why, so a half-built route degrades to today's answer instead of an
// error — and never silently.
func (s *Server) materializeRequestView(
	ctx context.Context,
	requested graphview.Selector,
	checkout store_sqlite.Checkout,
	strict bool,
) (*requestView, error) {
	rider := graphview.NewViewRider(requested)
	route, found, err := s.materializer.Catalog.GetCheckoutRoute(ctx, checkout.CheckoutID)
	switch {
	case err != nil:
		return viewFallback(strict, rider, graphview.WrapViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("read the route of checkout %q", checkout.CheckoutID), err))
	case !found || !graphview.RouteReady(route):
		return viewFallback(strict, rider, graphview.NewViewError(graphview.CodeViewBuilding,
			fmt.Sprintf("checkout %q is not fully routed yet", checkout.CheckoutID)))
	}
	view, err := s.materializer.MaterializeCheckout(ctx, checkout.CheckoutID)
	if err != nil {
		return viewFallback(strict, rider, err)
	}
	rider.MarkExact(requested.String())
	rider.GraphID = view.ID.BaseGraphID
	rider.CheckoutID = checkout.CheckoutID
	routed := &requestView{
		reader:       view.Reader,
		materialized: view,
		rider:        rider,
		viewRoot:     checkout.RootPath,
	}
	routed.bindSources(view.GenerationSources(), s.graph)
	return routed, nil
}

// viewFallback either propagates the failure (an explicit selector) or serves
// the base corpus with the reason recorded on the rider (a cwd binding).
func viewFallback(strict bool, rider *graphview.ViewRider, err error) (*requestView, error) {
	if strict {
		return nil, err
	}
	reason := graphview.CodeOf(err)
	if reason == "" {
		reason = graphview.CodeViewBuilding
	}
	if markErr := rider.MarkFallback(string(graphview.SelectorBase), reason); markErr != nil {
		return nil, markErr
	}
	return &requestView{rider: rider}, nil
}

// viewFamilies lists the checkout families the indexed corpus reaches, one
// per repository prefix it carries. The catalog indexes checkouts by family,
// so this is what turns a working directory into a checkout row.
func (s *Server) viewFamilies(ctx context.Context) []string {
	if s.graph == nil {
		return nil
	}
	prefixes := s.graph.RepoPrefixes()
	seen := make(map[string]bool, len(prefixes))
	out := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		dedicated, found, err := s.materializer.Catalog.GetDedicatedGraph(ctx, indexer.GraphIDFor(prefix))
		if err != nil || !found || dedicated.FamilyID == "" || seen[dedicated.FamilyID] {
			continue
		}
		seen[dedicated.FamilyID] = true
		out = append(out, dedicated.FamilyID)
	}
	return out
}

// familyHasPrimary refuses a family with no primary base graph. Every
// automatic checkout is served from the family's shared lane, so a family
// without one has nothing to compose a view over.
func (s *Server) familyHasPrimary(ctx context.Context, familyID string) error {
	graphs, err := s.materializer.Catalog.ListDedicatedGraphs(ctx, familyID)
	if err != nil {
		return graphview.WrapViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("list the graphs of family %q", familyID), err)
	}
	for _, dedicated := range graphs {
		if dedicated.IsPrimaryBase {
			return nil
		}
	}
	return graphview.NewViewError(graphview.CodeNoPrimary,
		fmt.Sprintf("family %q has no primary base graph", familyID))
}

// checkoutInSessionScope clamps an explicit selector to the repositories the
// calling session may see. Without it, naming a checkout id would reach
// across the workspace boundary every other query is held to.
func (s *Server) checkoutInSessionScope(ctx context.Context, checkout store_sqlite.Checkout) error {
	return s.repoPrefixInSessionScope(ctx, s.repoPrefixForCheckout(ctx, checkout), checkout.CheckoutID)
}

// repoPrefixInSessionScope reports whether the session may read a repository.
// An unbound session (no cwd, no multi-repo indexer) has no ceiling, which is
// the same posture every other scope consumer takes.
func (s *Server) repoPrefixInSessionScope(ctx context.Context, repoPrefix, subject string) error {
	repos, bound := s.sessionWorkspaceRepoSet(ctx)
	if !bound || len(repos) == 0 {
		return nil
	}
	if repoPrefix != "" && repos[repoPrefix] {
		return nil
	}
	return graphview.NewViewError(graphview.CodeSelectorOutOfScope,
		fmt.Sprintf("%q is outside this session's workspace", subject))
}

// repoPrefixForCheckout resolves the repository a checkout is served under:
// its own dedicated graph when it has one, and otherwise the family's primary
// base graph, which is the lane an automatic checkout reads through.
func (s *Server) repoPrefixForCheckout(ctx context.Context, checkout store_sqlite.Checkout) string {
	graphs, err := s.materializer.Catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
	if err != nil {
		return ""
	}
	primary := ""
	for _, dedicated := range graphs {
		if dedicated.OwnerCheckoutID == checkout.CheckoutID && dedicated.RepoPrefix != "" {
			return dedicated.RepoPrefix
		}
		if dedicated.IsPrimaryBase && dedicated.RepoPrefix != "" {
			primary = dedicated.RepoPrefix
		}
	}
	return primary
}

// refuseRoutedViewMutation blocks a source-mutating tool whose request reads
// through a routed view.
//
// The write tools resolve a path against the canonical checkout root, so a
// write issued while reading a worktree's view would land in the wrong
// working copy. Refusing is the honest answer until the write path learns to
// follow the view; editing an automatic worktree comes with that.
func (s *Server) refuseRoutedViewMutation(ctx context.Context, tool string) *mcp.CallToolResult {
	view := requestViewFromContext(ctx)
	if !view.routed() || !s.facades.mutatesSource(tool) {
		return nil
	}
	return mcp.NewToolResultError(fmt.Sprintf(
		"%s: this request reads through %s, and %s would write the canonical checkout instead of that one. "+
			"Read through the view; edit from the checkout's own working copy.",
		graphview.CodeViewReadOnly, view.rider.ActualView, tool))
}

// attachViewRider puts the view fields on the response, inside the freshness
// block every view-relevant answer already carries. It is the same rider
// channel, extended — a second block would let a client read one and miss the
// other.
func (s *Server) attachViewRider(ctx context.Context, res *mcp.CallToolResult) *mcp.CallToolResult {
	view := requestViewFromContext(ctx)
	if view == nil || view.rider == nil {
		return res
	}
	fields := map[string]any{
		"requested_view": view.rider.RequestedView,
		"actual_view":    view.rider.ActualView,
		"exact":          view.rider.Exact,
	}
	if view.rider.FallbackReason != "" {
		fields["fallback_reason"] = view.rider.FallbackReason
	}
	for name, value := range map[string]string{
		"view_fingerprint": view.rider.ViewFingerprint,
		"requested_ref":    view.rider.RequestedRef,
		"resolved_ref":     view.rider.ResolvedRef,
		"resolved_commit":  view.rider.ResolvedCommit,
		"resolved_tree":    view.rider.ResolvedTree,
		"build_token":      view.rider.BuildToken,
	} {
		if value != "" {
			fields[name] = value
		}
	}
	if view.rider.RetryAfter > 0 {
		fields["retry_after"] = view.rider.RetryAfter
	}
	// The capability annotations: what the view served thinly, and what a
	// base-scoped engine answered instead of the view. Both are omitted when
	// empty, so a request that hit neither carries exactly the fields it did
	// before either existed.
	if degraded, baseScoped := view.annotations(); len(degraded) > 0 || len(baseScoped) > 0 {
		if len(degraded) > 0 {
			fields["degraded_capabilities"] = degraded
		}
		if len(baseScoped) > 0 {
			fields["base_scoped"] = sortedCapabilityNames(baseScoped)
		}
	}
	text, ok := singleTextContent(res)
	if !ok || text == "" {
		return res
	}
	var asObj map[string]any
	if json.Unmarshal([]byte(text), &asObj) != nil {
		return res
	}
	rider, _ := asObj["freshness"].(map[string]any)
	if rider == nil {
		rider = make(map[string]any, len(fields))
	}
	for name, value := range fields {
		rider[name] = value
	}
	asObj["freshness"] = rider
	body, err := json.Marshal(asObj)
	if err != nil {
		return res
	}
	return rebuildTextResult(res, string(body))
}
