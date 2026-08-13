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
	budget := newLocalizationClaimBudget()
	explicitNone := false
	inSymbols := false
	symbolsSawContent := false

	for _, line := range strings.Split(message, "\n") {
		trimmed := strings.TrimSpace(line)
		if localizationSymbolsHeading(trimmed) {
			inSymbols = true
			symbolsSawContent = false
			continue
		}
		if inSymbols {
			if trimmed == "" {
				if symbolsSawContent {
					inSymbols = false
				}
				continue
			}
			if localizationMarkdownHeading(trimmed) || localizationEmptyClaimRoleLabel(trimmed) {
				inSymbols = false
				continue
			}
			token, rest, none, material := localizationStructuredClaimLine(trimmed)
			if material {
				symbolsSawContent = true
				if !budget.countToken(token) {
					return nil, false, false
				}
				if none {
					explicitNone = true
				} else if !budget.addClaim(localizationStructuredSymbolClaim(token), false) {
					return nil, false, false
				}
				if rest != "" && !localizationScanUnstructuredClaimBody(rest, budget) {
					return nil, false, false
				}
				continue
			}
			inSymbols = false
		}
		body, inspect := localizationUnstructuredClaimLine(line)
		if inspect && !localizationScanUnstructuredClaimBody(body, budget) {
			return nil, false, false
		}
	}
	return budget.claims, explicitNone, true
}

type localizationClaimBudget struct {
	claims []string
	seen   map[string]struct{}
	tokens int
}

func newLocalizationClaimBudget() *localizationClaimBudget {
	return &localizationClaimBudget{
		claims: make([]string, 0, 4),
		seen:   make(map[string]struct{}, 4),
	}
}

func (budget *localizationClaimBudget) countToken(token string) bool {
	budget.tokens++
	return budget.tokens <= localizationClaimCheckMaxTokens && len(token) <= localizationClaimCheckMaxTokenBytes
}

func (budget *localizationClaimBudget) addClaim(claim string, requireCodeShape bool) bool {
	if requireCodeShape && (len(claim) < 2 || !localizationCodeShapedClaim(claim)) {
		return true
	}
	if claim == "" {
		return true
	}
	if _, duplicate := budget.seen[claim]; duplicate {
		return true
	}
	if len(budget.claims) >= localizationClaimCheckMaxClaims {
		return false
	}
	budget.seen[claim] = struct{}{}
	budget.claims = append(budget.claims, claim)
	return true
}

func localizationScanUnstructuredClaimBody(body string, budget *localizationClaimBudget) bool {
	tokenStart := -1
	consume := func(token string) bool {
		if !budget.countToken(token) {
			return false
		}
		claim := strings.Trim(token, "_.$:#\\/-")
		return budget.addClaim(claim, true)
	}
	for index, r := range body {
		if localizationClaimTokenRune(r) {
			if tokenStart < 0 {
				tokenStart = index
			}
			continue
		}
		if tokenStart >= 0 {
			if !consume(body[tokenStart:index]) {
				return false
			}
			tokenStart = -1
		}
	}
	return tokenStart < 0 || consume(body[tokenStart:])
}

func localizationUnstructuredClaimLine(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || localizationMarkdownHeading(line) {
		return "", false
	}
	if colon := strings.IndexByte(line, ':'); colon >= 0 && localizationClaimRoleLabel(line[:colon]) {
		line = strings.TrimSpace(line[colon+1:])
		return line, line != ""
	}
	return line, true
}

func localizationMarkdownHeading(line string) bool {
	line = strings.TrimSpace(line)
	count := 0
	for count < len(line) && line[count] == '#' {
		count++
	}
	return count > 0 && count <= 6 && (count == len(line) || line[count] == ' ' || line[count] == '\t')
}

func localizationEmptyClaimRoleLabel(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasSuffix(line, ":") && localizationClaimRoleLabel(strings.TrimSuffix(line, ":"))
}

