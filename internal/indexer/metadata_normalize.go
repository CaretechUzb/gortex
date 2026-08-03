package indexer

import (
	"bytes"
	"path"
	"strconv"
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// normalizeExtractionMetadata adds retrieval-only metadata at the shared
// extraction boundary. Parser-owned signature, documentation, and QualName
// values remain untouched: resolvers may rely on their exact representation.
func normalizeExtractionMetadata(result *parser.ExtractionResult, src []byte) {
	if result == nil {
		return
	}
	sourceLines := newMetadataSourceLines(src)

	nodesByID := make(map[string]*graph.Node, len(result.Nodes))
	for _, n := range result.Nodes {
		if n != nil {
			nodesByID[n.ID] = n
		}
	}
	ownerIDs := make(map[string]string)
	legacyLocals := make(map[string]bool)
	for _, edge := range result.Edges {
		if edge == nil || edge.Kind != graph.EdgeMemberOf || edge.From == "" || edge.To == "" {
			continue
		}
		if current := ownerIDs[edge.From]; current == "" || edge.To < current {
			ownerIDs[edge.From] = edge.To
		}
		if owner := nodesByID[edge.To]; owner != nil && localScopeOwner(owner.Kind) {
			legacyLocals[edge.From] = true
		}
	}
	ownerNodes := make(map[string]*graph.Node, len(ownerIDs))
	ownerNames := make(map[string]string, len(ownerIDs))
	for childID, ownerID := range ownerIDs {
		if owner := nodesByID[ownerID]; owner != nil {
			ownerNodes[childID] = owner
		} else {
			ownerNames[childID] = ownerNameFromID(ownerID)
		}
	}
	qualNames := make(map[*graph.Node]string, len(result.Nodes))

	for _, n := range result.Nodes {
		if n == nil || n.Name == "" {
			continue
		}
		if legacyLocalVariable(n, legacyLocals[n.ID]) {
			graph.SuppressRetrievalMetadata(n)
			continue
		}
		if !shouldNormalizeDefinitionMetadata(n.Kind) {
			// Params, locals, imports, builtins, and synthetic graph entities
			// often share their owner's StartLine. Deriving from source would
			// copy the enclosing declaration and doc into every child node.
			graph.SetRetrievalMetadata(n, graph.RetrievalMetadata{})
			continue
		}
		sig := compactSignature(normalizedMetaString(n.Meta, "signature"), n.Language)
		if sig == "" || syntheticSignature(sig, n.Name) {
			if derived := declarationSignatureFromSource(&sourceLines, n); derived != "" {
				sig = derived
			}
		}
		if sig == "" {
			sig = fallbackSignature(n)
		}
		sig = compactSignature(sig, n.Language)

		doc := normalizedDoc(firstNonEmpty(
			metaString(n.Meta, "doc"),
			metaString(n.Meta, "documentation"),
			metaString(n.Meta, "comment"),
		))
		if doc == "" {
			doc = docAboveSource(&sourceLines, n.StartLine)
		}

		qual := retrievalQualName(n, ownerNodes, ownerNames, qualNames, make(map[*graph.Node]bool))
		graph.SetRetrievalMetadata(n, graph.RetrievalMetadata{
			Signature: sig,
			QualName:  qual,
			Doc:       doc,
		})
	}
}

func legacyLocalVariable(n *graph.Node, edgeLocal bool) bool {
	if n == nil || n.Kind != graph.KindVariable {
		return false
	}
	if edgeLocal {
		return true
	}
	for _, key := range []string{"local", "is_local"} {
		if value, ok := n.Meta[key].(bool); ok && value {
			return true
		}
	}
	for _, key := range []string{"scope", "scope_kind", "parent_kind", "enclosing_kind", "storage"} {
		value, _ := n.Meta[key].(string)
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "local", "function", "function-local", "method", "closure", "block":
			return true
		}
	}
	return false
}

