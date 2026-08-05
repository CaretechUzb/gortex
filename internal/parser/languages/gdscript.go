package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// GDScript is Godot's Python-flavored scripting language. The
// extractor handles the common surface: `func`, `class_name`,
// `extends`, `signal`, `var`, `const`, `enum`, inner `class`, and
// `preload(...)` / `load(...)` imports. Indentation defines scope, so
// `findBlockEnd` gives a reasonable function range.
//
// Call sites carry their receiver. A `.gd` file IS a class, and
// `class_name X` registers X as a PROJECT-GLOBAL name — no import
// statement exists in the language, so nothing in the graph's import
// closure can corroborate a cross-directory call. Emitting a call as a
// bare `unresolved::event` therefore hands the resolver a name with no
// evidence attached, and its locality fallback answers with whatever
// same-named method sits nearest the caller: `Notify.event()` in
// src/ui/ bound to an unrelated `event` in src/ui/, while the real
// `Notify.event` in src/systems/ was reverted by the cross-package
// guard for want of an import edge GDScript never emits.
//
// So: a call with a receiver is emitted in the member-call shape
// (`unresolved::*.event`) with Meta["receiver_type"] naming the
// receiver's type, and the funcs of a `class_name` script are emitted
// as KindMethod carrying Meta["receiver"] = that class name. Those are
// the two halves the resolver's exact-receiver passes match on, and
// together they bind the call structurally — at the ast_resolved tier,
// which the cross-package guard does not police — regardless of which
// directory the definition lives in.
var (
	gdFuncRe      = regexp.MustCompile(`(?m)^[ \t]*(?:static\s+)?func\s+(\w+)\s*\(`)
	gdClassNameRe = regexp.MustCompile(`(?m)^\s*class_name\s+(\w+)`)
	gdInnerClass  = regexp.MustCompile(`(?m)^[ \t]*class\s+(\w+)`)
	gdExtendsRe   = regexp.MustCompile(`(?m)^\s*extends\s+([\w.]+)`)
	gdVarRe       = regexp.MustCompile(`(?m)^[ \t]*(?:@\w+(?:\([^)]*\))?\s+)*(?:static\s+)?var\s+(\w+)`)
	gdConstRe     = regexp.MustCompile(`(?m)^[ \t]*const\s+(\w+)`)
	gdEnumRe      = regexp.MustCompile(`(?m)^[ \t]*enum\s+(\w+)`)
	gdSignalRe    = regexp.MustCompile(`(?m)^[ \t]*signal\s+(\w+)`)
	gdPreloadRe   = regexp.MustCompile(`\b(?:preload|load)\s*\(\s*["']([^"']+)["']\s*\)`)

	// gdCallRe captures an optional dotted receiver chain ahead of the
	// callee. `Notify.event(` yields ("Notify", "event"); `a.b.c(`
	// yields ("a.b", "c") — the last segment is the receiver
	// expression; a bare `hello(` yields ("", "hello").
	gdCallRe = regexp.MustCompile(`(?:([A-Za-z_][\w.]*)\s*\.\s*)?\b([A-Za-z_]\w*)\s*\(`)

	// Declared-type sources for the receiver type environment.
	gdTypedVarRe = regexp.MustCompile(`(?m)^[ \t]*(?:@\w+(?:\([^)]*\))?\s+)*(?:static\s+)?var\s+(\w+)\s*:\s*([A-Za-z_]\w*)`)
	gdNewVarRe   = regexp.MustCompile(`(?m)^[ \t]*(?:@\w+(?:\([^)]*\))?\s+)*(?:static\s+)?var\s+(\w+)\s*:?=\s*([A-Za-z_]\w*)\s*\.\s*new\s*\(`)
	gdFuncSigRe  = regexp.MustCompile(`(?m)^[ \t]*(?:static\s+)?func\s+\w+\s*\(([^)]*)\)`)
	gdParamRe    = regexp.MustCompile(`(\w+)\s*:\s*([A-Za-z_]\w*)`)
)

// GDScriptExtractor extracts Godot GDScript source using regex.
type GDScriptExtractor struct{}

func NewGDScriptExtractor() *GDScriptExtractor { return &GDScriptExtractor{} }

func (e *GDScriptExtractor) Language() string     { return "gdscript" }
func (e *GDScriptExtractor) Extensions() []string { return []string{".gd"} }

// gdOwner is a class body — an inner `class` — that owns the funcs
// declared inside its line span. The file-level `class_name` class owns
// everything outside any inner-class span.
type gdOwner struct {
	name  string
	start int
	end   int
}