func localizationSymbolsHeading(line string) bool {
	line = strings.TrimSpace(line)
	return strings.EqualFold(strings.TrimSuffix(line, ":"), "symbols")
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

func localizationStructuredClaimLine(line string) (token, rest string, explicitNone, material bool) {
	line, listed := localizationTrimClaimListMarker(line)
	if line == "" {
		return "", "", false, false
	}
	if localizationExplicitNoneClaim(line) {
		return localizationFirstClaimToken(line), "", true, true
	}
	token = localizationFirstClaimToken(line)
	claim := localizationStructuredSymbolClaim(token)
	if !listed && token != line && !localizationCodeShapedClaim(claim) {
		return "", "", false, false
	}
	rest = strings.TrimSpace(strings.TrimPrefix(line, token))
	return token, rest, false, true
}

func localizationTrimClaimListMarker(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	for _, marker := range []string{"-", "*", "•"} {
		if strings.HasPrefix(line, marker) {
			return strings.TrimSpace(strings.TrimPrefix(line, marker)), true
		}
	}
	index := 0
	for index < len(line) && line[index] >= '0' && line[index] <= '9' {
		index++
	}
	if index > 0 && index+1 < len(line) && line[index] == '.' && (line[index+1] == ' ' || line[index+1] == '\t') {
		return strings.TrimSpace(line[index+1:]), true
	}
	return line, false
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
	claim = localizationNormalizeGoPointerIdentity(strings.TrimSpace(claim))
	evidenceID = strings.TrimSpace(evidenceID)
	if claim == "" || claim == localizationRejectedClaim || evidenceID == "" {
		return false
	}
	if claim == evidenceID {
		return true
	}

	claimFile, claimSymbol, fileQualified := localizationFileQualifiedIdentity(claim)
	evidenceFile, evidenceSymbol := localizationEvidenceIdentity(evidenceID)
	if fileQualified {
		// Graph paths are case-sensitive identities even when a host filesystem
		// happens to fold case. Never let a differently-cased file authenticate.
		return claimFile == evidenceFile && localizationQualifiedSymbolMatches(claimSymbol, evidenceSymbol)
	}
	return localizationQualifiedSymbolMatches(claim, evidenceSymbol)
}

func localizationQualifiedSymbolMatches(claim, evidence string) bool {
	claim = localizationNormalizeGoPointerIdentity(strings.TrimSpace(claim))
	evidence = localizationNormalizeGoPointerIdentity(strings.TrimSpace(evidence))
	if claim == "" || evidence == "" {
		return false
	}
	if claim == evidence {
		return true
	}
	if !localizationQualifiedSymbolClaim(claim) {
		return claim == localizationSymbolLeaf(evidence)
	}
	// Preserve notation as identity: suffix matching is allowed only when the
	// evidence has the same literal separator immediately before the claim.
	for _, separator := range []string{"::", ".", "#", "\\"} {
		if strings.Contains(claim, separator) && strings.HasSuffix(evidence, separator+claim) {
			return true
		}
	}
	return false
}

func localizationQualifiedSymbolClaim(identity string) bool {
	return strings.Contains(identity, "::") || strings.ContainsAny(identity, ".#\\")
}

func localizationSymbolLeaf(identity string) string {
	last := -1
	width := 1
	for _, separator := range []string{"::", ".", "#", "\\"} {
		if index := strings.LastIndex(identity, separator); index > last {
			last = index
			width = len(separator)
		}
	}
	if last >= 0 && last+width < len(identity) {
		return identity[last+width:]
	}
	return identity
}

func localizationNormalizeGoPointerIdentity(identity string) string {
	identity = strings.TrimSpace(identity)
	if strings.HasPrefix(identity, "(*") {
		if close := strings.Index(identity, ")."); close > 2 {
			return identity[2:close] + identity[close+1:]
		}
	} else if strings.HasPrefix(identity, "(") {
		if close := strings.Index(identity, ")."); close > 1 {
			return identity[1:close] + identity[close+1:]
		}
	}
	return identity
}

func localizationFileQualifiedIdentity(identity string) (file, symbol string, ok bool) {
	separator := strings.Index(identity, "::")
	if separator <= 0 || !localizationFileQualifiedClaim(identity) {
		return "", identity, false
	}
	return identity[:separator], identity[separator+2:], true
}

func localizationEvidenceIdentity(evidenceID string) (file, symbol string) {
	if separator := strings.Index(evidenceID, "::"); separator >= 0 && separator+2 < len(evidenceID) {
		return evidenceID[:separator], evidenceID[separator+2:]
	}
	return "", evidenceID
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
