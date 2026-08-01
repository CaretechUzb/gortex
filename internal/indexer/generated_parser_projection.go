package indexer

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

const (
	generatedTreeSitterParserMinBytes       = 8 << 20
	generatedTreeSitterParserHeadBytes      = 256 << 10
	generatedTreeSitterPublicHeaderMaxBytes = 8 << 10
	generatedTreeSitterPublicBodyMaxBytes   = 64 << 10
	generatedTreeSitterPublicEntryMaxCount  = 16

	generatedTreeSitterParserInclude        = `#include "tree_sitter/parser.h"`
	generatedParserProjectionMetaKey        = "generated_parser_projection"
	generatedParserProjectionVersionMetaKey = "generated_parser_projection_version"
	generatedParserProjectionPolicyVersion  = 2
)

var (
	generatedTreeSitterParserMarkers = [][]byte{
		[]byte(generatedTreeSitterParserInclude),
		[]byte("#define LANGUAGE_VERSION"),
		[]byte("#define STATE_COUNT"),
	}
	generatedTreeSitterPublicCandidateRE = regexp.MustCompile(
		`(?m)^[\t ]*(TS_PUBLIC|extern)[ \t\r\n]+const[ \t\r\n]+TSLanguage[ \t\r\n]*\*[ \t\r\n]*(tree_sitter_[A-Za-z_][A-Za-z0-9_]*)[ \t\r\n]*\(`,
	)
	generatedTreeSitterPublicEntryRE = regexp.MustCompile(
		`^[\t ]*(TS_PUBLIC|extern)[ \t\r\n]+const[ \t\r\n]+TSLanguage[ \t\r\n]*\*[ \t\r\n]*(tree_sitter_[A-Za-z_][A-Za-z0-9_]*)[ \t\r\n]*\([ \t\r\n]*void[ \t\r\n]*\)[ \t\r\n]*\{`,
	)
)

type extractionDisposition uint8

const (
	extractionDispositionFull extractionDisposition = iota
	extractionDispositionGeneratedParserProjection
)

func extractionDispositionFor(result *parser.ExtractionResult) extractionDisposition {
	if result == nil {
		return extractionDispositionFull
	}
	for _, node := range result.Nodes {
		if node == nil || node.Kind != graph.KindFile {
			continue
		}
		projected, _ := node.Meta[generatedParserProjectionMetaKey].(bool)
		if projected {
			return extractionDispositionGeneratedParserProjection
		}
		break
	}
	return extractionDispositionFull
}

func (d extractionDisposition) omitSecondarySourceScans() bool {
	return d == extractionDispositionGeneratedParserProjection
}

type generatedTreeSitterPublicEntry struct {
	name      string
	signature string
	startLine int
	endLine   int
}

// generatedTreeSitterPublicEntries walks the complete source while retaining a
// fixed upper bound of public-entry candidates. A generated parser normally has
// one entry point. Any ambiguous shape, duplicate, oversized declaration/body,
// or unexpected candidate count fails closed to the full C extractor.
func generatedTreeSitterPublicEntries(src []byte) ([]generatedTreeSitterPublicEntry, bool) {
	entries := make([]generatedTreeSitterPublicEntry, 0, 1)
	seen := make(map[string]struct{}, 1)
	searchStart := 0
	for searchStart < len(src) {
		candidate := generatedTreeSitterPublicCandidateRE.FindSubmatchIndex(src[searchStart:])
		if candidate == nil {
			break
		}
		if len(candidate) < 6 || len(entries) >= generatedTreeSitterPublicEntryMaxCount {
			return nil, false
		}

		entryStart := searchStart + candidate[0]
		candidateEnd := searchStart + candidate[1]
		candidateName := string(src[searchStart+candidate[4] : searchStart+candidate[5]])
		headerEnd := min(len(src), entryStart+generatedTreeSitterPublicHeaderMaxBytes)
		entryMatch := generatedTreeSitterPublicEntryRE.FindSubmatchIndex(src[entryStart:headerEnd])
		if len(entryMatch) < 6 || entryMatch[0] != 0 {
			return nil, false
		}
		entryName := string(src[entryStart+entryMatch[4] : entryStart+entryMatch[5]])
		if entryName != candidateName {
			return nil, false
		}
		if _, duplicate := seen[entryName]; duplicate {
			return nil, false
		}

		openingBrace := entryStart + entryMatch[1] - 1
		bodyEnd := min(len(src), openingBrace+generatedTreeSitterPublicBodyMaxBytes)
		closingBrace := matchingCBraceEnd(src[:bodyEnd], openingBrace)
		if closingBrace < 0 {
			return nil, false
		}

		seen[entryName] = struct{}{}
		startLine := bytes.Count(src[:entryStart], []byte{'\n'}) + 1
		entries = append(entries, generatedTreeSitterPublicEntry{
			name:      entryName,
			signature: strings.Join(strings.Fields(string(src[entryStart:openingBrace])), " "),
			startLine: startLine,
			endLine:   startLine + bytes.Count(src[entryStart:closingBrace+1], []byte{'\n'}),
		})
		if candidateEnd <= searchStart {
			return nil, false
		}
		searchStart = candidateEnd
	}
	return entries, len(entries) > 0
}

