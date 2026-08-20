// Temporal Go SDK call attribution.
//
// Workflows orchestrate activities through a thin set of dispatch
// helpers exposed by `go.temporal.io/sdk/workflow`:
//
//	workflow.ExecuteActivity(ctx, ActivityFn, args...)
//	workflow.ExecuteLocalActivity(ctx, ActivityFn, args...)
//	workflow.ExecuteChildWorkflow(ctx, WorkflowFn, args...)
//
// and activities / workflows enter the runtime via
// `go.temporal.io/sdk/worker`:
//
//	w.RegisterActivity(MyActivity)
//	w.RegisterActivityWithOptions(MyActivity, activity.RegisterOptions{Name: "..."})
//	w.RegisterWorkflow(MyWorkflow)
//	w.RegisterWorkflowWithOptions(MyWorkflow, workflow.RegisterOptions{Name: "..."})
//
// Tree-sitter sees `workflow.ExecuteActivity(...)` as a selector_expression
// call whose receiver text is "workflow" and method is the helper name;
// `w.RegisterActivity(...)` as a selector call whose method is the
// register helper. Neither shape resolves to anything useful through
// the normal Go call-resolution path (the target lives in an external
// SDK module). The helpers below recognise the call shapes and stamp
// dedicated `via=temporal.stub` / `via=temporal.register` placeholders
// that the resolver's ResolveTemporalCalls pass turns into edges from
// the workflow to the activity (or from one workflow to the child
// workflow) it dispatches.

package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// goWorkflowPkgPath is the canonical import path of the Temporal Go SDK
// workflow package whose helpers (ExecuteActivity, SetQueryHandler,
// SignalExternalWorkflow, …) the detectors gate on.
const goWorkflowPkgPath = "go.temporal.io/sdk/workflow"

// goWorkflowReceiverAlias returns the local name the workflow package is
// imported under in this file — the explicit alias for
// `import wf "go.temporal.io/sdk/workflow"`, or "workflow" for a plain
// import. Returns "" when the file does not import the workflow package.
// The detectors canonicalise a matching receiver to "workflow" so an
// aliased import (`wf.ExecuteActivity(...)`) is still recognised.
func goWorkflowReceiverAlias(root *sitter.Node, src []byte) string {
	if root == nil {
		return ""
	}
	var found string
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil || found != "" {
			return
		}
		if n.Type() == "import_spec" {
			pathNode := n.ChildByFieldName("path")
			if pathNode != nil {
				p := pathNode.Content(src)
				if len(p) >= 2 {
					p = p[1 : len(p)-1] // strip the surrounding quotes
				}
				if p == goWorkflowPkgPath {
					if nameNode := n.ChildByFieldName("name"); nameNode != nil {
						found = nameNode.Content(src)
					} else if i := strings.LastIndex(goWorkflowPkgPath, "/"); i >= 0 {
						found = goWorkflowPkgPath[i+1:]
					}
					return
				}
			}
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			walk(n.NamedChild(i))
			if found != "" {
				return
			}
		}
	}
	walk(root)
	return found
}

// goCanonicalWorkflowReceiver maps a call receiver to "workflow" when it
// matches the file's workflow-package alias, so the receiver-gated
// detectors recognise an aliased import. Other receivers pass through
// unchanged. wfAlias == "" (package not imported) is a no-op.
func goCanonicalWorkflowReceiver(receiver, wfAlias string) string {
	if wfAlias != "" && receiver == wfAlias {
		return "workflow"
	}
	return receiver
}

// goTemporalDispatchKind reports whether (receiver, method) names one
// of the Temporal workflow dispatch helpers and, if so, returns the
// canonical kind ("activity" or "workflow") plus whether the call is
// the `LocalActivity` variant. Returns ok=false for everything else.
//
// We require the receiver text to be exactly "workflow" — the
// canonical SDK alias. Users who alias the import (e.g.
// `import wf "go.temporal.io/sdk/workflow"`) won't be detected, which
// matches how the existing gRPC stub detector handles SDK aliasing
// (the canonical alias dominates >99% of real-world code).
func goTemporalDispatchKind(receiver, method string) (kind string, local bool, ok bool) {
	if receiver != "workflow" {
		return "", false, false
	}
	switch method {
	case "ExecuteActivity":
		return "activity", false, true
	case "ExecuteLocalActivity":
		return "activity", true, true
	case "ExecuteChildWorkflow":
		return "workflow", false, true
	}
	return "", false, false
}

// goTemporalRegisterKind reports whether a method name is one of the
// Temporal worker registration helpers and, if so, returns the kind
// ("activity" or "workflow") being registered. The receiver isn't
// required — `RegisterActivity` is distinctive enough across the SDK
// surface that a name match has zero realistic false positives.
//
// `RegisterActivities` (plural — registers every exported method on
// a struct as an activity) is recognised too; the resolver pass will
// promote each method of the struct to a temporal activity.
func goTemporalRegisterKind(method string) (kind string, plural bool, ok bool) {
	switch method {
	case "RegisterActivity", "RegisterActivityWithOptions":
		return "activity", false, true
	case "RegisterWorkflow", "RegisterWorkflowWithOptions":
		return "workflow", false, true
	case "RegisterActivities":
		return "activity", true, true
	}
	return "", false, false
}

// goTemporalSignalQueryOutKind reports whether (receiver, method) names
// an OUTBOUND signal-send or query-call against an already-running
// workflow and, if so, returns the kind ("signal" / "query") plus the
// 1-based position of the signal/query-name argument.
//
//	workflow.SignalExternalWorkflow(ctx, wid, rid, "name", arg)  // wf -> wf
//	client.SignalWorkflow(ctx, wid, rid, "name", arg)           // svc -> wf
//	client.QueryWorkflow(ctx, wid, rid, "name", args...)        // svc -> wf
//
// SignalExternalWorkflow is gated on the canonical "workflow" receiver
// (it is a workflow-package function). SignalWorkflow / QueryWorkflow
// live on the client and are called on an arbitrary client variable, so
// — like the Register* helpers — they are matched by method name alone;
// the string-literal name gate below keeps that high-precision. There is
// deliberately no workflow.QueryWorkflow (querying is client-side) and no
// SignalExternalWorkflowAsync (SignalExternalWorkflow returns a Future).
func goTemporalSignalQueryOutKind(receiver, method string) (kind string, namePos int, ok bool) {
	switch method {
	case "SignalExternalWorkflow":
		if receiver == "workflow" {
			return "signal", 4, true
		}
	case "SignalWorkflow":
		return "signal", 4, true
	case "QueryWorkflow":
		return "query", 4, true
	}
	return "", 0, false
}