func (e *GDScriptExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	lines := strings.Split(string(src), "\n")
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: len(lines),
		Language: "gdscript",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	add := func(name string, kind graph.NodeKind, start, end int, meta map[string]any) {
		if name == "" || isGDKeyword(name) {
			return
		}
		id := filePath + "::" + name
		if seen[id] {
			return
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: kind, Name: name,
			FilePath: filePath, StartLine: start, EndLine: end,
			Language: "gdscript",
			Meta:     meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: start,
		})
	}

	// The script's own global class name, if it declares one. Godot
	// allows at most one `class_name` per script.
	selfClass := ""
	for _, m := range gdClassNameRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		add(name, graph.KindType, line, line, map[string]any{"gd_kind": "class_name"})
		if selfClass == "" {
			selfClass = name
		}
	}

	var owners []gdOwner
	for _, m := range gdInnerClass.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		end := findIndentedBlockEnd(lines, line)
		add(name, graph.KindType, line, end, map[string]any{"gd_kind": "inner_class"})
		owners = append(owners, gdOwner{name: name, start: line, end: end})
	}
	ownerAt := func(line int) string {
		for _, o := range owners {
			if line >= o.start && line <= o.end {
				return o.name
			}
		}
		return selfClass
	}

	// Func declarations. A func owned by a named class is a method
	// carrying Meta["receiver"] — what the resolver's exact
	// receiver-type passes match a call's receiver_type against. A func
	// in a script with no `class_name` and no enclosing inner class has
	// no nameable owner, so it stays a plain function.
	declOffsets := make(map[int]bool)
	for _, m := range gdFuncRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		end := findIndentedBlockEnd(lines, line)
		declOffsets[m[2]] = true
		if owner := ownerAt(line); owner != "" {
			add(name, graph.KindMethod, line, end, map[string]any{"receiver": owner})
		} else {
			add(name, graph.KindFunction, line, end, nil)
		}
	}
	for _, m := range gdVarRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		add(name, graph.KindVariable, line, line, nil)
	}
	for _, m := range gdConstRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		add(name, graph.KindVariable, line, line, map[string]any{"const": true})
	}
	for _, m := range gdEnumRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		add(name, graph.KindType, line, line, map[string]any{"gd_kind": "enum"})
	}
	for _, m := range gdSignalRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		// `signal died(reason: String)` is a declaration, not a call to
		// `died` — record the offset so the call scan skips it.
		declOffsets[m[2]] = true
		add(name, graph.KindMethod, line, line, map[string]any{"gd_kind": "signal"})
	}

	for _, m := range gdExtendsRe.FindAllSubmatchIndex(src, -1) {
		parent := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::" + parent,
			Kind: graph.EdgeImports, FilePath: filePath, Line: line,
		})
	}
	for _, m := range gdPreloadRe.FindAllSubmatchIndex(src, -1) {
		path := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::import::" + path,
			Kind: graph.EdgeImports, FilePath: filePath, Line: line,
		})
	}

	funcRanges := buildFuncRanges(result)
	types := gdBuildTypeEnv(src, funcRanges)

	// Comments and string literals are masked before the call scan so a
	// `#` note or a `"res://..."` path cannot mint a call edge. Masking
	// preserves length, so every offset below still indexes src alike.
	masked := gdMaskLiterals(src)
	for _, m := range gdCallRe.FindAllSubmatchIndex(masked, -1) {
		name := string(masked[m[4]:m[5]])
		if isGDKeyword(name) || declOffsets[m[4]] {
			continue
		}
		line := lineAt(masked, m[0])
		callerID := findEnclosingFunc(funcRanges, line)
		if callerID == "" {
			continue
		}
		recv := ""
		if m[2] >= 0 {
			recv = string(masked[m[2]:m[3]])
			if i := strings.LastIndexByte(recv, '.'); i >= 0 {
				recv = recv[i+1:]
			}
		}

		member, recvType := false, ""
		switch recv {
		case "":
			// A bare call to a name this script itself declares is a
			// self-call; anything else (an engine builtin, a method
			// inherited from Node) keeps the free-function shape it has
			// always had rather than claiming a receiver type nothing
			// in the file supports.
			if owner := ownerAt(line); owner != "" && seen[filePath+"::"+name] {
				member, recvType = true, owner
			}
		case "self":
			member, recvType = true, ownerAt(line)
		default:
			member = true
			if t := types.lookup(callerID, recv); t != "" {
				recvType = t
			} else if gdLooksGlobalName(recv) {
				// GDScript style reserves PascalCase for `class_name`
				// globals, autoload singletons and engine singletons,
				// and snake_case for locals — so an unbound PascalCase
				// receiver names a type directly. When the repo defines
				// no such type (an engine singleton like `Input`), the
				// resolver's own in-repo-type gate declines the hint.
				recvType = recv
			}
		}

		// Self-recursion carries no information the definition does not
		// already have, and a self-loop would show up as a cycle. Only a
		// call that really is to the enclosing func qualifies: gated on
		// the receiver, an `other.foo()` inside `func foo()` is a call to
		// a different object that must not be dropped for sharing a name.
		if (recv == "" || recv == "self") && strings.HasSuffix(callerID, "::"+name) {
			continue
		}

		to := "unresolved::" + name
		var meta map[string]any
		if member {
			to = "unresolved::*." + name
			if recvType != "" {
				meta = map[string]any{"receiver_type": recvType}
			}
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: to,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			Meta: meta,
		})
	}

	return result, nil
}

