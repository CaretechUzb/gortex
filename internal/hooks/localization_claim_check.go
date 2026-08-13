package hooks

import (
	"fmt"
	"strings"
	"unicode"
)

const localizationClaimCheckMaxChars = 600

// localizationClaimCheck returns a single bounded correction only when a Stop
// payload exposes the final assistant message and that message makes an
// explicit code-shaped claim outside the authenticated evidence digest.
func localizationClaimCheck(input PostTaskInput) string {
	message := strings.TrimSpace(input.LastAssistantMessage)
	if message == "" || input.StopHookActive {
		return ""
	}
	identity, ok := currentLocalizationTurn(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	if !ok {
		return ""
	}
	marker, ok := localizationTerminalMarkerFor(identity)
	if !ok || len(marker.PrimaryIDs) == 0 || len(marker.EvidenceIDs) == 0 {
		return ""
	}
	claims := localizationExplicitSymbolClaims(message)
	if len(claims) == 0 {
		return ""
	}
	for _, claim := range claims {
		for _, evidenceID := range marker.EvidenceIDs {
			if localizationClaimMatchesEvidence(claim, evidenceID) {
				return ""
			}
		}
	}
	prompt := "[Gortex claim_check] Your answer names no symbol in the authenticated evidence. Cite one of these PRIMARY IDs, or explicitly confirm that none fits: " + strings.Join(marker.PrimaryIDs, ", ") + ". Do not retrieve more evidence."
	return boundedLocalizationClaimCheck(prompt)
}

func localizationExplicitSymbolClaims(message string) []string {
	claims := localizationStructuredSymbolClaims(message)
	if len(claims) > 0 {
		return claims
	}
	fields := strings.FieldsFunc(message, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("_.$:#\\/-", r)
	})
	claims = make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	for _, field := range fields {
		claim := strings.Trim(field, "_.$:#\\/-")
		if len(claim) < 2 || !localizationCodeShapedClaim(claim) {
			continue
		}
		key := strings.ToLower(claim)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		claims = append(claims, claim)
	}
	return claims
}

func localizationStructuredSymbolClaims(message string) []string {
	var claims []string
	inSymbols := false
	for _, line := range strings.Split(message, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.EqualFold(strings.TrimSuffix(trimmed, ":"), "symbols") {
			inSymbols = true
			continue
		}
		if !inSymbols {
			continue
		}
		if trimmed == "" {
			if len(claims) > 0 {
				break
			}
			continue
		}
		if strings.HasSuffix(trimmed, ":") && !strings.HasPrefix(trimmed, "-") {
			break
		}
		trimmed = strings.TrimSpace(strings.TrimLeft(trimmed, "-*•0123456789. "))
		if fields := strings.Fields(trimmed); len(fields) > 0 {
			claim := strings.Trim(fields[0], "`_.$:#\\/-")
			if claim != "" {
				claims = append(claims, claim)
			}
		}
	}
	return claims
}

func localizationCodeShapedClaim(claim string) bool {
	lower := strings.ToLower(claim)
	for _, extension := range []string{".go", ".py", ".js", ".ts", ".tsx", ".rs", ".java", ".php", ".rb", ".swift", ".dart", ".cs"} {
		if strings.HasSuffix(lower, extension) {
			return false
		}
	}
	if strings.Contains(claim, "/") && !strings.Contains(claim, "::") && !strings.Contains(claim, "#") {
		return false
	}
	if strings.ContainsAny(claim, "._:#\\/") {
		return true
	}
	for index, r := range claim {
		if index > 0 && unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

func localizationClaimMatchesEvidence(claim, evidenceID string) bool {
	claim = strings.ToLower(strings.TrimSpace(claim))
	evidenceID = strings.ToLower(strings.TrimSpace(evidenceID))
	if claim == "" || evidenceID == "" {
		return false
	}
	if claim == evidenceID || strings.HasSuffix(evidenceID, "::"+claim) {
		return true
	}
	for _, candidate := range localizationEvidenceClaimForms(evidenceID) {
		if claim == candidate || strings.HasSuffix(claim, "::"+candidate) || strings.HasSuffix(claim, "."+candidate) {
			return true
		}
	}
	return false
}

func localizationEvidenceClaimForms(evidenceID string) []string {
	forms := []string{evidenceID}
	if index := strings.LastIndex(evidenceID, "::"); index >= 0 && index+2 < len(evidenceID) {
		forms = append(forms, evidenceID[index+2:])
	}
	leaf := forms[len(forms)-1]
	if index := strings.LastIndexAny(leaf, ".#"); index >= 0 && index+1 < len(leaf) {
		forms = append(forms, leaf[index+1:])
	}
	return forms
}

func boundedLocalizationClaimCheck(prompt string) string {
	if len(prompt) <= localizationClaimCheckMaxChars {
		return prompt
	}
	const suffix = "… Do not retrieve more evidence."
	return fmt.Sprintf("%s%s", prompt[:localizationClaimCheckMaxChars-len(suffix)], suffix)
}