func localScopeOwner(kind graph.NodeKind) bool {
	switch kind {
	case graph.KindFunction, graph.KindMethod, graph.KindClosure:
		return true
	default:
		return false
	}
}

func shouldNormalizeDefinitionMetadata(kind graph.NodeKind) bool {
	switch kind {
	case graph.KindFunction,
		graph.KindMethod,
		graph.KindType,
		graph.KindInterface,
		graph.KindVariable,
		graph.KindField,
		graph.KindClosure,
		graph.KindConstant,
		graph.KindEnumMember,
		graph.KindMacro:
		return true
	default:
		return false
	}
}

func retrievalQualName(
	n *graph.Node,
	ownerNodes map[string]*graph.Node,
	ownerNames map[string]string,
	cache map[*graph.Node]string,
	visiting map[*graph.Node]bool,
) string {
	if n == nil || strings.TrimSpace(n.Name) == "" {
		return ""
	}
	if cached := cache[n]; cached != "" {
		return cached
	}
	if visiting[n] {
		return boundedMetadata(strings.TrimSpace(n.Name), 512)
	}
	visiting[n] = true
	defer delete(visiting, n)

	separator := qualifierSeparator(n.Language)
	module := moduleQualifier(n)
	native := normalizeQualifier(n.QualName, separator)
	qual := native
	if strings.EqualFold(n.Language, "rust") && native != "" {
		qual = qualifyRustNative(module, native)
	}
	if qual == "" {
		owner := ""
		if ownerNode := ownerNodes[n.ID]; ownerNode != nil {
			owner = retrievalQualName(ownerNode, ownerNodes, ownerNames, cache, visiting)
		}
		if owner == "" {
			owner = normalizeQualifier(normalizedMetaString(n.Meta, "receiver"), separator)
		}
		if owner == "" {
			owner = normalizeQualifier(ownerNames[n.ID], separator)
		}
		if strings.EqualFold(n.Language, "rust") && owner != "" {
			owner = qualifyRustNative(module, owner)
		}
		if owner != "" {
			qual = joinQualifiedWithSeparator(owner, n.Name, separator)
		} else if module != "" {
			qual = joinQualifiedWithSeparator(module, n.Name, separator)
		} else {
			qual = strings.TrimSpace(n.Name)
		}
	}
	qual = boundedMetadata(qual, 512)
	cache[n] = qual
	return qual
}

func qualifyRustNative(module, native string) string {
	module = normalizeQualifier(module, "::")
	native = normalizeQualifier(native, "::")
	if module == "" || native == "" || native == module || strings.HasPrefix(native, module+"::") {
		return native
	}
	native = strings.TrimPrefix(native, "crate::")
	moduleParts := strings.Split(module, "::")
	nativeParts := strings.Split(native, "::")
	overlap := 0
	for size := 1; size <= len(moduleParts) && size <= len(nativeParts); size++ {
		if equalQualifierParts(moduleParts[len(moduleParts)-size:], nativeParts[:size]) {
			overlap = size
		}
	}
	return strings.Join(append(moduleParts, nativeParts[overlap:]...), "::")
}

