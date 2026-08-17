package languages

import (
	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// CyclomaticComplexity returns the McCabe cyclomatic complexity of a
// function body — 1 plus the number of decision points (branches that
// can take more than one path). The body is walked once recursively;
// nested function/class definitions are skipped because their
// complexity belongs to their own nodes, not to the enclosing scope.
//
// The decision-point set is the cross-language overlap of common
// branch nodes, plus per-language extensions. Each language passes
// its own table of node-type names; tree-sitter grammars vary
// (`if_statement` vs `if_expression`, etc.) so we don't hardcode a
// single set here.
//
// Returns 1 for an empty / nil body — the canonical "no branches"
// score.
func CyclomaticComplexity(body *sitter.Node, decisionTypes map[string]bool, skipDescent map[string]bool) int {
	score := 1
	if body == nil || len(decisionTypes) == 0 {
		return score
	}
	walkComplexity(body, decisionTypes, skipDescent, &score)
	return score
}

func walkComplexity(n *sitter.Node, decisionTypes, skipDescent map[string]bool, score *int) {
	if n == nil {
		return
	}
	t := n.Type()
	if decisionTypes[t] {
		*score++
	}
	if skipDescent != nil && skipDescent[t] {
		return
	}
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		walkComplexity(n.NamedChild(i), decisionTypes, skipDescent, score)
	}
}

// Cross-language decision-point tables. Each value is a map for O(1)
// lookup. Tree-sitter grammars vary on AST node names — Go uses
// `if_statement`, Rust uses `if_expression`, Python uses
// `if_statement`, etc. Each language's complexity counter passes the
// table that matches its grammar.
//
// Boolean operator nodes (`&&`/`||`/`and`/`or`) are intentionally NOT
// in these tables today. Counting them double-counts conditions and
// makes scores noisy on guards like `if a && b && c`. If a project
// wants strict McCabe parity later, add `binary_expression` plus a
// post-filter that checks the operator text.

var goComplexityNodes = map[string]bool{
	"if_statement":                true,
	"for_statement":               true,
	"expression_switch_statement": true,
	"type_switch_statement":       true,
	"select_statement":            true,
	"case_clause":                 true,
	"communication_case":          true,
	"type_case":                   true,
}

var goComplexitySkip = map[string]bool{
	"func_literal":         true, // closures
	"function_declaration": true, // nested defs (rare in Go)
	"method_declaration":   true,
}

var tsComplexityNodes = map[string]bool{
	"if_statement":           true,
	"for_statement":          true,
	"for_in_statement":       true,
	"for_of_statement":       true,
	"while_statement":        true,
	"do_statement":           true,
	"switch_case":            true,
	"switch_default":         true,
	"catch_clause":           true,
	"ternary_expression":     true,
	"conditional_expression": true,
}

var tsComplexitySkip = map[string]bool{
	"function_declaration": true,
	"function_expression":  true,
	"arrow_function":       true,
	"method_definition":    true,
	"class_declaration":    true,
}

var pyComplexityNodes = map[string]bool{
	"if_statement":             true,
	"elif_clause":              true,
	"for_statement":            true,
	"while_statement":          true,
	"except_clause":            true,
	"match_statement":          true,
	"case_clause":              true,
	"conditional_expression":   true,
	"list_comprehension":       true,
	"dictionary_comprehension": true,
	"set_comprehension":        true,
	"generator_expression":     true,
}

var pyComplexitySkip = map[string]bool{
	"function_definition":  true,
	"class_definition":     true,
	"lambda":               true,
	"decorated_definition": true,
}

var rustComplexityNodes = map[string]bool{
	"if_expression":     true,
	"if_let_expression": true,
	"for_expression":    true,
	"while_expression":  true,
	"loop_expression":   true,
	"match_arm":         true,
	"match_expression":  true,
}

var rustComplexitySkip = map[string]bool{
	"function_item":      true,
	"closure_expression": true,
}

var javaComplexityNodes = map[string]bool{
	"if_statement":                 true,
	"for_statement":                true,
	"enhanced_for_statement":       true,
	"while_statement":              true,
	"do_statement":                 true,
	"switch_label":                 true,
	"switch_block_statement_group": true,
	"catch_clause":                 true,
	"ternary_expression":           true,
}

