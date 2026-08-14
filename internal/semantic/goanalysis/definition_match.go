package goanalysis

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

type goDefinitionRole uint8

const (
	goDefinitionUnknown goDefinitionRole = iota
	goDefinitionFunction
	goDefinitionMethod
	goDefinitionField
	goDefinitionReceiver
	goDefinitionParam
	goDefinitionResult
	goDefinitionLocal
	goDefinitionPackageVar
	goDefinitionConstant
	goDefinitionType
	goDefinitionInterface
	goDefinitionGenericParam
)

// goDefinitionContext records syntax facts that go/types.Object does not
// expose, notably whether a *types.Var is a receiver, parameter, result, or
// local. owner is the graph-facing owner identity (for example Widget.Run).
type goDefinitionContext struct {
	role  goDefinitionRole
	owner string
}

// buildGoDefinitionContexts projects just enough AST parent information to
// classify TypesInfo.Defs entries. The map is scoped to one packages.Package
// and is discarded as soon as its definitions have been matched.
func buildGoDefinitionContexts(files []*ast.File) map[*ast.Ident]goDefinitionContext {
	contexts := make(map[*ast.Ident]goDefinitionContext)
	for _, file := range files {
		if file == nil {
			continue
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.GenDecl:
				markGoGenDeclDefinitions(decl, true, "", contexts)
			case *ast.FuncDecl:
				markGoFuncDeclDefinitions(decl, contexts)
			}
		}
	}
	return contexts
}

func markGoFuncDeclDefinitions(decl *ast.FuncDecl, contexts map[*ast.Ident]goDefinitionContext) {
	if decl == nil || decl.Name == nil || decl.Type == nil {
		return
	}

	receiver := goASTReceiverTypeName(decl.Recv)
	callableOwner := decl.Name.Name
	role := goDefinitionFunction
	if receiver != "" {
		role = goDefinitionMethod
		callableOwner = receiver + "." + decl.Name.Name
	}
	contexts[decl.Name] = goDefinitionContext{role: role, owner: receiver}
	markGoFieldListDefinitions(decl.Recv, goDefinitionReceiver, callableOwner, contexts)
	markGoFieldListDefinitions(decl.Type.TypeParams, goDefinitionGenericParam, callableOwner, contexts)
	markGoFieldListDefinitions(decl.Type.Params, goDefinitionParam, callableOwner, contexts)
	markGoFieldListDefinitions(decl.Type.Results, goDefinitionResult, callableOwner, contexts)
	markGoLocalDefinitions(decl.Body, callableOwner, contexts)
}

func markGoFieldListDefinitions(list *ast.FieldList, role goDefinitionRole, owner string, contexts map[*ast.Ident]goDefinitionContext) {
	if list == nil {
		return
	}
	for _, field := range list.List {
		if field == nil {
			continue
		}
		for _, name := range field.Names {
			if name != nil {
				contexts[name] = goDefinitionContext{role: role, owner: owner}
			}
		}
	}
}

func markGoGenDeclDefinitions(decl *ast.GenDecl, packageScope bool, owner string, contexts map[*ast.Ident]goDefinitionContext) {
	if decl == nil {
		return
	}
	for _, spec := range decl.Specs {
		switch spec := spec.(type) {
		case *ast.ValueSpec:
			role := goDefinitionLocal
			switch decl.Tok {
			case token.CONST:
				role = goDefinitionConstant
			case token.VAR:
				if packageScope {
					role = goDefinitionPackageVar
				}
			}
			for _, name := range spec.Names {
				if name != nil {
					contexts[name] = goDefinitionContext{role: role, owner: owner}
				}
			}
		case *ast.TypeSpec:
			markGoTypeSpecDefinitions(spec, contexts)
		}
	}
}

func markGoTypeSpecDefinitions(spec *ast.TypeSpec, contexts map[*ast.Ident]goDefinitionContext) {
	if spec == nil || spec.Name == nil {
		return
	}
	role := goDefinitionType
	if _, ok := spec.Type.(*ast.InterfaceType); ok {
		role = goDefinitionInterface
	}
	contexts[spec.Name] = goDefinitionContext{role: role}
	markGoFieldListDefinitions(spec.TypeParams, goDefinitionGenericParam, spec.Name.Name, contexts)

	switch typ := spec.Type.(type) {
	case *ast.StructType:
		markGoFieldListDefinitions(typ.Fields, goDefinitionField, spec.Name.Name, contexts)
	case *ast.InterfaceType:
		// Named interface fields are methods. Embedded fields have no Defs
		// entry and are intentionally ignored here.
		markGoFieldListDefinitions(typ.Methods, goDefinitionMethod, spec.Name.Name, contexts)
	}
}