func equalQualifierParts(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func moduleQualifier(n *graph.Node) string {
	if n == nil {
		return ""
	}
	separator := qualifierSeparator(n.Language)
	for _, key := range []string{"module_path", "module", "namespace", "package"} {
		if value := normalizeQualifier(metaString(n.Meta, key), separator); value != "" {
			return boundedMetadata(value, 384)
		}
	}

	filePath := strings.Trim(strings.ReplaceAll(strings.TrimSpace(n.FilePath), "\\", "/"), "/")
	if filePath == "" {
		if strings.EqualFold(n.Language, "rust") {
			return "crate"
		}
		return normalizeQualifier(n.RepoPrefix, separator)
	}
	parts := strings.Split(filePath, "/")
	base := parts[len(parts)-1]
	stem := strings.TrimSuffix(base, path.Ext(base))
	dirs := append([]string(nil), parts[:len(parts)-1]...)
	language := strings.ToLower(strings.TrimSpace(n.Language))
	components := dirs
	switch language {
	case "rust":
		components = append(components, stem)
		if stem == "lib" || stem == "main" || stem == "mod" {
			components = components[:len(components)-1]
		}
		for i := len(components) - 1; i >= 0; i-- {
			if components[i] == "src" {
				components = append(components[:i], components[i+1:]...)
				break
			}
		}
	case "python":
		if stem != "__init__" {
			components = append(components, stem)
		}
	case "typescript", "tsx", "javascript", "jsx":
		if stem != "index" {
			components = append(components, stem)
		}
	case "go", "golang", "java":
		// Go package and Java namespace identity comes from the directory;
		// the declaration name already represents the file's primary type.
	default:
		components = append(components, stem)
	}

	module := normalizeQualifier(strings.Join(components, "/"), separator)
	if module == "" {
		module = normalizeQualifier(n.RepoPrefix, separator)
	}
	if module == "" && language == "rust" {
		module = "crate"
	}
	return boundedMetadata(module, 384)
}

func qualifierSeparator(language string) string {
	if strings.EqualFold(strings.TrimSpace(language), "rust") {
		return "::"
	}
	return "."
}

func normalizeQualifier(value, separator string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '/' || r == '\\' || r == '.' || r == ':'
	})
	clean := parts[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" && part != "." {
			clean = append(clean, part)
		}
	}
	return strings.Join(clean, separator)
}

func joinQualifiedWithSeparator(owner, name, separator string) string {
	owner = strings.TrimSpace(owner)
	name = strings.Trim(strings.TrimSpace(name), ".:")
	if owner == "" || name == "" {
		return ""
	}
	if owner == name || strings.HasSuffix(owner, separator+name) {
		return owner
	}
	return owner + separator + name
}

func fallbackSignature(n *graph.Node) string {
	if n == nil {
		return ""
	}
	return boundedMetadata(strings.Join(strings.Fields(string(n.Kind)+" "+n.Name), " "), 512)
}

func metaString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	v, _ := meta[key].(string)
	return v
}

func normalizedMetaString(meta map[string]any, key string) string {
	return strings.Join(strings.Fields(metaString(meta, key)), " ")
}

func syntheticSignature(sig, name string) bool {
	compact := strings.ReplaceAll(sig, " ", "")
	return strings.Contains(compact, name+"(...)") ||
		compact == "function"+name+"()" ||
		compact == "fn"+name+"(...)"
}

type metadataSourceLines struct {
	src     []byte
	starts  []int
	indexed bool
}

func newMetadataSourceLines(src []byte) metadataSourceLines {
	return metadataSourceLines{src: src}
}

func (s *metadataSourceLines) ensureIndexed() {
	if s == nil || s.indexed {
		return
	}
	s.indexed = true
	if len(s.src) == 0 {
		return
	}
	s.starts = append(s.starts, 0)
	for i, c := range s.src {
		if c == '\n' {
			s.starts = append(s.starts, i+1)
		}
	}
}

func (s *metadataSourceLines) len() int {
	s.ensureIndexed()
	return len(s.starts)
}

func (s *metadataSourceLines) line(index int) []byte {
	s.ensureIndexed()
	if index < 0 || index >= len(s.starts) {
		return nil
	}
	start := s.starts[index]
	end := len(s.src)
	if index+1 < len(s.starts) {
		end = s.starts[index+1] - 1
	}
	return s.src[start:end]
}

func (s *metadataSourceLines) materialize(start, end int) []string {
	count := s.len()
	if start < 0 {
		start = 0
	}
	if end > count {
		end = count
	}
	if start >= end {
		return nil
	}
	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		lines = append(lines, string(s.line(i)))
	}
	return lines
}

