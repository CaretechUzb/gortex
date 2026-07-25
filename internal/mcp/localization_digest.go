package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// Terminal evidence retention.
//
// The localize handler builds a byte-budgeted evidence envelope once and
// retains a compact projection for host-side fallback and deterministic replay.
// A post-terminal navigation call returns the same successful ready-to-emit
// answer instead of an error that would invite another recovery loop.

const (
	// localizationDigestMaxBytes bounds retained session state independently of
	// the original envelope budget.
	localizationDigestMaxBytes = 4096
	// localizationReplayEvidenceLimit preserves the five strongest direct rows
	// plus at most one graph-validated direct relationship for each. The retained
	// byte cap remains authoritative when long identities cannot all fit.
	localizationReplayEvidenceLimit = 10
	// localizationFinalResponseMaxBytes bounds the ready-to-emit answer that
	// accompanies the retained digest on terminal responses and replays.
	localizationFinalResponseMaxBytes = 4096
	// This canonical envelope is deliberately carried in MCP _meta. Adapting
	// hosts may render its ordered evidence deterministically without exposing
	// retained rows to model-visible text or structuredContent.
	localizationHostMetaKey = "gortex/localization"
)

// localizationEvidenceDigest is the compact, session-retained projection of
// an answer envelope: ranked candidate evidence without source bodies.
type localizationEvidenceDigest struct {
	Files    []string                `json:"files,omitempty"`
	Symbols  []string                `json:"symbols,omitempty"`
	Evidence []localizationDigestRow `json:"evidence,omitempty"`

	// finalResponse is derived from Evidence and excluded from digest JSON so
	// the retained-state byte cap does not count the same identities twice.
	finalResponse string
}