func markGoLocalDefinitions(body *ast.BlockStmt, owner string, contexts map[*ast.Ident]goDefinitionContext) {
	if body == nil {
		return
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			// A closure has its own parameters/results and lexical body. Its
			// graph owner is line-based rather than a source identifier, so do
			// not borrow the outer callable identity for owner matching.
			markGoFieldListDefinitions(node.Type.TypeParams, goDefinitionGenericParam, "", contexts)
			markGoFieldListDefinitions(node.Type.Params, goDefinitionParam, "", contexts)
			markGoFieldListDefinitions(node.Type.Results, goDefinitionResult, "", contexts)
			markGoLocalDefinitions(node.Body, "", contexts)
			return false
		case *ast.AssignStmt:
			if node.Tok == token.DEFINE {
				for _, expr := range node.Lhs {
					if ident, ok := expr.(*ast.Ident); ok {
						contexts[ident] = goDefinitionContext{role: goDefinitionLocal, owner: owner}
					}
				}
			}
		case *ast.RangeStmt:
			if node.Tok == token.DEFINE {
				for _, expr := range []ast.Expr{node.Key, node.Value} {
					if ident, ok := expr.(*ast.Ident); ok {
						contexts[ident] = goDefinitionContext{role: goDefinitionLocal, owner: owner}
					}
				}
			}
		case *ast.DeclStmt:
			if decl, ok := node.Decl.(*ast.GenDecl); ok {
				markGoGenDeclDefinitions(decl, false, owner, contexts)
			}
			return false
		}
		return true
	})
}

func goASTReceiverTypeName(list *ast.FieldList) string {
	if list == nil || len(list.List) == 0 || list.List[0] == nil {
		return ""
	}
	return goASTTypeName(list.List[0].Type)
}

func goASTTypeName(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr.Name
	case *ast.StarExpr:
		return goASTTypeName(expr.X)
	case *ast.ParenExpr:
		return goASTTypeName(expr.X)
	case *ast.IndexExpr:
		return goASTTypeName(expr.X)
	case *ast.IndexListExpr:
		return goASTTypeName(expr.X)
	case *ast.SelectorExpr:
		return expr.Sel.Name
	default:
		return ""
	}
}

type goDefinitionCandidate struct {
	node      *graph.Node
	kindRank  int
	ownerRank int
	rangeRank int
	distance  int
	span      int
	owner     string
	stableKey string
}

// matchRepoDefinitionNode matches one compiler definition to a graph node only
// after its source identity is compatible. Name and object kind are mandatory;
// receiver/owner identity is enforced whenever both sides expose it. Location
// ranks compatible candidates but can never turn a parameter/local into a
// function target. Ambiguous semantic identities fail closed.
func matchRepoDefinitionNode(nodes []*graph.Node, position token.Position, identifier string, obj types.Object, context goDefinitionContext) *graph.Node {
	if obj == nil || identifier == "" || position.Line <= 0 {
		return nil
	}
	kindRanks := goDefinitionKindRanks(obj, context)
	if len(kindRanks) == 0 {
		return nil
	}
	expectedOwner := goDefinitionExpectedOwner(obj, context)

	candidates := make([]goDefinitionCandidate, 0, 2)
	for _, node := range nodes {
		if node == nil || node.Kind == graph.KindFile || node.Kind == graph.KindImport || node.Name != identifier {
			continue
		}
		kindRank, compatible := kindRanks[node.Kind]
		if !compatible {
			continue
		}

		owner := goDefinitionNodeOwner(node)
		ownerRank := 0
		if expectedOwner != "" {
			switch {
			case owner == expectedOwner:
			case owner == "":
				ownerRank = 1
			default:
				continue
			}
		}

		endLine := node.EndLine
		if endLine < node.StartLine {
			endLine = node.StartLine
		}
		rangeRank := 0
		distance := 0
		if node.StartLine <= position.Line && position.Line <= endLine {
			// Containing declarations rank before the constrained nearest
			// fallback, with the innermost source span winning.
		} else {
			rangeRank = 1
			distance = goDefinitionAbs(node.StartLine - position.Line)
			if distance > 2 {
				continue
			}
		}

		candidates = append(candidates, goDefinitionCandidate{
			node:      node,
			kindRank:  kindRank,
			ownerRank: ownerRank,
			rangeRank: rangeRank,
			distance:  distance,
			span:      endLine - node.StartLine,
			owner:     owner,
			stableKey: goDefinitionStableNodeKey(node),
		})
	}
	if len(candidates) == 0 {
		return nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		for _, pair := range [][2]int{
			{left.kindRank, right.kindRank},
			{left.ownerRank, right.ownerRank},
			{left.rangeRank, right.rangeRank},
			{left.distance, right.distance},
			{left.span, right.span},
		} {
			if pair[0] != pair[1] {
				return pair[0] < pair[1]
			}
		}
		return left.stableKey < right.stableKey
	})

	// Stable ID ordering resolves duplicate projections of the same semantic
	// declaration. Equal-ranked candidates with different kinds or owners are
	// genuinely ambiguous and must not be guessed.
	if len(candidates) > 1 && goDefinitionSameRank(candidates[0], candidates[1]) {
		first, second := candidates[0], candidates[1]
		if first.node.Kind != second.node.Kind || first.owner != second.owner {
			return nil
		}
	}
	return candidates[0].node
}