// staleGeneratedParserProjectionPaths returns only previously projected
// parser.c file nodes produced by an older projection policy. The lookup is one
// bounded ID batch over parser.c candidates; normal C files never opt into this
// refresh path merely because their basename matches.
func (idx *Indexer) staleGeneratedParserProjectionPaths(relPaths []string) []string {
	ids := make([]string, 0, len(relPaths))
	relByID := make(map[string]string, len(relPaths))
	seen := make(map[string]struct{}, len(relPaths))
	for _, relPath := range relPaths {
		if filepath.Base(filepath.FromSlash(relPath)) != "parser.c" {
			continue
		}
		id := relPath
		if idx.repoPrefix != "" {
			id = idx.repoPrefix + "/" + relPath
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		relByID[id] = relPath
	}
	if len(ids) == 0 {
		return nil
	}

	nodes := idx.graph.GetNodesByIDs(ids)
	stale := make([]string, 0, len(ids))
	for _, id := range ids {
		node := nodes[id]
		if node == nil || node.Kind != graph.KindFile || node.Meta == nil {
			continue
		}
		projected, _ := node.Meta[generatedParserProjectionMetaKey].(bool)
		if projected && metaInt(node.Meta, generatedParserProjectionVersionMetaKey) < generatedParserProjectionPolicyVersion {
			stale = append(stale, relByID[id])
		}
	}
	return stale
}

func (idx *Indexer) hasStaleGeneratedParserProjections() bool {
	idx.mtimeMu.RLock()
	candidates := make([]string, 0, 1)
	for relPath := range idx.fileMtimes {
		if filepath.Base(filepath.FromSlash(relPath)) == "parser.c" {
			candidates = append(candidates, relPath)
		}
	}
	idx.mtimeMu.RUnlock()
	return len(idx.staleGeneratedParserProjectionPaths(candidates)) > 0
}

// generatedTreeSitterParserProjection recognizes only the pathological,
// generated parser tables emitted by tree-sitter and returns a compact graph
// projection for them. The multi-megabyte parse tables contain implementation
// data rather than useful declarations; retaining the file and its public
// tree_sitter_* entry point preserves the generated parser's queryable API
// without constructing a full C syntax tree.
func generatedTreeSitterParserProjection(relPath, lang string, src []byte) (*parser.ExtractionResult, bool) {
	if !isGeneratedTreeSitterParserTable(relPath, lang, src) {
		return nil, false
	}

	entries, certain := generatedTreeSitterPublicEntries(src)
	if !certain {
		return nil, false
	}

	fileEndLine := bytes.Count(src, []byte{'\n'}) + 1
	fileMeta := map[string]any{
		"generated":                             true,
		"codegen_tool":                          "tree-sitter",
		"skip_reason":                           "generated_parser_table",
		"skipped_due_to_generated_parser_table": true,
		generatedParserProjectionMetaKey:        true,
		generatedParserProjectionVersionMetaKey: generatedParserProjectionPolicyVersion,
		"generated_parser_internals_omitted":    true,
		"projection":                            "tree_sitter_public_entry",
		"source_bytes":                          len(src),
		"projected_symbol_count":                len(entries),
	}
	result := &parser.ExtractionResult{
		Nodes: []*graph.Node{{
			ID:        relPath,
			Kind:      graph.KindFile,
			Name:      relPath,
			FilePath:  relPath,
			StartLine: 1,
			EndLine:   fileEndLine,
			Language:  lang,
			Meta:      fileMeta,
		}},
	}

	includeOffset := bytes.Index(src[:min(len(src), generatedTreeSitterParserHeadBytes)], []byte(generatedTreeSitterParserInclude))
	if includeOffset < 0 {
		return nil, false
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     relPath,
		To:       "unresolved::import::tree_sitter/parser.h",
		Kind:     graph.EdgeImports,
		FilePath: relPath,
		Line:     bytes.Count(src[:includeOffset], []byte{'\n'}) + 1,
		Meta:     map[string]any{"include_kind": "quoted"},
	})

	for _, entry := range entries {
		id := relPath + "::" + entry.name
		function := &graph.Node{
			ID:        id,
			Kind:      graph.KindFunction,
			Name:      entry.name,
			FilePath:  relPath,
			StartLine: entry.startLine,
			EndLine:   entry.endLine,
			Language:  lang,
			Meta: map[string]any{
				"signature":                             entry.signature,
				"generated":                             true,
				"codegen_tool":                          "tree-sitter",
				generatedParserProjectionMetaKey:        true,
				generatedParserProjectionVersionMetaKey: generatedParserProjectionPolicyVersion,
			},
		}
		result.Nodes = append(result.Nodes, function)
		result.Edges = append(result.Edges, &graph.Edge{
			From:     relPath,
			To:       id,
			Kind:     graph.EdgeDefines,
			FilePath: relPath,
			Line:     entry.startLine,
		})
	}

	// Keep the compact projection on the same metadata contract as every full
	// extractor. Passing nil source avoids copying/splitting a multi-megabyte
	// generated table; the explicit signature is already sufficient for the
	// shared normalizer to derive the canonical retrieval QualName.
	normalizeExtractionMetadata(result, nil)
	return result, true
}