// goTemporalHandlerKind reports whether (receiver, method) names one of
// the Temporal in-workflow handler-declaration helpers and, if so,
// returns the canonical kind ("query" / "signal" / "update").
//
//	workflow.SetQueryHandler(ctx, "name", fn)
//	workflow.SetQueryHandlerWithOptions(ctx, "name", fn, opts)
//	workflow.GetSignalChannel(ctx, "name")
//	workflow.GetSignalChannelWithOptions(ctx, "name", opts)
//	workflow.SetUpdateHandler(ctx, "name", fn)
//	workflow.SetUpdateHandlerWithOptions(ctx, "name", fn, opts)
//
// These mirror the Java SDK's `@QueryMethod` / `@SignalMethod` /
// `@UpdateMethod` annotations: a workflow declares, from inside its
// body, the named query / signal / update channels it serves. As with
// the dispatch helpers we require the receiver text to be exactly the
// canonical "workflow" alias.
func goTemporalHandlerKind(receiver, method string) (kind string, ok bool) {
	if receiver != "workflow" {
		return "", false
	}
	switch method {
	case "SetQueryHandler", "SetQueryHandlerWithOptions":
		return "query", true
	case "GetSignalChannel", "GetSignalChannelWithOptions":
		return "signal", true
	case "SetUpdateHandler", "SetUpdateHandlerWithOptions":
		return "update", true
	}
	return "", false
}

// goTemporalHandlerName extracts the query / signal / update name from a
// handler-declaration call — the second positional argument (after the
// workflow.Context). Unlike dispatch names we accept ONLY a string
// literal: handler names are matched by string at runtime, so a
// non-literal (variable / selector) can't be pinned to a name here and
// is left undetected, keeping the detector high-precision. Returns ""
// when the second argument is missing or is not a string literal.
func goTemporalHandlerName(callNode *sitter.Node, src []byte) string {
	if callNode == nil || callNode.Type() != "call_expression" {
		return ""
	}
	args := callNode.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	count := 0
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		count++
		if count == 2 {
			switch c.Type() {
			case "interpreted_string_literal", "raw_string_literal":
				return goTemporalNameFromExpr(c, src)
			}
			return ""
		}
	}
	return ""
}

// goTemporalDispatchArg returns the second positional argument node of a
// dispatch call (`workflow.ExecuteActivity(ctx, X, args...)` → X), or
// nil. X is either a string literal ("MyActivity"), a bare identifier
// (MyActivity), or a selector expression (pkg.MyActivity, recv.Method);
// goTemporalNameFromExpr reduces it to the trailing identifier — the
// name the worker registers under (the bare function name unless
// `RegisterActivityWithOptions` overrides it). Returned as a node, not a
// reduced name, so the env-default refinement can inspect the argument's
// shape (a bare identifier is the only case it tries to resolve to a
// literal default). Returns nil when the call has fewer than two
// positional arguments.
func goTemporalDispatchArg(callNode *sitter.Node) *sitter.Node {
	if callNode == nil || callNode.Type() != "call_expression" {
		return nil
	}
	args := callNode.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	count := 0
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		count++
		if count == 2 {
			return c
		}
	}
	return nil
}

// goTemporalRegisterName extracts the registered function name from a
// `worker.RegisterActivity(F)` / `worker.RegisterWorkflow(F)` call —
// the first positional argument, which is the function reference.
// Same expression shapes as the dispatch-name argument.
func goTemporalRegisterName(callNode *sitter.Node, src []byte) string {
	if callNode == nil || callNode.Type() != "call_expression" {
		return ""
	}
	args := callNode.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		return goTemporalNameFromExpr(c, src)
	}
	return ""
}

// applyGoTemporalRegisterMeta stamps `via=temporal.register` plus
// `temporal_kind` (activity / workflow) and `temporal_name` (the
// function-reference identifier) onto an EdgeCalls edge derived from
// a Temporal worker-registration call. No-op when c.tempKind isn't
// the "register_*" form set by goTemporalRegisterKind.
//
// The resolver's ResolveTemporalCalls pass walks every edge carrying
// this meta to discover (name → registered function) pairs, then
// stamps `temporal_role` on the registered function nodes and uses
// the map to rewrite matching stub-call placeholders.
func applyGoTemporalRegisterMeta(edge *graph.Edge, c goDeferredCall) {
	if edge == nil || c.tempKind == "" || c.tempName == "" {
		return
	}
	var kind string
	switch c.tempKind {
	case "register_activity":
		kind = "activity"
	case "register_workflow":
		kind = "workflow"
	default:
		return
	}
	if edge.Meta == nil {
		edge.Meta = map[string]any{}
	}
	edge.Meta["via"] = "temporal.register"
	edge.Meta["temporal_kind"] = kind
	edge.Meta["temporal_name"] = c.tempName
	if c.tempRegisteredName != "" {
		edge.Meta["temporal_registered_name"] = c.tempRegisteredName
	}
	if c.tempRegisterPlural {
		edge.Meta["temporal_register_plural"] = true
	}
}

// goTemporalRegisterStructType returns the struct TYPE name from the first
// argument of a `w.RegisterActivities(&MyActivities{})` call — the struct
// whose exported methods are each registered as an activity. Handles the
// `&T{}` pointer and `T{}` value composite-literal forms and a qualified
// `pkg.T{}`. Returns "" when the argument is not a composite literal (e.g.
// a pre-built variable, which carries no static type here).
func goTemporalRegisterStructType(callNode *sitter.Node, src []byte) string {
	if callNode == nil || callNode.Type() != "call_expression" {
		return ""
	}
	args := callNode.ChildByFieldName("arguments")
	if args == nil || args.NamedChildCount() == 0 {
		return ""
	}
	arg := args.NamedChild(0)
	if arg == nil {
		return ""
	}
	if arg.Type() == "unary_expression" {
		if op := arg.ChildByFieldName("operand"); op != nil {
			arg = op
		}
	}
	if arg.Type() != "composite_literal" {
		return ""
	}
	typ := arg.ChildByFieldName("type")
	if typ == nil {
		return ""
	}
	switch typ.Type() {
	case "type_identifier", "identifier":
		return typ.Content(src)
	case "qualified_type":
		if name := typ.ChildByFieldName("name"); name != nil {
			return name.Content(src)
		}
	case "pointer_type":
		// `&T` already unwrapped above, but a `*T` element type can appear.
		if inner := typ.ChildByFieldName("type"); inner != nil {
			switch inner.Type() {
			case "type_identifier", "identifier":
				return inner.Content(src)
			case "qualified_type":
				if name := inner.ChildByFieldName("name"); name != nil {
					return name.Content(src)
				}
			}
		}
	}
	return ""
}

