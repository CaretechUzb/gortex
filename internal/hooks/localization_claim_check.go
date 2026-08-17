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
	lines := strings.Split(message, "\n")
	var fence localizationMarkdownFenceState
	var symbolsContainer localizationMarkdownContainerIdentity

	for index, line := range lines {
		markdown := localizationParseMarkdownContainer(line)
		if !markdown.valid {
			return nil, false, false
		}
		if fence.open {
			contained, sameContainer := localizationParseMarkdownContinuation(line, fence.container)
			if sameContainer {
				if marker, ok := localizationMarkdownFenceMarker(contained.content); ok &&
					!contained.codeIndented && marker.character == fence.character &&
					marker.length >= fence.length && marker.closing {
					fence = localizationMarkdownFenceState{}
					continue
				}
				// A heading-looking line inside a code fence is code, not a heading.
				// Scan it so a fabricated qualified identity cannot hide behind '#'.
				if !localizationScanUnstructuredClaimBody(contained.content, budget) {
					return nil, false, false
				}
				continue
			}
			// A list or quote container ended before its fence closed. CommonMark
			// ends the fenced block with that container; process this same line
			// again as ordinary answer content instead of dropping it.
			fence = localizationMarkdownFenceState{}
		}

		if inSymbols && symbolsContainer.depth > 0 {
			scoped, sameContainer := localizationParseMarkdownContinuationWithChildren(line, symbolsContainer)
			if sameContainer {
				if !scoped.valid {
					return nil, false, false
				}
				markdown = scoped
			} else {
				inSymbols = false
				symbolsSawContent = false
				symbolsContainer = localizationMarkdownContainerIdentity{}
			}
		}
		if marker, ok := localizationMarkdownFenceMarker(markdown.content); ok && !markdown.codeIndented {
			marker.container = markdown.identity()
			fence = marker
			continue
		}

		trimmed := strings.TrimSpace(markdown.content)
		headingBody, heading := localizationMarkdownHeadingBodyAt(lines, index, markdown)
		_, atxHeading := localizationMarkdownATXHeadingBody(markdown.content)
		if heading {
			if role, body, roleLine := localizationClaimRoleLine(headingBody, true); roleLine {
				inSymbols = role == localizationClaimRoleOpen
				symbolsSawContent = false
				symbolsContainer = localizationMarkdownContainerIdentity{}
				if inSymbols {
					symbolsContainer = markdown.identity()
				}
				if body == "" {
					continue
				}
				if inSymbols {
					material, none, valid := localizationAddStructuredClaimLine(body, budget)
					if !valid {
						return nil, false, false
					}
					symbolsSawContent = material
					explicitNone = explicitNone || none
				} else if !localizationScanHeadingClaimBody(body, budget) {
					return nil, false, false
				}
				continue
			}
			if atxHeading {
				inSymbols = false
				symbolsSawContent = false
				symbolsContainer = localizationMarkdownContainerIdentity{}
				if !localizationScanHeadingClaimBody(headingBody, budget) {
					return nil, false, false
				}
				continue
			}
		}
		if localizationMarkdownSetextUnderlineContent(markdown.content) {
			continue
		}
		if role, body, roleLine := localizationClaimRoleLine(trimmed, false); roleLine {
			inSymbols = role == localizationClaimRoleOpen
			symbolsSawContent = false
			symbolsContainer = localizationMarkdownContainerIdentity{}
			if inSymbols {
				symbolsContainer = markdown.identity()
			}
			if body == "" {
				continue
			}
			if inSymbols {
				material, none, valid := localizationAddStructuredClaimLine(body, budget)
				if !valid {
					return nil, false, false
				}
				symbolsSawContent = material
				explicitNone = explicitNone || none
			} else if !localizationScanUnstructuredClaimBody(body, budget) {
				return nil, false, false
			}
			continue
		}
		if inSymbols {
			if trimmed == "" {
				if symbolsSawContent {
					inSymbols = false
					symbolsContainer = localizationMarkdownContainerIdentity{}
				}
				continue
			}
			material, none, valid := localizationAddStructuredClaimLine(trimmed, budget)
			if !valid {
				return nil, false, false
			}
			if material {
				symbolsSawContent = true
				explicitNone = explicitNone || none
				continue
			}
			inSymbols = false
			symbolsContainer = localizationMarkdownContainerIdentity{}
		}
		if heading {
			if !localizationScanHeadingClaimBody(headingBody, budget) {
				return nil, false, false
			}
			continue
		}
		body, inspect := localizationUnstructuredClaimLine(markdown.content)
		if inspect && !localizationScanUnstructuredClaimBody(body, budget) {
			return nil, false, false
		}
	}
	return budget.claims, explicitNone, true
}

