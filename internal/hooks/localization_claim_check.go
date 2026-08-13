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

	for index, line := range lines {
		markdown := localizationParseMarkdownContainer(line)
		if marker, ok := localizationMarkdownFenceMarker(markdown.content); ok && !markdown.codeIndented {
			// CommonMark indented code cannot open or close a fenced block.
			if !fence.open {
				fence = marker
				continue
			}
			if marker.character == fence.character && marker.length >= fence.length && marker.closing {
				fence = localizationMarkdownFenceState{}
				continue
			}
		}
		if fence.open {
			// A heading-looking line inside a code fence is code, not a heading.
			// Scan it so a fabricated qualified identity cannot hide behind '#'.
			if !localizationScanUnstructuredClaimBody(line, budget) {
				return nil, false, false
			}
			continue
		}

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
			// Structured rows are explicit identity claims. Do not let a later
			// thematic break masquerade as a Setext underline and suppress one.
			if localizationMarkdownHeading(trimmed) || localizationEmptyClaimRoleLabel(trimmed) {
				inSymbols = false
				continue
			}
			token, rest, none, material := localizationStructuredClaimLine(trimmed)
			if material {
				symbolsSawContent = true
				if token != "" && !budget.countToken(token) {
					return nil, false, false
				}
				if none {
					explicitNone = true
				} else if token != "" && !budget.addClaim(localizationStructuredSymbolClaim(token), false) {
					return nil, false, false
				}
				if rest != "" && !localizationScanUnstructuredClaimBody(rest, budget) {
					return nil, false, false
				}
				continue
			}
			inSymbols = false
		}
		if localizationMarkdownHeadingAt(lines, index) {
			continue
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
		return budget.addClaim(claim, !explicitSyntax)
	}
	for index, r := range body {
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
	if line == "" || localizationMarkdownHeading(line) {
		return "", false
	}
	if colon := strings.IndexByte(line, ':'); colon >= 0 && localizationClaimRoleLabel(line[:colon]) {
		line = strings.TrimSpace(line[colon+1:])
		return line, line != ""
	}
	return line, true
}

type localizationMarkdownFenceState struct {
	open      bool
	character byte
	length    int
	closing   bool
}

func localizationMarkdownFenceMarker(line string) (localizationMarkdownFenceState, bool) {
	line = strings.TrimLeft(line, " \t")
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
	quoteDepth   int
	listItem     bool
	codeIndented bool
}

func localizationParseMarkdownContainer(line string) localizationMarkdownContainer {
	container := localizationMarkdownContainer{content: strings.TrimRight(line, " \t\r")}
	for depth := 0; depth < 8 && container.content != ""; depth++ {
		content, codeIndented := localizationMarkdownIndent(container.content)
		container.content = strings.TrimRight(content, " \t\r")
		if codeIndented {
			container.codeIndented = true
			break
		}
		line = container.content
		if line == "" {
			break
		}
		if line[0] == '>' && (len(line) == 1 || line[1] == ' ' || line[1] == '\t') {
			container.quoteDepth++
			container.content = localizationMarkdownMarkerRemainder(line[1:])
			continue
		}
		if len(line) >= 2 && strings.ContainsRune("-*+", rune(line[0])) && (line[1] == ' ' || line[1] == '\t') {
			container.listItem = true
			container.content = localizationMarkdownMarkerRemainder(line[1:])
			continue
		}
		index := 0
		for index < len(line) && line[index] >= '0' && line[index] <= '9' {
			index++
		}
		if index > 0 && index+1 < len(line) && (line[index] == '.' || line[index] == ')') && (line[index+1] == ' ' || line[index+1] == '\t') {
			container.listItem = true
			container.content = localizationMarkdownMarkerRemainder(line[index+1:])
			continue
		}
		break
	}
	return container
}

func localizationMarkdownIndent(line string) (string, bool) {
	columns := 0
	for index := 0; index < len(line); index++ {
		switch line[index] {
		case ' ':
			columns++
			if columns >= 4 {
				return line[index+1:], true
			}
		case '\t':
			// A leading tab advances to at least the four-column code indent.
			return line[index+1:], true
		default:
			return line[index:], false
		}
	}
	return "", false
}

func localizationMarkdownMarkerRemainder(line string) string {
	if line != "" && (line[0] == ' ' || line[0] == '\t') {
		line = line[1:]
	}
	return strings.TrimRight(line, " \t\r")
}

func localizationMarkdownContainerContent(line string) string {
	return localizationParseMarkdownContainer(line).content
}

func localizationMarkdownHeading(line string) bool {
	container := localizationParseMarkdownContainer(line)
	if container.codeIndented {
		return false
	}
	line = container.content
	count := 0
	for count < len(line) && line[count] == '#' {
		count++
	}
	return count > 0 && count <= 6 && (count == len(line) || line[count] == ' ' || line[count] == '\t')
}

func localizationMarkdownSetextUnderline(line string) bool {
	container := localizationParseMarkdownContainer(line)
	if container.codeIndented {
		return false
	}
	line = container.content
	if line == "" || (line[0] != '=' && line[0] != '-') {
		return false
	}
	for index := 1; index < len(line); index++ {
		if line[index] != line[0] {
			return false
		}
	}
	return true
}

func localizationMarkdownHeadingAt(lines []string, index int) bool {
	if index < 0 || index >= len(lines) {
		return false
	}
	if localizationMarkdownHeading(lines[index]) || localizationMarkdownSetextUnderline(lines[index]) {
		return true
	}
	current := localizationParseMarkdownContainer(lines[index])
	if current.content == "" || current.codeIndented || current.listItem || index+1 >= len(lines) {
		return false
	}
	next := localizationParseMarkdownContainer(lines[index+1])
	return !next.codeIndented && !next.listItem && current.quoteDepth == next.quoteDepth &&
		localizationMarkdownSetextUnderline(next.content)
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
