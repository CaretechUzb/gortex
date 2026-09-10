package languages

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

func captureOdooJavaScript(r *parser.ExtractionResult, root *sitter.Node, file string, src []byte) {
	if !bytes.Contains(src, []byte("@odoo-module")) && !bytes.Contains(src, []byte("@web/")) && !bytes.Contains(src, []byte("odoo.define")) && !bytes.Contains(src, []byte("@odoo/owl")) {
		return
	}
	byName := map[string]*graph.Node{}
	for _, n := range r.Nodes {
		byName[n.Name] = n
	}
	owner := func(at *sitter.Node) *graph.Node {
		line := int(at.StartPoint().Row) + 1
		var best *graph.Node
		for _, n := range r.Nodes {
			if n.Kind == graph.KindFile {
				if best == nil {
					best = n
				}
				continue
			}
			if (n.Kind == graph.KindMethod || n.Kind == graph.KindFunction || n.Kind == graph.KindType) && n.StartLine <= line && n.EndLine >= line && (best == nil || best.Kind == graph.KindFile || n.EndLine-n.StartLine < best.EndLine-best.StartLine) {
				best = n
			}
		}
		return best
	}
	category := func(call *sitter.Node) string {
		if call == nil || call.Type() != "call_expression" {
			return ""
		}
		fn := call.ChildByFieldName("function")
		if fn == nil || odooText(fn.ChildByFieldName("property"), src) != "category" {
			return ""
		}
		args, _ := odooArgs(call.ChildByFieldName("arguments"), src)
		if len(args) == 0 {
			return ""
		}
		return odooLiteral(args[0], src)
	}
	categories := map[string]string{}
	odooWalk(root, func(at *sitter.Node) {
		if at.Type() == "variable_declarator" {
			if cat := category(at.ChildByFieldName("value")); cat != "" {
				categories[odooText(at.ChildByFieldName("name"), src)] = cat
			}
		}
	})
	odooWalk(root, func(at *sitter.Node) {
		if at.Type() == "public_field_definition" || at.Type() == "field_definition" || at.Type() == "assignment_expression" || at.Type() == "pair" {
			left := at.ChildByFieldName("name")
			if left == nil {
				left = at.ChildByFieldName("property")
			}
			if left == nil {
				left = at.ChildByFieldName("left")
			}
			if left == nil {
				left = at.ChildByFieldName("key")
			}
			key := odooText(left, src)
			value := at.ChildByFieldName("value")
			if value == nil {
				value = at.ChildByFieldName("right")
			}
			if key == "template" || strings.HasSuffix(key, ".template") {
				n := owner(at)
				if strings.HasSuffix(key, ".template") {
					if target := byName[strings.TrimSuffix(key, ".template")]; target != nil {
						n = target
					}
				}
				if n != nil {
					odooRef(n, "template", odooLiteral(value, src), "template", int(at.StartPoint().Row)+1)
				}
			}
		}
		if at.Type() != "call_expression" {
			return
		}
		n := owner(at)
		if n == nil {
			return
		}
		line := int(at.StartPoint().Row) + 1
		fn := at.ChildByFieldName("function")
		name := odooText(fn, src)
		args, _ := odooArgs(at.ChildByFieldName("arguments"), src)
		if len(args) == 0 {
			return
		}
		if name == "useService" {
			odooRef(n, "registry", "services/"+odooLiteral(args[0], src), "service", line)
		}
		if name == "odoo.define" {
			odooMeta(n)["odoo_js_module"] = odooLiteral(args[0], src)
		}
		if name == "patch" {
			patch := odooNode(r, file, "patch:"+strconv.Itoa(line), graph.KindResource, line, n.Language)
			patch.Meta["odoo_kind"], patch.Meta["odoo_spec"] = "patch", at.Content(src)
			if target := byName[strings.TrimSuffix(odooText(args[0], src), ".prototype")]; target != nil {
				r.Edges = append(r.Edges, &graph.Edge{From: patch.ID, To: target.ID, Kind: graph.EdgeExtends, FilePath: file, Line: line})
			}
		}
		if fn != nil && fn.Type() == "member_expression" {
			method := odooText(fn.ChildByFieldName("property"), src)
			obj := fn.ChildByFieldName("object")
			cat := category(obj)
			if cat == "" {
				cat = categories[odooText(obj, src)]
			}
			if cat != "" && (method == "add" || method == "get" || method == "contains" || method == "remove") {
				key := odooLiteral(args[0], src)
				if key == "" {
					return
				}
				if method == "add" && len(args) > 1 {
					entry := odooNode(r, file, "registry:"+cat+"/"+key, graph.KindResource, line, n.Language)
					entry.Meta["odoo_kind"], entry.Meta["odoo_registry"], entry.Meta["odoo_spec"] = "registry", cat+"/"+key, at.Content(src)
					if target := byName[odooText(args[1], src)]; target != nil {
						r.Edges = append(r.Edges, &graph.Edge{From: entry.ID, To: target.ID, Kind: graph.EdgeReferences, FilePath: file, Line: line})
					}
				} else {
					odooRef(n, "registry", cat+"/"+key, method, line)
				}
			}
			if method == "call" && len(args) >= 2 && (strings.HasSuffix(odooText(obj, src), "orm") || strings.HasSuffix(odooText(obj, src), "ormService")) {
				model := odooLiteral(args[0], src)
				methodName := odooLiteral(args[1], src)
				if model != "" {
					odooRef(n, "model", model, "rpc_model", line)
					if methodName != "" {
						odooRef(n, "method", model+"."+methodName, "rpc", line)
					}
				}
			}
		}
	})
}