func localizationAddStructuredClaimLine(line string, budget *localizationClaimBudget) (bool, bool, bool) {
	token, rest, none, material := localizationStructuredClaimLine(line)
	if !material {
		return false, false, true
	}
	if token != "" && !budget.countToken(token) {
		return false, false, false
	}
	if !none && token != "" && !budget.addClaim(localizationStructuredSymbolClaim(token), false) {
		return false, false, false
	}
	if rest != "" && !localizationScanUnstructuredClaimBody(rest, budget) {
		return false, false, false
	}
	return true, none, true
}

func localizationClaimRoleLine(line string, heading bool) (localizationClaimRoleMode, string, bool) {
	line = strings.TrimSpace(line)
	if colon := strings.IndexByte(line, ':'); colon >= 0 &&
		(colon+1 >= len(line) || line[colon+1] != ':') {
		if role := localizationClaimRole(line[:colon]); role != localizationClaimRoleNone {
			return role, strings.TrimSpace(line[colon+1:]), true
		}
	}
	if heading {
		if role := localizationClaimRole(strings.TrimSuffix(line, ":")); role != localizationClaimRoleNone {
			return role, "", true
		}
	}
	return localizationClaimRoleNone, "", false
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
	return localizationScanClaimBody(body, budget, false)
}

func localizationScanHeadingClaimBody(body string, budget *localizationClaimBudget) bool {
	return localizationScanClaimBody(body, budget, true)
}

func localizationScanClaimBody(body string, budget *localizationClaimBudget, heading bool) bool {
	tokenStart := -1
	skipUntil := -1
	consume := func(start, end int) bool {
		token := body[start:end]
		if !budget.countToken(token) {
			return false
		}
		// '_' and '$' are identifier characters, not wrappers. Trimming them
		// silently changed _private/$foo into another identity (or no claim).
		claim := strings.Trim(token, ".:#\\/-")
		if localizationContextualFileClaim(body, start, end, claim) {
			return true
		}
		explicitSyntax := localizationExplicitInlineClaim(body, start, end, claim)
		if heading {
			qualified := strings.ContainsAny(claim, ".:#") && localizationCodeShapedClaim(claim) ||
				localizationBackslashQualifiedClaim(claim)
			if !explicitSyntax && !qualified {
				return true
			}
			return budget.addClaim(claim, false)
		}
		return budget.addClaim(claim, !explicitSyntax)
	}
	for index, r := range body {
		if index < skipUntil {
			continue
		}
		if r == '(' {
			receiverStart := index
			if tokenStart >= 0 {
				prefix := body[tokenStart:index]
				if strings.HasSuffix(prefix, ".") || strings.HasSuffix(prefix, "::") {
					receiverStart = tokenStart
				}
			}
			if end, claim, ok := localizationGoReceiverClaimAt(body, receiverStart, index); ok {
				if tokenStart >= 0 && tokenStart < receiverStart && !consume(tokenStart, receiverStart) {
					return false
				}
				tokenStart = -1
				if !budget.countToken(body[receiverStart:end]) || !budget.addClaim(claim, false) {
					return false
				}
				skipUntil = end
				continue
			}
		}
		if localizationClaimTokenRune(r) {
			if tokenStart < 0 {
				tokenStart = index
			}
			continue
		}
		if tokenStart >= 0 {
			if !consume(tokenStart, index) {
				return false
			}
			tokenStart = -1
		}
	}
	return tokenStart < 0 || consume(tokenStart, len(body))
}