// gdTypeEnv maps a receiver expression to its declared type, scoped to
// the function that declares it with a file-level fallback for the
// script's member vars.
type gdTypeEnv struct {
	file map[string]string
	byFn map[string]map[string]string
}

func (t gdTypeEnv) lookup(fnID, name string) string {
	if fn, ok := t.byFn[fnID]; ok {
		if v := fn[name]; v != "" {
			return v
		}
	}
	return t.file[name]
}

// gdBuildTypeEnv collects declared receiver types from typed vars
// (`var x: Techs`), constructor-inferred vars (`var x := Techs.new()`)
// and typed parameters (`func f(a: Techs)`). A declaration inside a
// function scopes to that function; a member var scopes to the file.
func gdBuildTypeEnv(src []byte, ranges []funcRange) gdTypeEnv {
	env := gdTypeEnv{file: map[string]string{}, byFn: map[string]map[string]string{}}
	bind := func(fnID, name, typ string) {
		if name == "" || typ == "" || isGDKeyword(typ) {
			return
		}
		if fnID == "" {
			env.file[name] = typ
			return
		}
		if env.byFn[fnID] == nil {
			env.byFn[fnID] = map[string]string{}
		}
		env.byFn[fnID][name] = typ
	}
	for _, re := range []*regexp.Regexp{gdTypedVarRe, gdNewVarRe} {
		for _, m := range re.FindAllSubmatchIndex(src, -1) {
			line := lineAt(src, m[0])
			bind(findEnclosingFunc(ranges, line), string(src[m[2]:m[3]]), string(src[m[4]:m[5]]))
		}
	}
	for _, m := range gdFuncSigRe.FindAllSubmatchIndex(src, -1) {
		line := lineAt(src, m[0])
		fnID := findEnclosingFunc(ranges, line)
		if fnID == "" {
			continue
		}
		for _, p := range gdParamRe.FindAllSubmatch(src[m[2]:m[3]], -1) {
			bind(fnID, string(p[1]), string(p[2]))
		}
	}
	return env
}

// gdMaskLiterals blanks GDScript comments and string literals, keeping
// the byte length (and therefore every offset and line number) intact
// so the caller can index the masked copy and the original alike.
func gdMaskLiterals(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	for i := 0; i < len(out); {
		switch quote := out[i]; quote {
		case '#':
			for ; i < len(out) && out[i] != '\n'; i++ {
				blank(i)
			}
		case '"', '\'':
			n := 1
			if i+2 < len(out) && out[i+1] == quote && out[i+2] == quote {
				n = 3
			}
			for j := 0; j < n; j++ {
				blank(i + j)
			}
			i += n
			for i < len(out) {
				if out[i] == '\\' && n == 1 {
					blank(i)
					if i+1 < len(out) {
						blank(i + 1)
					}
					i += 2
					continue
				}
				if out[i] == quote &&
					(n == 1 || (i+2 < len(out) && out[i+1] == quote && out[i+2] == quote)) {
					for j := 0; j < n && i+j < len(out); j++ {
						blank(i + j)
					}
					i += n
					break
				}
				if out[i] == '\n' && n == 1 {
					break // an unterminated single-line string ends at EOL
				}
				blank(i)
				i++
			}
		default:
			i++
		}
	}
	return out
}

// gdLooksGlobalName reports whether an identifier follows the Godot
// convention for a global class / singleton name: PascalCase, i.e. a
// leading uppercase letter and at least one lowercase character (so a
// SCREAMING_CASE constant is not mistaken for a type).
func gdLooksGlobalName(s string) bool {
	if s == "" || s[0] < 'A' || s[0] > 'Z' || isGDKeyword(s) {
		return false
	}
	return strings.ToUpper(s) != s
}

func isGDKeyword(s string) bool {
	switch s {
	case "func", "var", "const", "class", "class_name", "extends", "enum",
		"signal", "static", "if", "elif", "else", "for", "while", "break",
		"continue", "return", "match", "pass", "in", "and", "or", "not",
		"is", "as", "self", "true", "false", "null", "void", "int", "float",
		"bool", "String", "Array", "Dictionary", "Vector2", "Vector3",
		"preload", "load":
		return true
	}
	return false
}

var _ parser.Extractor = (*GDScriptExtractor)(nil)
