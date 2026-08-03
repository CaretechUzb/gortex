package contracts

import (
	"fmt"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// TerraformExtractor turns Terraform-declared environment values into
// contract providers so a value defined in an infrastructure repo pairs
// with the os.Getenv / process.env read that consumes it in a different
// application repo.
//
// Coverage is deliberately one resource type: an aws_lambda_function's
// `environment { variables = {...} }` map, where the declared key IS the
// environment variable name the function sees at runtime. Other Terraform
// config sources — bare variable/output blocks, kubernetes_config_map and
// kubernetes_secret data keys — carry no such structural guarantee (a
// ConfigMap key only becomes an env var if some Pod spec binds it via
// envFrom/valueFrom, which this extractor doesn't inspect), so minting a
// provider from them would assert a binding we can't verify. Those stay
// graph-level only.
type TerraformExtractor struct{}

const terraformLambdaResource = "aws_lambda_function"

// terraformPrefilterMarkers short-circuits the tree-sitter parse for the
// overwhelming majority of .tf files, which declare no Lambda at all.
var terraformPrefilterMarkers = [][]byte{[]byte(terraformLambdaResource)}

func (e *TerraformExtractor) SupportedLanguages() []string { return []string{"hcl"} }

// Extract parses src with the HCL grammar itself, the same lazy-parse
// shape HTTPExtractor.Extract uses when the caller has no tree to hand
// it. ParseTreeForLang is the shared helper the indexer's incremental
// re-walk path also uses to get a tree for .tf files.
func (e *TerraformExtractor) Extract(filePath string, src []byte, nodes []*graph.Node, _ []*graph.Edge) []Contract {
	tree := ParseTreeForLang("hcl", src)
	defer tree.Release()
	return e.extract(filePath, src, nodes, tree)
}

// ExtractWithTree implements TreeAwareExtractor, consuming a parse tree
// the caller already built instead of reparsing.
func (e *TerraformExtractor) ExtractWithTree(
	filePath string,
	src []byte,
	nodes []*graph.Node,
	_ []*graph.Edge,
	tree *parser.ParseTree,
) []Contract {
	return e.extract(filePath, src, nodes, tree)
}

func (e *TerraformExtractor) extract(filePath string, src []byte, nodes []*graph.Node, tree *parser.ParseTree) []Contract {
	if !srcHasAnyMarker(src, terraformPrefilterMarkers) {
		return nil
	}
	st := tree.Tree()
	if st == nil {
		return nil
	}
	root := st.RootNode()
	if root == nil {
		return nil
	}

	fileNodes := filterFileNodes(filePath, nodes)
	var out []Contract
	for _, block := range hclTopLevelBlocks(root) {
		blockType, labels, body := hclBlockParts(block, src)
		if blockType != "resource" || len(labels) < 2 || labels[0] != terraformLambdaResource || body == nil {
			continue
		}
		address := labels[0] + "." + labels[1]
		symbolID := hclBlockSymbolID(fileNodes, address)

		for _, envBody := range hclNestedBlockBodies(body, "environment", src) {
			obj := hclLiteralObject(hclAttrExpr(envBody, "variables", src))
			for _, entry := range hclObjectKeys(obj, src) {
				out = append(out, Contract{
					ID:       fmt.Sprintf("env::%s", entry.name),
					Type:     ContractEnv,
					Role:     RoleProvider,
					SymbolID: symbolID,
					FilePath: filePath,
					Line:     entry.line,
					Meta: map[string]any{
						"var":        entry.name,
						"source":     "terraform",
						"tf_address": address,
					},
					// Attribute-inferred rather than an unambiguous
					// declaration, matching EnvVarExtractor's 0.95/0.9 split.
					Confidence: 0.9,
				})
			}
		}
	}
	return out
}

// hclBlockSymbolID returns the ID of the graph node the HCL language
// extractor emitted for the block at the given Terraform address. HCL
// blocks are graph.KindType nodes, so findEnclosingSymbol — which only
// considers functions and methods — would silently return "" for every
// Terraform contract.
func hclBlockSymbolID(fileNodes []*graph.Node, address string) string {
	for _, n := range fileNodes {
		if n.Kind != graph.KindType {
			continue
		}
		if addr, _ := n.Meta["tf_address"].(string); addr == address {
			return n.ID
		}
	}
	return ""
}

// hclTopLevelBlocks returns every block that isn't nested inside another
// block, descending only the config_file / body wrappers.
func hclTopLevelBlocks(root *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		for i, nc := 0, int(n.ChildCount()); i < nc; i++ {
			child := n.Child(i)
			if child == nil {
				continue
			}
			switch child.Type() {
			case "block":
				out = append(out, child)
			case "config_file", "body":
				walk(child)
			}
		}
	}
	walk(root)
	return out
}