var javaComplexitySkip = map[string]bool{
	"method_declaration":      true,
	"constructor_declaration": true,
	"lambda_expression":       true,
	"class_declaration":       true,
}

// --- Cognitive complexity & loop depth (NEW-CBM-1) ------------------
//
// Cyclomatic complexity counts decision points flatly; cognitive
// complexity additionally penalises *nesting*, so deeply-nested control
// flow (the kind that is genuinely hard to follow and often hides
// quadratic behaviour) scores higher than the same number of flat
// branches. Loop depth is the maximum syntactic nesting of loops in a
// body — the per-function input the interprocedural bottleneck analyzer
// propagates along the call graph to surface hidden-O(n^2) chains.

// nestingTypes — control-flow nodes that increase the cognitive nesting
// level. A subset of the decision tables: the statement-level constructs
// (loops, if, switch, try/catch, match) but not their clause sub-nodes.
var goNestingTypes = map[string]bool{
	"if_statement": true, "for_statement": true,
	"expression_switch_statement": true, "type_switch_statement": true,
	"select_statement": true,
}
var tsNestingTypes = map[string]bool{
	"if_statement": true, "for_statement": true, "for_in_statement": true,
	"for_of_statement": true, "while_statement": true, "do_statement": true,
	"switch_statement": true, "catch_clause": true,
}
var pyNestingTypes = map[string]bool{
	"if_statement": true, "for_statement": true, "while_statement": true,
	"try_statement": true, "match_statement": true,
}
var rustNestingTypes = map[string]bool{
	"if_expression": true, "if_let_expression": true, "for_expression": true,
	"while_expression": true, "loop_expression": true, "match_expression": true,
}
var javaNestingTypes = map[string]bool{
	"if_statement": true, "for_statement": true, "enhanced_for_statement": true,
	"while_statement": true, "do_statement": true, "switch_statement": true,
	"switch_expression": true, "catch_clause": true, "try_statement": true,
}

// loopTypes — nodes that are loops, for max-loop-depth measurement.
var goLoopTypes = map[string]bool{"for_statement": true}
var tsLoopTypes = map[string]bool{
	"for_statement": true, "for_in_statement": true, "for_of_statement": true,
	"while_statement": true, "do_statement": true,
}
var pyLoopTypes = map[string]bool{
	"for_statement": true, "while_statement": true,
	"list_comprehension": true, "dictionary_comprehension": true,
	"set_comprehension": true, "generator_expression": true,
}
var rustLoopTypes = map[string]bool{
	"for_expression": true, "while_expression": true, "loop_expression": true,
}
var javaLoopTypes = map[string]bool{
	"for_statement": true, "enhanced_for_statement": true,
	"while_statement": true, "do_statement": true,
}

// CognitiveComplexity returns a nesting-weighted complexity score: every
// decision point costs 1 plus the control-nesting level it sits at.
// nestingTypes is the set of nodes that raise the nesting level for
// their descendants. Returns 0 for an empty body (no cognitive load).
func CognitiveComplexity(body *sitter.Node, decisionTypes, nestingTypes, skipDescent map[string]bool) int {
	score := 0
	if body == nil || len(decisionTypes) == 0 {
		return score
	}
	var walk func(n *sitter.Node, nesting int)
	walk = func(n *sitter.Node, nesting int) {
		if n == nil {
			return
		}
		t := n.Type()
		if decisionTypes[t] {
			score += 1 + nesting
		}
		if skipDescent != nil && skipDescent[t] {
			return
		}
		childNesting := nesting
		if nestingTypes[t] {
			childNesting++
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			walk(n.NamedChild(i), childNesting)
		}
	}
	walk(body, 0)
	return score
}

// MaxLoopDepth returns the deepest syntactic nesting of loops in a body.
// A function with a loop inside a loop returns 2; one with no loops, 0.
func MaxLoopDepth(body *sitter.Node, loopTypes, skipDescent map[string]bool) int {
	maxDepth := 0
	if body == nil || len(loopTypes) == 0 {
		return 0
	}
	var walk func(n *sitter.Node, depth int)
	walk = func(n *sitter.Node, depth int) {
		if n == nil {
			return
		}
		t := n.Type()
		d := depth
		if loopTypes[t] {
			d++
			if d > maxDepth {
				maxDepth = d
			}
		}
		if skipDescent != nil && skipDescent[t] {
			return
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			walk(n.NamedChild(i), d)
		}
	}
	walk(body, 0)
	return maxDepth
}

