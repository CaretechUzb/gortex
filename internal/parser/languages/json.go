package languages

import (
	"bytes"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// JSON has no symbol semantics, but we still want CLAUDE.md /
// package.json / tsconfig keys to show up in the graph so they can
// be referenced and searched. Extract top-level keys of the outermost
// object as variable nodes. Nested keys are intentionally skipped.
type JSONExtractor struct{}

func NewJSONExtractor() *JSONExtractor { return &JSONExtractor{} }

func (e *JSONExtractor) Language() string     { return "json" }
func (e *JSONExtractor) Extensions() []string { return []string{".json", ".json5", ".jsonc"} }

func (e *JSONExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	endLine := bytes.Count(src, []byte{'\n'}) + 1
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: endLine,
		Language: "json",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	add := func(name string, start int) {
		if name == "" {
			return
		}
		id := filePath + "::" + name
		if seen[id] {
			return
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindVariable, Name: name,
			FilePath: filePath, StartLine: start, EndLine: start,
			Language: "json",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: start,
		})
	}

	// Manual scan: track brace/bracket depth while skipping strings and
	// comments (JSON5/JSONC). Emit each string key that sits at depth == 1
	// inside an object (not an array) and is followed by a `:`.
	//
	// Depth convention: incremented on `{` / `[`, decremented on `}` / `]`.
	// A key is "top-level" when the innermost open container is the
	// outermost `{` — i.e. depth transitions 0 -> 1 on `{`, and we accept
	// keys at depth == 1 while the innermost container is an object.
	var stack []byte // tracks nested container chars: '{' or '['
	line := 1
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == '/' && i+1 < n && src[i+1] == '/':
			// Line comment (JSON5/JSONC).
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			i += 2
			for i+1 < n && (src[i] != '*' || src[i+1] != '/') {
				if src[i] == '\n' {
					line++
				}
				i++
			}
			if i+1 < n {
				i += 2
			}
		case c == '{' || c == '[':
			stack = append(stack, c)
			i++
		case c == '}' || c == ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			i++
		case c == '"':
			// String literal — capture the contents.
			keyStart := i + 1
			j := i + 1
			for j < n {
				if src[j] == '\\' && j+1 < n {
					if src[j+1] == '\n' {
						line++
					}
					j += 2
					continue
				}
				if src[j] == '"' {
					break
				}
				if src[j] == '\n' {
					line++
				}
				j++
			}
			if j >= n {
				i = n
				break
			}
			key := string(src[keyStart:j])
			// Move past closing quote.
			i = j + 1
			// Look ahead past whitespace/comments for `:` — if present at
			// depth 1 inside an object, it's a top-level key.
			k := i
			for k < n {
				if src[k] == ' ' || src[k] == '\t' || src[k] == '\r' || src[k] == '\n' {
					k++
					continue
				}
				break
			}
			if k < n && src[k] == ':' {
				// At depth 1 means stack length is exactly 1 and top is '{'.
				if len(stack) == 1 && stack[0] == '{' {
					add(key, line)
				}
			}
		default:
			i++
		}
	}

	return result, nil
}

var _ parser.Extractor = (*JSONExtractor)(nil)