func goDefinitionKindRanks(obj types.Object, context goDefinitionContext) map[graph.NodeKind]int {
	ranks := make(map[graph.NodeKind]int, 2)
	switch obj := obj.(type) {
	case *types.Func:
		isMethod := context.role == goDefinitionMethod
		if signature, ok := obj.Type().(*types.Signature); ok && signature.Recv() != nil {
			isMethod = true
		}
		if isMethod {
			ranks[graph.KindMethod] = 0
		} else {
			ranks[graph.KindFunction] = 0
		}
	case *types.Const:
		ranks[graph.KindConstant] = 0
	case *types.TypeName:
		if context.role == goDefinitionGenericParam {
			ranks[graph.KindGenericParam] = 0
			break
		}
		isInterface := context.role == goDefinitionInterface
		if obj.Type() != nil {
			if _, ok := obj.Type().Underlying().(*types.Interface); ok {
				isInterface = true
			}
		}
		if isInterface {
			ranks[graph.KindInterface] = 0
			// Type aliases and older projections may retain the general type
			// kind. It is compatible, but never preferred over an interface.
			ranks[graph.KindType] = 1
		} else {
			ranks[graph.KindType] = 0
		}
	case *types.Var:
		switch context.role {
		case goDefinitionField:
			ranks[graph.KindField] = 0
		case goDefinitionParam:
			ranks[graph.KindParam] = 0
		case goDefinitionLocal:
			ranks[graph.KindLocal] = 0
		case goDefinitionPackageVar:
			ranks[graph.KindVariable] = 0
		case goDefinitionReceiver, goDefinitionResult:
			// Receivers and named results currently have no dedicated graph
			// definition nodes. Mapping either to a nearby param/local would
			// fabricate identity, so leave them unmapped.
			return ranks
		default:
			switch {
			case obj.IsField():
				ranks[graph.KindField] = 0
			case obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope():
				ranks[graph.KindVariable] = 0
			default:
				// Without AST role information a non-package variable might be
				// either a parameter or a local. Permit a unique exact candidate,
				// but the ambiguity guard rejects equal-ranked cross-kind matches.
				ranks[graph.KindParam] = 0
				ranks[graph.KindLocal] = 0
			}
		}
	}
	return ranks
}

func goDefinitionExpectedOwner(obj types.Object, context goDefinitionContext) string {
	switch obj := obj.(type) {
	case *types.Func:
		if signature, ok := obj.Type().(*types.Signature); ok && signature.Recv() != nil {
			if receiver := goTypesReceiverName(signature.Recv().Type()); receiver != "" {
				return receiver
			}
		}
		if context.role == goDefinitionMethod {
			return context.owner
		}
	case *types.Var:
		switch context.role {
		case goDefinitionField, goDefinitionParam, goDefinitionLocal:
			return context.owner
		}
	case *types.TypeName:
		if context.role == goDefinitionGenericParam {
			return context.owner
		}
	}
	return ""
}

func goTypesReceiverName(typ types.Type) string {
	switch typ := typ.(type) {
	case *types.Pointer:
		return goTypesReceiverName(typ.Elem())
	case *types.Named:
		if typ.Obj() != nil {
			return typ.Obj().Name()
		}
	}
	return ""
}

func goDefinitionNodeOwner(node *graph.Node) string {
	if node == nil {
		return ""
	}
	switch node.Kind {
	case graph.KindMethod, graph.KindField:
		_, owner := graph.EnclosingFromID(node.ID, node.Kind)
		return owner
	case graph.KindParam:
		return goDefinitionOwnerBeforeMarker(node.ID, "#param:")
	case graph.KindLocal:
		return goDefinitionOwnerBeforeMarker(node.ID, "#local:")
	case graph.KindGenericParam:
		return goDefinitionOwnerBeforeMarker(node.ID, "#tparam:")
	default:
		return ""
	}
}

func goDefinitionOwnerBeforeMarker(id, marker string) string {
	index := strings.LastIndex(id, marker)
	if index < 0 {
		return ""
	}
	owner := id[:index]
	if scope := strings.LastIndex(owner, "::"); scope >= 0 {
		owner = owner[scope+2:]
	}
	return owner
}

func goDefinitionStableNodeKey(node *graph.Node) string {
	if node.ID != "" {
		return node.ID
	}
	return strings.Join([]string{
		string(node.Kind),
		node.QualName,
		node.Name,
		strconv.Itoa(node.StartLine),
		strconv.Itoa(node.EndLine),
		strconv.Itoa(node.StartColumn),
		strconv.Itoa(node.EndColumn),
	}, "\x00")
}

func goDefinitionSameRank(left, right goDefinitionCandidate) bool {
	return left.kindRank == right.kindRank &&
		left.ownerRank == right.ownerRank &&
		left.rangeRank == right.rangeRank &&
		left.distance == right.distance &&
		left.span == right.span
}

func goDefinitionAbs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
