package contracts

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Odoo controller routes.
//
// Odoo declares HTTP endpoints with an @http.route decorator on a method
// of a Controller subclass. Three things make it unlike the Flask shape
// the generic decorator pass handles:
//
//  1. One decorator may declare a LIST of paths — @http.route(['/shop',
//     '/shop/page/<int:page>']) — which is one method serving several
//     routes, so a per-line regex that grabs the first string literal
//     silently loses the rest.
//  2. The decorator is routinely wrapped across several source lines, so
//     the argument text has to be accumulated by paren balance rather
//     than read off one line.
//  3. Odoo's path converters include a record form,
//     <model("res.partner"):partner>, whose quotes and parens defeat the
//     shared paramPatterns regex. Left alone it survives normalisation
//     verbatim and the contract ID becomes unmatchable garbage — so it is
//     rewritten to a plain <partner> slot first, and the model name it
//     named is kept in Meta as a free route → model link.
//
// Odoo's own default when `methods=` is absent is "any method", which in
// practice means GET and POST — NOT Flask's GET-only. Copying the Flask
// default here would silently drop every form POST in an Odoo codebase.

// odooRouteDecoratorRE matches the opening of an Odoo route decorator:
// the qualified `@http.route(` / `@odoo.http.route(`, and the bare
// `@route(` that `from odoo.http import route` allows.
//
// The `http.` segment is REQUIRED on the qualified form. A looser
// `@\w+\.route\(` also matches Flask's `@app.route(` / `@bp.route(`,
// which would make this pass mint a second, wrong set of contracts for
// every Flask app in the corpus — with Odoo's GET+POST default rather
// than Flask's GET-only.
var odooRouteDecoratorRE = regexp.MustCompile(`@(?:[\w.]+\.)?http\.route\(|@route\(`)

// odooModelConverterRE matches Odoo's record converters —
// <model("res.partner"):partner> and the plural <models("...")::...> — so
// they can be reduced to the bare slot name the shared normaliser
// understands.
var odooModelConverterRE = regexp.MustCompile(`<models?\(\s*["']([\w.]+)["']\s*\)\s*:\s*(\w+)\s*>`)

// odooKwargRE reads a simple `name='value'` keyword argument.
var odooKwargRE = regexp.MustCompile(`\b(type|auth)\s*=\s*["'](\w+)["']`)

// odooBoolKwargRE reads a `name=True|False` keyword argument.
var odooBoolKwargRE = regexp.MustCompile(`\b(website|csrf|sitemap)\s*=\s*(True|False)`)

// odooDefaultMethods is Odoo's effective default when the decorator names
// no methods: the endpoint answers both a link and a form submission.
var odooDefaultMethods = []string{"GET", "POST"}

// extractOdooRoutes emits one provider contract per (path, method) pair
// declared by an @http.route decorator.
func (h *HTTPExtractor) extractOdooRoutes(filePath, text string, lines []string, fileNodes []*graph.Node, lang string, tree *parser.ParseTree) []Contract {
	var out []Contract
	for i, line := range lines {
		loc := odooRouteDecoratorRE.FindStringIndex(line)
		if loc == nil {
			continue
		}
		args, endLine := odooDecoratorArgs(lines, i, loc[1])
		if args == "" {
			continue
		}
		paths := odooRoutePaths(args)
		if len(paths) == 0 {
			continue
		}
		methods := flaskMethodsKwarg(args)
		if len(methods) == 0 {
			methods = odooDefaultMethods
		}
		handlerID := odooHandlerBelow(lines, endLine, fileNodes)

		for _, p := range paths {
			for _, method := range methods {
				out = append(out, h.buildOdooContract(
					filePath, method, p, handlerID, args, i+1, lines, fileNodes, lang, tree))
			}
		}
	}
	return out
}

// odooDecoratorArgs accumulates the decorator's argument text from the
// opening paren until the parens balance, and reports the last line it
// consumed. Odoo decorators wrap freely, so reading one line would
// truncate the path list and the methods= kwarg alike.
func odooDecoratorArgs(lines []string, start, openAt int) (args string, endLine int) {
	const maxDecoratorLines = 40
	var b strings.Builder
	depth := 0
	for i := start; i < len(lines) && i-start < maxDecoratorLines; i++ {
		segment := lines[i]
		if i == start {
			segment = segment[openAt:]
			depth = 1
		}
		for _, r := range segment {
			switch r {
			case '(', '[':
				depth++
			case ')', ']':
				depth--
			}
			if depth == 0 {
				return b.String(), i
			}
			b.WriteRune(r)
		}
		b.WriteByte('\n')
	}
	return "", start
}

