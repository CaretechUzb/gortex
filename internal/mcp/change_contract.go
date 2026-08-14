package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic/lsp"
)

// change_contract is the one envelope every change source lowers into. Rather
// than a sibling verb per question (api_drift, refuse_gate, risk_guard, …) it
// runs one pipeline — LOWER → PREDICT → EVALUATE → SCORE → CLASSIFY → EMIT —
// and returns one verdict envelope. The analysis emits data the agent reads;
// a thin enforcement layer (a pretooluse hook) is the only thing that turns a
// `refuse` verdict into a block. The graph advises; it does not wall.

// changeVerdict is the top-level decision an agent (or hook) acts on.
type changeVerdict string

const (
	verdictAllow  changeVerdict = "allow"
	verdictWarn   changeVerdict = "warn"
	verdictRefuse changeVerdict = "refuse"
)

func verdictRank(v changeVerdict) int {
	switch v {
	case verdictRefuse:
		return 2
	case verdictWarn:
		return 1
	default:
		return 0
	}
}

// escalate returns the stricter of two verdicts.
func escalate(a, b changeVerdict) changeVerdict {
	if verdictRank(b) > verdictRank(a) {
		return b
	}
	return a
}

// verdictForSeverity maps a rule severity to the verdict it implies:
// error → refuse, warn → warn, info → annotate (allow).
func verdictForSeverity(sev string) changeVerdict {
	switch strings.ToLower(sev) {
	case "error", "critical":
		return verdictRefuse
	case "warn", "warning":
		return verdictWarn
	default:
		return verdictAllow
	}
}

// changeReason is one finding carrying its provenance (which rule family) and
// a confidence — a `warn` from a heuristic is not the same as a `refuse` from
// a compile-breaking caller, and the envelope says which.
type changeReason struct {
	Family     string  `json:"family"`
	Severity   string  `json:"severity"`
	Message    string  `json:"message"`
	Confidence float64 `json:"confidence"`
	Symbol     string  `json:"symbol,omitempty"`
}

// changeRisk is the SCORE stage output — PageRank·blast·class folded to 0..100.
type changeRisk struct {
	Score      int     `json:"score"`
	Tier       string  `json:"tier"` // low | medium | high
	PageRank   float64 `json:"pagerank,omitempty"`
	BlastSize  int     `json:"blast_size"`
	LowerBound bool    `json:"lower_bound,omitempty"`
}

// changedSymbolRef is a thin symbol descriptor carried in the envelope.
type changedSymbolRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	File string `json:"file"`
}

// editStrategy is the "here is the safe path" remedy — refuse + remedy in one
// reply. Populated for symbol/range/edit sources where a single dominant
// changed symbol has a recognised refactor shape.
type editStrategy struct {
	Technique string   `json:"technique,omitempty"`
	Steps     []string `json:"steps,omitempty"`
	CCImpact  string   `json:"cc_impact,omitempty"`
	Safety    []string `json:"safety_signals,omitempty"`
}

// changeEnvelope is the packaged verdict — the output contract of the whole
// pipeline.
type changeEnvelope struct {
	Verdict             changeVerdict      `json:"verdict"`
	Source              string             `json:"source"`
	Classification      string             `json:"classification"`
	ChangedSymbols      []changedSymbolRef `json:"changed_symbols"`
	Reasons             []changeReason     `json:"reasons"`
	Risk                changeRisk         `json:"risk"`
	Blast               map[string]any     `json:"blast,omitempty"`
	VerificationCommand string             `json:"verification_command,omitempty"`
	StopCondition       string             `json:"stop_condition,omitempty"`
	EditStrategy        *editStrategy      `json:"edit_strategy,omitempty"`
	APISurface          []apiSurfaceEntry  `json:"api_surface,omitempty"`
}

// prediction is the normalised PREDICT-stage result. step is non-nil only for
// the workspace_edit source (a true speculative simulation); the other sources
// fill blast/impact from the change set without an edit to apply.
type prediction struct {
	source       string
	lens         string
	riskGate     bool
	changed      []changedSymbolRef
	changedIDs   []string
	nodes        []*graph.Node
	step         *simulationStep
	impact       *analysis.ImpactResult
	touchedFiles []string
}