// PHP. `else_if_clause` is a decision point but a bare `else_clause` is not;
// a `match` arm (match_conditional_expression) counts like a switch case.
// PHP has no separate ternary node — `conditional_expression` covers both
// `?:` and the short form.
var phpComplexityNodes = map[string]bool{
	"if_statement":                 true,
	"else_if_clause":               true,
	"for_statement":                true,
	"foreach_statement":            true,
	"while_statement":              true,
	"do_statement":                 true,
	"case_statement":               true,
	"match_conditional_expression": true,
	"catch_clause":                 true,
	"conditional_expression":       true,
}

var phpNestingTypes = map[string]bool{
	"if_statement": true, "for_statement": true, "foreach_statement": true,
	"while_statement": true, "do_statement": true, "switch_statement": true,
	"match_expression": true, "catch_clause": true, "try_statement": true,
}

var phpLoopTypes = map[string]bool{
	"for_statement": true, "foreach_statement": true,
	"while_statement": true, "do_statement": true,
}

// A nested declaration owns its own metrics, so the walk stops there rather
// than folding a closure's branches into its enclosing function's score.
var phpComplexitySkip = map[string]bool{
	"function_definition":                    true,
	"method_declaration":                     true,
	"anonymous_function":                     true,
	"anonymous_function_creation_expression": true,
	"arrow_function":                         true,
	"class_declaration":                      true,
	"anonymous_class":                        true,
}

// complexityTables bundles the per-language node-type tables so a single
// stamping helper can serve every extractor.
type complexityTables struct {
	decision map[string]bool
	nesting  map[string]bool
	loop     map[string]bool
	skip     map[string]bool
}

var langComplexityTables = map[string]complexityTables{
	"go":         {goComplexityNodes, goNestingTypes, goLoopTypes, goComplexitySkip},
	"typescript": {tsComplexityNodes, tsNestingTypes, tsLoopTypes, tsComplexitySkip},
	"tsx":        {tsComplexityNodes, tsNestingTypes, tsLoopTypes, tsComplexitySkip},
	"javascript": {tsComplexityNodes, tsNestingTypes, tsLoopTypes, tsComplexitySkip},
	"jsx":        {tsComplexityNodes, tsNestingTypes, tsLoopTypes, tsComplexitySkip},
	"python":     {pyComplexityNodes, pyNestingTypes, pyLoopTypes, pyComplexitySkip},
	"rust":       {rustComplexityNodes, rustNestingTypes, rustLoopTypes, rustComplexitySkip},
	"java":       {javaComplexityNodes, javaNestingTypes, javaLoopTypes, javaComplexitySkip},
	"php":        {phpComplexityNodes, phpNestingTypes, phpLoopTypes, phpComplexitySkip},
}

// StampFunctionMetrics computes cyclomatic + cognitive complexity and max
// loop depth for a function/method body and stamps them on the node's
// Meta — complexity / cognitive only when > 1, loop_depth only when > 0,
// matching the existing cyclomatic convention so consumers (analyze
// kind=impact / bottlenecks) read a single shape. A no-op for languages
// without a complexity table or a nil body.
func StampFunctionMetrics(node *graph.Node, body *sitter.Node, lang string) {
	if node == nil || body == nil {
		return
	}
	tbl, ok := langComplexityTables[lang]
	if !ok {
		return
	}
	cyc := CyclomaticComplexity(body, tbl.decision, tbl.skip)
	cog := CognitiveComplexity(body, tbl.decision, tbl.nesting, tbl.skip)
	loop := MaxLoopDepth(body, tbl.loop, tbl.skip)
	if cyc <= 1 && cog <= 1 && loop == 0 {
		return
	}
	if node.Meta == nil {
		node.Meta = map[string]any{}
	}
	ApplyComplexityMeta(node.Meta, cyc, cog, loop)
}