func localizationGoReceiverClaimAt(body string, start, receiverOpen int) (int, string, bool) {
	if start < 0 || start > receiverOpen || receiverOpen >= len(body) || body[receiverOpen] != '(' {
		return 0, "", false
	}
	prefix := ""
	fileQualified := false
	if start < receiverOpen {
		rawPrefix := body[start:receiverOpen]
		switch {
		case strings.HasSuffix(rawPrefix, "::"):
			file := strings.TrimSuffix(rawPrefix, "::")
			if !localizationFileQualifiedClaim(file + "::receiver") {
				return 0, "", false
			}
			prefix = file + "::"
			fileQualified = true
		case strings.HasSuffix(rawPrefix, "."):
			qualifier := strings.TrimSuffix(rawPrefix, ".")
			if !localizationGoPackageQualifier(qualifier) {
				return 0, "", false
			}
			prefix = qualifier + "."
		default:
			return 0, "", false
		}
	}
	index := receiverOpen + 1
	if index < len(body) && body[index] == '*' {
		index++
	}
	receiverStart := index
	for index < len(body) {
		r, width := utf8.DecodeRuneInString(body[index:])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '$' && r != '.' {
			break
		}
		index += width
	}
	if receiverStart == index || index+2 >= len(body) || body[index] != ')' || body[index+1] != '.' {
		return 0, "", false
	}
	methodStart := index + 2
	index = methodStart
	for index < len(body) {
		r, width := utf8.DecodeRuneInString(body[index:])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '$' {
			break
		}
		index += width
	}
	if methodStart == index {
		return 0, "", false
	}
	receiver := localizationNormalizeGoPointerIdentity(body[receiverOpen:index])
	if !localizationInlineIdentitySyntax(receiver) {
		return 0, "", false
	}
	claim := prefix + receiver
	if !fileQualified && !localizationInlineIdentitySyntax(claim) {
		return 0, "", false
	}
	end := index
	if strings.HasPrefix(body[end:], "()") {
		end += 2
	}
	return end, claim, true
}

func localizationGoPackageQualifier(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if !localizationExplicitIdentityRow(part, part) {
			return false
		}
	}
	return true
}

// localizationExplicitInlineClaim admits a simple lower-case leaf only when
// the answer marks it as code or a call. Ordinary prose words remain outside
// claim checking; qualified and file-qualified identities continue through
// localizationCodeShapedClaim instead.
func localizationContextualFileClaim(body string, start, _ int, claim string) bool {
	if !localizationAmbiguousFileExtension(claim) {
		return false
	}
	const contextBytes = 64
	beforeStart := start - contextBytes
	if beforeStart < 0 {
		beforeStart = 0
	}
	before := body[beforeStart:start]
	if boundary := strings.LastIndexAny(before, ".;!?\n\r"); boundary >= 0 {
		before = before[boundary+1:]
	}
	return localizationFileContextBefore(localizationContextWords(before))
}

func localizationContextWords(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
}

func localizationFileContextBefore(words []string) bool {
	if len(words) == 0 {
		return false
	}
	if localizationFileContextWord(words[len(words)-1]) {
		return true
	}
	if len(words) < 2 {
		return false
	}
	switch words[len(words)-1] {
	case "is", "at", "named", "called":
		return localizationFileContextWord(words[len(words)-2])
	default:
		return false
	}
}

func localizationFileContextWord(word string) bool {
	switch word {
	case "file", "path", "source", "header", "document", "manifest":
		return true
	default:
		return false
	}
}

func localizationExplicitInlineClaim(body string, start, end int, claim string) bool {
	if !localizationInlineIdentitySyntax(claim) {
		return false
	}
	callEnd := end
	callShaped := strings.HasPrefix(body[end:], "()")
	if callShaped {
		callEnd += 2
	}
	return callShaped || localizationInlineCodeDelimited(body, start, callEnd)
}

