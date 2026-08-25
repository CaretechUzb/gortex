package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/frameworkgate"
	"github.com/zzet/gortex/internal/resolver"
)

// Framework inventory.
//
// index.frameworks.allow narrows Gortex's framework intelligence, but
// until this analyzer there was nowhere to look up what may go in that
// list. analyze kind=route_frameworks covers only the extract-time route
// registry; analyze kind=synthesizers reports passes that PRODUCED edges,
// so a pass excluded by config simply vanishes from it rather than
// showing up as excluded. Neither lists the claiming resolvers at all.
//
// This analyzer is the single answer to "what can I allow?": a registry
// read across all three layers plus a config read, with no graph walk.

// frameworkLayer names the registry a pass belongs to.
const (
	frameworkLayerRoute       = "route"
	frameworkLayerSynthesizer = "synthesizer"
	frameworkLayerClaiming    = "claiming"
)

type frameworkInventoryRow struct {
	Name string `json:"name"`
	// Layers lists every registry that registers this name. A framework
	// whose route pass and synthesizer share a name — odoo, or django
	// and its django-descriptor claimer — is admitted at every layer by
	// one allow-list entry, so the layers belong on one row rather than
	// split across several.
	Layers    []string `json:"layers"`
	Languages []string `json:"languages,omitempty"`
	// Active reports whether the pass may run. With no allow-list
	// configured anywhere every pass is active.
	Active bool `json:"active"`
	// AllowedIn names the repositories whose allow-list admits this
	// pass, and is populated only when repositories disagree — the case
	// where the effective behaviour is otherwise hard to predict.
	AllowedIn []string `json:"allowed_in,omitempty"`
}

// handleAnalyzeFrameworks lists every registered framework pass across
// the three registries — route passes, dispatch synthesizers and claiming
// resolvers — with whether the current configuration admits it. It is the
// discovery surface for index.frameworks.allow.
func (s *Server) handleAnalyzeFrameworks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rows := s.frameworkInventory()

	var excluded int
	for _, r := range rows {
		if !r.Active {
			excluded++
		}
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			state := "active"
			if !r.Active {
				state = "excluded"
			}
			fmt.Fprintf(&b, "%-28s %-32s %-9s", r.Name, strings.Join(r.Layers, "+"), state)
			if len(r.Languages) > 0 {
				fmt.Fprintf(&b, " %v", r.Languages)
			}
			if len(r.AllowedIn) > 0 {
				fmt.Fprintf(&b, " allowed_in=%v", r.AllowedIn)
			}
			b.WriteString("\n")
		}
		if len(rows) == 0 {
			b.WriteString("no registered frameworks\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"frameworks":      rows,
		"total":           len(rows),
		"excluded":        excluded,
		"config_key":      "index.frameworks.allow",
		"unknown_allowed": s.unknownAllowedFrameworks(rows),
	})
}

// frameworkInventory unions the three registries and folds in the
// per-repository allow-lists.
func (s *Server) frameworkInventory() []frameworkInventoryRow {
	byName := map[string]*frameworkInventoryRow{}
	add := func(name, layer string, langs []string) {
		row := byName[name]
		if row == nil {
			row = &frameworkInventoryRow{Name: name, Active: true}
			byName[name] = row
		}
		row.Layers = append(row.Layers, layer)
		if len(langs) > 0 {
			row.Languages = langs
		}
	}
	for _, p := range contracts.RegisteredFrameworkRoutePasses() {
		add(p.Name(), frameworkLayerRoute, p.Languages())
	}
	for _, name := range resolver.RegisteredFrameworkSynthesizerNames() {
		add(name, frameworkLayerSynthesizer, nil)
	}
	for _, name := range resolver.RegisteredClaimingResolverNames() {
		add(name, frameworkLayerClaiming, nil)
	}

	perRepo := s.frameworkAllowListsByRepo()
	for _, row := range byName {
		row.Active, row.AllowedIn = frameworkAdmission(row.Name, perRepo)
	}

	out := make([]frameworkInventoryRow, 0, len(byName))
	for _, row := range byName {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// frameworkAllowListsByRepo returns each tracked repository's allow-list.
// A repository with no list is present with an unset Set, which allows
// everything — that distinction is what makes the union rule visible.
func (s *Server) frameworkAllowListsByRepo() map[string]frameworkgate.Set {
	if s.configManager == nil {
		return nil
	}
	out := map[string]frameworkgate.Set{}
	for _, prefix := range s.configManager.WorkspacePrefixes() {
		if cfg := s.configManager.GetRepoConfig(prefix); cfg != nil {
			out[prefix] = cfg.Index.AllowedFrameworks()
		}
	}
	return out
}

// frameworkAdmission applies the workspace union: a pass runs when any
// repository allows it, matching the synthesis pass's own rule. AllowedIn
// is reported only when the repositories disagree, since listing every
// repository for every pass in the coherent case is noise.
func frameworkAdmission(name string, perRepo map[string]frameworkgate.Set) (active bool, allowedIn []string) {
	if len(perRepo) == 0 {
		return true, nil
	}
	var allowing, restricting []string
	for prefix, set := range perRepo {
		if set.Allows(name) {
			allowing = append(allowing, prefix)
		}
		if set.Configured() {
			restricting = append(restricting, prefix)
		}
	}
	active = len(allowing) > 0
	if !active || len(restricting) == 0 || len(allowing) == len(perRepo) {
		return active, nil
	}
	sort.Strings(allowing)
	return active, allowing
}

// unknownAllowedFrameworks reports allow-list entries naming no
// registered pass. On an allow-list a typo does not merely fail to take
// effect — it silently drops the framework it meant to keep — so the
// inventory surfaces it next to the names that would have worked.
func (s *Server) unknownAllowedFrameworks(rows []frameworkInventoryRow) []string {
	perRepo := s.frameworkAllowListsByRepo()
	if len(perRepo) == 0 {
		return nil
	}
	known := make([]string, 0, len(rows))
	for _, r := range rows {
		known = append(known, r.Name)
	}
	seen := map[string]bool{}
	var out []string
	for _, set := range perRepo {
		for _, name := range set.Unknown(known) {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}