// nodesForIDs resolves symbol IDs to graph nodes, dropping any that no longer
// exist.
func (s *Server) nodesForIDs(ids []string) []*graph.Node {
	if s.graph == nil {
		return nil
	}
	out := make([]*graph.Node, 0, len(ids))
	for _, id := range ids {
		if n := s.graph.GetNode(id); n != nil {
			out = append(out, n)
		}
	}
	return out
}

func refFromNode(n *graph.Node) changedSymbolRef {
	return changedSymbolRef{ID: n.ID, Name: n.Name, Kind: string(n.Kind), File: n.FilePath}
}

// lowerChange dispatches on the requested source and returns a normalised
// prediction. source "auto" picks the most specific input present.
func (s *Server) lowerChange(ctx context.Context, req mcp.CallToolRequest) (*prediction, error) {
	source := strings.ToLower(strings.TrimSpace(req.GetString("source", "auto")))
	if source == "auto" {
		switch {
		case strings.TrimSpace(req.GetString("workspace_edit", "")) != "":
			source = "edit"
		case strings.TrimSpace(req.GetString("ranges", "")) != "" || strings.TrimSpace(req.GetString("path", "")) != "":
			source = "ranges"
		case strings.TrimSpace(req.GetString("symbols", "")) != "":
			source = "symbols"
		default:
			source = "diff"
		}
	}

	var p *prediction
	var err error
	switch source {
	case "edit":
		p, err = s.lowerEditSource(ctx, req)
	case "ranges":
		p, err = s.lowerRangeSource(ctx, req)
	case "symbols":
		p, err = s.lowerSymbolSource(ctx, req)
	case "diff":
		p, err = s.lowerDiffSource(ctx, req)
	default:
		return nil, fmt.Errorf("unknown source %q (want auto|edit|diff|symbols|ranges)", source)
	}
	if err != nil {
		return nil, err
	}
	p.lens = strings.ToLower(strings.TrimSpace(req.GetString("lens", "")))
	p.riskGate = riskGateEnabled(req)
	return p, nil
}

// lowerEditSource runs a real speculative simulation of a WorkspaceEdit.
func (s *Server) lowerEditSource(ctx context.Context, req mcp.CallToolRequest) (*prediction, error) {
	raw, err := req.RequireString("workspace_edit")
	if err != nil {
		return nil, fmt.Errorf("source=edit requires workspace_edit")
	}
	edit, perr := parseWorkspaceEdit(raw)
	if perr != nil {
		return nil, fmt.Errorf("invalid workspace_edit: %w", perr)
	}
	if isEmptyEdit(edit) {
		return nil, fmt.Errorf("workspace_edit contains no document changes")
	}
	sim, serr := s.buildSimulation(ctx, []lsp.WorkspaceEdit{edit}, false)
	if serr != nil {
		return nil, serr
	}
	step := sim.steps[0]

	ids := append([]string{}, step.symbolsAdded...)
	ids = append(ids, step.symbolsRemoved...)
	for _, r := range step.symbolsRenamed {
		if v := r["old"]; v != "" {
			ids = append(ids, v)
		}
		if v := r["new"]; v != "" {
			ids = append(ids, v)
		}
	}
	// Edited ranges that did not add/remove a symbol still touch their
	// enclosing symbol — lower the edit's ranges so a body change counts.
	loweredEdit := s.lowerWorkspaceEditRanges(ctx, edit)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(loweredEdit.saturated) > 0 {
		return nil, saturatedRangeLoweringError(loweredEdit.saturated)
	}
	for _, h := range loweredEdit.hits {
		ids = append(ids, h.ID)
	}
	ids = dedupeStrings(ids)

	nodes := s.nodesForIDs(ids)
	changed := make([]changedSymbolRef, 0, len(nodes))
	for _, n := range nodes {
		changed = append(changed, refFromNode(n))
	}
	return &prediction{
		source:       "edit",
		changed:      changed,
		changedIDs:   ids,
		nodes:        nodes,
		step:         &step,
		impact:       s.analyzeImpactLazy(ctx, ids),
		touchedFiles: step.touchedFiles,
	}, nil
}