func localizationInlineIdentitySyntax(claim string) bool {
	if localizationExplicitIdentityRow(claim, claim) {
		return true
	}
	if claim == "" || strings.ContainsAny(claim, "/\\") {
		return false
	}
	parts := strings.FieldsFunc(claim, func(r rune) bool {
		return strings.ContainsRune(".:#", r)
	})
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || !localizationExplicitIdentityRow(part, part) {
			return false
		}
	}
	return true
}

func localizationBackslashQualifiedClaim(claim string) bool {
	parts := strings.Split(claim, "\\")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if !localizationExplicitIdentityRow(part, part) {
			return false
		}
	}
	return true
}

func localizationInlineCodeDelimited(body string, start, end int) bool {
	opening := 0
	for index := start - 1; index >= 0 && body[index] == '`'; index-- {
		opening++
	}
	if opening < 1 || opening > 3 {
		return false
	}
	closing := 0
	for index := end; index < len(body) && body[index] == '`'; index++ {
		closing++
	}
	return closing == opening
}

func localizationUnstructuredClaimLine(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	if _, heading := localizationMarkdownATXHeadingBody(line); heading {
		return "", false
	}
	if localizationClaimRole(line) != localizationClaimRoleNone {
		return "", false
	}
	if colon := strings.IndexByte(line, ':'); colon >= 0 &&
		(colon+1 >= len(line) || line[colon+1] != ':') && localizationClaimRoleLabel(line[:colon]) {
		line = strings.TrimSpace(line[colon+1:])
		return line, line != ""
	}
	return line, true
}

const localizationMarkdownContainerMaxDepth = 8

type localizationMarkdownContainerKind uint8

const (
	localizationMarkdownQuoteContainer localizationMarkdownContainerKind = iota + 1
	localizationMarkdownListContainer
)

type localizationMarkdownContainerFrame struct {
	kind                localizationMarkdownContainerKind
	continuationColumns uint8
}

type localizationMarkdownContainerIdentity struct {
	depth  uint8
	frames [localizationMarkdownContainerMaxDepth]localizationMarkdownContainerFrame
}

type localizationMarkdownFenceState struct {
	open      bool
	character byte
	length    int
	closing   bool
	container localizationMarkdownContainerIdentity
}

func localizationMarkdownFenceMarker(line string) (localizationMarkdownFenceState, bool) {
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return localizationMarkdownFenceState{}, false
	}
	character := line[0]
	length := 0
	for length < len(line) && line[length] == character {
		length++
	}
	if length < 3 {
		return localizationMarkdownFenceState{}, false
	}
	return localizationMarkdownFenceState{
		open:      true,
		character: character,
		length:    length,
		closing:   strings.TrimSpace(line[length:]) == "",
	}, true
}

type localizationMarkdownContainer struct {
	content      string
	path         localizationMarkdownContainerIdentity
	codeIndented bool
	valid        bool
}

func (container localizationMarkdownContainer) identity() localizationMarkdownContainerIdentity {
	return container.path
}

func (container *localizationMarkdownContainer) push(frame localizationMarkdownContainerFrame) bool {
	if int(container.path.depth) >= len(container.path.frames) {
		return false
	}
	container.path.frames[container.path.depth] = frame
	container.path.depth++
	return true
}

func localizationParseMarkdownContainer(line string) localizationMarkdownContainer {
	container := localizationMarkdownContainer{
		content: strings.TrimRight(line, " \t\r"),
		valid:   true,
	}
	return localizationParseMarkdownChildContainers(container, 0)
}