func declarationSignatureFromSource(lines *metadataSourceLines, n *graph.Node) string {
	if lines == nil || n == nil || n.StartLine < 1 || lines.len() == 0 {
		return ""
	}
	if n.EndColumn > 0 && n.EndLine >= n.StartLine && n.EndLine-n.StartLine < 12 {
		if signature := declarationHeader(declarationCandidateFromSource(lines, n, true), n.Name, n.Language); signature != "" {
			return signature
		}
	}
	return declarationHeader(declarationCandidateFromSource(lines, n, false), n.Name, n.Language)
}

func declarationCandidateFromSource(lines *metadataSourceLines, n *graph.Node, exactSpan bool) string {
	start := n.StartLine - 1
	if start < 0 || start >= lines.len() {
		return ""
	}
	end := start + 12
	if exactSpan {
		end = n.EndLine
	} else if n.EndLine > n.StartLine && n.EndLine < end {
		end = n.EndLine
	}
	if end <= start {
		end = start + 1
	}
	if end > lines.len() {
		end = lines.len()
	}

	const candidateBufferBytes = 2048 + 4
	candidate := make([]byte, 0, candidateBufferBytes)
	started := false
	for lineIndex := start; lineIndex < end && len(candidate) < candidateBufferBytes; lineIndex++ {
		line := lines.line(lineIndex)
		if lineIndex == start && n.StartColumn > 0 {
			if n.StartColumn >= len(line) {
				return ""
			}
			line = line[n.StartColumn:]
		}
		if exactSpan && lineIndex == end-1 && n.EndColumn > 0 && n.EndColumn < len(line) {
			line = line[:n.EndColumn]
		}
		if !started {
			line = bytes.TrimLeftFunc(line, unicode.IsSpace)
			if len(line) == 0 {
				continue
			}
			started = true
		} else {
			candidate = append(candidate, '\n')
		}
		remaining := candidateBufferBytes - len(candidate)
		if len(line) > remaining {
			line = line[:remaining]
		}
		candidate = append(candidate, line...)
	}
	return boundedMetadata(string(candidate), 2048)
}

func docAboveSource(lines *metadataSourceLines, startLine int) string {
	if lines == nil || startLine <= 1 {
		return ""
	}
	count := lines.len()
	end := startLine - 1
	if end > count {
		return ""
	}
	const maxDocScanLines = 96
	start := end - maxDocScanLines
	if start < 0 {
		start = 0
	}
	window := lines.materialize(start, end)
	return docAbove(window, len(window)+1)
}

// declarationSignature extracts only a declaration header from the node's
// source range. It is deliberately retrieval-only and bounded, so an
// imperfect language heuristic can never change symbol identity or resolution.
func declarationSignature(lines []string, n *graph.Node) string {
	if len(lines) == 0 || n == nil || n.StartLine < 1 {
		return ""
	}
	// Columns plus a bounded end range are the closest thing to a parser-owned
	// header span shared by every extractor. Prefer them when present, then fall
	// back to a small source window beginning at the declaration.
	if n.EndColumn > 0 && n.EndLine >= n.StartLine && n.EndLine-n.StartLine < 12 {
		if signature := declarationHeader(declarationCandidate(lines, n, true), n.Name, n.Language); signature != "" {
			return signature
		}
	}
	return declarationHeader(declarationCandidate(lines, n, false), n.Name, n.Language)
}