// lowerWorkspaceEditRanges maps each TextEdit's range to its enclosing symbols.
func (s *Server) lowerWorkspaceEditRanges(ctx context.Context, edit lsp.WorkspaceEdit) loweredRanges {
	fileEdits, err := s.groupEditByFile(edit)
	if err != nil {
		return loweredRanges{}
	}
	var specs []rangeSpec
	for _, fe := range fileEdits {
		target := fe.absPath
		if target == "" {
			target = fe.overlayPath
		}
		for _, te := range fe.edits {
			// LSP positions are 0-based; graph lines are 1-based.
			specs = append(specs, rangeSpec{
				File:      target,
				StartLine: te.Range.Start.Line + 1,
				EndLine:   te.Range.End.Line + 1,
			})
		}
	}
	return s.lowerRangesDetailedContext(ctx, specs)
}

func saturatedRangeLoweringError(paths []string) error {
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	return fmt.Errorf("bounded range ownership is unavailable for: %s", strings.Join(dedupeStrings(paths), ", "))
}

func (s *Server) lowerRangeSource(ctx context.Context, req mcp.CallToolRequest) (*prediction, error) {
	specs, err := parseRangeSpecs(req)
	if err != nil {
		return nil, err
	}
	lowered := s.lowerRangesDetailedContext(ctx, specs)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(lowered.saturated) > 0 {
		return nil, saturatedRangeLoweringError(lowered.saturated)
	}
	hits := lowered.hits
	ids := make([]string, 0, len(hits))
	changed := make([]changedSymbolRef, 0, len(hits))
	files := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ID)
		changed = append(changed, changedSymbolRef{ID: h.ID, Name: h.Name, Kind: h.Kind, File: h.File})
		files = append(files, h.File)
	}
	ids = dedupeStrings(ids)
	return &prediction{
		source:       "ranges",
		changed:      changed,
		changedIDs:   ids,
		nodes:        s.nodesForIDs(ids),
		impact:       s.analyzeImpactLazy(ctx, ids),
		touchedFiles: dedupeStrings(files),
	}, nil
}

func (s *Server) lowerSymbolSource(ctx context.Context, req mcp.CallToolRequest) (*prediction, error) {
	ids := splitCSV(req.GetString("symbols", ""))
	if len(ids) == 0 {
		return nil, fmt.Errorf("source=symbols requires a comma-separated `symbols` list")
	}
	ids = dedupeStrings(ids)
	nodes := s.nodesForIDs(ids)
	changed := make([]changedSymbolRef, 0, len(nodes))
	files := make([]string, 0, len(nodes))
	for _, n := range nodes {
		changed = append(changed, refFromNode(n))
		files = append(files, n.FilePath)
	}
	return &prediction{
		source:       "symbols",
		changed:      changed,
		changedIDs:   ids,
		nodes:        nodes,
		impact:       s.analyzeImpactLazy(ctx, ids),
		touchedFiles: dedupeStrings(files),
	}, nil
}

func (s *Server) lowerDiffSource(ctx context.Context, req mcp.CallToolRequest) (*prediction, error) {
	scope := strings.TrimSpace(req.GetString("scope", "unstaged"))
	base := strings.TrimSpace(req.GetString("base", ""))
	if base != "" && (scope == "" || scope == "unstaged") {
		scope = "compare"
	}
	if scope == "" {
		scope = "unstaged"
	}
	repoRoot, repoPrefix, rootErr := s.resolveDiffRoot(ctx, strings.TrimSpace(req.GetString("repo", "")))
	if rootErr != nil {
		return nil, rootErr
	}
	diff, err := analysis.MapGitDiff(s.graph, repoRoot, repoPrefix, scope, base)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(diff.ChangedSymbols))
	changed := make([]changedSymbolRef, 0, len(diff.ChangedSymbols))
	for _, cs := range diff.ChangedSymbols {
		ids = append(ids, cs.ID)
		changed = append(changed, changedSymbolRef{ID: cs.ID, Name: cs.Name, Kind: cs.Kind, File: cs.FilePath})
	}
	ids = dedupeStrings(ids)
	return &prediction{
		source:       "diff",
		changed:      changed,
		changedIDs:   ids,
		nodes:        s.nodesForIDs(ids),
		impact:       s.analyzeImpactLazy(ctx, ids),
		touchedFiles: diff.ChangedFiles,
	}, nil
}