func localizationParseMarkdownChildContainers(
	container localizationMarkdownContainer,
	column int,
) localizationMarkdownContainer {
	for container.content != "" {
		content, indent, codeIndented := localizationMarkdownIndentWidthAt(container.content, column)
		column += indent
		container.content = strings.TrimRight(content, " \t\r")
		if codeIndented {
			container.codeIndented = true
			break
		}
		line := container.content
		if line == "" {
			break
		}
		if line[0] == '>' && (len(line) == 1 || line[1] == ' ' || line[1] == '\t') {
			if !container.push(localizationMarkdownContainerFrame{kind: localizationMarkdownQuoteContainer}) {
				container.valid = false
				return container
			}
			var padding int
			container.content, padding = localizationMarkdownMarkerRemainderAt(line[1:], column+1)
			column += 1 + padding
			continue
		}
		frame, remainder, markerColumns, listItem := localizationMarkdownListMarker(line, column)
		if !listItem {
			break
		}
		frame.continuationColumns += uint8(indent)
		if !container.push(frame) {
			container.valid = false
			return container
		}
		column += markerColumns
		container.content = strings.TrimRight(remainder, " \t\r")
	}
	return container
}

func localizationParseMarkdownContinuation(
	line string,
	identity localizationMarkdownContainerIdentity,
) (localizationMarkdownContainer, bool) {
	return localizationParseMarkdownContinuationMode(line, identity, false)
}

func localizationParseMarkdownContinuationWithChildren(
	line string,
	identity localizationMarkdownContainerIdentity,
) (localizationMarkdownContainer, bool) {
	return localizationParseMarkdownContinuationMode(line, identity, true)
}

func localizationParseMarkdownContinuationMode(
	line string,
	identity localizationMarkdownContainerIdentity,
	parseChildren bool,
) (localizationMarkdownContainer, bool) {
	container := localizationMarkdownContainer{
		content: strings.TrimRight(line, " \t\r"),
		path:    identity,
		valid:   true,
	}
	column := 0
	for index := 0; index < int(identity.depth); index++ {
		frame := identity.frames[index]
		if strings.TrimSpace(container.content) == "" {
			for remaining := index; remaining < int(identity.depth); remaining++ {
				if identity.frames[remaining].kind == localizationMarkdownQuoteContainer {
					return localizationMarkdownContainer{}, false
				}
			}
			container.content = ""
			return container, true
		}
		switch frame.kind {
		case localizationMarkdownQuoteContainer:
			content, indent, codeIndented := localizationMarkdownIndentWidthAt(container.content, column)
			column += indent
			if codeIndented || content == "" || content[0] != '>' ||
				len(content) > 1 && content[1] != ' ' && content[1] != '\t' {
				return localizationMarkdownContainer{}, false
			}
			var padding int
			container.content, padding = localizationMarkdownMarkerRemainderAt(content[1:], column+1)
			column += 1 + padding
		case localizationMarkdownListContainer:
			required := int(frame.continuationColumns)
			content, indent, ok := localizationMarkdownConsumeIndentAt(
				container.content, required, column,
			)
			if !ok {
				return localizationMarkdownContainer{}, false
			}
			if indent > required {
				content = strings.Repeat(" ", indent-required) + content
				indent = required
			}
			column += indent
			container.content = strings.TrimRight(content, " \t\r")
		default:
			return localizationMarkdownContainer{}, false
		}
	}
	if parseChildren {
		return localizationParseMarkdownChildContainers(container, column), true
	}
	content, _, codeIndented := localizationMarkdownIndentWidthAt(container.content, column)
	container.content = strings.TrimRight(content, " \t\r")
	container.codeIndented = codeIndented
	return container, true
}