// goTemporalRegisterNameOverride extracts the `Name:` string-literal
// field from the RegisterOptions composite literal passed as the second
// argument of a `RegisterActivityWithOptions` / `RegisterWorkflowWithOptions`
// call — the canonical registered name that overrides the bare function
// name (the name an `ExecuteActivity(ctx, "<name>", …)` dispatch must
// match). Returns "" when there is no second composite-literal argument or
// no string-literal Name field.
//
//	w.RegisterActivityWithOptions(MyActivity,
//	    activity.RegisterOptions{Name: "ChargeCard"})
func goTemporalRegisterNameOverride(callNode *sitter.Node, src []byte) string {
	if callNode == nil || callNode.Type() != "call_expression" {
		return ""
	}
	args := callNode.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	// Second positional argument = the options struct.
	var opts *sitter.Node
	count := 0
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		count++
		if count == 2 {
			opts = c
			break
		}
	}
	if opts == nil {
		return ""
	}
	// Unwrap a `&RegisterOptions{...}` pointer literal.
	if opts.Type() == "unary_expression" {
		if op := opts.ChildByFieldName("operand"); op != nil {
			opts = op
		}
	}
	if opts.Type() != "composite_literal" {
		return ""
	}
	body := opts.ChildByFieldName("body")
	if body == nil {
		return ""
	}
	unwrap := func(n *sitter.Node) *sitter.Node {
		// A keyed-element key/value may be wrapped in a literal_element
		// node depending on the grammar revision; reduce to the inner node.
		if n != nil && n.Type() == "literal_element" && n.NamedChildCount() == 1 {
			return n.NamedChild(0)
		}
		return n
	}
	for i, _nc := 0, int(body.NamedChildCount()); i < _nc; i++ {
		kv := body.NamedChild(i)
		if kv == nil || kv.Type() != "keyed_element" || kv.NamedChildCount() < 2 {
			continue
		}
		key := unwrap(kv.NamedChild(0))
		val := unwrap(kv.NamedChild(1))
		if key == nil || val == nil || key.Content(src) != "Name" {
			continue
		}
		if lit, ok := goStringLiteralValue(val, src); ok {
			return lit
		}
	}
	return ""
}

// applyGoTemporalHandlerMeta stamps `via=temporal.handler` plus
// `temporal_kind` (query / signal / update) and `temporal_name` (the
// handler's string name) onto the EdgeCalls edge derived from a
// `workflow.SetQueryHandler` / `GetSignalChannel` / `SetUpdateHandler`
// call. No-op when c.tempHandlerKind / c.tempName are unset.
//
// The edge originates from the enclosing workflow function, so the
// graph records — per workflow — the named query / signal / update
// handlers it exposes, symmetric with the Java side's per-method
// `@QueryMethod` / `@SignalMethod` / `@UpdateMethod` annotation edges.
func applyGoTemporalHandlerMeta(edge *graph.Edge, c goDeferredCall) {
	if edge == nil || c.tempHandlerKind == "" || c.tempName == "" {
		return
	}
	if edge.Meta == nil {
		edge.Meta = map[string]any{}
	}
	edge.Meta["via"] = "temporal.handler"
	edge.Meta["temporal_kind"] = c.tempHandlerKind
	edge.Meta["temporal_name"] = c.tempName
}

// applyGoTemporalSignalQueryMeta stamps the outbound signal-send /
// query-call meta onto an EdgeCalls edge derived from
// `SignalExternalWorkflow` / `SignalWorkflow` / `QueryWorkflow`:
// `via=temporal.signal-send` or `temporal.query-call`, plus
// `temporal_kind` (signal / query) and `temporal_name` (the literal
// signal/query name). No-op when c.tempOutKind / c.tempName are unset.
//
// These are the consumer side of the signal/query namespaces; the
// provider side is the in-workflow handler (GetSignalChannel /
// SetQueryHandler), tagged via=temporal.handler.
func applyGoTemporalSignalQueryMeta(edge *graph.Edge, c goDeferredCall) {
	if edge == nil || c.tempOutKind == "" || c.tempName == "" {
		return
	}
	var via string
	switch c.tempOutKind {
	case "signal":
		via = "temporal.signal-send"
	case "query":
		via = "temporal.query-call"
	default:
		return
	}
	if edge.Meta == nil {
		edge.Meta = map[string]any{}
	}
	edge.Meta["via"] = via
	edge.Meta["temporal_kind"] = c.tempOutKind
	edge.Meta["temporal_name"] = c.tempName
}

// markGoTemporalWrapper stamps a dispatch-wrapper marker on the enclosing
// function node: a function that calls workflow.ExecuteActivity /
// ExecuteChildWorkflow with one of its own parameters as the dispatch
// name. temporal_wrapper_kind records the kind (activity / workflow) and
// temporal_wrapper_param the forwarded parameter name. The marker lets a
// future interprocedural pass propagate a caller's literal/const argument
// through the wrapper to the real handler; today it documents the wrapper
// so the unresolvable parameter-named stub is suppressed rather than
// emitted as noise.
func markGoTemporalWrapper(result *parser.ExtractionResult, callerID, kind, param string) {
	if result == nil || callerID == "" {
		return
	}
	for _, n := range result.Nodes {
		if n.ID == callerID {
			if n.Meta == nil {
				n.Meta = map[string]any{}
			}
			n.Meta["temporal_wrapper_kind"] = kind
			n.Meta["temporal_wrapper_param"] = param
			return
		}
	}
}

