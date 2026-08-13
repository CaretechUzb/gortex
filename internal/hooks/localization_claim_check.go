package hooks

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	localizationClaimCheckMaxChars        = 600
	localizationClaimCheckMaxMessageBytes = 64 << 10
	localizationClaimCheckMaxClaims       = 32
	localizationClaimCheckMaxTokens       = 2048
	localizationClaimCheckMaxTokenBytes   = 256
	localizationRejectedClaim             = "__gortex_invalid_claim_input__"
)

// localizationClaimCheck returns a single bounded correction only when a Stop
// payload exposes the final assistant message and that message makes an
// explicit code-shaped claim outside the authenticated evidence digest.
func localizationClaimCheck(input PostTaskInput) string {
	if input.LastAssistantMessage == "" || input.StopHookActive {
		return ""
	}
	claims := []string(nil)
	if len(input.LastAssistantMessage) > localizationClaimCheckMaxMessageBytes {
		claims = []string{localizationRejectedClaim}
	} else {
		message := strings.TrimSpace(input.LastAssistantMessage)
		if message == "" {
			return ""
		}
		claims = localizationExplicitSymbolClaims(message)
	}
	if len(claims) == 0 {
		return ""
	}
	identity, ok := currentLocalizationTurn(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	if !ok {
		return ""
	}
	marker, consumed := consumeLocalizationClaimCheck(identity, claims)
	if !consumed {
		return ""
	}
	prompt := "[Gortex claim_check] Your answer names one or more symbols outside the authenticated evidence. Cite only these PRIMARY IDs, or explicitly confirm that none fits: " + strings.Join(marker.PrimaryIDs, ", ") + ". Do not retrieve more evidence."
	return boundedLocalizationClaimCheck(prompt)
}

// localizationExplicitSymbolClaims preserves the slice-only helper contract
// while making every resource-limit violation an unmatchable claim.
func localizationExplicitSymbolClaims(message string) []string {
	claims, _, valid := localizationBoundedSymbolClaims(message)
	if !valid {
		return []string{localizationRejectedClaim}
	}
	return claims
}

func localizationBoundedSymbolClaims(message string) ([]string, bool, bool) {
	if len(message) > localizationClaimCheckMaxMessageBytes {
		return nil, false, false
	}
	claims, explicitNone, valid := localizationStructuredSymbolClaimsBounded(message)
	if !valid || len(claims) > 0 || explicitNone {
		return claims, explicitNone, valid
	}
	claims, valid = localizationUnstructuredSymbolClaims(message)
	return claims, false, valid
}

func localizationUnstructuredSymbolClaims(message string) ([]string, bool) {
	claims := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	tokenCount := 0

	consume := func(token string) bool {
		tokenCount++
		if tokenCount > localizationClaimCheckMaxTokens || len(token) > localizationClaimCheckMaxTokenBytes {
			return false
		}
		claim := strings.Trim(token, "_.$:#\\/-")
		if len(claim) < 2 || !localizationCodeShapedClaim(claim) {
			return true
		}
		key := strings.ToLower(claim)
		if _, duplicate := seen[key]; duplicate {
			return true
		}
		if len(claims) >= localizationClaimCheckMaxClaims {
			return false
		}
		seen[key] = struct{}{}
		claims = append(claims, claim)
		return true
	}

	for _, line := range strings.Split(message, "\n") {
		body, inspect := localizationUnstructuredClaimLine(line)
		if !inspect {
			continue
		}
		tokenStart := -1
		for index, r := range body {
			if localizationClaimTokenRune(r) {
				if tokenStart < 0 {
					tokenStart = index
				}
				continue
			}
			if tokenStart >= 0 {
				if !consume(body[tokenStart:index]) {
					return nil, false
				}
				tokenStart = -1
			}
		}
		if tokenStart >= 0 && !consume(body[tokenStart:]) {
			return nil, false
		}
	}
	return claims, true
}

func localizationUnstructuredClaimLine(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", false
	}
	if colon := strings.IndexByte(line, ':'); colon >= 0 && localizationClaimRoleLabel(line[:colon]) {
		line = strings.TrimSpace(line[colon+1:])
		return line, line != ""
	}
	// A stand-alone colon-terminated line is prose structure, not a material
	// code claim. This covers arbitrary headings such as
	// "Implementation_details:" without maintaining an open-ended vocabulary.
	if strings.HasSuffix(line, ":") {
		return "", false
	}
	return line, true
}

func localizationClaimRoleLabel(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	switch value {
	case "primary", "supporting", "evidence", "file", "files", "symbol", "symbols",
		"implementation", "implementation_details", "details", "answer", "result", "results":
		return true
	default:
		return false
	}
}

func localizationClaimTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_.$:#\\/-", r)
}