func localizationMarkdownListMarker(
	line string,
	column int,
) (localizationMarkdownContainerFrame, string, int, bool) {
	markerEnd := 0
	if len(line) >= 2 && strings.ContainsRune("-*+", rune(line[0])) {
		markerEnd = 1
	} else {
		for markerEnd < len(line) && markerEnd < 9 && line[markerEnd] >= '0' && line[markerEnd] <= '9' {
			markerEnd++
		}
		if markerEnd == 0 || markerEnd >= len(line) ||
			(line[markerEnd] != '.' && line[markerEnd] != ')') {
			return localizationMarkdownContainerFrame{}, "", 0, false
		}
		markerEnd++
	}
	if markerEnd >= len(line) || line[markerEnd] != ' ' && line[markerEnd] != '\t' {
		return localizationMarkdownContainerFrame{}, "", 0, false
	}

	paddingEnd := markerEnd
	paddingColumns := 0
	for paddingEnd < len(line) && (line[paddingEnd] == ' ' || line[paddingEnd] == '\t') {
		if line[paddingEnd] == ' ' {
			paddingColumns++
		} else {
			paddingColumns += 4 - (column+markerEnd+paddingColumns)%4
		}
		paddingEnd++
	}
	if paddingColumns > 4 {
		paddingEnd = markerEnd + 1
		paddingColumns = 1
		if line[markerEnd] == '\t' {
			paddingColumns = 4 - (column+markerEnd)%4
		}
	}
	markerColumns := markerEnd + paddingColumns
	return localizationMarkdownContainerFrame{
		kind:                localizationMarkdownListContainer,
		continuationColumns: uint8(markerColumns),
	}, line[paddingEnd:], markerColumns, true
}

func localizationMarkdownIndentWidthAt(line string, column int) (string, int, bool) {
	columns := 0
	for index := 0; index < len(line); index++ {
		switch line[index] {
		case ' ':
			columns++
		case '\t':
			columns += 4 - (column+columns)%4
		default:
			return line[index:], columns, false
		}
		if columns >= 4 {
			return line[index+1:], columns, true
		}
	}
	return "", columns, columns >= 4
}

func localizationMarkdownConsumeIndentAt(line string, required, column int) (string, int, bool) {
	columns := 0
	index := 0
	for index < len(line) && columns < required {
		switch line[index] {
		case ' ':
			columns++
		case '\t':
			columns += 4 - (column+columns)%4
		default:
			return "", 0, false
		}
		index++
	}
	if columns < required {
		return "", 0, false
	}
	return line[index:], columns, true
}

func localizationMarkdownMarkerRemainderAt(line string, column int) (string, int) {
	padding := 0
	if line != "" {
		switch line[0] {
		case ' ':
			padding = 1
			line = line[1:]
		case '\t':
			// CommonMark permits one optional padding column after a quote
			// marker. Preserve the tab's remaining virtual columns so they
			// still contribute to code indentation after the container prefix.
			width := 4 - column%4
			line = line[1:]
			if width > 1 {
				line = strings.Repeat(" ", width-1) + line
			}
			padding = 1
		}
	}
	return strings.TrimRight(line, " \t\r"), padding
}

func localizationMarkdownATXHeadingBody(line string) (string, bool) {
	count := 0
	for count < len(line) && line[count] == '#' {
		count++
	}
	if count == 0 || count > 6 || count < len(line) && line[count] != ' ' && line[count] != '\t' {
		return "", false
	}
	body := strings.TrimSpace(line[count:])
	closing := len(body)
	for closing > 0 && body[closing-1] == '#' {
		closing--
	}
	if closing < len(body) && (closing == 0 || body[closing-1] == ' ' || body[closing-1] == '\t') {
		body = strings.TrimSpace(body[:closing])
	}
	return body, true
}

func localizationMarkdownSetextUnderlineContent(content string) bool {
	if content == "" || (content[0] != '=' && content[0] != '-') {
		return false
	}
	for index := 1; index < len(content); index++ {
		if content[index] != content[0] {
			return false
		}
	}
	return true
}

func localizationMarkdownHeadingBodyAt(
	lines []string,
	index int,
	current localizationMarkdownContainer,
) (string, bool) {
	if index < 0 || index >= len(lines) || current.codeIndented {
		return "", false
	}
	if body, ok := localizationMarkdownATXHeadingBody(current.content); ok {
		return body, true
	}
	if current.content == "" || index+1 >= len(lines) ||
		localizationMarkdownSetextUnderlineContent(current.content) {
		return "", false
	}
	next, sameContainer := localizationParseMarkdownContinuation(lines[index+1], current.identity())
	if !sameContainer || next.codeIndented || !localizationMarkdownSetextUnderlineContent(next.content) {
		return "", false
	}
	return strings.TrimSpace(current.content), true
}