// odooRoutePaths reads the decorator's first positional argument, which
// is either one path string or a list of them. Only leading-slash
// literals are taken, so a keyword value like type='http' can never be
// mistaken for a route.
func odooRoutePaths(args string) []string {
	head := args
	// Stop at the first keyword argument so a later quoted value cannot
	// be read as a path.
	if idx := odooFirstKwargIndex(head); idx >= 0 {
		head = head[:idx]
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range odooStringLiterals(head) {
		p = strings.TrimSpace(p)
		if !strings.HasPrefix(p, "/") || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// odooStringLiterals scans out Python string literals, closing each on
// its own opening quote character.
//
// The shared quotedTokenRE cannot be used here: an Odoo path routinely
// nests the other quote style inside itself —
// '/partner/<model("res.partner"):partner>/edit' — and a regex that ends
// a literal at whichever quote comes first truncates that path to
// "/partner/<model(".
func odooStringLiterals(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		q := s[i]
		if q != '\'' && q != '"' {
			continue
		}
		j := i + 1
		for j < len(s) && s[j] != q {
			if s[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(s) {
			break
		}
		out = append(out, s[i+1:j])
		i = j
	}
	return out
}

// odooFirstKwargIndex finds where the keyword arguments begin — the first
// `name=` that is not an `==`, `!=`, `<=` or `>=` comparison.
func odooFirstKwargIndex(s string) int {
	for i := 1; i < len(s)-1; i++ {
		if s[i] != '=' || s[i+1] == '=' {
			continue
		}
		switch s[i-1] {
		case '=', '!', '<', '>':
			continue
		}
		j := i - 1
		for j >= 0 && (isOdooIdentByte(s[j]) || s[j] == ' ') {
			j--
		}
		if j+1 < i {
			return j + 1
		}
	}
	return -1
}

func isOdooIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// odooHandlerBelow resolves the controller method the decorator stack
// wraps — the next `def` below it, skipping further decorators.
func odooHandlerBelow(lines []string, from int, fileNodes []*graph.Node) string {
	for j := from + 1; j < len(lines); j++ {
		trimmed := strings.TrimSpace(lines[j])
		if trimmed == "" || strings.HasPrefix(trimmed, "@") || strings.HasPrefix(trimmed, ")") {
			continue
		}
		if dm := flaskDefRE.FindStringSubmatch(lines[j]); dm != nil {
			return findFunctionByName(fileNodes, dm[1])
		}
		return ""
	}
	return ""
}

// buildOdooContract assembles one provider contract, rewriting Odoo's
// record converters before the shared path normaliser sees them.
func (h *HTTPExtractor) buildOdooContract(filePath, method, path, symbolID, args string, lineNum int, lines []string, fileNodes []*graph.Node, lang string, tree *parser.ParseTree) Contract {
	rewritten, pathModels := rewriteOdooModelConverters(path)
	normPath, origNames := NormalizeHTTPPathWithParams(rewritten)

	meta := map[string]any{
		"method":    method,
		"path":      normPath,
		"framework": "odoo",
	}
	if len(origNames) > 0 {
		meta["path_param_names"] = origNames
	}
	// A <model("res.partner"):partner> slot names the model the endpoint
	// operates on. Recording it gives a route → model link with no XML
	// and no resolution pass involved.
	if len(pathModels) > 0 {
		meta["odoo_path_models"] = pathModels
	}
	for _, kv := range odooKwargRE.FindAllStringSubmatch(args, -1) {
		meta["odoo_"+kv[1]] = kv[2]
	}
	for _, kv := range odooBoolKwargRE.FindAllStringSubmatch(args, -1) {
		meta["odoo_"+kv[1]] = kv[2] == "True"
	}

	c := Contract{
		ID:         fmt.Sprintf("http::%s::%s", method, normPath),
		Type:       ContractHTTP,
		Role:       RoleProvider,
		SymbolID:   symbolID,
		FilePath:   filePath,
		Line:       lineNum,
		Meta:       meta,
		Confidence: 0.9,
	}
	EnrichHTTPContractWithTree(&c, lines, fileNodes, lang, tree)
	return c
}

// rewriteOdooModelConverters reduces <model("res.partner"):partner> to
// <partner> and returns the slot → model mapping it carried.
func rewriteOdooModelConverters(path string) (string, map[string]string) {
	matches := odooModelConverterRE.FindAllStringSubmatch(path, -1)
	if len(matches) == 0 {
		return path, nil
	}
	models := make(map[string]string, len(matches))
	for _, m := range matches {
		models[m[2]] = m[1]
	}
	return odooModelConverterRE.ReplaceAllString(path, "<$2>"), models
}