func localizationStructuredSymbolClaimsBounded(message string) ([]string, bool, bool) {
	if len(message) > localizationClaimCheckMaxMessageBytes {
		return nil, false, false
	}
	claims := make([]string, 0, 4)
	explicitNone := false
	inSymbols := false
	seen := make(map[string]struct{}, 4)
	tokenCount := 0
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
		tokenCount++
		if tokenCount > localizationClaimCheckMaxTokens {
			return nil, false, false
		}
		if localizationExplicitNoneClaim(trimmed) {
			explicitNone = true
			continue
		}
		token := localizationFirstClaimToken(trimmed)
		if len(token) > localizationClaimCheckMaxTokenBytes {
			return nil, false, false
		}
		claim := localizationStructuredSymbolClaim(token)
		if claim == "" {
			continue
		}
		key := strings.ToLower(claim)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		if len(claims) >= localizationClaimCheckMaxClaims {
			return nil, false, false
		}
		seen[key] = struct{}{}
		claims = append(claims, claim)
	}
	return claims, explicitNone, true
}

func localizationFirstClaimToken(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexFunc(value, unicode.IsSpace); index >= 0 {
		return value[:index]
	}
	return value
}

func localizationExplicitNoneClaim(value string) bool {
	value = strings.ToLower(strings.Trim(strings.TrimSpace(value), "`._:;,-"))
	switch value {
	case "none", "none fits", "none fit", "none of these", "none of the above",
		"no symbol fits", "no symbols fit", "no listed symbol fits", "no listed symbols fit":
		return true
	default:
		return false
	}
}

func localizationStructuredSymbolClaim(value string) string {
	claim := strings.Trim(localizationFirstClaimToken(value), "`_.$:#\\/-,;")
	for strings.HasSuffix(claim, "()") {
		claim = strings.TrimSuffix(claim, "()")
	}
	if strings.HasPrefix(claim, "(*") {
		if close := strings.Index(claim, ")."); close > 2 {
			claim = claim[2:close] + claim[close+1:]
		}
	} else if strings.HasPrefix(claim, "(") {
		if close := strings.Index(claim, ")."); close > 1 {
			claim = claim[1:close] + claim[close+1:]
		}
	}
	return strings.Trim(claim, "`_.$:#\\/-,;")
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
	if claim == "" || claim == localizationRejectedClaim || evidenceID == "" {
		return false
	}
	if claim == evidenceID {
		return true
	}
	// A file-qualified claim carries stronger identity than its symbol suffix.
	// It must match the full authenticated graph ID, never a same-named symbol
	// in another file.
	if localizationFileQualifiedClaim(claim) {
		return false
	}

	claimParts := localizationSymbolIdentityParts(claim)
	evidenceParts := localizationSymbolIdentityParts(localizationEvidenceSymbolIdentity(evidenceID))
	if len(claimParts) == 0 || len(evidenceParts) == 0 || len(claimParts) > len(evidenceParts) {
		return false
	}
	start := len(evidenceParts) - len(claimParts)
	for index := range claimParts {
		if claimParts[index] != evidenceParts[start+index] {
			return false
		}
	}
	return true
}

func localizationFileQualifiedClaim(claim string) bool {
	separator := strings.Index(claim, "::")
	if separator <= 0 {
		return false
	}
	prefix := claim[:separator]
	if strings.Contains(prefix, "/") || (len(prefix) >= 2 && prefix[1] == ':') {
		return true
	}
	return localizationLooksLikeSourcePath(prefix)
}

func localizationLooksLikeSourcePath(value string) bool {
	value = strings.ToLower(value)
	for _, extension := range []string{".go", ".py", ".js", ".ts", ".tsx", ".rs", ".java", ".php", ".rb", ".swift", ".dart", ".cs"} {
		if strings.HasSuffix(value, extension) {
			return true
		}
	}
	return false
}

func localizationEvidenceSymbolIdentity(evidenceID string) string {
	if separator := strings.Index(evidenceID, "::"); separator >= 0 && separator+2 < len(evidenceID) {
		return evidenceID[separator+2:]
	}
	return evidenceID
}

func localizationSymbolIdentityParts(identity string) []string {
	return strings.FieldsFunc(strings.ToLower(strings.TrimSpace(identity)), func(r rune) bool {
		return strings.ContainsRune(".:#\\/", r) || unicode.IsSpace(r) || r == '(' || r == ')' || r == '*'
	})
}

func boundedLocalizationClaimCheck(prompt string) string {
	if len(prompt) <= localizationClaimCheckMaxChars {
		return prompt
	}
	const suffix = "… Do not retrieve more evidence."
	cut := localizationClaimCheckMaxChars - len(suffix)
	for cut > 0 && !utf8.RuneStart(prompt[cut]) {
		cut--
	}
	return fmt.Sprintf("%s%s", prompt[:cut], suffix)
}
