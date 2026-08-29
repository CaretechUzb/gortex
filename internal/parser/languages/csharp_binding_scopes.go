package languages

import (
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// csharpCollectExtraBindingScopes adds the binding forms the lvar
// capture cannot see to the local-scope index: foreach variables,
// declaration patterns (`o is T x`, `o is var x`), out-var declaration
// expressions, lambda parameters, and the parenthesized
// `using (var x = ...)` resource. (`using var x = ...;` IS a
// local_declaration_statement and needs nothing here.)
//
// The index answers "does a local bind this name at this site" for the
// receiver_name shadow refusal, and function-wide for the
// field-identifier emitter. A name bound by any of these forms shadows a
// same-named field exactly like a declared local does, and these forms
// are idiomatic modern C# - the miss fires precisely when such a name
// coincides with an injected-repository-style field (`repo`, `store`,
// `handler`), which is the shape the dispatch gate exists for.
//
// Extents: a declaration pattern and an out-var escape to the enclosing
// block (C# definite-assignment scoping) - the same extent an ordinary
// local gets. A foreach variable, a lambda parameter, and a using
// resource bind only over their own statement or lambda, so they carry
// that node's span rather than the enclosing block's; a call after the
// statement is back on the field and keeps its evidence.
func csharpCollectExtraBindingScopes(root *sitter.Node, src []byte, funcRanges *csharpFuncLookup, scopes csharpLocalScopes) {
	add := func(nameNode *sitter.Node, sc csharpLocalScope) {
		if nameNode == nil {
			return
		}
		name := nameNode.Content(src)
		if name == "" {
			return
		}
		owner := funcRanges.enclosing(int(nameNode.StartPoint().Row) + 1)
		if owner == "" {
			return
		}
		m := scopes[owner]
		if m == nil {
			m = map[string][]csharpLocalScope{}
			scopes[owner] = m
		}
		m[name] = append(m[name], sc)
	}
	spanOf := func(n *sitter.Node) csharpLocalScope {
		return csharpLocalScope{start: int(n.StartByte()), end: int(n.EndByte())}
	}
	addParams := func(params *sitter.Node, sc csharpLocalScope) {
		if params == nil {
			return
		}
		switch params.Type() {
		case "implicit_parameter", "identifier":
			// `x => ...` - the parameters node IS the name.
			add(params, sc)
		case "parameter_list":
			for i, _nc := 0, int(params.NamedChildCount()); i < _nc; i++ {
				if p := params.NamedChild(i); p != nil && p.Type() == "parameter" {
					add(p.ChildByFieldName("name"), sc)
				}
			}
		}
	}
	// A query's range variables bind across the query's later clauses;
	// their extent is the whole query expression.
	queryScope := func(n *sitter.Node) csharpLocalScope {
		for cur := n; cur != nil; cur = cur.Parent() {
			if cur.Type() == "query_expression" {
				return spanOf(cur)
			}
		}
		return csharpLocalScopeOf(n)
	}
	walkNodes(root, func(n *sitter.Node) {
		switch n.Type() {
		case "foreach_statement":
			// left is the loop variable - an identifier, or a tuple
			// pattern whose every identifier binds. All of them scope
			// over the statement.
			if left := n.ChildByFieldName("left"); left != nil {
				if left.Type() == "identifier" {
					add(left, spanOf(n))
				} else {
					walkNodes(left, func(c *sitter.Node) {
						if c.Type() == "identifier" {
							add(c, spanOf(n))
						}
					})
				}
			}
		case "declaration_pattern", "var_pattern", "declaration_expression",
			"recursive_pattern", "list_pattern":
			// All of these escape to the enclosing scope (definite-
			// assignment scoping), like an ordinary local.
			sc := csharpLocalScopeOf(n)
			add(n.ChildByFieldName("name"), sc)
			// A parenthesized designation (`var (a, b)`) hangs off the
			// pattern as an unfielded child; every identifier inside it
			// binds. This is also how a deconstruction declaration
			// (`var (a, b) = t;`) spells its names.
			walkNodes(n, func(c *sitter.Node) {
				if c.Type() != "parenthesized_variable_designation" {
					return
				}
				walkNodes(c, func(d *sitter.Node) {
					if d.Type() == "identifier" {
						add(d, sc)
					}
				})
			})
		case "lambda_expression":
			addParams(n.ChildByFieldName("parameters"), spanOf(n))
		case "anonymous_method_expression":
			// The C# 1 spelling of a lambda: `delegate(T x) { ... }`.
			addParams(n.ChildByFieldName("parameters"), spanOf(n))
		case "local_function_statement":
			// A local function mints no function node, so its
			// parameters are invisible to paramsByOwner - this index is
			// the only place they can refuse anything.
			addParams(n.ChildByFieldName("parameters"), spanOf(n))
		case "catch_declaration":
			// The catch variable binds over its catch clause.
			sc := spanOf(n)
			if p := n.Parent(); p != nil && p.Type() == "catch_clause" {
				sc = spanOf(p)
			}
			add(n.ChildByFieldName("name"), sc)
		case "for_statement":
			// `for (var x = ...; ...)` - the initializer is a bare
			// variable_declaration, not a local_declaration_statement,
			// so the lvar capture cannot see it. Scopes over the
			// statement.
			if init := n.ChildByFieldName("initializer"); init != nil && init.Type() == "variable_declaration" {
				sc := spanOf(n)
				walkNodes(init, func(d *sitter.Node) {
					if d.Type() == "variable_declarator" {
						if name := d.ChildByFieldName("name"); name != nil && name.Type() == "identifier" {
							add(name, sc)
						}
					}
				})
			}
		case "from_clause":
			add(n.ChildByFieldName("name"), queryScope(n))
		case "let_clause", "join_clause":
			// The introduced name is an unfielded child: the FIRST
			// identifier (`let x = expr` / `join x in ...`). Later
			// identifier children belong to the expression side and
			// must not be collected.
			for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
				if c := n.NamedChild(i); c != nil && c.Type() == "identifier" {
					add(c, queryScope(n))
					break
				}
			}
		case "tuple_pattern":
			// A deconstruction declaration (`var (a, b) = t;`) parses
			// as a variable_declarator carrying a tuple_pattern - the
			// names live inside the pattern, invisible to the lvar
			// capture's direct-identifier match. They escape to the
			// enclosing scope like any local.
			if p := n.Parent(); p != nil && p.Type() == "variable_declarator" {
				sc := csharpLocalScopeOf(n)
				walkNodes(n, func(d *sitter.Node) {
					if d.Type() == "identifier" {
						add(d, sc)
					}
				})
			}
		case "invocation_expression":
			// The grammar misparses `o is var (a, b)` as an invocation
			// of the is_expression - `(o is var)(a, b)` - so the
			// designation's names land in the ARGUMENT list. Recognize
			// exactly that shape (an is_expression whose pattern is the
			// bare implicit type) and index the argument identifiers;
			// they escape to the enclosing scope like a declaration
			// pattern's name.
			fn := n.ChildByFieldName("function")
			if fn == nil || fn.Type() != "is_expression" {
				return
			}
			last := fn.NamedChild(int(fn.NamedChildCount()) - 1)
			if last == nil || last.Type() != "implicit_type" {
				return
			}
			if args := n.ChildByFieldName("arguments"); args != nil {
				sc := csharpLocalScopeOf(n)
				walkNodes(args, func(d *sitter.Node) {
					if d.Type() == "identifier" {
						add(d, sc)
					}
				})
			}
		case "using_statement":
			// Only the parenthesized resource form carries a
			// variable_declaration child here.
			for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
				c := n.NamedChild(i)
				if c == nil || c.Type() != "variable_declaration" {
					continue
				}
				walkNodes(c, func(d *sitter.Node) {
					if d.Type() == "variable_declarator" {
						if name := d.ChildByFieldName("name"); name != nil && name.Type() == "identifier" {
							add(name, spanOf(n))
						}
					}
				})
			}
		}
	})
}