func declarationCandidate(lines []string, n *graph.Node, exactSpan bool) string {
	start := n.StartLine - 1
	if start < 0 || start >= len(lines) {
		return ""
	}
	end := start + 12
	if exactSpan {
		end = n.EndLine
	} else if n.EndLine > n.StartLine && n.EndLine < end {
		end = n.EndLine
	}
	if end <= start {
		end = start + 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	selected := append([]string(nil), lines[start:end]...)
	if len(selected) == 0 {
		return ""
	}
	if exactSpan && n.EndColumn > 0 {
		last := len(selected) - 1
		if n.EndColumn < len(selected[last]) {
			selected[last] = selected[last][:n.EndColumn]
		}
	}
	if n.StartColumn > 0 {
		if n.StartColumn >= len(selected[0]) {
			return ""
		}
		selected[0] = selected[0][n.StartColumn:]
	}
	candidate := strings.TrimSpace(strings.Join(selected, "\n"))
	return boundedMetadata(candidate, 2048)
}

func declarationHeader(candidate, name, language string) string {
	if candidate == "" || name == "" {
		return ""
	}
	candidateLines := strings.Split(candidate, "\n")
	for len(candidateLines) > 1 {
		line := strings.TrimSpace(candidateLines[0])
		if strings.Contains(line, name) || (!strings.HasPrefix(line, "@") && !strings.HasPrefix(line, "#[")) {
			break
		}
		candidateLines = candidateLines[1:]
	}
	candidate = compactSignature(strings.Join(candidateLines, "\n"), language)
	if candidate == "" || !strings.Contains(candidate, name) {
		return ""
	}
	return candidate
}

func compactSignature(candidate, language string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return ""
	}
	language = strings.ToLower(strings.TrimSpace(language))
	parenDepth, bracketDepth, angleDepth := 0, 0, 0
	quote := rune(0)
	escaped := false
	cut := len(candidate)
	for i, r := range candidate {
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
				continue
			}
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'':
			if language == "rust" && rustLifetimeAt(candidate, i) {
				continue
			}
			quote = r
		case '"', '`':
			quote = r
		case '(':
			parenDepth++
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
		case '[':
			bracketDepth++
		case ']':
			if bracketDepth > 0 {
				bracketDepth--
			}
		case '<':
			if (language == "go" || language == "golang") && i+1 < len(candidate) && candidate[i+1] == '-' {
				continue
			}
			angleDepth++
		case '>':
			if angleDepth > 0 {
				angleDepth--
			}
		case '{', ';':
			if parenDepth == 0 && bracketDepth == 0 && angleDepth == 0 {
				cut = i
			}
		case '\n', '\r':
			if !signatureLanguageAllowsMultiline(language) && parenDepth == 0 && bracketDepth == 0 && angleDepth == 0 {
				cut = i
			}
		}
		if cut != len(candidate) {
			break
		}
	}
	return boundedMetadata(strings.Join(strings.Fields(candidate[:cut]), " "), 512)
}

func signatureLanguageAllowsMultiline(language string) bool {
	switch language {
	case "go", "golang", "rust", "java", "kotlin", "typescript", "tsx", "javascript", "jsx", "c", "cpp", "c++", "csharp", "c#", "swift":
		return true
	default:
		return false
	}
}

func rustLifetimeAt(value string, quote int) bool {
	if quote+1 >= len(value) || !signatureIdentifierByte(value[quote+1]) {
		return false
	}
	end := quote + 2
	for end < len(value) && signatureIdentifierByte(value[end]) {
		end++
	}
	return end >= len(value) || value[end] != '\''
}