type localizationClaimRoleMode uint8

const (
	localizationClaimRoleNone localizationClaimRoleMode = iota
	localizationClaimRoleOpen
	localizationClaimRoleClose
)

func localizationClaimRole(value string) localizationClaimRoleMode {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	switch value {
	case "primary", "supporting", "symbol", "symbols":
		return localizationClaimRoleOpen
	case "evidence", "file", "files", "implementation", "implementation_details",
		"details", "answer", "result", "results":
		return localizationClaimRoleClose
	default:
		return localizationClaimRoleNone
	}
}

func localizationClaimRoleLabel(value string) bool {
	return localizationClaimRole(value) != localizationClaimRoleNone
}

func localizationClaimTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_.$:#\\/-", r)
}

func localizationStructuredClaimLine(line string) (token, rest string, explicitNone, material bool) {
	line, _ = localizationTrimClaimListMarker(line)
	if line == "" {
		return "", "", false, false
	}
	if localizationExplicitNoneClaim(line) {
		return localizationFirstClaimToken(line), "", true, true
	}
	token = localizationFirstClaimToken(line)
	claim := localizationStructuredSymbolClaim(token)
	rest = strings.TrimSpace(strings.TrimPrefix(line, token))
	if rest == "" && localizationExplicitIdentityRow(token, claim) {
		return token, "", false, true
	}
	if localizationCodeShapedClaim(claim) {
		return token, rest, false, true
	}
	// A prose row inside SYMBOLS is not permission to accept its first bare
	// word as an identity. Scan the complete row for qualified claims instead.
	return "", line, false, true
}