// hclBlockParts splits a block into its type identifier, its string
// labels, and its body — `resource "aws_lambda_function" "worker" {…}`
// yields ("resource", ["aws_lambda_function", "worker"], body).
func hclBlockParts(block *sitter.Node, src []byte) (blockType string, labels []string, body *sitter.Node) {
	for i, nc := 0, int(block.ChildCount()); i < nc; i++ {
		child := block.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "identifier":
			if blockType == "" {
				blockType = child.Content(src)
			}
		case "string_lit":
			if text := hclStringText(child, src); text != "" {
				labels = append(labels, text)
			}
		case "body":
			body = child
		}
	}
	return blockType, labels, body
}

// hclNestedBlockBodies returns the bodies of every block named name
// declared directly inside body.
func hclNestedBlockBodies(body *sitter.Node, name string, src []byte) []*sitter.Node {
	var out []*sitter.Node
	for i, nc := 0, int(body.ChildCount()); i < nc; i++ {
		child := body.Child(i)
		if child == nil || child.Type() != "block" {
			continue
		}
		if blockType, _, nested := hclBlockParts(child, src); blockType == name && nested != nil {
			out = append(out, nested)
		}
	}
	return out
}

// hclAttrExpr returns the value expression of the attribute named name
// declared directly inside body, or nil when there is none.
func hclAttrExpr(body *sitter.Node, name string, src []byte) *sitter.Node {
	for i, nc := 0, int(body.ChildCount()); i < nc; i++ {
		attr := body.Child(i)
		if attr == nil || attr.Type() != "attribute" {
			continue
		}
		var key string
		var expr *sitter.Node
		for j, anc := 0, int(attr.ChildCount()); j < anc; j++ {
			c := attr.Child(j)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "identifier":
				if key == "" {
					key = c.Content(src)
				}
			case "expression":
				expr = c
			}
		}
		if key == name {
			return expr
		}
	}
	return nil
}

// hclLiteralObject returns the object literal an expression evaluates to,
// or nil when the expression is anything else. A `for` expression, a
// merge(...) call, or a reference to a local map all parse to different
// node types, and their individual keys aren't statically enumerable —
// skipping them is the point, not a limitation to work around.
func hclLiteralObject(expr *sitter.Node) *sitter.Node {
	if expr == nil {
		return nil
	}
	for i, nc := 0, int(expr.ChildCount()); i < nc; i++ {
		coll := expr.Child(i)
		if coll == nil || coll.Type() != "collection_value" {
			continue
		}
		for j, cnc := 0, int(coll.ChildCount()); j < cnc; j++ {
			if obj := coll.Child(j); obj != nil && obj.Type() == "object" {
				return obj
			}
		}
	}
	return nil
}

type hclObjectKey struct {
	name string
	line int
}

// hclObjectKeys returns the statically known keys of an object literal.
// Terraform accepts both a bare identifier and a quoted string as a key
// and treats each as a literal name; anything else (a parenthesised
// expression, an interpolated string) names no single key and is skipped.
func hclObjectKeys(obj *sitter.Node, src []byte) []hclObjectKey {
	if obj == nil {
		return nil
	}
	var out []hclObjectKey
	for i, nc := 0, int(obj.ChildCount()); i < nc; i++ {
		elem := obj.Child(i)
		if elem == nil || elem.Type() != "object_elem" {
			continue
		}
		var keyExpr *sitter.Node
		for j, enc := 0, int(elem.ChildCount()); j < enc; j++ {
			if c := elem.Child(j); c != nil && c.Type() == "expression" {
				keyExpr = c
				break
			}
		}
		// A single child means an undecorated name: a longer chain is a
		// traversal like local.key, which names no literal key.
		if keyExpr == nil || keyExpr.ChildCount() != 1 {
			continue
		}
		var name string
		switch head := keyExpr.Child(0); head.Type() {
		case "variable_expr":
			name = hclIdentifierText(head, src)
		case "literal_value":
			for j, hnc := 0, int(head.ChildCount()); j < hnc; j++ {
				if lit := head.Child(j); lit != nil && lit.Type() == "string_lit" {
					name = hclStringText(lit, src)
				}
			}
		}
		if name == "" {
			continue
		}
		out = append(out, hclObjectKey{name: name, line: int(elem.StartPoint().Row) + 1})
	}
	return out
}

// hclIdentifierText returns the identifier a variable_expr names.
func hclIdentifierText(varExpr *sitter.Node, src []byte) string {
	for i, nc := 0, int(varExpr.ChildCount()); i < nc; i++ {
		if c := varExpr.Child(i); c != nil && c.Type() == "identifier" {
			return c.Content(src)
		}
	}
	return ""
}

// hclStringText returns the text of a plain quoted string, or "" when the
// string interpolates ("PREFIX_${var.env}") — a name assembled at plan
// time isn't one this extractor can report.
func hclStringText(lit *sitter.Node, src []byte) string {
	var text string
	for i, nc := 0, int(lit.ChildCount()); i < nc; i++ {
		c := lit.Child(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "quoted_template_start", "quoted_template_end":
		case "template_literal":
			if text != "" {
				return ""
			}
			text = c.Content(src)
		default:
			return ""
		}
	}
	return text
}