// ruleFamilies returns the change-gate rule families configured for this
// server, in evaluation order. Each implements analysis.RuleFamily; adding a
// family (events, taint) is a registration here, not a new pipeline branch.
func (s *Server) ruleFamilies() []analysis.RuleFamily {
	fams := []analysis.RuleFamily{
		guardsFamily{srv: s},
		analysis.ArchitectureFamily{Config: s.architecture},
	}
	fams = append(fams, s.extraRuleFamilies()...)
	return fams
}

// extraRuleFamilies returns rule families beyond the always-on guards +
// architecture pair — populated as families (event boundaries, taint) come
// online.
func (s *Server) extraRuleFamilies() []analysis.RuleFamily {
	var fams []analysis.RuleFamily
	if len(s.eventRules) > 0 {
		fams = append(fams, analysis.EventBoundaryFamily{Rules: s.eventRules})
	}
	return fams
}

// evaluateChange runs every registered rule family over the changed set.
func (s *Server) evaluateChange(p *prediction) []analysis.GuardViolation {
	if len(p.changedIDs) == 0 {
		return nil
	}
	var violations []analysis.GuardViolation
	for _, fam := range s.ruleFamilies() {
		violations = append(violations, fam.Evaluate(s.graph, p.changedIDs)...)
	}
	return violations
}

// severityForViolation prefers the severity the rule stamped; absent that it
// falls back to a conservative family default (advisory rules warn).
func severityForViolation(v analysis.GuardViolation) string {
	if v.Severity != "" {
		return v.Severity
	}
	switch v.Kind {
	case "layer", "boundary", "fan_out", "caller_boundary", "co-change":
		return "warn"
	default:
		return "info"
	}
}

func familyForViolation(v analysis.GuardViolation) string {
	switch v.Kind {
	case "co-change":
		return "co_change"
	case "layer", "boundary", "fan_out", "caller_boundary":
		return "architecture"
	default:
		return "guards"
	}
}

// scoreChangeRisk folds blast radius and centrality into a 0..100 score.
func (s *Server) scoreChangeRisk(p *prediction) changeRisk {
	blast := 0
	lowerBound := false
	var impactRisk analysis.RiskLevel
	if p.impact != nil {
		blast = p.impact.TotalAffected
		lowerBound = p.impact.LowerBound
		impactRisk = p.impact.Risk
	}
	var maxPR float64
	if metrics, err := s.analysisNodeMetricsBatched(p.changedIDs); err == nil {
		for _, metric := range metrics {
			if metric.PageRank > maxPR {
				maxPR = metric.PageRank
			}
		}
	}
	prNorm := 0.0
	if globalMax := s.topAnalysisMetricValue(graph.AnalysisMetricPageRank); globalMax > 0 {
		prNorm = maxPR / globalMax
	}
	score := int(100 * (0.6*saturate(float64(blast), 30) + 0.4*prNorm))
	if score > 100 {
		score = 100
	}

	tier := "low"
	switch {
	case score >= 67 || impactRisk == analysis.RiskHigh || impactRisk == analysis.RiskCritical:
		tier = "high"
	case score >= 34 || impactRisk == analysis.RiskMedium:
		tier = "medium"
	}
	return changeRisk{Score: score, Tier: tier, PageRank: maxPR, BlastSize: blast, LowerBound: lowerBound}
}

var configExts = map[string]bool{
	".yaml": true, ".yml": true, ".json": true, ".toml": true, ".ini": true,
	".env": true, ".tf": true, ".hcl": true, ".conf": true, ".properties": true,
}

var docExts = map[string]bool{
	".md": true, ".markdown": true, ".rst": true, ".txt": true, ".adoc": true,
}

func allMatch(files []string, pred func(string) bool) bool {
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		if !pred(f) {
			return false
		}
	}
	return true
}

func isConfigFile(f string) bool {
	base := strings.ToLower(filepath.Base(f))
	if base == "dockerfile" || strings.HasPrefix(base, "dockerfile.") || base == "makefile" {
		return true
	}
	return configExts[strings.ToLower(filepath.Ext(f))]
}