func localizationExplicitIdentityRow(token, claim string) bool {
	if token == "" || claim == "" || localizationStructuredSymbolClaim(token) != claim {
		return false
	}
	for index, r := range claim {
		if index == 0 {
			if !unicode.IsLetter(r) && r != '_' && r != '$' {
				return false
			}
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '$' {
			return false
		}
	}
	return true
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
	claim := strings.Trim(localizationFirstClaimToken(value), "` .:#\\/-,;")
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
	// '_' and '$' remain identity bytes at either edge.
	return strings.Trim(claim, "` .:#\\/-,;")
}

// localizationLooksLikeFileToken rejects stable file basenames and extensions
// from prose claim extraction. File-qualified identities such as foo.c::flush
// are deliberately exempt: their suffix is an explicit symbol identity.
func localizationLooksLikeFileToken(value string) bool {
	if value == "" || strings.Contains(value, "::") || strings.Contains(value, "#") {
		return false
	}
	if strings.ContainsAny(value, "/\\") {
		return true
	}
	if localizationKnownFileBasename(value) {
		return true
	}
	dot := strings.LastIndexByte(value, '.')
	return dot > 0 && dot+1 < len(value) && localizationKnownFileExtension(value[dot+1:])
}

func localizationKnownFileBasename(value string) bool {
	name := strings.ToLower(strings.Trim(value, "`.,;:"))
	for _, stem := range []string{"dockerfile.", "containerfile.", "makefile."} {
		if strings.HasPrefix(name, stem) && len(name) > len(stem) {
			return true
		}
	}
	switch name {
	case "readme", "license", "licence", "copying", "notice", "changelog", "changes", "authors",
		"makefile", "dockerfile", "containerfile", "rakefile", "gemfile", "procfile", "cmakelists.txt":
		return true
	default:
		return false
	}
}

func localizationAmbiguousFileExtension(value string) bool {
	dot := strings.LastIndexByte(value, '.')
	if dot <= 0 || dot+2 != len(value) {
		return false
	}
	switch strings.ToLower(value[dot+1:]) {
	case "m", "r", "s", "v", "d":
		return true
	default:
		return false
	}
}

func localizationKnownFileExtension(extension string) bool {
	switch strings.ToLower(extension) {
	// Native and systems languages. One-letter m/r/s/v/d remain ambiguous
	// unless the surrounding answer explicitly identifies a file or path.
	case "c", "h", "cc", "cp", "cpp", "cxx", "hh", "hpp", "hxx", "mm",
		"go", "rs", "zig", "odin", "hare", "carbon", "asm",
		"sol", "move", "cairo", "nr", "noir", "tact", "bal":
		return true
	// Scripting, web, JVM, .NET, functional, and shell.
	case "py", "pyi", "pyx", "rb", "php", "pl", "pm", "raku", "lua", "tcl",
		"js", "jsx", "mjs", "cjs", "ts", "tsx", "mts", "cts", "coffee",
		"java", "kt", "kts", "scala", "groovy", "clj", "cljs", "cljc", "edn",
		"cs", "fs", "fsx", "vb", "swift", "dart", "ex", "exs", "erl", "hrl",
		"hs", "lhs", "ml", "mli", "mll", "elm", "gleam", "res", "re", "rei",
		"sh", "bash", "zsh", "fish", "ps1", "bat", "cmd", "ahk":
		return true
	// UI, schemas, queries, config, and manifests.
	case "html", "htm", "css", "scss", "sass", "less", "vue", "svelte", "astro",
		"sql", "graphql", "gql", "proto", "thrift", "capnp", "prisma", "wit",
		"json", "jsonc", "json5", "yaml", "yml", "toml", "xml", "xsd", "xsl", "xslt",
		"ini", "cfg", "conf", "properties", "env", "hcl", "tf", "tfvars", "nix",
		"mod", "sum", "work", "lock", "ipynb":
		return true
	// Documents, templates, build files, and data assets.
	case "md", "markdown", "mdx", "rst", "txt", "adoc", "asciidoc", "org", "tex",
		"razor", "cshtml", "jsp", "ejs", "hbs", "twig", "erb", "liquid", "pug",
		"blade", "tmpl", "tpl", "gotmpl", "mustache", "cmake", "gradle", "bazel", "bzl",
		"make", "mk", "ninja", "csv", "tsv", "parquet", "avro", "pdf", "doc", "docx",
		"ppt", "pptx", "xls", "xlsx":
		return true
	default:
		return false
	}
}

func localizationCodeShapedClaim(claim string) bool {
	if localizationLooksLikeFileToken(claim) {
		return false
	}
	if strings.Contains(claim, "/") && !strings.Contains(claim, "::") && !strings.Contains(claim, "#") {
		return false
	}
	if strings.Contains(claim, ":") && !strings.Contains(claim, "::") {
		return false
	}

	parts := strings.FieldsFunc(claim, func(r rune) bool {
		return strings.ContainsRune(".:#\\/", r)
	})
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			continue
		}
		for index, r := range part {
			if index == 0 {
				if !unicode.IsLetter(r) && r != '_' && r != '$' {
					return false
				}
				continue
			}
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '$' {
				return false
			}
		}
	}
	if strings.ContainsAny(claim, "_#$\\") || strings.Contains(claim, "::") {
		return true
	}
	if strings.Contains(claim, ".") {
		// Reject prose abbreviations such as e.g. while retaining ordinary
		// qualified identities such as writer.flush.
		for _, part := range parts {
			if utf8.RuneCountInString(part) > 1 {
				return true
			}
		}
		return false
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
	if !strings.HasSuffix(evidence, claim) {
		return false
	}
	prefix := strings.TrimSuffix(evidence, claim)
	// The claim's internal notation remains literal. Its containing identity
	// may use another language-appropriate separator, but must end at a real
	// boundary rather than merely sharing a textual suffix.
	for _, boundary := range []string{"::", ".", "#", "\\"} {
		if strings.HasSuffix(prefix, boundary) {
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