// goTemporalStartKind reports whether a method name is one of the
// service-side workflow-START helpers and, if so, returns the 1-based
// positional index of the workflow argument.
//
//	client.ExecuteWorkflow(ctx, opts, workflow, args...)               // workflow @ 3
//	client.SignalWithStartWorkflow(ctx, wfID, sig, arg, opts, workflow, args...) // workflow @ 6
//
// Both are client methods invoked on an arbitrary client variable, so —
// like SignalWorkflow / QueryWorkflow and the Register* helpers — they are
// matched by method name alone; ExecuteWorkflow / SignalWithStartWorkflow
// are distinctive enough across the SDK surface for that to be precise.
func goTemporalStartKind(method string) (wfPos int, ok bool) {
	switch method {
	case "ExecuteWorkflow":
		return 3, true
	case "SignalWithStartWorkflow":
		return 6, true
	}
	return 0, false
}

// goTemporalNthArgName reduces the n-th (1-based) positional argument of a
// call to the trailing identifier that names a workflow — handling a func
// reference (OrderWorkflow), a selector (pkg.OrderWorkflow), or a string
// type name ("OrderWorkflow"), via goTemporalNameFromExpr. Returns "" when
// the call has fewer than n positional arguments or the argument is not a
// reducible name. Unlike goTemporalNthStringLiteralArg this accepts a
// non-literal, because a workflow START usually passes the workflow
// function value, whose name is the registered type.
func goTemporalNthArgName(callNode *sitter.Node, n int, src []byte) string {
	if callNode == nil || callNode.Type() != "call_expression" {
		return ""
	}
	args := callNode.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	count := 0
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		count++
		if count == n {
			return goTemporalNameFromExpr(c, src)
		}
	}
	return ""
}

// applyGoTemporalStartMeta stamps `via=temporal.start` plus
// `temporal_kind=workflow` and `temporal_name` (the started workflow's
// name) onto the EdgeCalls edge derived from a client.ExecuteWorkflow /
// SignalWithStartWorkflow call. No-op when c.tempStartName is unset. The
// resolver rewrites this edge to the registered workflow node, so
// get_callers on a Go workflow surfaces the services that start it.
func applyGoTemporalStartMeta(edge *graph.Edge, c goDeferredCall) {
	if edge == nil || c.tempStartName == "" {
		return
	}
	if edge.Meta == nil {
		edge.Meta = map[string]any{}
	}
	edge.Meta["via"] = "temporal.start"
	edge.Meta["temporal_kind"] = "workflow"
	edge.Meta["temporal_name"] = c.tempStartName
}

// goTemporalNthStringLiteralArg returns the unquoted value of the n-th
// (1-based) positional argument of a call when that argument is a string
// literal, else "". Used to extract the signal/query name from an
// outbound send/call — names are matched by string at runtime, so only a
// literal can be pinned here (a variable / constant is left undetected,
// keeping the detector high-precision).
func goTemporalNthStringLiteralArg(callNode *sitter.Node, n int, src []byte) string {
	if callNode == nil || callNode.Type() != "call_expression" {
		return ""
	}
	args := callNode.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	count := 0
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		count++
		if count == n {
			switch c.Type() {
			case "interpreted_string_literal", "raw_string_literal":
				return goTemporalNameFromExpr(c, src)
			}
			return ""
		}
	}
	return ""
}

// goTemporalNameFromExpr reduces a single argument expression to the
// trailing identifier that names the activity / workflow. Handles
// string literals (`"MyActivity"` and the Go raw-string variant),
// bare identifiers (`MyActivity`), and selector expressions
// (`pkg.MyActivity`, `a.Method`). Returns "" for any other shape
// (function literals, ternary-style expressions, etc.) — keeps the
// detector high-precision rather than guessing.
func goTemporalNameFromExpr(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	switch node.Type() {
	case "interpreted_string_literal", "raw_string_literal":
		text := node.Content(src)
		if len(text) >= 2 && (text[0] == '"' || text[0] == '`') {
			return text[1 : len(text)-1]
		}
		return text
	case "identifier":
		return node.Content(src)
	case "selector_expression":
		if field := node.ChildByFieldName("field"); field != nil {
			return field.Content(src)
		}
	case "unary_expression":
		// `&MyActivity` (rare; mostly seen for struct-method registration)
		if op := node.ChildByFieldName("operand"); op != nil {
			return goTemporalNameFromExpr(op, src)
		}
	case "call_expression":
		// `GetX()` / `pkg.GetX()` — the dispatch name is the called func's
		// trailing identifier; the resolver derefs it to the func's return
		// literal via the const-deref map. Recurse into the `function`
		// child: a selector resolves to "GetX", a bare identifier to "GetX".
		if fn := node.ChildByFieldName("function"); fn != nil {
			return goTemporalNameFromExpr(fn, src)
		}
	}
	return ""
}