func isDocFile(f string) bool { return docExts[strings.ToLower(filepath.Ext(f))] }

// classifyChange tags the change behavioral / structural / runtime_drift /
// metadata_only — a pure function of the prediction, feeding both the risk
// reasoning and the verdict.
func classifyChange(p *prediction) string {
	if p.step != nil {
		renamedOnly := len(p.step.symbolsRenamed) > 0 &&
			len(p.step.symbolsAdded) == 0 &&
			len(p.step.symbolsRemoved) == 0 &&
			len(p.step.brokenCallers) == 0
		if renamedOnly {
			return "structural"
		}
	}
	if allMatch(p.touchedFiles, isConfigFile) {
		return "runtime_drift"
	}
	if len(p.changedIDs) == 0 {
		if allMatch(p.touchedFiles, isDocFile) {
			return "metadata_only"
		}
		// No symbols resolved and not pure-docs — most likely a non-indexed
		// or comment-only change; treat as metadata unless config-driven.
		return "metadata_only"
	}
	return "behavioral"
}

// buildVerificationCommand synthesises the command that proves the change is
// safe — drawn from the covering tests of the changed set.
func buildVerificationCommand(p *prediction) string {
	testFiles := map[string]bool{}
	if p.impact != nil {
		for _, f := range p.impact.TestFiles {
			testFiles[f] = true
		}
	}
	if p.step != nil {
		for _, t := range p.step.testTargets {
			if strings.HasSuffix(t, "_test.go") {
				testFiles[t] = true
			}
		}
	}

	goChange := false
	for _, f := range p.touchedFiles {
		if strings.HasSuffix(f, ".go") {
			goChange = true
			break
		}
	}

	if len(testFiles) > 0 {
		dirs := map[string]bool{}
		for f := range testFiles {
			dirs["./"+filepath.ToSlash(filepath.Dir(f))] = true
		}
		ds := make([]string, 0, len(dirs))
		for d := range dirs {
			ds = append(ds, d)
		}
		sort.Strings(ds)
		if goChange {
			return "go test -race " + strings.Join(ds, " ")
		}
		return "run the covering tests in: " + strings.Join(ds, " ")
	}

	if goChange {
		dirs := map[string]bool{}
		for _, f := range p.touchedFiles {
			if strings.HasSuffix(f, ".go") {
				dirs["./"+filepath.ToSlash(filepath.Dir(f))+"/..."] = true
			}
		}
		ds := make([]string, 0, len(dirs))
		for d := range dirs {
			ds = append(ds, d)
		}
		sort.Strings(ds)
		if len(ds) > 0 {
			return "go build " + strings.Join(ds, " ") + " && go test -race " + strings.Join(ds, " ")
		}
		return "go build ./... && go test -race ./..."
	}
	return ""
}

// buildStopCondition states the checkable predicate that, once true, means the
// change is safe to land — the one field with no pre-existing source.
func buildStopCondition(p *prediction, risk changeRisk, verCmd string) string {
	var parts []string
	if p.step != nil && len(p.step.brokenCallers) > 0 {
		parts = append(parts, fmt.Sprintf("the %d broken caller(s) are updated to the new signature", len(p.step.brokenCallers)))
	}
	if p.step != nil && len(p.step.brokenImplementors) > 0 {
		parts = append(parts, fmt.Sprintf("the %d affected interface implementor(s) are reconciled", len(p.step.brokenImplementors)))
	}
	parts = append(parts, "no new tree-sitter parse errors are introduced")
	if verCmd != "" {
		parts = append(parts, fmt.Sprintf("`%s` exits 0", verCmd))
	}
	if risk.Tier == "high" {
		parts = append(parts, "the blast radius has been reviewed (re-run change_contract to confirm the verdict clears)")
	}
	return "Done when " + strings.Join(parts, " AND ") + "."
}