// BodyComplexityMetrics returns cyclomatic, cognitive, and max-loop-depth
// for a body — for extractors (Python, Rust) that compute metrics into
// locals before the node Meta exists. Returns zeros for an unknown
// language or nil body.
func BodyComplexityMetrics(body *sitter.Node, lang string) (cyc, cognitive, loopDepth int) {
	tbl, ok := langComplexityTables[lang]
	if !ok || body == nil {
		return 0, 0, 0
	}
	return CyclomaticComplexity(body, tbl.decision, tbl.skip),
		CognitiveComplexity(body, tbl.decision, tbl.nesting, tbl.skip),
		MaxLoopDepth(body, tbl.loop, tbl.skip)
}

// ApplyComplexityMeta stamps the three metric keys onto a Meta map with
// the canonical thresholds (complexity / cognitive only when > 1,
// loop_depth only when > 0).
func ApplyComplexityMeta(meta map[string]any, cyc, cognitive, loopDepth int) {
	if meta == nil {
		return
	}
	if cyc > 1 {
		meta["complexity"] = cyc
	}
	if cognitive > 1 {
		meta["cognitive"] = cognitive
	}
	if loopDepth > 0 {
		meta["loop_depth"] = loopDepth
	}
}

// --- Loop-region bottleneck signals ---------------------------------
//
// Four additional per-function signals that only mean something with
// loop-region membership. "Inside a loop" is decided structurally: a
// node is in a loop iff some AST ancestor on its descent path is a loop
// node (the loopTypes table) — never by line range. So a call that
// merely shares a line span with a loop but sits outside its body is not
// flagged, and a call nested under a loop through an intermediate block
// is.
//
// This walks the *sitter.Node body the extractor already holds rather
// than rebuilding a control-flow graph: a control-flow graph would have
// to re-parse the function's source text (that package is query-time-
// only by design), whereas the loop-ancestor walk over the node in hand
// is both cheaper — no re-parse — and directly precise for the membership
// question these signals ask.

// linearScanCallNames are call names whose body performs a linear scan
// over a collection. One of these inside a loop is the classic
// accidental-quadratic membership test (e.g. a Contains / Index call run
// once per outer iteration).
var linearScanCallNames = map[string]bool{
	"Contains": true, "ContainsAny": true, "ContainsRune": true, "ContainsFunc": true,
	"Index": true, "IndexAny": true, "IndexByte": true, "IndexRune": true, "IndexFunc": true,
	"LastIndex": true, "LastIndexByte": true,
}

// loopSignalSpec carries the per-language AST node names the loop-signal
// walk needs. callType / memberType are node types; the *Field entries
// are tree-sitter field names.
type loopSignalSpec struct {
	loop            map[string]bool // loop node types (also the nesting source)
	skip            map[string]bool // do not descend (nested function bodies)
	callType        string          // call-expression node type
	calleeField     string          // field on a call holding the callee
	memberType      string          // member-access (selector / attribute) node type
	memberObjField  string          // field on a member-access holding the object operand
	memberNameField string          // field on a member-access holding the trailing name
	compositeTypes  map[string]bool // allocation literal node types
	allocCallNames  map[string]bool // builtin allocation call names
}

// loopSignalTables is keyed by language. Only languages wired to call
// StampLoopSignals need an entry; an unknown language is a no-op.
var loopSignalTables = map[string]loopSignalSpec{
	"go": {
		loop:            goLoopTypes,
		skip:            goComplexitySkip,
		callType:        "call_expression",
		calleeField:     "function",
		memberType:      "selector_expression",
		memberObjField:  "operand",
		memberNameField: "field",
		compositeTypes:  map[string]bool{"composite_literal": true},
		allocCallNames:  map[string]bool{"make": true, "new": true, "append": true},
	},
}