// goTemporalEnvDefaultName attempts to resolve a bare-identifier dispatch
// name to the string-literal default of an env-var-with-default
// assignment in the enclosing function. Returns the default and true for
// one of these shapes (anchored on a literal os.Getenv / os.LookupEnv
// read so the value is provably env-sourced):
//
//	name := cmp.Or(os.Getenv("KEY"), "Default")   // any call mixing an
//	                                              // os.Getenv read with a
//	                                              // string-literal arg
//	name := os.Getenv("KEY")
//	if name == "" { name = "Default" }            // (or `name, ok := os.LookupEnv(...)`
//	                                              //  followed by a literal assign)
//
// Intra-procedural and literal-only: only assignments lexically before
// the dispatch call are considered, and anything that isn't an
// os.Getenv-anchored literal default returns "", false. This is a
// deliberately narrow data-flow shortcut, not general constant
// propagation — see the speculative tier the resolver lands it at.
// The result is reported as exactly one of `litDef` (a string-literal default)
// or `constName` (a constant-reference default the resolver substitutes through
// constVal), plus a `source` tier marker: "os_getenv" / "allowlist" (trusted
// literal), "const_ref" (trusted helper with a constant default — same visible
// tier as allowlist), or "heuristic" (env-named-helper guess, hidden tier;
// stays heuristic even when its default is a constant, the helper itself being
// the weak link).
func goTemporalEnvDefaultName(callNode *sitter.Node, name string, src []byte, extra map[string]bool) (litDef string, constName string, source string, ok bool) {
	body := goEnclosingFuncBody(callNode)
	if body == nil {
		return "", "", "", false
	}
	limit := callNode.StartByte()

	// Collect every assignment to `name` lexically before the dispatch
	// call, in source order, WITHOUT descending into nested func_literal
	// bodies — a closure is a separate scope, and matching a shadowing
	// same-named variable declared there would be a false positive.
	var assigns []*sitter.Node
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "func_literal" {
			return // do not descend into nested closures
		}
		if (n.Type() == "short_var_declaration" || n.Type() == "assignment_statement") &&
			n.StartByte() < limit && goAssignHasTarget(n, name, src) {
			assigns = append(assigns, n)
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(body)

	// Replay the writes in order. The dispatch name is env-default-sourced
	// only if, after the LAST write before the call, the variable still
	// holds an env-or-default value: either a `cmp.Or(os.Getenv, "lit")`
	// assignment, an allow-listed / heuristic env-helper call
	// (`GetEnvOrDefault(KEY, "lit")` / `cfg.ActivityFromEnv(KEY, "lit")`),
	// or a string-literal assignment that followed an os.Getenv /
	// os.LookupEnv read (the `name := os.Getenv(...); if name == "" { name =
	// "lit" }` shape). Any other later write — a plain reassignment `name =
	// pick()` — clears the env-sourcing, and we leave the dispatch
	// unresolved rather than guess.
	//
	// resolvedSource records HOW the default was recognised so the resolver
	// can tier the edge: "os_getenv" / "allowlist" / "const_ref" land at the
	// inferred (visible) tier, "heuristic" stays at the hidden speculative
	// tier. The default is reported as exactly one of resLit (a string-literal
	// default) or resConst (a constant-reference default the resolver
	// substitutes through its const-deref index).
	resLit := ""
	resConst := ""
	resolvedSource := ""
	resolvedOK := false
	envReadSeen := false
	// commit records a resolved default, routing a constant-reference value
	// into resConst (and downgrading "const_ref" → "heuristic" when the helper
	// itself is only a heuristic match — the helper, not the constant, is the
	// weak link).
	commit := func(val string, isConst bool, trustedSource string) {
		if isConst {
			resLit, resConst = "", val
			if trustedSource == "heuristic" {
				resolvedSource = "heuristic"
			} else {
				resolvedSource = "const_ref"
			}
		} else {
			resLit, resConst, resolvedSource = val, "", trustedSource
		}
		resolvedOK, envReadSeen = true, false
	}
	clear := func() {
		resLit, resConst, resolvedSource, resolvedOK, envReadSeen = "", "", "", false, false
	}
	for _, a := range assigns {
		rhs := goAssignRHSExpr(a, name, src)
		switch {
		case rhs == nil:
			clear()
		case rhs.Type() == "call_expression" && goIsEnvRead(rhs, src):
			// `name := os.Getenv("K")` — default still pending.
			clear()
			envReadSeen = true
		case rhs.Type() == "call_expression":
			// `name := cmp.Or(os.Getenv("K"), "lit")` — self-contained.
			if v, isC, ok := goCallEnvDefaultLiteral(rhs, src); ok {
				commit(v, isC, "os_getenv")
			} else if v, isC, ok := goEnvHelperDefaultLiteral(rhs, src, extra); ok {
				// Allow-listed env-helper (`GetEnvOrDefault(KEY, "lit")`).
				commit(v, isC, "allowlist")
			} else if v, isC, ok := goEnvHelperHeuristicDefault(rhs, src); ok {
				// Generic "env"-named helper — lower-trust heuristic.
				commit(v, isC, "heuristic")
			} else {
				clear()
			}
		default:
			// `name = "lit"` / `name = CONST` — only a default when it
			// follows an env read.
			if v, isC, ok := goArgDefaultValue(rhs, src); ok && envReadSeen {
				commit(v, isC, "os_getenv")
			} else {
				clear()
			}
		}
	}
	return resLit, resConst, resolvedSource, resolvedOK
}

// goTemporalVarTrace traces a bare-identifier dispatch name to its
// intra-procedural assignment when that assignment is a plain literal /
// constant reference / const-returning func call. The env-var-with-default
// shape is handled by goTemporalEnvDefaultName and takes precedence; this is
// the general fallback for `name := <value>; workflow.ExecuteActivity(ctx,
// name, …)` — the "meta_vars" broken-dispatch category (activity /
// activityName / type). It returns the LAST assignment to `name` lexically
// before the dispatch, reduced to exactly one of:
//
//	litDef   : string-literal value       (`name := "ChargeActivity"`)
//	constName: a constant NAME            (`name := pkg.ChargeName` / `name := CHARGE`)
//	funcName : a const-returning func name (`name := GetChargeName()`)
//
// The resolver validates constName / funcName against its constVal index, so
// an identifier that is not actually a string constant simply fails to
// resolve and stays a broken_dispatch — no false-resolution risk. The
// last-assignment-wins rule is a best-effort static guess (a later
// conditional reassignment may not execute), so the resolver lands these at
// the inferred / convention tier, not the register-confirmed 0.9. Returns
// ok=false when there is no traceable assignment to `name`.
func goTemporalVarTrace(callNode *sitter.Node, name string, src []byte) (litDef, constName string, ok bool) {
	body := goEnclosingFuncBody(callNode)
	if body == nil {
		return "", "", false
	}
	limit := callNode.StartByte()
	var rhs *sitter.Node
	assigns := 0
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if (n.Type() == "short_var_declaration" || n.Type() == "assignment_statement") &&
			n.StartByte() < limit && goAssignHasTarget(n, name, src) {
			// Count every assignment to `name` (the single-assignment guard
			// below relies on the total); rhs is the index-matched value, which
			// may be nil for a multi-value call.
			assigns++
			rhs = goAssignRHSExpr(n, name, src)
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(body)
	// Trace ONLY when the variable has exactly ONE assignment before the
	// dispatch. Multiple assignments mean the live value is conditional /
	// reassigned (e.g. an env-default write later overwritten by a plain
	// call) — guessing the last one would be a false-resolution risk, so we
	// leave the dispatch a broken_dispatch rather than guess.
	if assigns != 1 || rhs == nil {
		return "", "", false
	}
	switch rhs.Type() {
	case "interpreted_string_literal", "raw_string_literal":
		if v, okk := goStringLiteralValue(rhs, src); okk && v != "" {
			return v, "", true
		}
	case "identifier":
		// Bare const reference (`name := CHARGE`). Admitted ONLY when the
		// referenced identifier is a package-level CONST declared in this
		// file — a function parameter or arbitrary local (`actName :=
		// picked`) is exactly the "don't guess at arbitrary variables" case
		// the plain-var path deliberately refuses. The const NAME is emitted
		// as temporal_name so the resolver's const-deref map substitutes the
		// literal. Guarded by length to skip throwaway names; skip
		// self-reference.
		if cn := rhs.Content(src); len(cn) > 2 && cn != name && goIdentIsFileConst(callNode, cn, src) {
			return "", cn, true
		}
	case "selector_expression":
		// Package / receiver const reference (`name := config.ChargeName`).
		if field := rhs.ChildByFieldName("field"); field != nil {
			if cn := field.Content(src); len(cn) > 2 {
				return "", cn, true
			}
		}
	case "call_expression":
		// Const-returning name getter (`name := GetChargeName()`). os.Getenv
		// is the env path's job, not this one. Require ZERO arguments: a call
		// WITH args (`wfutils.PickActivity("KEY", "default")`) is an
		// env/helper-style call whose default is the env path's responsibility
		// (and which the env path deliberately declines for unknown helpers) —
		// treating it as a const-return getter would mislabel it. The callee's
		// returned value is resolved in-file to a literal / const name, since
		// the resolver has no func-returning-name channel.
		if goIsEnvRead(rhs, src) {
			return "", "", false
		}
		if a := rhs.ChildByFieldName("arguments"); a != nil && a.NamedChildCount() > 0 {
			return "", "", false
		}
		if lit, cn, okk := goTemporalFuncReturnName(rhs, callNode, src); okk {
			return lit, cn, true
		}
	}
	return "", "", false
}

// goIdentIsFileConst reports whether `ident` is the name of a constant
// declared anywhere in the same source file as fromNode. Used to gate the
// variable-trace bare-identifier case so it admits a `name := chargeName`
// const reference but refuses a `actName := picked` parameter / local —
// honouring the "don't guess at arbitrary variables" invariant. The scan
// inspects every const_spec name under the file root.
func goIdentIsFileConst(fromNode *sitter.Node, ident string, src []byte) bool {
	root := fromNode
	for root.Parent() != nil {
		root = root.Parent()
	}
	found := false
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil || found {
			return
		}
		if n.Type() == "const_spec" {
			if nm := n.ChildByFieldName("name"); nm != nil && nm.Content(src) == ident {
				found = true
				return
			}
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
	return found
}

// goTemporalFuncReturnName resolves a no-argument func/method call used to
// supply a dispatch name (`name := GetChargeName()`) to the value the callee
// unconditionally returns — either a string literal (emitted as the dispatch
// name directly) or a bare const reference (emitted as a const name for the
// resolver to dereference). It locates the callee declaration by simple name
// within the SAME file as the call, then returns the single `return <expr>`
// it finds when that expr is a string literal or a one-hop identifier
// reference. Returns ok=false when the callee is not found in-file, is not a
// plain identifier call, has multiple/none returns, or returns something
// other than a literal / bare const — keeping resolution precision-safe.
func goTemporalFuncReturnName(call, fromNode *sitter.Node, src []byte) (litDef, constName string, ok bool) {
	fn := call.ChildByFieldName("function")
	if fn == nil || fn.Type() != "identifier" {
		return "", "", false
	}
	callee := fn.Content(src)
	if callee == "" {
		return "", "", false
	}
	root := fromNode
	for root.Parent() != nil {
		root = root.Parent()
	}
	var decl *sitter.Node
	var find func(n *sitter.Node)
	find = func(n *sitter.Node) {
		if n == nil || decl != nil {
			return
		}
		if n.Type() == "function_declaration" {
			if nm := n.ChildByFieldName("name"); nm != nil && nm.Content(src) == callee {
				decl = n
				return
			}
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			find(n.NamedChild(i))
		}
	}
	find(root)
	if decl == nil {
		return "", "", false
	}
	body := decl.ChildByFieldName("body")
	if body == nil {
		return "", "", false
	}
	var ret *sitter.Node
	count := 0
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "return_statement" {
			count++
			if n.NamedChildCount() == 1 {
				ret = n.NamedChild(0)
			}
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(body)
	if count != 1 || ret == nil {
		return "", "", false
	}
	// A return_statement's value is wrapped in an expression_list; unwrap a
	// single-expression list down to the lone expression.
	if ret.Type() == "expression_list" {
		if ret.NamedChildCount() != 1 {
			return "", "", false
		}
		ret = ret.NamedChild(0)
	}
	if ret == nil {
		return "", "", false
	}
	switch ret.Type() {
	case "interpreted_string_literal", "raw_string_literal":
		if v, okk := goStringLiteralValue(ret, src); okk && v != "" {
			return v, "", true
		}
	case "identifier":
		if cn := ret.Content(src); len(cn) > 2 {
			return "", cn, true
		}
	case "selector_expression":
		if field := ret.ChildByFieldName("field"); field != nil {
			if cn := field.Content(src); len(cn) > 2 {
				return "", cn, true
			}
		}
	}
	return "", "", false
}

// goEnclosingFuncBody walks up from n to the nearest function-like
// ancestor and returns its body block, or nil.
func goEnclosingFuncBody(n *sitter.Node) *sitter.Node {
	for cur := n; cur != nil; cur = cur.Parent() {
		switch cur.Type() {
		case "function_declaration", "method_declaration", "func_literal":
			return cur.ChildByFieldName("body")
		}
	}
	return nil
}

// goAssignHasTarget reports whether `name` appears among the left-hand
// targets of a short_var_declaration / assignment_statement.
func goAssignHasTarget(assign *sitter.Node, name string, src []byte) bool {
	left := assign.ChildByFieldName("left")
	if left == nil {
		return false
	}
	for i, _nc := 0, int(left.NamedChildCount()); i < _nc; i++ {
		c := left.NamedChild(i)
		if c != nil && c.Type() == "identifier" && c.Content(src) == name {
			return true
		}
	}
	return false
}

// goAssignRHSExpr returns the right-hand expression assigned to `name`,
// matching the RHS position to the matched LHS target position for a
// parallel assignment (`a, name := x, "v"` → "v"). A single RHS shared by
// multiple targets is a multi-value call (`a, b := f()`) with no per-target
// literal to trace, so it returns nil. Returns nil when `name` is not a
// target of the assignment.
func goAssignRHSExpr(assign *sitter.Node, name string, src []byte) *sitter.Node {
	left := assign.ChildByFieldName("left")
	right := assign.ChildByFieldName("right")
	if left == nil || right == nil || right.NamedChildCount() == 0 {
		return nil
	}
	idx := -1
	for i, _nc := 0, int(left.NamedChildCount()); i < _nc; i++ {
		c := left.NamedChild(i)
		if c != nil && c.Type() == "identifier" && c.Content(src) == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	// Parallel assignment: take the RHS at the matched target position.
	if left.NamedChildCount() == right.NamedChildCount() {
		return right.NamedChild(idx)
	}
	// Single target, single value.
	if left.NamedChildCount() == 1 && right.NamedChildCount() == 1 {
		return right.NamedChild(0)
	}
	// Multi-value call (`a, b := f()`): no per-target literal to trace.
	return nil
}

// goIsEnvRead reports whether a call_expression is `os.Getenv(...)` or
// `os.LookupEnv(...)`.
func goIsEnvRead(call *sitter.Node, src []byte) bool {
	fn := call.ChildByFieldName("function")
	if fn == nil || fn.Type() != "selector_expression" {
		return false
	}
	op := fn.ChildByFieldName("operand")
	field := fn.ChildByFieldName("field")
	if op == nil || field == nil || op.Content(src) != "os" {
		return false
	}
	switch field.Content(src) {
	case "Getenv", "LookupEnv":
		return true
	}
	return false
}

// goArgDefaultValue reduces an env-or-default helper's DEFAULT argument to its
// dispatch-name value. A string literal (`"ChargeActivity"`) yields the literal
// with isConst=false. A constant REFERENCE — a bare identifier
// (`ACTIVITY_NAME_DEFAULT`) or a selector_expression
// (`config.ACTIVITY_NAME_DEFAULT`) — yields the constant's NAME with
// isConst=true; the resolver later substitutes the constant's literal value
// through its const-deref index (the constant body usually lives in another
// package, invisible at extract time). Identifiers are admitted optimistically
// because the resolver validates them against the const-deref index — an
// identifier that is not actually a string constant simply fails to resolve and
// stays a broken_dispatch, so there is no false-resolution risk. Returns
// ("", false, false) for any other shape.
func goArgDefaultValue(node *sitter.Node, src []byte) (val string, isConst bool, ok bool) {
	if node == nil {
		return "", false, false
	}
	if lit, okk := goStringLiteralValue(node, src); okk {
		return lit, false, true
	}
	switch node.Type() {
	case "identifier":
		return node.Content(src), true, true
	case "selector_expression":
		if field := node.ChildByFieldName("field"); field != nil {
			return field.Content(src), true, true
		}
	}
	return "", false, false
}

// goCallEnvDefaultLiteral inspects a `cmp.Or(os.Getenv("KEY"), "Default")`
// call and returns its literal default. cmp.Or returns the FIRST non-zero
// argument, so when the env read yields "" at runtime the value is the
// first string-literal argument that follows — hence we return the FIRST
// literal, not the last. Gated on the cmp.Or callee: an arbitrary user
// function mixing an env read with a literal (`combine(os.Getenv("K"),
// "Suffix")`) is deliberately NOT treated as env-or-default — only the
// stdlib cmp.Or idiom qualifies, since cmp.Or is the one combinator whose
// "first non-zero" semantics make the literal a provable default. Returns
// ("", false, false) when the callee is not cmp.Or, no os.Getenv /
// os.LookupEnv read is present, or there is no default argument. The default
// may be a string literal (isConst=false) or a constant reference
// (isConst=true) the resolver substitutes through its const-deref index.
func goCallEnvDefaultLiteral(call *sitter.Node, src []byte) (val string, isConst bool, ok bool) {
	if !goIsCmpOr(call, src) {
		return "", false, false
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return "", false, false
	}
	hasEnvRead := false
	firstVal := ""
	firstConst := false
	haveDefault := false
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() == "call_expression" && goIsEnvRead(c, src) {
			hasEnvRead = true
			continue
		}
		if v, isC, okk := goArgDefaultValue(c, src); okk && !haveDefault {
			firstVal, firstConst, haveDefault = v, isC, true
		}
	}
	if hasEnvRead && haveDefault {
		return firstVal, firstConst, true
	}
	return "", false, false
}

// goIsCmpOr reports whether a call_expression is a call to the stdlib
// `cmp.Or` — the canonical "first non-zero" combinator used for the
// env-or-default idiom. Matched by the canonical `cmp` package alias
// (consistent with the os.Getenv / "workflow" receiver gates elsewhere).
func goIsCmpOr(call *sitter.Node, src []byte) bool {
	fn := call.ChildByFieldName("function")
	if fn == nil || fn.Type() != "selector_expression" {
		return false
	}
	op := fn.ChildByFieldName("operand")
	field := fn.ChildByFieldName("field")
	return op != nil && field != nil &&
		op.Content(src) == "cmp" && field.Content(src) == "Or"
}

// goEnvHelperNames is the built-in allow-list of project-local env-or-default
// helper function names whose 2nd argument is the literal default. Matched
// case-insensitively; a corporate fork extends it at runtime via the per-repo
// allow-list threaded through `extra` (see goEnvHelperDefaultLiteral).
var goEnvHelperNames = []string{
	"GetEnvOrDefault",
	"GetEnvOrDefaultValue",
	"EnvOr",
	"GetenvDefault",
	"GetEnvDefault",
}

// goEnvHelperDefaultLiteral recognises a call to a project-local
// env-or-default helper by name — `wfutils.GetEnvOrDefault(KEY, "Default")`
// or the bare `EnvOr(KEY, "Default")` — and returns the string-literal 2nd
// argument as the default. The callee name is taken from a bare identifier
// or, for a selector_expression, its trailing `field`; it is compared
// case-insensitively (strings.EqualFold) against the built-in goEnvHelperNames
// PLUS `extra` — the per-repo corporate allow-list (lower-cased keys) loaded
// from the git-ignored `.gortex/temporal-allowlist.yaml`. A match here is
// "allowlist"-sourced, so the resolver lands the edge at the inferred (visible)
// tier — that is how a corporate agent PROMOTES its own helper above the
// generic "env"-name heuristic. Returns ("", false) for any non-matching name
// or a non-string-literal 2nd arg.
func goEnvHelperDefaultLiteral(call *sitter.Node, src []byte, extra map[string]bool) (val string, isConst bool, ok bool) {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return "", false, false
	}
	var callee string
	switch fn.Type() {
	case "identifier":
		callee = fn.Content(src)
	case "selector_expression":
		if field := fn.ChildByFieldName("field"); field != nil {
			callee = field.Content(src)
		}
	}
	if callee == "" {
		return "", false, false
	}
	matched := false
	for _, name := range goEnvHelperNames {
		if strings.EqualFold(callee, name) {
			matched = true
			break
		}
	}
	if !matched && extra[strings.ToLower(callee)] {
		matched = true
	}
	if !matched {
		return "", false, false
	}
	args := call.ChildByFieldName("arguments")
	if args == nil || args.NamedChildCount() < 2 {
		return "", false, false
	}
	return goArgDefaultValue(args.NamedChild(1), src)
}

// goEnvHelperHeuristicDefault is the generic-recall fallback for env-or-default
// helpers whose NAME is not in the allow-list. It fires on a structural anchor
// — the callee's (bare or selector-trailing) name contains "env"
// (case-insensitive), the near-universal marker of an env-reading helper
// (`cfg.ActivityFromEnv("KEY", "Default")`, `getEnvActivity(...)`) — and takes
// the 2nd argument's string literal as the default. Deliberately lower-trust
// than the allow-list path: the caller tags the resulting edge
// `temporal_env_source=heuristic` so the resolver keeps it at the hidden
// speculative tier (where the LLM cleaning pass verifies or prunes it), rather
// than asserting it as a real dispatch. Returns ("", false) when the name lacks
// the "env" marker or the 2nd argument is not a string literal — so a plain
// `pick(KEY, "X")` (no env marker) is left untouched, preserving precision.
func goEnvHelperHeuristicDefault(call *sitter.Node, src []byte) (val string, isConst bool, ok bool) {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return "", false, false
	}
	var callee string
	switch fn.Type() {
	case "identifier":
		callee = fn.Content(src)
	case "selector_expression":
		if field := fn.ChildByFieldName("field"); field != nil {
			callee = field.Content(src)
		}
	}
	if callee == "" || !strings.Contains(strings.ToLower(callee), "env") {
		return "", false, false
	}
	args := call.ChildByFieldName("arguments")
	if args == nil || args.NamedChildCount() < 2 {
		return "", false, false
	}
	return goArgDefaultValue(args.NamedChild(1), src)
}

// goStringLiteralValue returns the unquoted value of a Go string literal
// node, or ("", false) for any other node type.
func goStringLiteralValue(n *sitter.Node, src []byte) (string, bool) {
	if n == nil {
		return "", false
	}
	switch n.Type() {
	case "interpreted_string_literal", "raw_string_literal":
		return goTemporalNameFromExpr(n, src), true
	}
	return "", false
}

// goTemporalCallArgNames extracts positional arg names from a call expression.
//
// PURPOSE — extract positional arg names from a call expression for wrapper-following
// RATIONALE — qualifying args are string literals, selectors, Capitalized
//
//	identifiers, OR a bare lowercase identifier that is one of the ENCLOSING
//	function's own parameters (a name forwarded THROUGH this call) — the latter
//	is what lets depth>1 wrapper-following propagate a name across multiple hops.
//	`callerParams` is the enclosing function's parameter-name set (may be nil).
//
// KEYWORDS — arg_names, wrapper-following, call_expression, forwarded-param
func goTemporalCallArgNames(callNode *sitter.Node, src []byte, callerParams map[string]bool) ([]string, bool) {
	if callNode == nil || callNode.Type() != "call_expression" {
		return nil, false
	}
	args := callNode.ChildByFieldName("arguments")
	if args == nil {
		return nil, false
	}
	const maxArgs = 8
	var out []string
	qualifying := false
	count := 0
	for i := 0; i < int(args.NamedChildCount()) && count < maxArgs; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		count++
		name := ""
		switch c.Type() {
		case "interpreted_string_literal", "raw_string_literal":
			name = goTemporalNameFromExpr(c, src)
			qualifying = true
		case "selector_expression":
			name = goTemporalNameFromExpr(c, src)
			qualifying = true
		case "identifier":
			name = c.Content(src)
			if name != "" && name[0] >= 'A' && name[0] <= 'Z' {
				qualifying = true
			} else if callerParams[name] {
				// A bare lowercase identifier that is the enclosing function's
				// own parameter: this call forwards a caller-supplied name
				// THROUGH (`runStep(ctx, name, …)` inside a wrapper). Recording
				// it lets the wrapper-following resolver discover the caller as
				// a transitive wrapper and propagate the name up another level.
				qualifying = true
			}
		}
		out = append(out, name)
	}
	if !qualifying {
		return nil, false
	}
	return out, true
}

// attachGoTemporalCallArgNames attaches arg_names + callee meta to a call edge.
//
// PURPOSE — attach arg_names and callee meta to a call edge for wrapper-following
// RATIONALE — the resolver's wrapper pass needs both the arg values and the callee name
//
//	to match caller edges to wrapper definitions
//
// KEYWORDS — arg_names, callee, wrapper-following, edge meta
func attachGoTemporalCallArgNames(edge *graph.Edge, c goDeferredCall, callNode *sitter.Node, src []byte, callerParams map[string]bool) {
	names, ok := goTemporalCallArgNames(callNode, src, callerParams)
	if !ok {
		return
	}
	if edge.Meta == nil {
		edge.Meta = map[string]any{}
	}
	edge.Meta["arg_names"] = names
	// callee: the function/method name being called
	if c.isSelector {
		edge.Meta["callee"] = c.method
	} else if c.callName != "" {
		edge.Meta["callee"] = c.callName
	}
}

// goCompositeLiteralType walks up from a keyed_element node to find the
// enclosing composite_literal and returns the simple type name.
//
// PURPOSE — extracts the receiver struct type from a struct literal so the
// executor-field pass can key the field assignment by (type, field).
// RATIONALE — tree-sitter does not expose a direct parent-of-kind API;
// walking the Parent chain is the standard idiom in this codebase.
// KEYWORDS — composite-literal, type-name, executor-field
func goCompositeLiteralType(keyed *sitter.Node, src []byte) string {
	for n := keyed; n != nil; n = n.Parent() {
		if n.Type() != "composite_literal" {
			continue
		}
		t := n.ChildByFieldName("type")
		if t == nil {
			return ""
		}
		switch t.Type() {
		case "type_identifier":
			return t.Content(src)
		case "pointer_type":
			if inner := t.NamedChild(0); inner != nil && inner.Type() == "type_identifier" {
				return inner.Content(src)
			}
		case "qualified_type":
			if f := t.ChildByFieldName("name"); f != nil {
				return f.Content(src)
			}
		}
		return ""
	}
	return ""
}
