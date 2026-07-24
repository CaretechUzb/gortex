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

// newLocalizationEvidenceDigest retains only concrete ranked evidence rows.
// Files and Symbols are rebuilt from those rows, so an item that was shed by
// the replay limit or byte budget cannot survive as an unsupported answer
// candidate. The upstream ordering already reserves the strongest direct,
// exact, literal, and promoted structural targets before lower-ranked fan-out.
func newLocalizationEvidenceDigest(envelope localizationExploreEnvelope) *localizationEvidenceDigest {
	digest := &localizationEvidenceDigest{}
	seen := make(map[string]struct{}, localizationReplayEvidenceLimit)
	for _, row := range envelope.Evidence {
		if len(digest.Evidence) >= localizationReplayEvidenceLimit {
			break
		}
		if row.ID == "" || row.File == "" {
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

func renderLocalizationFinalResponse(rows []localizationDigestRow) string {
	if len(rows) == 0 {
		return "FILES:\n(none)\n\nSYMBOLS:\n(none)\n\nEVIDENCE:\nNo bounded localization evidence was found.\n\n" + localizationAnswerReadyDirective
	}
	var response strings.Builder
	response.WriteString("FILES:\n")
	for index, row := range rows {
		fmt.Fprintf(&response, "#%d %s\n", index+1, localizationFinalResponseField(row.File))
	}
	response.WriteString("\nSYMBOLS:\n")
	for index, row := range rows {
		fmt.Fprintf(&response, "#%d %s\n", index+1, localizationFinalResponseField(row.ID))
	}
	response.WriteString("\nEVIDENCE:\n")
	for index, row := range rows {
		file := localizationFinalResponseField(row.File)
		id := localizationFinalResponseField(row.ID)
		if row.Line > 0 {
			fmt.Fprintf(&response, "#%d %s:%d — %s\n", index+1, file, row.Line, id)
			continue
		}
		fmt.Fprintf(&response, "#%d %s — %s\n", index+1, file, id)
	}
	response.WriteString("\n")
	response.WriteString(localizationAnswerReadyDirective)
	return response.String()
}

const localizationAnswerReadyDirective = "You already hold the localization answer — respond now using this evidence; do not call another tool."

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
		structured["final_response"] = contract.Completion.FinalResponse
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
		result.StructuredContent = localizationTerminalStructuredContent(result.StructuredContent, contract)
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