// StampLoopSignals computes four loop-region-aware bottleneck signals for
// a function/method body and stamps them on the node's Meta:
//
//   - max_access_depth (int): the number of identifier segments in the
//     deepest member-access chain — a.b.c.d.e is 5 (four selector hops
//     plus the base operand). High depth flags pointer-chasing / a
//     Law-of-Demeter coupling smell. Stamped only when >= 3 so the common
//     shallow case stays out of Meta.
//   - linear_scan_in_loop (bool): a linear-scan call occurs inside a loop
//     region — an accidental-quadratic membership test.
//   - alloc_in_loop (bool): an allocation (make / new / append / a
//     composite literal) occurs inside a loop region — per-iteration churn
//     / GC pressure.
//   - recursion_in_loop (bool): the function calls itself inside a loop
//     region — compounding blow-up.
//
// Loop membership is structural (a loop AST ancestor), never a line-range
// guess. funcName is the enclosing function's bare name, used to spot
// direct self-recursion. A no-op for a language without a loop-signal
// table, a nil body, or nil source.
func StampLoopSignals(node *graph.Node, body *sitter.Node, src []byte, lang string) {
	if node == nil || body == nil || src == nil {
		return
	}
	spec, ok := loopSignalTables[lang]
	if !ok {
		return
	}
	depth, linear, alloc, recur := computeLoopSignals(body, src, spec, node.Name)
	if depth < 3 && !linear && !alloc && !recur {
		return
	}
	if node.Meta == nil {
		node.Meta = map[string]any{}
	}
	if depth >= 3 {
		node.Meta["max_access_depth"] = depth
	}
	if linear {
		node.Meta["linear_scan_in_loop"] = true
	}
	if alloc {
		node.Meta["alloc_in_loop"] = true
	}
	if recur {
		node.Meta["recursion_in_loop"] = true
	}
}

// computeLoopSignals walks body once, tracking loop-ancestor membership,
// and returns the four signals. Nested function bodies (closures) are not
// descended into — their signals belong to their own nodes, matching the
// cognitive-complexity / loop-depth convention.
func computeLoopSignals(body *sitter.Node, src []byte, spec loopSignalSpec, funcName string) (maxAccessDepth int, linearScanInLoop, allocInLoop, recursionInLoop bool) {
	var walk func(n *sitter.Node, inLoop bool)
	walk = func(n *sitter.Node, inLoop bool) {
		if n == nil {
			return
		}
		t := n.Type()

		if spec.memberType != "" && t == spec.memberType {
			if d := memberChainSegments(n, spec); d > maxAccessDepth {
				maxAccessDepth = d
			}
		}

		if inLoop {
			if spec.callType != "" && t == spec.callType {
				if name := calleeFinalName(n, src, spec); name != "" {
					if linearScanCallNames[name] {
						linearScanInLoop = true
					}
					if spec.allocCallNames[name] {
						allocInLoop = true
					}
					if name == funcName {
						recursionInLoop = true
					}
				}
			}
			if spec.compositeTypes[t] {
				allocInLoop = true
			}
		}

		if spec.skip != nil && spec.skip[t] {
			return
		}
		childInLoop := inLoop || spec.loop[t]
		for c := range n.NamedChildren() {
			walk(c, childInLoop)
		}
	}
	walk(body, false)
	return
}

// memberChainSegments returns the number of identifier segments in the
// member-access chain whose outermost node is n: the run of contiguous
// member-access nodes along the object spine, plus the base operand.
// a.b.c.d.e -> 5. Measuring at every member-access node is safe — the
// outermost yields the largest count, so the running max is correct.
func memberChainSegments(n *sitter.Node, spec loopSignalSpec) int {
	hops := 0
	for cur := n; cur != nil && cur.Type() == spec.memberType; cur = cur.ChildByFieldName(spec.memberObjField) {
		hops++
	}
	if hops == 0 {
		return 0
	}
	return hops + 1
}

// calleeFinalName returns the bare final name of a call's callee: the
// identifier for a direct call, or the trailing selector field for a
// qualified / method call. Empty for a more complex callee expression.
func calleeFinalName(call *sitter.Node, src []byte, spec loopSignalSpec) string {
	fn := call.ChildByFieldName(spec.calleeField)
	if fn == nil {
		return ""
	}
	switch {
	case spec.memberType != "" && fn.Type() == spec.memberType:
		if field := fn.ChildByFieldName(spec.memberNameField); field != nil {
			return field.Content(src)
		}
		return ""
	case fn.Type() == "identifier":
		return fn.Content(src)
	}
	return ""
}
