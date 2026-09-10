package languages

import (
	"path"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Odoo facts stay on source-owned nodes. The synthesizer can rebuild links
// after a target is removed and reintroduced without reading the filesystem.
func odooMeta(n *graph.Node) map[string]any {
	if n.Meta == nil {
		n.Meta = map[string]any{}
	}
	n.Meta["framework"] = "odoo"
	return n.Meta
}

func odooRef(n *graph.Node, domain, name, relation string, line int) {
	if name == "" {
		return
	}
	m := odooMeta(n)
	refs, _ := m["odoo_refs"].([]map[string]any)
	m["odoo_refs"] = append(refs, map[string]any{"domain": domain, "name": name, "relation": relation, "line": line})
}

func odooNode(r *parser.ExtractionResult, file, name string, kind graph.NodeKind, line int, language string) *graph.Node {
	for _, old := range r.Nodes {
		if old.ID == file+"::odoo:"+name {
			return old
		}
	}
	n := &graph.Node{ID: file + "::odoo:" + name, Name: name, QualName: name, Kind: kind, FilePath: file, StartLine: line, EndLine: line, Language: language}
	odooMeta(n)
	r.Nodes = append(r.Nodes, n)
	r.Edges = append(r.Edges, &graph.Edge{From: file, To: n.ID, Kind: graph.EdgeDefines, FilePath: file, Line: line})
	return n
}

func odooText(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	return n.Content(src)
}

func odooImportedName(name string, imports map[string]string) string {
	for prefix := name; prefix != ""; {
		if module, ok := imports[prefix]; ok {
			return module + strings.TrimPrefix(name, prefix)
		}
		dot := strings.LastIndex(prefix, ".")
		if dot < 0 {
			break
		}
		prefix = prefix[:dot]
	}
	return ""
}

// Only literal strings qualify as binding keys. In particular f-strings,
// concatenations with variables and calls must never create invented targets.
func odooLiteral(n *sitter.Node, src []byte) string {
	if n == nil || n.Type() != "string" {
		return ""
	}
	t := n.Content(src)
	start := strings.IndexAny(t, "\"'")
	if start < 0 || strings.ContainsAny(strings.ToLower(t[:start]), "fb") {
		return ""
	}
	t = t[start:]
	q := t[:1]
	if strings.HasPrefix(t, q+q+q) && len(t) >= 6 {
		return t[3 : len(t)-3]
	}
	if len(t) < 2 {
		return ""
	}
	v := t[1 : len(t)-1]
	if !strings.Contains(v, "\\") {
		return v
	}
	if t[0] == '"' {
		if s, err := strconv.Unquote(t); err == nil {
			return s
		}
	}
	return strings.ReplaceAll(strings.ReplaceAll(v, "\\'", "'"), "\\\\", "\\")
}

func odooStrings(n *sitter.Node, src []byte) []string {
	if v := odooLiteral(n, src); v != "" {
		return []string{v}
	}
	if n == nil || (n.Type() != "list" && n.Type() != "tuple" && n.Type() != "set") {
		return nil
	}
	var out []string
	for i := 0; i < int(n.NamedChildCount()); i++ {
		if v := odooLiteral(n.NamedChild(i), src); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func odooDict(n *sitter.Node, src []byte) map[string]*sitter.Node {
	out := map[string]*sitter.Node{}
	if n == nil || n.Type() != "dictionary" {
		return out
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		p := n.NamedChild(i)
		if p.Type() == "pair" {
			if k := odooLiteral(p.ChildByFieldName("key"), src); k != "" {
				out[k] = p.ChildByFieldName("value")
			}
		}
	}
	return out
}

func odooArgs(n *sitter.Node, src []byte) ([]*sitter.Node, map[string]*sitter.Node) {
	var pos []*sitter.Node
	kw := map[string]*sitter.Node{}
	if n == nil {
		return pos, kw
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		if c.Type() == "keyword_argument" {
			kw[odooText(c.ChildByFieldName("name"), src)] = c.ChildByFieldName("value")
		} else if c.Type() != "comment" {
			pos = append(pos, c)
		}
	}
	return pos, kw
}

func odooWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i := 0; i < int(n.NamedChildCount()); i++ {
		odooWalk(n.NamedChild(i), fn)
	}
}

func odooManifest(r *parser.ExtractionResult, root *sitter.Node, file string, src []byte) {
	base := path.Base(file)
	if base != "__manifest__.py" && base != "__openerp__.py" {
		return
	}
	for i := 0; i < int(root.NamedChildCount()); i++ {
		s := root.NamedChild(i)
		if s.Type() != "expression_statement" || s.NamedChildCount() != 1 || s.NamedChild(0).Type() != "dictionary" {
			continue
		}
		values := odooDict(s.NamedChild(0), src)
		if values["name"] == nil {
			continue
		}
		n := odooNode(r, file, path.Base(path.Dir(file)), graph.KindModule, int(s.StartPoint().Row)+1, "python")
		n.Meta["odoo_kind"], n.Meta["odoo_module"], n.Meta["odoo_root"] = "module", n.Name, path.Dir(file)
		attrs := map[string]string{}
		for key, value := range values {
			attrs[key] = odooText(value, src)
		}
		n.Meta["odoo_spec"] = attrs
		for _, dep := range odooStrings(values["depends"], src) {
			odooRef(n, "module", dep, "depends_on", n.StartLine)
		}
		for _, key := range []string{"data", "demo", "qweb"} {
			for _, p := range odooStrings(values[key], src) {
				odooRef(n, "file", path.Join(path.Dir(file), p), key, n.StartLine)
			}
		}
		for _, key := range []string{"pre_init_hook", "post_init_hook", "uninstall_hook", "post_load"} {
			odooRef(n, "function", odooLiteral(values[key], src), key, n.StartLine)
		}
		for bundle, entries := range odooDict(values["assets"], src) {
			asset := odooNode(r, file, "asset:"+bundle, graph.KindResource, int(entries.StartPoint().Row)+1, "python")
			asset.Meta["odoo_kind"], asset.Meta["odoo_asset"] = "asset", bundle
			asset.Meta["odoo_spec"] = entries.Content(src)
			for _, p := range odooStrings(entries, src) {
				odooRef(asset, "asset_file", p, "asset", asset.StartLine)
			}
		}
	}
}

// captureOdooPython augments normal Python symbols, preserving their IDs.
func captureOdooPython(r *parser.ExtractionResult, root *sitter.Node, file string, src []byte, imports map[string]string) {
	odooManifest(r, root, file, src)
	active := false
	for _, imp := range imports {
		if imp == "odoo" || imp == "openerp" || strings.HasPrefix(imp, "odoo.") || strings.HasPrefix(imp, "openerp.") {
			active = true
		}
	}
	if !active {
		return
	}
	byID := map[string]*graph.Node{}
	for _, n := range r.Nodes {
		byID[n.ID] = n
	}
	odooWalk(root, func(cls *sitter.Node) {
		if cls.Type() != "class_definition" {
			return
		}
		name := odooText(cls.ChildByFieldName("name"), src)
		n := byID[file+"::"+name]
		if n == nil {
			return
		}
		attrs := map[string]*sitter.Node{}
		body := cls.ChildByFieldName("body")
		if body == nil {
			return
		}
		for i := 0; i < int(body.NamedChildCount()); i++ {
			s := body.NamedChild(i)
			if s.Type() == "expression_statement" && s.NamedChildCount() == 1 {
				a := s.NamedChild(0)
				if a.Type() == "assignment" && a.ChildByFieldName("left") != nil && a.ChildByFieldName("left").Type() == "identifier" {
					attrs[odooText(a.ChildByFieldName("left"), src)] = a.ChildByFieldName("right")
				}
			}
		}
		model := odooLiteral(attrs["_name"], src)
		inherits := odooStrings(attrs["_inherit"], src)
		if model == "" && len(inherits) == 0 {
			return
		}
		m := odooMeta(n)
		m["odoo_kind"], m["odoo_model"], m["odoo_inherit"] = "model", model, inherits
		m["odoo_bases"] = odooText(cls.ChildByFieldName("superclasses"), src)
		spec := map[string]string{}
		for k, v := range attrs {
			if strings.HasPrefix(k, "_") {
				spec[k] = odooText(v, src)
			}
		}
		m["odoo_spec"] = spec
		if model == "" && len(inherits) > 0 {
			model = inherits[0]
		}
		m["odoo_effective_model"] = model
		for _, parent := range inherits {
			odooRef(n, "model_base", parent, "inherit", n.StartLine)
		}
		for parent, field := range odooDict(attrs["_inherits"], src) {
			odooRef(n, "model", parent, "delegates", n.StartLine)
			spec["delegate:"+parent] = odooText(field, src)
		}
		for field, call := range attrs {
			if call == nil || call.Type() != "call" {
				continue
			}
			callee := odooText(call.ChildByFieldName("function"), src)
			parts := strings.Split(callee, ".")
			if len(parts) < 2 {
				continue
			}
			prefix := strings.Join(parts[:len(parts)-1], ".")
			imp := odooImportedName(prefix, imports)
			if imp != "odoo.fields" && imp != "openerp.fields" {
				continue
			}
			f := byID[n.ID+"."+field]
			if f == nil {
				f = &graph.Node{ID: n.ID + "." + field, Name: field, QualName: name + "." + field, Kind: graph.KindField, FilePath: file, StartLine: int(call.StartPoint().Row) + 1, EndLine: int(call.EndPoint().Row) + 1, Language: "python"}
				r.Nodes = append(r.Nodes, f)
				r.Edges = append(r.Edges, &graph.Edge{From: f.ID, To: n.ID, Kind: graph.EdgeMemberOf, FilePath: file, Line: f.StartLine})
			}
			fm := odooMeta(f)
			fm["odoo_kind"], fm["odoo_model"], fm["odoo_field"] = "field", model, field
			fm["odoo_field_type"], fm["odoo_spec"] = parts[len(parts)-1], call.Content(src)
			pos, kw := odooArgs(call.ChildByFieldName("arguments"), src)
			comodel := odooLiteral(kw["comodel_name"], src)
			if comodel == "" && len(pos) > 0 {
				comodel = odooLiteral(pos[0], src)
			}
			switch parts[len(parts)-1] {
			case "Many2one", "One2many", "Many2many":
				fm["odoo_comodel"] = comodel
				odooRef(f, "model", comodel, "relation", f.StartLine)
			}
			for _, key := range []string{"compute", "inverse", "search", "default", "selection", "domain"} {
				v := ""
				if key == "compute" || key == "inverse" || key == "search" || key == "selection" {
					v = odooLiteral(kw[key], src)
				}
				if kw[key] != nil && kw[key].Type() == "identifier" {
					v = kw[key].Content(src)
				}
				if v != "" && !strings.ContainsAny(v, " [](),") && model != "" {
					odooRef(f, "method", model+"."+v, key, f.StartLine)
				}
			}
			if related := odooLiteral(kw["related"], src); related != "" {
				fm["odoo_related"] = related
				odooRef(f, "field_path", model+"/"+related, "related", f.StartLine)
			}
		}
		for _, method := range r.Nodes {
			if method.Kind != graph.KindMethod || !strings.HasPrefix(method.ID, n.ID+".") {
				continue
			}
			mm := odooMeta(method)
			mm["odoo_kind"], mm["odoo_model"], mm["odoo_method"] = "method", model, method.Name
		}
	})
	// Site attribution uses the smallest enclosing code symbol.
	owner := func(at *sitter.Node) *graph.Node {
		line := int(at.StartPoint().Row) + 1
		for p := at.Parent(); p != nil; p = p.Parent() {
			if p.Type() == "decorated_definition" {
				for i := 0; i < int(p.NamedChildCount()); i++ {
					def := p.NamedChild(i)
					if def.Type() == "function_definition" {
						name := odooText(def.ChildByFieldName("name"), src)
						if cls := pyDirectClassParent(def, src); cls != "" {
							name = cls + "." + name
						}
						if n := byID[file+"::"+name]; n != nil {
							return n
						}
					}
				}
				break
			}
			if p.Type() == "function_definition" || p.Type() == "class_definition" {
				break
			}
		}
		var best *graph.Node
		for _, n := range r.Nodes {
			if (n.Kind == graph.KindMethod || n.Kind == graph.KindFunction || n.Kind == graph.KindType) && n.StartLine <= line && n.EndLine >= line && (best == nil || n.EndLine-n.StartLine < best.EndLine-best.StartLine) {
				best = n
			}
		}
		if best == nil {
			best = byID[file]
		}
		return best
	}
	odooWalk(root, func(at *sitter.Node) {
		if at.Type() != "call" && at.Type() != "subscript" {
			return
		}
		n := owner(at)
		if n == nil {
			return
		}
		line := int(at.StartPoint().Row) + 1
		if at.Type() == "subscript" {
			recv := odooText(at.ChildByFieldName("value"), src)
			if recv == "env" || strings.HasSuffix(recv, ".env") || recv == "self.pool" {
				odooRef(n, "model", odooLiteral(at.ChildByFieldName("subscript"), src), "env", line)
			}
		}
		if at.Type() == "call" {
			fn := odooText(at.ChildByFieldName("function"), src)
			pos, kw := odooArgs(at.ChildByFieldName("arguments"), src)
			if (strings.HasSuffix(fn, ".env.ref") || fn == "env.ref" || strings.HasSuffix(fn, ".has_group")) && len(pos) > 0 {
				odooRef(n, "xmlid", odooLiteral(pos[0], src), "ref", line)
			}
			resolved := odooImportedName(fn, imports)
			if resolved == "odoo.http.route" || resolved == "openerp.http.route" {
				paths := odooStrings(kw["route"], src)
				if len(pos) > 0 {
					paths = odooStrings(pos[0], src)
				}
				for _, p := range paths {
					route := odooNode(r, file, "route:"+strconv.Itoa(line)+":"+p, graph.KindResource, line, "python")
					route.Meta["odoo_kind"], route.Meta["odoo_route"], route.Meta["odoo_spec"] = "route", p, at.Content(src)
					route.Meta["odoo_handler"], route.Meta["odoo_methods"] = n.ID, odooStrings(kw["methods"], src)
					options := map[string]string{}
					for k, v := range kw {
						options[k] = odooText(v, src)
					}
					route.Meta["odoo_options"] = options
					r.Edges = append(r.Edges, &graph.Edge{From: route.ID, To: n.ID, Kind: graph.EdgeReferences, FilePath: file, Line: line})
				}
			}
			if strings.HasPrefix(resolved, "odoo.api.") || strings.HasPrefix(resolved, "openerp.api.") {
				fn = "api." + resolved[strings.LastIndex(resolved, ".")+1:]
				model, _ := n.Meta["odoo_model"].(string)
				if strings.HasSuffix(fn, ".depends") || strings.HasSuffix(fn, ".constrains") || strings.HasSuffix(fn, ".onchange") {
					for _, p := range pos {
						if model != "" {
							odooRef(n, "field_path", model+"/"+odooLiteral(p, src), fn, line)
						}
					}
				}
				odooMeta(n)["odoo_decorator:"+fn] = at.Content(src)
			}
		}
	})
}
