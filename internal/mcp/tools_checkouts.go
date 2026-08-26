package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/reconcile"
)

// The checkout administration surface.
//
// Five tools over one shared implementation: the lifecycle's read model
// answers the listings and the previews, and the lifecycle's own flows run the
// confirms. The CLI verbs call these same tools through the daemon, so there
// is one description of what a family looks like and one decision about what a
// destructive verb does — not one per front door.
//
// Every destructive tool is a preview by default. A call without `confirm`
// reads the catalog, returns what would happen, and writes nothing; only an
// explicit `confirm: true` runs the transaction. The one exception is an
// untrack whose plan keeps the checkout — that is not destructive, so it runs
// as it always has.

// registerCheckoutAdminTools registers the checkout administration tools:
// list_checkouts, set_primary_checkout, forget_checkout, reconcile_checkouts
// and explain_view.
func (s *Server) registerCheckoutAdminTools() {
	s.addTool(
		mcp.NewTool("list_checkouts",
			mcp.WithDescription("List the checkout families this daemon tracks. Each family reports its "+
				"primary corpus and epoch, its dedicated graphs, every registered working copy (mode, "+
				"state, both reconciler clocks with their deadlines, path evidence, route and whether a "+
				"build coordinator is live for it), and the named views rooted in its graphs. Reads the "+
				"catalog only — run reconcile_checkouts for a fresh look at the filesystem."),
			mcp.WithString("family", mcp.Description("Narrow to one family: a family id, a graph id, a repo prefix, or a path inside a tracked repository. Omit for every family.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleListCheckouts,
	)

	s.addTool(
		mcp.NewTool("set_primary_checkout",
			mcp.WithDescription("Make one corpus the base every automatic checkout of its family composes over. "+
				"Without confirm this previews the move: the incumbent, the family's epoch, whether the move "+
				"would be accepted, and every automatic checkout that has to rebuild its layers over the new base."),
			mcp.WithString("graph", mcp.Required(), mcp.Description("The corpus to promote: a graph id, a repo prefix, or a path inside a tracked repository.")),
			mcp.WithBoolean("confirm", mcp.Description("Run the move. Without it nothing is written.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleSetPrimaryCheckout,
	)

	s.addTool(
		mcp.NewTool("forget_checkout",
			mcp.WithDescription("Remove one checkout, its corpus and everything rooted in it. Unlike untrack_repository "+
				"this never demotes the checkout into the family's automatic lane — it is the deliberate removal. "+
				"Without confirm it previews the closure and writes nothing."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Path or repo prefix naming the checkout to forget")),
			mcp.WithBoolean("confirm", mcp.Description("Run the removal. Without it nothing is written.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleForgetCheckout,
	)

	s.addTool(
		mcp.NewTool("reconcile_checkouts",
			mcp.WithDescription("Reconcile checkout families against git and the filesystem now, instead of waiting "+
				"for the janitor: identities are confirmed or allocated, the availability and removal clocks move, "+
				"and the build coordinators are brought in line with the verdicts."),
			mcp.WithString("family", mcp.Description("Reconcile one family: a family id, a graph id, a repo prefix, or a path inside a tracked repository. Omit to reconcile every family the daemon knows.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleReconcileCheckouts,
	)

	s.addTool(
		mcp.NewTool("explain_view",
			mcp.WithDescription("Explain which graph answers for one filesystem path: the checkout the path binds to, "+
				"how that checkout is served, its route and the generations behind it — or, when no composed view "+
				"answers, the step in the chain that could not be taken and left the base corpus to answer."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Filesystem path to explain")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleExplainView,
	)
}

// handleListCheckouts answers the administrative read model.
func (s *Server) handleListCheckouts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.lifecycle == nil {
		return mcp.NewToolResultError("checkout lifecycle is not wired"), nil
	}
	overview, err := s.lifecycle.FamiliesOverview(ctx, req.GetString("family", ""))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return s.respondJSONOrTOON(ctx, req, overview)
}

// handleSetPrimaryCheckout previews or runs a primary move.
func (s *Server) handleSetPrimaryCheckout(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.lifecycle == nil {
		return mcp.NewToolResultError("checkout lifecycle is not wired"), nil
	}
	target, err := req.RequireString("graph")
	if err != nil {
		return mcp.NewToolResultError("graph is required"), nil
	}
	dedicated, err := s.lifecycle.ResolveGraph(ctx, target)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	preview, err := s.lifecycle.PreviewSetPrimary(ctx, dedicated.GraphID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	payload := map[string]any{
		"family_id":     preview.FamilyID,
		"graph_id":      preview.GraphID,
		"repo_prefix":   preview.RepoPrefix,
		"primary_epoch": preview.PrimaryEpoch,
		"ready":         preview.Ready,
	}
	if preview.CurrentGraphID != "" {
		payload["current_graph_id"] = preview.CurrentGraphID
		payload["current_repo_prefix"] = preview.CurrentRepoPrefix
	}
	if len(preview.Blockers) > 0 {
		payload["blockers"] = preview.Blockers
	}
	if len(preview.Dependents) > 0 {
		payload["dependents"] = renderDependents(preview.Dependents)
	}

	if !req.GetBool("confirm", false) {
		payload["status"] = "preview"
		payload["confirm_required"] = true
		payload["detail"] = "nothing was written; call set_primary_checkout again with confirm:true to move the family primary"
		return s.respondJSONOrTOON(ctx, req, payload)
	}

	result, err := s.lifecycle.SetPrimary(ctx, dedicated.GraphID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	payload["status"] = "primary_set"
	payload["rebuilt"] = result.Rebuilt
	if len(result.Stale) > 0 {
		payload["stale"] = result.Stale
		payload["stale_detail"] = "these checkouts kept the route they had; the next reconcile tries again"
	}
	if len(result.Errors) > 0 {
		messages := make([]string, 0, len(result.Errors))
		for _, e := range result.Errors {
			messages = append(messages, e.Error())
		}
		payload["errors"] = messages
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// handleForgetCheckout previews or runs a deliberate removal.
func (s *Server) handleForgetCheckout(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.lifecycle == nil {
		return mcp.NewToolResultError("checkout lifecycle is not wired"), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	preview, err := s.lifecycle.PreviewForget(ctx, path)
	if errors.Is(err, indexer.ErrCheckoutNotTracked) {
		return repoNotTrackedGuidance(path), nil
	}
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if !req.GetBool("confirm", false) {
		return s.respondJSONOrTOON(ctx, req, untrackPreviewPayload("forget", preview,
			"nothing was written; call forget_checkout again with confirm:true to remove it"))
	}
	result, err := s.lifecycle.ApplyUntrack(ctx, preview)
	if err != nil {
		return untrackFailure(path, err), nil
	}
	return s.respondJSONOrTOON(ctx, req, untrackResultPayload("forgotten", result))
}

// handleReconcileCheckouts forces a reconciliation pass.
func (s *Server) handleReconcileCheckouts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.lifecycle == nil {
		return mcp.NewToolResultError("checkout lifecycle is not wired"), nil
	}
	if selector := req.GetString("family", ""); selector != "" {
		familyID, err := s.lifecycle.ResolveFamilyID(ctx, selector)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		report, err := s.lifecycle.ReconcileFamily(ctx, familyID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"status":   "reconciled",
			"families": []map[string]any{renderFamilyReport(report)},
			// Counted for the family that was asked about, the way the
			// whole-daemon scope counts every family. Leaving it out renders a
			// family whose build loops are running as one running none.
			"coordinators": s.lifecycle.LiveCoordinators(familyID),
		})
	}

	sweep, err := s.lifecycle.Sweep(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	families := make([]map[string]any, 0, len(sweep.Reports))
	for _, report := range sweep.Reports {
		families = append(families, renderFamilyReport(report))
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"status":            "reconciled",
		"families":          families,
		"removed":           sweep.Removed,
		"coordinators":      sweep.Coordinators,
		"retired":           sweep.Retired,
		"ref_views_retired": sweep.RefViewsRetired,
	})
}

// handleExplainView walks one path's binding chain.
func (s *Server) handleExplainView(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.lifecycle == nil {
		return mcp.NewToolResultError("checkout lifecycle is not wired"), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	binding, err := s.lifecycle.ExplainView(ctx, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return s.respondJSONOrTOON(ctx, req, binding)
}

// destructiveUntrackPlan reports whether a plan removes anything a confirm has
// to be asked for. A plan that keeps the checkout — an eviction of a
// repository with no catalog identity, or a demotion into the family's
// automatic lane — is the ordinary untrack and runs as it always has.
func destructiveUntrackPlan(plan indexer.UntrackPlan) bool {
	switch plan {
	case indexer.UntrackPlanForget, indexer.UntrackPlanPrimaryClosure:
		return true
	default:
		return false
	}
}

// untrackPreviewPayload renders one previewed plan.
func untrackPreviewPayload(action string, preview indexer.UntrackPreview, detail string) map[string]any {
	payload := map[string]any{
		"status":           "preview",
		"action":           action,
		"plan":             string(preview.Plan),
		"prefix":           preview.Prefix,
		"accessible":       preview.Accessible,
		"is_primary":       preview.IsPrimary,
		"confirm_required": true,
		"detail":           detail,
	}
	for name, value := range map[string]string{
		"checkout_id": preview.CheckoutID,
		"family_id":   preview.FamilyID,
		"graph_id":    preview.GraphID,
	} {
		if value != "" {
			payload[name] = value
		}
	}
	if preview.IsPrimary {
		payload["sole_primary"] = preview.SolePrimary
		payload["primary_epoch"] = preview.PrimaryEpoch
	}
	if len(preview.Closure) > 0 {
		payload["closure"] = renderDependents(preview.Closure)
	}
	if len(preview.Preserved) > 0 {
		payload["preserved"] = renderDependents(preview.Preserved)
	}
	if len(preview.Blockers) > 0 {
		payload["blockers"] = preview.Blockers
	}
	return payload
}

// untrackResultPayload renders one executed plan.
func untrackResultPayload(status string, result indexer.UntrackResult) map[string]any {
	payload := map[string]any{
		"status":        status,
		"plan":          string(result.Plan),
		"prefix":        result.Prefix,
		"nodes_removed": result.NodesRemoved,
		"edges_removed": result.EdgesRemoved,
	}
	if result.Demoted {
		payload["demoted"] = true
	}
	if len(result.Revoked) > 0 {
		payload["revoked_intents"] = result.Revoked
	}
	if len(result.Dependents) > 0 {
		dependents := make([]string, 0, len(result.Dependents))
		for _, dep := range result.Dependents {
			dependents = append(dependents, dep.Detail)
		}
		payload["dependents"] = dependents
	}
	return payload
}

// renderDependents projects closure rows into the response.
func renderDependents(dependents []reconcile.Dependent) []map[string]string {
	out := make([]map[string]string, 0, len(dependents))
	for _, dep := range dependents {
		out = append(out, map[string]string{
			"kind":   string(dep.Kind),
			"id":     dep.ID,
			"detail": dep.Detail,
		})
	}
	return out
}

// renderFamilyReport projects one reconciliation verdict.
func renderFamilyReport(report reconcile.FamilyReport) map[string]any {
	checkouts := make([]map[string]any, 0, len(report.Checkouts))
	for _, entry := range report.Checkouts {
		row := map[string]any{
			"admin_name": entry.AdminName,
			"root_path":  entry.RootPath,
			"action":     string(entry.Action),
			"durable":    entry.Durable,
		}
		for name, value := range map[string]string{
			"checkout_id": entry.CheckoutID,
			"state":       string(entry.State),
			"disposition": string(entry.Classification.Disposition),
			"evidence":    string(entry.Classification.Evidence),
			"code":        entry.Classification.Code,
			"detail":      entry.Detail,
		} {
			if value != "" {
				row[name] = value
			}
		}
		checkouts = append(checkouts, row)
	}
	out := map[string]any{
		"family_id":        report.FamilyID,
		"common_dir":       report.CommonDir,
		"inventory_usable": report.InventoryUsable,
		"checkouts":        checkouts,
	}
	if report.PrimaryGraphID != "" {
		out["primary_graph_id"] = report.PrimaryGraphID
	}
	if report.Code != "" {
		out["code"] = report.Code
	}
	return out
}

// untrackFailure renders a lifecycle refusal the way the untrack tool always
// has: a non-revocable intent names the source still asking for the
// repository, and a blocked plan names the ways forward.
func untrackFailure(path string, err error) *mcp.CallToolResult {
	if errors.Is(err, reconcile.ErrIntentNotRevocable) {
		return mcp.NewToolResultError(fmt.Sprintf(
			"cannot untrack %s: it is still wanted by another tracking source (%v)", path, err))
	}
	return mcp.NewToolResultError(err.Error())
}
