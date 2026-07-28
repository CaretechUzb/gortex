package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Declaration forms the member walk did not mint.

// phpMemberModifiers records the declaration modifiers PHP allows on a class
// member. Only `visibility` was stamped before, so `static`, `abstract`,
// `final` and `readonly` were invisible: an analyzer could not tell a static
// method from an instance one, and `readonly` — the marker that a property is
// never written after construction — was lost entirely.
func phpMemberModifiers(member *sitter.Node, meta map[string]any) {
	if member == nil || meta == nil {
		return
	}
	for i, n := 0, int(member.NamedChildCount()); i < n; i++ {
		switch member.NamedChild(i).Type() {
		case "static_modifier":
			meta["static"] = true
		case "abstract_modifier":
			meta["abstract"] = true
		case "final_modifier":
			meta["final"] = true
		case "readonly_modifier":
			meta["readonly"] = true
		}
	}
}

// extractPHPPromotedProperties mints a field for every PHP 8 constructor
// promoted property. Promotion declares the property in the constructor's
// parameter list, so the class body holds no property_declaration for it and
// the member walk minted nothing — on a modern PHP codebase, where promotion is
// the idiomatic way to declare dependencies, the class's fields were simply
// absent from the graph.
func (e *PHPExtractor) extractPHPPromotedProperties(
	ctor *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	ownerName, ownerID string,
) {
	if ctor == nil {
		return
	}
	params := ctor.ChildByFieldName("parameters")
	if params == nil {
		return
	}
	for i, n := 0, int(params.NamedChildCount()); i < n; i++ {
		p := params.NamedChild(i)
		if p.Type() != "property_promotion_parameter" {
			continue
		}
		name := phpParamVarName(p, src)
		if name == "" {
			continue
		}
		line := int(p.StartPoint().Row) + 1
		id, ok := disambiguateID(seen, filePath+"::"+ownerName+"."+name, line)
		if !ok {
			continue
		}
		vis := VisibilityPublic
		if v := p.ChildByFieldName("visibility"); v != nil {
			vis = strings.ToLower(strings.TrimSpace(v.Content(src)))
		}
		meta := map[string]any{
			"receiver": ownerName, "visibility": vis, "promoted": true,
		}
		if p.ChildByFieldName("readonly") != nil {
			meta["readonly"] = true
		}
		propType := phpParameterType(p, src)
		if propType != "" {
			meta["field_type"] = propType
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindField, Name: name,
			FilePath: filePath, StartLine: line, EndLine: line, Language: "php", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: line,
		})
		emitPHPTypeUseEdges(id, propType, filePath, line, result)
	}
}

// extractPHPFileConstant mints a file-scope `const NAME = ...;`. The member
// walk only reached const_declaration inside a class body, so a constants file
// — a common PHP idiom — defined no symbols at all.
func (e *PHPExtractor) extractPHPFileConstant(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	for i, n := 0, int(node.NamedChildCount()); i < n; i++ {
		ce := node.NamedChild(i)
		if ce.Type() != "const_element" {
			continue
		}
		nameNode := e.findChildByType(ce, "name")
		if nameNode == nil {
			continue
		}
		e.emitPHPFileConstantNode(nameNode.Content(src), int(ce.StartPoint().Row)+1,
			filePath, fileNode, result, seen)
	}
}

// extractPHPDefineConstant mints the constant declared by `define('NAME', ...)`.
// PHP's other constant-declaration form is an ordinary function call, so no
// declaration node exists for it.
func (e *PHPExtractor) extractPHPDefineConstant(
	call *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	fn := call.ChildByFieldName("function")
	if fn == nil || !strings.EqualFold(strings.TrimLeft(fn.Content(src), `\`), "define") {
		return
	}
	args := call.ChildByFieldName("arguments")
	if args == nil || args.NamedChildCount() == 0 {
		return
	}
	name := e.extractStringContent(args.NamedChild(0), src)
	// A computed name (`define($k, …)`) declares nothing statically knowable.
	if name == "" || strings.ContainsAny(name, "$ ") {
		return
	}
	e.emitPHPFileConstantNode(name, int(call.StartPoint().Row)+1, filePath, fileNode, result, seen)
}

func (e *PHPExtractor) emitPHPFileConstantNode(
	name string, line int,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	if name == "" {
		return
	}
	id, ok := disambiguateID(seen, filePath+"::"+name, line)
	if !ok {
		return
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindConstant, Name: name,
		FilePath: filePath, StartLine: line, EndLine: line, Language: "php",
		Meta: map[string]any{"visibility": VisibilityPublic},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
	})
}