// shouldRecordNativeParsePressure reports whether extraction entered an
// in-process tree-sitter parser. A generated-parser projection is successful,
// but it is the one disposition known to bypass native parsing entirely.
func shouldRecordNativeParsePressure(result *parser.ExtractionResult) bool {
	return !extractionDispositionFor(result).omitSecondarySourceScans()
}

func isGeneratedTreeSitterParserTable(relPath, lang string, src []byte) bool {
	if lang != "c" || filepath.Base(relPath) != "parser.c" || len(src) < generatedTreeSitterParserMinBytes {
		return false
	}
	headEnd := len(src)
	if headEnd > generatedTreeSitterParserHeadBytes {
		headEnd = generatedTreeSitterParserHeadBytes
	}
	head := src[:headEnd]
	for _, marker := range generatedTreeSitterParserMarkers {
		if !bytes.Contains(head, marker) {
			return false
		}
	}
	return true
}

// matchingCBraceEnd finds the closing brace for a C function body without
// allocating a syntax tree. Strings and comments are skipped so braces in
// generated symbol names or comments do not truncate the projected range.
func matchingCBraceEnd(src []byte, openingBrace int) int {
	if openingBrace < 0 || openingBrace >= len(src) || src[openingBrace] != '{' {
		return -1
	}

	const (
		cCode = iota
		cSingleQuote
		cDoubleQuote
		cLineComment
		cBlockComment
	)
	state := cCode
	depth := 0
	for i := openingBrace; i < len(src); i++ {
		c := src[i]
		switch state {
		case cCode:
			switch c {
			case '\'':
				state = cSingleQuote
			case '"':
				state = cDoubleQuote
			case '/':
				if i+1 < len(src) && src[i+1] == '/' {
					state = cLineComment
					i++
				} else if i+1 < len(src) && src[i+1] == '*' {
					state = cBlockComment
					i++
				}
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return i
				}
			}
		case cSingleQuote:
			if c == '\\' && i+1 < len(src) {
				i++
			} else if c == '\'' {
				state = cCode
			}
		case cDoubleQuote:
			if c == '\\' && i+1 < len(src) {
				i++
			} else if c == '"' {
				state = cCode
			}
		case cLineComment:
			if c == '\n' {
				state = cCode
			}
		case cBlockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state = cCode
				i++
			}
		}
	}
	return -1
}