func signatureIdentifierByte(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func normalizedDoc(doc string) string {
	if doc == "" {
		return ""
	}
	if attributeDoc := rustDocAttribute(doc); attributeDoc != "" {
		doc = attributeDoc
	}
	lines := strings.Split(doc, "\n")
	for i := range lines {
		line := strings.TrimSpace(lines[i])
		line = strings.TrimSpace(strings.TrimPrefix(line, "/**"))
		line = strings.TrimSpace(strings.TrimPrefix(line, "/*"))
		line = strings.TrimSpace(strings.TrimSuffix(line, "*/"))
		line = strings.TrimSpace(strings.TrimPrefix(line, "///"))
		line = strings.TrimSpace(strings.TrimPrefix(line, "//!"))
		line = strings.TrimSpace(strings.TrimPrefix(line, "//"))
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		line = strings.TrimSpace(strings.TrimPrefix(line, "*"))
		lines[i] = line
	}
	return boundedMetadata(strings.Join(strings.Fields(strings.Join(lines, " ")), " "), 1024)
}

func docAbove(lines []string, startLine int) string {
	if len(lines) == 0 || startLine <= 1 {
		return ""
	}
	const maxDocScanLines = 96
	i := startLine - 2
	scanned := 0
	var reversed []string
	for i >= 0 && scanned < maxDocScanLines {
		if start, attribute, ok := rustAttributeAbove(lines, i, maxDocScanLines-scanned); ok {
			if doc := rustDocAttribute(attribute); doc != "" {
				reversed = append(reversed, doc)
			}
			scanned += i - start + 1
			i = start - 1
			continue
		}

		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "@") {
			i--
			scanned++
			continue
		}
		if strings.HasSuffix(trimmed, "*/") {
			end := i
			for i >= 0 && scanned < maxDocScanLines {
				scanned++
				if strings.Contains(lines[i], "/*") {
					reversed = append(reversed, normalizedDoc(strings.Join(lines[i:end+1], "\n")))
					i--
					break
				}
				i--
			}
			continue
		}
		if isLineDoc(trimmed) {
			end := i
			for i >= 0 && scanned < maxDocScanLines && isLineDoc(strings.TrimSpace(lines[i])) {
				i--
				scanned++
			}
			reversed = append(reversed, normalizedDoc(strings.Join(lines[i+1:end+1], "\n")))
			continue
		}
		break
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return normalizedDoc(strings.Join(reversed, " "))
}

func isLineDoc(line string) bool {
	return strings.HasPrefix(line, "//") || strings.HasPrefix(line, "# ")
}

func rustAttributeAbove(lines []string, end, limit int) (int, string, bool) {
	if end < 0 || end >= len(lines) || limit <= 0 {
		return 0, "", false
	}
	depth := 0
	for i := end; i >= 0 && end-i < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || isLineDoc(line) || strings.HasSuffix(line, "*/") {
			return 0, "", false
		}
		for _, r := range line {
			switch r {
			case '[':
				depth--
			case ']':
				depth++
			}
		}
		if strings.HasPrefix(line, "#[") && depth <= 0 {
			return i, strings.Join(lines[i:end+1], "\n"), true
		}
	}
	return 0, "", false
}

func rustDocAttribute(attribute string) string {
	attribute = strings.TrimSpace(attribute)
	if !strings.HasPrefix(attribute, "#[doc") {
		return ""
	}
	rest := strings.TrimSpace(strings.TrimPrefix(attribute, "#[doc"))
	if !strings.HasPrefix(rest, "=") {
		return ""
	}
	literal := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(rest, "=")), "]"))
	if strings.HasPrefix(literal, "r") {
		quote := strings.IndexByte(literal, '"')
		if quote > 0 {
			hashes := literal[1:quote]
			closing := "\"" + hashes
			if end := strings.LastIndex(literal, closing); end > quote {
				return boundedMetadata(literal[quote+1:end], 1024)
			}
		}
	}
	firstQuote := strings.IndexByte(literal, '"')
	lastQuote := strings.LastIndexByte(literal, '"')
	if firstQuote < 0 || lastQuote <= firstQuote {
		return ""
	}
	value, err := strconv.Unquote(literal[firstQuote : lastQuote+1])
	if err != nil {
		return ""
	}
	return boundedMetadata(value, 1024)
}

func boundedMetadata(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && value[cut]&0xc0 == 0x80 {
		cut--
	}
	return strings.TrimSpace(value[:cut])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func ownerNameFromID(id string) string {
	if id == "" || strings.HasPrefix(id, "unresolved::") {
		return ""
	}
	if idx := strings.LastIndex(id, "::"); idx >= 0 {
		id = id[idx+2:]
	}
	if idx := strings.LastIndexAny(id, ".#"); idx >= 0 {
		id = id[idx+1:]
	}
	return strings.TrimSpace(id)
}
