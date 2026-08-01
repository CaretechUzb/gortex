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
	generatedTreeSitterParserMinBytes  = 8 << 20
	generatedTreeSitterParserHeadBytes = 256 << 10
	generatedTreeSitterParserTailBytes = 1 << 20

	generatedTreeSitterParserInclude = `#include "tree_sitter/parser.h"`
	generatedParserProjectionMetaKey = "generated_parser_projection"
)

var (
	generatedTreeSitterParserMarkers = [][]byte{
		[]byte(generatedTreeSitterParserInclude),
		[]byte("#define LANGUAGE_VERSION"),
		[]byte("#define STATE_COUNT"),
	}
	generatedTreeSitterPublicEntryRE = regexp.MustCompile(
		`(?m)^[\t ]*(TS_PUBLIC|extern)[ \t\r\n]+const[ \t\r\n]+TSLanguage[ \t\r\n]*\*[ \t\r\n]*(tree_sitter_[A-Za-z_][A-Za-z0-9_]*)[ \t\r\n]*\([ \t\r\n]*void[ \t\r\n]*\)[ \t\r\n]*\{`,
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

	tailStart := 0
	if len(src) > generatedTreeSitterParserTailBytes {
		tailStart = len(src) - generatedTreeSitterParserTailBytes
	}
	tail := src[tailStart:]
	matches := generatedTreeSitterPublicEntryRE.FindAllSubmatchIndex(tail, -1)

	type projectedEntry struct {
		name      string
		signature string
		startLine int
		endLine   int
	}
	fileEndLine := bytes.Count(src, []byte{'\n'}) + 1
	entries := make([]projectedEntry, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if len(match) < 6 {
			continue
		}
		name := string(tail[match[4]:match[5]])
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		entryStart := tailStart + match[0]
		openingBrace := tailStart + match[1] - 1
		closingBrace := matchingCBraceEnd(src, openingBrace)
		if closingBrace < 0 {
			continue
		}
		seen[name] = struct{}{}
		startLine := fileEndLine - bytes.Count(src[entryStart:], []byte{'\n'})
		entries = append(entries, projectedEntry{
			name:      name,
			signature: strings.Join(strings.Fields(string(src[entryStart:openingBrace])), " "),
			startLine: startLine,
			endLine:   startLine + bytes.Count(src[entryStart:closingBrace], []byte{'\n'}),
		})
	}
	// A generated parser always exposes at least one tree_sitter_* language
	// function. If the bounded recognizer cannot recover it, fail closed and
	// let the normal C extractor preserve the complete graph rather than
	// silently committing a file-only projection.
	if len(entries) == 0 {
		return nil, false
	}

	fileMeta := map[string]any{
		"generated":                             true,
		"codegen_tool":                          "tree-sitter",
		"skip_reason":                           "generated_parser_table",
		"skipped_due_to_generated_parser_table": true,
		generatedParserProjectionMetaKey:        true,
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
				"signature":                      entry.signature,
				"generated":                      true,
				"codegen_tool":                   "tree-sitter",
				generatedParserProjectionMetaKey: true,
			},
		}
		graph.SetRetrievalMetadata(function, graph.RetrievalMetadata{
			Signature: entry.signature,
			QualName:  entry.name,
		})
		result.Nodes = append(result.Nodes, function)
		result.Edges = append(result.Edges, &graph.Edge{
			From:     relPath,
			To:       id,
			Kind:     graph.EdgeDefines,
			FilePath: relPath,
			Line:     entry.startLine,
		})
	}
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