// assembleEnvelope is the EMIT stage — fold prediction + violations + risk +
// classification into one verdict.
func (s *Server) assembleEnvelope(p *prediction, violations []analysis.GuardViolation) changeEnvelope {
	risk := s.scoreChangeRisk(p)
	verdict := verdictAllow
	var reasons []changeReason

	for _, v := range violations {
		sev := severityForViolation(v)
		reasons = append(reasons, changeReason{
			Family:     familyForViolation(v),
			Severity:   sev,
			Message:    v.Description,
			Confidence: 0.85,
			Symbol:     v.Violator,
		})
		verdict = escalate(verdict, verdictForSeverity(sev))
	}

	if p.step != nil && len(p.step.brokenCallers) > 0 {
		reasons = append(reasons, changeReason{
			Family:     "broken_callers",
			Severity:   "error",
			Message:    fmt.Sprintf("%d caller(s) would no longer compile against the changed signature", len(p.step.brokenCallers)),
			Confidence: 0.95,
		})
		verdict = escalate(verdict, verdictRefuse)
	}
	if p.step != nil && len(p.step.brokenImplementors) > 0 {
		reasons = append(reasons, changeReason{
			Family:     "broken_implementors",
			Severity:   "error",
			Message:    fmt.Sprintf("%d interface implementor(s) would break", len(p.step.brokenImplementors)),
			Confidence: 0.9,
		})
		verdict = escalate(verdict, verdictRefuse)
	}

	if risk.Tier == "high" {
		reasons = append(reasons, changeReason{
			Family:     "risk",
			Severity:   "warn",
			Message:    fmt.Sprintf("high change risk (score %d, blast %d) — review impact before landing", risk.Score, risk.BlastSize),
			Confidence: 0.7,
		})
		verdict = escalate(verdict, verdictWarn)
	}

	// Co-change omissions: files this set historically moves with but left out.
	for _, r := range s.coChangeOmissions(p.touchedFiles) {
		reasons = append(reasons, r)
		verdict = escalate(verdict, verdictForSeverity(r.Severity))
	}

	// API-drift lens: focus the verdict on the public surface and its consumers.
	var apiSurface []apiSurfaceEntry
	if p.lens == "api" {
		var apiReasons []changeReason
		apiReasons, apiSurface = s.apiDriftReasons(p)
		for _, r := range apiReasons {
			reasons = append(reasons, r)
			verdict = escalate(verdict, verdictForSeverity(r.Severity))
		}
	}

	// Risk gate: load-bearing symbols need a fresh impact-review ack.
	if p.riskGate {
		for _, r := range s.riskGateReasons(p, risk) {
			reasons = append(reasons, r)
			verdict = escalate(verdict, verdictForSeverity(r.Severity))
		}
	}

	verCmd := buildVerificationCommand(p)
	classification := classifyChange(p)

	env := changeEnvelope{
		Verdict:             verdict,
		Source:              p.source,
		Classification:      classification,
		ChangedSymbols:      p.changed,
		Reasons:             reasons,
		Risk:                risk,
		VerificationCommand: verCmd,
		StopCondition:       buildStopCondition(p, risk, verCmd),
		EditStrategy:        s.buildEditStrategy(p),
		APISurface:          apiSurface,
	}
	if p.impact != nil {
		env.Blast = map[string]any{
			"total_affected":       p.impact.TotalAffected,
			"risk":                 p.impact.Risk,
			"affected_communities": p.impact.AffectedCommunities,
			"affected_processes":   p.impact.AffectedProcesses,
			"test_files":           p.impact.TestFiles,
			"lower_bound":          p.impact.LowerBound,
		}
	}
	if env.ChangedSymbols == nil {
		env.ChangedSymbols = []changedSymbolRef{}
	}
	if env.Reasons == nil {
		env.Reasons = []changeReason{}
	}
	return env
}

func (s *Server) handleChangeContract(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s == nil || s.graph == nil {
		return mcp.NewToolResultError("change_contract: server not fully initialised"), nil
	}
	p, err := s.lowerChange(ctx, req)
	if err != nil {
		if ctxErr := requestContextError(ctx, err); ctxErr != nil {
			return nil, ctxErr
		}
		return mcp.NewToolResultError(err.Error()), nil
	}
	if req.GetBool("ack", false) {
		return s.handleRiskAck(ctx, req, p)
	}
	violations := s.evaluateChange(p)
	env := s.assembleEnvelope(p, violations)
	return s.respondJSONOrTOON(ctx, req, env)
}