type localizationDigestRow struct {
	Rank       int      `json:"rank,omitempty"`
	ID         string   `json:"id,omitempty"`
	Name       string   `json:"name,omitempty"`
	QualName   string   `json:"qual_name,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	File       string   `json:"file,omitempty"`
	Line       int      `json:"line,omitempty"`
	Signature  string   `json:"signature,omitempty"`
	Callers    []string `json:"callers,omitempty"`
	Callees    []string `json:"callees,omitempty"`
	Provenance string   `json:"provenance,omitempty"`
}

// newLocalizationEvidenceDigestForTask retains only concrete ranked evidence
// rows. Files and Symbols are rebuilt from those rows, so an item that was shed
// by the replay limit or byte budget cannot survive as an unsupported answer
// candidate. Exact and refinement-authorized rows form a stable protected
// prefix in retained state only; the visible envelope keeps its original order.
// The request is rendered into the ready-to-emit answer, so a page completing
// on its first call presents the same task-scored rows a later merge would.
func newLocalizationEvidenceDigestForTask(task string, envelope localizationExploreEnvelope) *localizationEvidenceDigest {
	digest := &localizationEvidenceDigest{}
	priorityIDs := localizationDigestPriorityIDs(envelope.Completion, envelope.Evidence)

	seen := make(map[string]struct{}, localizationReplayEvidenceLimit)
	appendRows := func(priority bool) {
		for _, row := range envelope.Evidence {
			if len(digest.Evidence) >= localizationReplayEvidenceLimit {
				return
			}
			if row.ID == "" || row.File == "" {
				continue
			}
			_, prioritized := priorityIDs[row.ID]
			if prioritized != priority {
				continue
			}
			if _, exists := seen[row.ID]; exists {
				continue
			}
			seen[row.ID] = struct{}{}
			digest.Evidence = append(digest.Evidence, localizationDigestRow{
				Rank:       row.Rank,
				ID:         row.ID,
				Name:       row.Name,
				QualName:   row.QualName,
				Kind:       row.Kind,
				File:       row.File,
				Line:       row.Line,
				Signature:  row.Signature,
				Callers:    append([]string(nil), row.Callers...),
				Callees:    append([]string(nil), row.Callees...),
				Provenance: row.Provenance,
			})
		}
	}
	appendRows(true)
	appendRows(false)

	for {
		rebuildLocalizationDigestSkeleton(digest)
		digest.finalResponse = renderLocalizationFinalResponseForTask(task, nil, digest.Evidence)
		encoded, err := json.Marshal(digest)
		if err == nil && len(encoded) <= localizationDigestMaxBytes && len(digest.finalResponse) <= localizationFinalResponseMaxBytes {
			return digest
		}
		if len(digest.Evidence) == 0 {
			return digest
		}
		last := len(digest.Evidence) - 1
		if shedLocalizationDigestRowOptionalFields(&digest.Evidence[last]) {
			continue
		}
		// The identity and file are the irreducible row. If even one pathological
		// row cannot fit, prefer a bounded empty answer over retaining oversized
		// session state or emitting a truncated, misleading identity.
		if last == 0 {
			digest.Evidence = nil
			continue
		}
		digest.Evidence = digest.Evidence[:last]
	}
}

// localizationDigestPriorityIDs protects every identity required to justify a
// live authorization, not only the identity the client may read. Dependencies
// still remain subject to the hard byte cap; post-pack reconciliation below
// removes or downgrades any authorization whose complete proof did not fit.
func localizationDigestPriorityIDs(completion localizationCompletion, evidence []localizationEvidence) map[string]struct{} {
	priority := make(map[string]struct{}, 1+len(completion.AllowedSymbols)+len(completion.refinementRoutes)*3)
	add := func(symbol string) {
		if symbol = strings.TrimSpace(symbol); symbol != "" {
			priority[symbol] = struct{}{}
		}
	}
	add(completion.ExactSymbol)
	for _, symbol := range completion.AllowedSymbols {
		add(symbol)
	}
	for symbol, route := range completion.refinementRoutes {
		add(symbol)
		add(route.implementationSymbol)
		add(route.proofSymbol)
	}
	for _, row := range evidence {
		switch row.Provenance {
		case localizationProvenanceSourceLiteralCallee,
			localizationProvenanceDivergentDefault,
			localizationProvenanceDivergentDefaultType,
			localizationProvenanceImplementationRoute,
			localizationProvenanceImplementationTarget:
			add(row.ID)
		}
	}
	return priority
}

func cloneLocalizationDigestRows(rows []localizationDigestRow) []localizationDigestRow {
	if len(rows) == 0 {
		return nil
	}
	cloned := make([]localizationDigestRow, len(rows))
	for index, row := range rows {
		cloned[index] = row
		cloned[index].Callers = append([]string(nil), row.Callers...)
		cloned[index].Callees = append([]string(nil), row.Callees...)
	}
	return cloned
}

// mergeLocalizationEvidenceDigest puts evidence returned by the terminalizing
// permitted call first, then fills the bounded tail from the retained localize
// digest. Files, Symbols, and Evidence are rebuilt from the same rows so their
// ordinal positions can never diverge.
func mergeLocalizationEvidenceDigest(current []localizationDigestRow, retained *localizationEvidenceDigest) *localizationEvidenceDigest {
	digest := &localizationEvidenceDigest{}
	seen := make(map[string]struct{}, localizationReplayEvidenceLimit)
	appendRows := func(rows []localizationDigestRow) {
		for _, row := range rows {
			if len(digest.Evidence) >= localizationReplayEvidenceLimit {
				return
			}
			row.ID = strings.TrimSpace(row.ID)
			row.File = strings.TrimSpace(row.File)
			if row.ID == "" || row.File == "" {
				continue
			}
			if _, exists := seen[row.ID]; exists {
				continue
			}
			seen[row.ID] = struct{}{}
			row.Callers = append([]string(nil), row.Callers...)
			row.Callees = append([]string(nil), row.Callees...)
			digest.Evidence = append(digest.Evidence, row)
		}
	}
	appendRows(current)
	if retained != nil {
		appendRows(retained.Evidence)
	}
	for {
		rebuildLocalizationDigestSkeleton(digest)
		digest.finalResponse = renderLocalizationFinalResponse(digest.Evidence)
		encoded, err := json.Marshal(digest)
		if err == nil && len(encoded) <= localizationDigestMaxBytes && len(digest.finalResponse) <= localizationFinalResponseMaxBytes {
			return digest
		}
		if len(digest.Evidence) == 0 {
			return digest
		}
		last := len(digest.Evidence) - 1
		if shedLocalizationDigestRowOptionalFields(&digest.Evidence[last]) {
			continue
		}
		if last == 0 {
			digest.Evidence = nil
			continue
		}
		digest.Evidence = digest.Evidence[:last]
	}
}

const localizationSameOwnerEvidenceReserve = 3

// mergeLocalizationEvidenceDigestForTask preserves a small coherent method
// cohort around the terminalizing read before byte shedding considers the
// unrelated tail. It only reorders already-retained rows; cardinality and byte
// limits remain centralized in mergeLocalizationEvidenceDigest.
func mergeLocalizationEvidenceDigestForTask(task string, current []localizationDigestRow, retained *localizationEvidenceDigest) *localizationEvidenceDigest {
	ordered := &localizationEvidenceDigest{Evidence: localizationTaskAwareRetainedRows(task, current, retained)}
	digest := mergeLocalizationEvidenceDigest(current, ordered)
	finalResponse := renderLocalizationFinalResponseForTask(task, current, digest.Evidence)
	if len(finalResponse) <= localizationFinalResponseMaxBytes {
		digest.finalResponse = finalResponse
	}
	return digest
}

func localizationTaskAwareRetainedRows(task string, current []localizationDigestRow, retained *localizationEvidenceDigest) []localizationDigestRow {
	if retained == nil || len(retained.Evidence) == 0 {
		return nil
	}
	rows := retained.Evidence
	ordered := make([]localizationDigestRow, 0, len(rows))
	selected := make(map[string]struct{}, len(rows)+len(current))
	ownerKeys := make(map[string]struct{}, len(current))
	seenFiles := make(map[string]struct{}, len(current))
	for _, row := range current {
		if id := strings.TrimSpace(row.ID); id != "" {
			selected[id] = struct{}{}
		}
		if file := strings.TrimSpace(row.File); file != "" {
			seenFiles[file] = struct{}{}
		}
		if key := localizationDigestRowOwnerKey(row); key != "" {
			ownerKeys[key] = struct{}{}
		}
	}
	appendWhere := func(limit int, keep func(localizationDigestRow) bool) {
		added := 0
		for _, row := range rows {
			id := strings.TrimSpace(row.ID)
			if id == "" {
				continue
			}
			if _, exists := selected[id]; exists || !keep(row) {
				continue
			}
			selected[id] = struct{}{}
			ordered = append(ordered, row)
			if file := strings.TrimSpace(row.File); file != "" {
				seenFiles[file] = struct{}{}
			}
			added++
			if limit > 0 && added == limit {
				return
			}
		}
	}
	sameOwner := func(row localizationDigestRow) bool {
		key := localizationDigestRowOwnerKey(row)
		_, exists := ownerKeys[key]
		return key != "" && exists
	}
	appendWhere(0, func(row localizationDigestRow) bool {
		return localizationDigestRowTaskCited(task, row)
	})
	appendWhere(localizationSameOwnerEvidenceReserve, sameOwner)
	taskTerms := exploreTerminalTerms(task)
	appendWhere(0, func(row localizationDigestRow) bool {
		return !sameOwner(row) && localizationDigestRowTaskAligned(task, taskTerms, row)
	})
	appendWhere(0, func(row localizationDigestRow) bool {
		_, seen := seenFiles[strings.TrimSpace(row.File)]
		return !seen
	})
	appendWhere(0, func(localizationDigestRow) bool { return true })
	return ordered
}

func localizationDigestRowTaskCited(task string, row localizationDigestRow) bool {
	for _, value := range []string{row.ID, row.Name, row.QualName, row.File, row.Signature} {
		if localizationTaskCitesConcreteEvidence(task, value) {
			return true
		}
	}
	return false
}

func localizationDigestRowTaskAligned(task string, taskTerms map[string]struct{}, row localizationDigestRow) bool {
	if localizationDigestRowTaskCited(task, row) {
		return true
	}
	values := []string{row.ID, row.Name, row.QualName, row.File, row.Signature}
	for term := range exploreTerminalTerms(strings.Join(values, " ")) {
		if _, aligned := taskTerms[term]; aligned {
			return true
		}
	}
	return false
}

func localizationDigestRowOwnerKey(row localizationDigestRow) string {
	if !strings.EqualFold(strings.TrimSpace(row.Kind), "method") {
		return ""
	}
	file := strings.TrimSpace(row.File)
	owner := strings.TrimSpace(row.QualName)
	if owner == "" {
		owner = strings.TrimSpace(row.ID)
		if prefix := file + "::"; file != "" && strings.HasPrefix(owner, prefix) {
			owner = strings.TrimPrefix(owner, prefix)
		}
	}
	if cut := strings.LastIndex(owner, "."); cut > 0 {
		owner = owner[:cut]
	} else if cut := strings.LastIndex(owner, "::"); cut > 0 {
		owner = owner[:cut]
	} else {
		return ""
	}
	owner = strings.TrimSpace(owner)
	if file == "" || owner == "" {
		return ""
	}
	return file + "\x00" + owner
}

func shedLocalizationDigestRowOptionalFields(row *localizationDigestRow) bool {
	if row == nil {
		return false
	}
	if len(row.Callers) > 0 || len(row.Callees) > 0 {
		row.Callers = nil
		row.Callees = nil
		return true
	}
	if row.Signature != "" {
		row.Signature = ""
		return true
	}
	if row.QualName != "" {
		row.QualName = ""
		return true
	}
	if row.Name != "" || row.Kind != "" {
		row.Name = ""
		row.Kind = ""
		return true
	}
	return false
}

func rebuildLocalizationDigestSkeleton(digest *localizationEvidenceDigest) {
	digest.Files = digest.Files[:0]
	digest.Symbols = digest.Symbols[:0]
	for index := range digest.Evidence {
		row := &digest.Evidence[index]
		row.Rank = index + 1
		// Keep these arrays positional, including repeated files. A consumer can
		// now pair FILES #N, SYMBOLS #N, and EVIDENCE #N without guessing.
		digest.Files = append(digest.Files, row.File)
		digest.Symbols = append(digest.Symbols, row.ID)
	}
}

func localizationFinalResponseField(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

const (
	localizationFinalResponsePrimaryLimit = 3
	// The answer asks the caller to reproduce these lines verbatim, so the
	// presentation must cover every row the digest retained: a retained row
	// that never reaches the answer is evidence the page found and then hid.
	localizationFinalResponseSupportingLimit = localizationReplayEvidenceLimit - localizationFinalResponsePrimaryLimit
)

type localizationFinalResponseRow struct {
	row     localizationDigestRow
	primary bool
}

type localizationFinalResponseTaskScore struct {
	matched  int
	longest  int
	callable bool
}

func localizationFinalResponsePrimaryProvenance(provenance string) bool {
	switch provenance {
	case localizationProvenanceSourceLiteralCallee,
		localizationProvenanceDivergentDefault,
		localizationProvenanceImplementationTarget,
		localizationProvenanceTypedAnchorProjection:
		return true
	default:
		return false
	}
}

func localizationFinalResponseSupportingProvenance(provenance string) bool {
	switch provenance {
	case localizationProvenanceDivergentDefaultType,
		localizationProvenanceImplementationRoute,
		"direct_caller", "direct_callee":
		return true
	default:
		return false
	}
}

func localizationFinalResponseIdentifierText(row localizationDigestRow) string {
	id := strings.TrimSpace(row.ID)
	if cut := strings.LastIndex(id, "::"); cut >= 0 {
		id = id[cut+2:]
	}
	return strings.ToLower(strings.Join([]string{row.Name, row.QualName, id}, " "))
}

func scoreLocalizationFinalResponseTask(taskTerms map[string]struct{}, row localizationDigestRow) localizationFinalResponseTaskScore {
	score := localizationFinalResponseTaskScore{
		callable: strings.EqualFold(strings.TrimSpace(row.Kind), "function") ||
			strings.EqualFold(strings.TrimSpace(row.Kind), "method"),
	}
	text := localizationFinalResponseIdentifierText(row)
	for term := range taskTerms {
		if !exploreConceptTermPresent(text, term) {
			continue
		}
		score.matched++
		if len(term) > score.longest {
			score.longest = len(term)
		}
	}
	return score
}

func localizationFinalResponseBetterTaskScore(left, right localizationFinalResponseTaskScore) bool {
	if left.matched != right.matched {
		return left.matched > right.matched
	}
	if left.callable != right.callable {
		return left.callable
	}
	return left.longest > right.longest
}

func localizationFinalResponseNeighborContains(ids []string, id string) bool {
	for _, candidate := range ids {
		if strings.TrimSpace(candidate) == id {
			return true
		}
	}
	return false
}

func localizationFinalResponseDirectRelation(row localizationDigestRow, primaries []localizationDigestRow) bool {
	if localizationFinalResponseSupportingProvenance(row.Provenance) {
		return true
	}
	id := strings.TrimSpace(row.ID)
	for _, primary := range primaries {
		primaryID := strings.TrimSpace(primary.ID)
		if localizationFinalResponseNeighborContains(primary.Callers, id) ||
			localizationFinalResponseNeighborContains(primary.Callees, id) ||
			localizationFinalResponseNeighborContains(row.Callers, primaryID) ||
			localizationFinalResponseNeighborContains(row.Callees, primaryID) {
			return true
		}
	}
	return false
}

// localizationFinalResponseRows selects a bounded model-facing projection
// without changing the retained evidence rows or their positional JSON arrays.
func localizationFinalResponseRows(task string, current, rows []localizationDigestRow) []localizationFinalResponseRow {
	if len(rows) == 0 {
		return nil
	}
	primaries := make([]localizationDigestRow, 0, localizationFinalResponsePrimaryLimit)
	supporting := make([]localizationDigestRow, 0, localizationFinalResponseSupportingLimit)
	selected := make(map[string]struct{}, localizationFinalResponsePrimaryLimit+localizationFinalResponseSupportingLimit)
	appendRow := func(dst *[]localizationDigestRow, limit int, row localizationDigestRow) bool {
		id := strings.TrimSpace(row.ID)
		if id == "" || strings.TrimSpace(row.File) == "" || len(*dst) >= limit {
			return false
		}
		if _, exists := selected[id]; exists {
			return false
		}
		selected[id] = struct{}{}
		*dst = append(*dst, row)
		return true
	}

	rowsByID := make(map[string]localizationDigestRow, len(rows))
	for _, row := range rows {
		rowsByID[strings.TrimSpace(row.ID)] = row
	}
	// A successful authorized read is the freshest bounded evidence and leads
	// the presentation even when its retained predecessor carried more metadata.
	for _, row := range current {
		if retained, exists := rowsByID[strings.TrimSpace(row.ID)]; exists {
			appendRow(&primaries, localizationFinalResponsePrimaryLimit, retained)
		}
	}
	for _, row := range rows {
		if localizationFinalResponsePrimaryProvenance(row.Provenance) {
			appendRow(&primaries, localizationFinalResponsePrimaryLimit, row)
		}
	}

	taskTerms := exploreTerminalTerms(task)
	ownerKeys := make(map[string]struct{}, len(current))
	for _, row := range current {
		key := localizationDigestRowOwnerKey(row)
		if key == "" {
			if retained, exists := rowsByID[strings.TrimSpace(row.ID)]; exists {
				key = localizationDigestRowOwnerKey(retained)
			}
		}
		if key != "" {
			ownerKeys[key] = struct{}{}
		}
	}
	bestSameOwner := -1
	var bestSameOwnerScore localizationFinalResponseTaskScore
	if len(primaries) < localizationFinalResponsePrimaryLimit && len(ownerKeys) > 0 {
		for index, row := range rows {
			if _, exists := selected[strings.TrimSpace(row.ID)]; exists {
				continue
			}
			key := localizationDigestRowOwnerKey(row)
			if _, sameOwner := ownerKeys[key]; key == "" || !sameOwner {
				continue
			}
			score := scoreLocalizationFinalResponseTask(taskTerms, row)
			if bestSameOwner < 0 || localizationFinalResponseBetterTaskScore(score, bestSameOwnerScore) {
				bestSameOwner, bestSameOwnerScore = index, score
			}
		}
		if bestSameOwner >= 0 {
			appendRow(&primaries, localizationFinalResponsePrimaryLimit, rows[bestSameOwner])
		}
	}

	bestTaskMatch := -1
	var bestTaskScore localizationFinalResponseTaskScore
	if len(primaries) < localizationFinalResponsePrimaryLimit && len(taskTerms) > 0 {
		for index, row := range rows {
			if _, exists := selected[strings.TrimSpace(row.ID)]; exists {
				continue
			}
			score := scoreLocalizationFinalResponseTask(taskTerms, row)
			if score.matched == 0 {
				continue
			}
			if bestTaskMatch < 0 || localizationFinalResponseBetterTaskScore(score, bestTaskScore) {
				bestTaskMatch, bestTaskScore = index, score
			}
		}
		if bestTaskMatch >= 0 {
			appendRow(&primaries, localizationFinalResponsePrimaryLimit, rows[bestTaskMatch])
		}
	}
	for _, row := range rows {
		appendRow(&primaries, localizationFinalResponsePrimaryLimit, row)
	}

	for _, row := range rows {
		if localizationFinalResponseDirectRelation(row, primaries) {
			appendRow(&supporting, localizationFinalResponseSupportingLimit, row)
		}
	}
	for _, row := range rows {
		appendRow(&supporting, localizationFinalResponseSupportingLimit, row)
	}

	presented := make([]localizationFinalResponseRow, 0, len(primaries)+len(supporting))
	for _, row := range primaries {
		presented = append(presented, localizationFinalResponseRow{row: row, primary: true})
	}
	for _, row := range supporting {
		presented = append(presented, localizationFinalResponseRow{row: row})
	}
	return presented
}

func renderLocalizationFinalResponse(rows []localizationDigestRow) string {
	return renderLocalizationFinalResponseForTask("", nil, rows)
}

func renderLocalizationFinalResponseForTask(task string, current, rows []localizationDigestRow) string {
	presented := localizationFinalResponseRows(task, current, rows)
	if len(presented) == 0 {
		return "LOCALIZATION:\nNo bounded localization evidence was found.\n\n" + localizationAnswerReadyDirective
	}
	var response strings.Builder
	response.WriteString("LOCALIZATION:\n")
	for _, item := range presented {
		role := "SUPPORTING"
		if item.primary {
			role = "PRIMARY"
		}
		file := localizationFinalResponseField(item.row.File)
		id := localizationFinalResponseField(item.row.ID)
		if item.row.Line > 0 {
			fmt.Fprintf(&response, "- %s — %s:%d — %s\n", role, file, item.row.Line, id)
			continue
		}
		fmt.Fprintf(&response, "- %s — %s — %s\n", role, file, id)
	}
	response.WriteString("\n")
	response.WriteString(localizationAnswerReadyDirective)
	return response.String()
}

// The directive is the only instruction the caller sees on a terminal page, so
// it names the one failure mode measurement keeps finding: an answer that
// paraphrases the located identifier into a neighbouring one it inferred from
// the request text, losing the located symbol.
// The conclusion is bounded deliberately. Output is the most expensive token
// class in a session, and asking for the located lines verbatim already moved
// median output up measurably; an unbounded "explain" invites the model to
// restate evidence it has just quoted.
// Wording matters more than it looks. Ordering a caller to reproduce a page it
// judges wrong reads as coercion: measured across 336 sessions, pages carrying
// the word "verbatim" drew an explicit manipulation accusation in 30% of the
// caller's own statements against 2% on pages without it. Ask for the answer,
// name what the answer should carry, and leave the caller free to disagree —
// its disagreement is right more often than not.
const localizationAnswerReadyDirective = "Localization for this task is complete. Answer now from this evidence, naming the files and symbols you rely on. If it does not fit the request, say so and name what does — your judgement about the code is welcome, another navigation call is not."

func localizationDigestRowsByID(digest *localizationEvidenceDigest) map[string]localizationDigestRow {
	retained := make(map[string]localizationDigestRow)
	if digest == nil {
		return retained
	}
	for _, row := range digest.Evidence {
		if symbol := strings.TrimSpace(row.ID); symbol != "" {
			retained[symbol] = row
		}
	}
	return retained
}

func localizationDigestHasProvenance(rows map[string]localizationDigestRow, provenance string) bool {
	for _, row := range rows {
		if row.Provenance == provenance {
			return true
		}
	}
	return false
}

func localizationDigestStrongProofRetained(rows map[string]localizationDigestRow, symbol string) bool {
	row, exists := rows[strings.TrimSpace(symbol)]
	if !exists {
		return false
	}
	switch row.Provenance {
	case localizationProvenanceSourceLiteralCallee:
		return true
	case localizationProvenanceDivergentDefault:
		return localizationDigestHasProvenance(rows, localizationProvenanceDivergentDefaultType)
	case localizationProvenanceImplementationRoute:
		return localizationDigestHasProvenance(rows, localizationProvenanceImplementationTarget)
	case localizationProvenanceImplementationTarget:
		return localizationDigestHasProvenance(rows, localizationProvenanceImplementationRoute)
	default:
		return false
	}
}

func localizationDigestAnyStrongProofRetained(rows map[string]localizationDigestRow) bool {
	for symbol := range rows {
		if localizationDigestStrongProofRetained(rows, symbol) {
			return true
		}
	}
	return false
}

// localizationDigestReconcileRoute preserves a concrete advisory read when its
// optional proof was shed, but never preserves a generic-wrapper hop without
// the concrete implementation that makes the route useful.
func localizationDigestReconcileRoute(
	symbol string,
	route localizationRefinementRoute,
	rows map[string]localizationDigestRow,
) (localizationRefinementRoute, bool) {
	row, retained := rows[symbol]
	if !retained {
		return localizationRefinementRoute{}, false
	}
	if route.implementationSymbol != "" {
		implementation, implementationRetained := rows[route.implementationSymbol]
		if !implementationRetained {
			return localizationRefinementRoute{}, false
		}
		if route.enforceable && (row.Provenance != localizationProvenanceImplementationRoute ||
			implementation.Provenance != localizationProvenanceImplementationTarget) {
			route.enforceable = false
		}
	}
	if route.proofSymbol != "" {
		proof, proofRetained := rows[route.proofSymbol]
		if !proofRetained || proof.Provenance != localizationProvenanceImplementationRoute ||
			row.Provenance != localizationProvenanceImplementationTarget {
			route.proofSymbol = ""
			route.enforceable = false
		}
	}
	if route.enforceable && route.implementationSymbol == "" && route.proofSymbol == "" &&
		row.Provenance != localizationProvenanceSourceLiteralCallee {
		route.enforceable = false
	}
	return route, true
}

func localizationCompletionBoundedByDigest(completion localizationCompletion, digest *localizationEvidenceDigest) localizationCompletion {
	if digest == nil {
		return completion
	}
	retained := localizationDigestRowsByID(digest)
	advisory := func() localizationCompletion {
		bounded := newLocalizationCompletion(true, "")
		bounded.taskLead = completion.taskLead
		return bounded
	}

	switch completion.State {
	case localizationStateAnswerReady:
		if completion.Enforceable && !localizationDigestAnyStrongProofRetained(retained) {
			completion.Enforceable = false
		}
	case localizationStateNeedsExactRead:
		if _, exists := retained[completion.ExactSymbol]; !exists || completion.ExactSymbol == "" {
			return advisory()
		}
		if completion.enforceableOnAnswerReady &&
			!localizationDigestStrongProofRetained(retained, completion.ExactSymbol) {
			completion.enforceableOnAnswerReady = false
		}
	case localizationStateNeedsRefinement:
		allowed := make([]string, 0, len(completion.AllowedSymbols))
		seen := make(map[string]struct{}, len(completion.AllowedSymbols))
		var routes map[string]localizationRefinementRoute
		if len(completion.refinementRoutes) > 0 {
			routes = make(map[string]localizationRefinementRoute, len(completion.refinementRoutes))
		}
		for _, symbol := range completion.AllowedSymbols {
			symbol = strings.TrimSpace(symbol)
			if symbol == "" {
				continue
			}
			if _, exists := retained[symbol]; !exists {
				continue
			}
			if _, duplicate := seen[symbol]; duplicate {
				continue
			}
			if route, exists := completion.refinementRoutes[symbol]; exists {
				reconciled, usable := localizationDigestReconcileRoute(symbol, route, retained)
				if !usable {
					continue
				}
				routes[symbol] = reconciled
			}
			seen[symbol] = struct{}{}
			allowed = append(allowed, symbol)
		}
		if len(allowed) == 0 {
			return advisory()
		}
		preferred := strings.TrimSpace(completion.refinementSymbol)
		if _, exists := seen[preferred]; !exists {
			preferred = allowed[0]
		}
		bounded := newLocalizationRefinementCompletionForSymbols(preferred, allowed)
		bounded.enforceableOnAnswerReady = completion.enforceableOnAnswerReady
		bounded.taskLead = completion.taskLead
		if routes != nil {
			bounded.refinementRoutes = routes
			bounded.correctionSymbol, bounded.correctionRoute = localizationRankedCorrection(
				preferred, allowed, bounded.refinementRoutes,
			)
		}
		return bounded
	}
	return completion
}

func localizationCompletionWithDigest(completion localizationCompletion, digest *localizationEvidenceDigest) localizationCompletion {
	if digest == nil {
		digest = completion.digest
	}
	completion.digest = digest
	if completion.State != localizationStateAnswerReady {
		completion.FinalResponse = ""
		return completion
	}
	if digest != nil && digest.finalResponse != "" {
		completion.FinalResponse = digest.finalResponse
	} else if completion.FinalResponse == "" {
		completion.FinalResponse = renderLocalizationFinalResponse(nil)
	}
	return completion
}

func localizationTerminalStructuredContent(payload any, contract localizationTerminalContract) map[string]any {
	var structured map[string]any
	switch existing := payload.(type) {
	case map[string]any:
		structured = make(map[string]any, len(existing)+4)
		for key, value := range existing {
			structured[key] = value
		}
	case nil:
		structured = make(map[string]any, 4)
	default:
		structured = map[string]any{"payload": existing}
	}
	structured["completion"] = contract.Completion
	structured["terminal"] = contract.Terminal
	if contract.Terminal && contract.Completion.FinalResponse != "" {
		// completion already carries the answer, and the directive is its closing
		// line. A second top-level copy is the same block billed twice at the
		// cache-write rate, which is the most expensive place to repeat oneself:
		// point at it instead.
		structured["directive"] = localizationAnswerReadyDirective
	}
	return structured
}

// localizationHostEnvelope stores each retained row exactly once. Hosts render
// the ordered rows with fallback_format; no prewritten answer or duplicate row
// string crosses the wire.
type localizationHostEnvelope struct {
	Version        int                          `json:"version"`
	FallbackFormat string                       `json:"fallback_format"`
	Evidence       *localizationEvidenceDigest  `json:"evidence"`
	Contract       localizationTerminalContract `json:"contract"`
}

// Initial localization and authorized reads call this only after byte-budget
// packing and evidence-policy finalization, so visible and authoritative host
// contracts always describe the same completion.
func attachLocalizationHostEnvelope(result *mcpgo.CallToolResult, completion localizationCompletion, digest *localizationEvidenceDigest) *mcpgo.CallToolResult {
	if result == nil {
		return result
	}
	completion = localizationCompletionWithDigest(completion, digest)
	contract := localizationContractFor(completion)
	// Preserve preterminal tool payloads byte-for-byte. Only answer_ready adds
	// the terminal host projection; refinement and recovery retain their
	// existing structuredContent shape and visible completion envelope.
	if completion.State == localizationStateAnswerReady {
		base := result.StructuredContent
		if base == nil {
			// A host that renders structured content in preference to text sees
			// only this projection. Replacing a nil payload would therefore erase
			// the tool's own answer — including the source an authorized read was
			// just permitted to fetch — so decode the text payload and keep it
			// underneath the terminal keys.
			if text, ok := singleTextContent(result); ok {
				var decoded map[string]any
				if err := json.Unmarshal([]byte(text), &decoded); err == nil {
					base = decoded
				}
			}
		}
		result.StructuredContent = localizationTerminalStructuredContent(base, contract)
	}
	if result.Meta == nil {
		result.Meta = &mcpgo.Meta{}
	}
	if result.Meta.AdditionalFields == nil {
		result.Meta.AdditionalFields = make(map[string]any)
	}
	result.Meta.AdditionalFields[localizationHostMetaKey] = localizationHostEnvelope{
		Version:        1,
		FallbackFormat: "{file}:{line} — {id} ({signature})",
		Evidence:       digest,
		Contract:       contract,
	}
	return result
}

// localizationAnswerReadyResult is the successful, deterministic evidence
// replay. Hooked hosts may stop before dispatch; every other host receives the
// same ready-to-emit answer and directive on every post-terminal navigation.
func localizationAnswerReadyResult(completion localizationCompletion) *mcpgo.CallToolResult {
	completion = localizationCompletionWithDigest(completion, completion.digest)
	visible := completion.FinalResponse
	// Older retained completions may predate the in-response convergence cue.
	// Preserve their successful replay shape without duplicating the directive
	// for newly rendered terminal evidence.
	if !strings.HasSuffix(visible, localizationAnswerReadyDirective) {
		visible += "\n\n" + localizationAnswerReadyDirective
	}
	result := mcpgo.NewToolResultText(visible)
	return attachLocalizationHostEnvelope(result, completion, completion.digest)
}

// newLocalizationEvidenceDigest retains rows without a request in hand. Callers
// that know the request use newLocalizationEvidenceDigestForTask so the
// presented rows are scored against it.
func newLocalizationEvidenceDigest(envelope localizationExploreEnvelope) *localizationEvidenceDigest {
	return newLocalizationEvidenceDigestForTask("", envelope)
}
