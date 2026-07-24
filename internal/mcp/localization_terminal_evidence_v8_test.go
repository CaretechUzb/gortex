package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/localizationauth"
)

func localizationV8Node(id, name, file string) *graph.Node {
	return &graph.Node{
		ID:        id,
		Name:      name,
		QualName:  name,
		Kind:      graph.KindMethod,
		FilePath:  file,
		StartLine: 11,
		EndLine:   19,
		Meta:      map[string]any{"signature": "func " + name + "()"},
	}
}

func TestLocalizationDirectRelationsInterleaveDeterministicallyWithinExistingCap(t *testing.T) {
	targets := make([]exploreTarget, 0, localizationReplayEvidenceLimit)
	for index := 0; index < localizationReplayEvidenceLimit; index++ {
		name := "DirectTarget" + string(rune('A'+index))
		node := localizationV8Node("repo/direct_"+string(rune('a'+index))+".go::"+name, name, "repo/direct_"+string(rune('a'+index))+".go")
		targets = append(targets, exploreTarget{node: node})
	}
	callee := localizationV8Node("repo/policy.go::PolicyEngine.ApplyRule", "ApplyRule", "repo/policy.go")
	unrelatedCaller := localizationV8Node("repo/bootstrap.go::Bootstrap.Start", "Start", "repo/bootstrap.go")
	targets[0].callees = []*graph.Node{callee}
	targets[0].callers = []*graph.Node{unrelatedCaller}
	unrelatedCallee := localizationV8Node("repo/alloc.go::Allocator.Grow", "Grow", "repo/alloc.go")
	caller := localizationV8Node("repo/defaults.go::Registry.BuildDefaultRegistry", "BuildDefaultRegistry", "repo/defaults.go")
	targets[1].callees = []*graph.Node{unrelatedCallee}
	targets[1].callers = []*graph.Node{caller}

	task := "trace ApplyRule and BuildDefaultRegistry behavior"
	got := interleaveLocalizationDirectRelations(task, "", targets)
	again := interleaveLocalizationDirectRelations(task, "", targets)
	if !reflect.DeepEqual(got, again) {
		t.Fatal("relationship interleaving is not deterministic")
	}
	if len(got) != len(targets) || len(got) != localizationReplayEvidenceLimit {
		t.Fatalf("interleaved rows = %d, want existing cap %d", len(got), len(targets))
	}
	wantHead := []string{targets[0].node.ID, callee.ID, targets[1].node.ID, caller.ID, targets[2].node.ID}
	for index, want := range wantHead {
		if got[index].node.ID != want {
			t.Fatalf("position %d = %q, want %q", index+1, got[index].node.ID, want)
		}
	}
	if got[1].localizationRelation != "direct_callee" || got[3].localizationRelation != "direct_caller" {
		t.Fatalf("relationship provenance = %q/%q", got[1].localizationRelation, got[3].localizationRelation)
	}

	completion := newLocalizationCompletion(true, "")
	result, _, digest, packed := buildLocalizationExploreResultForTaskFinalized(completion, task, targets, exploreMaxBudgetTokens)
	if result == nil || result.IsError || digest == nil || packed.State == localizationStateInactive {
		t.Fatalf("packed relationship result = (%#v, %#v, %#v)", result, digest, packed)
	}
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("packed result content = %#v", result.Content)
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode packed relationship envelope: %v", err)
	}
	if len(envelope.Evidence) > localizationReplayEvidenceLimit || len(envelope.Files) != len(envelope.Evidence) || len(envelope.Symbols) != len(envelope.Evidence) {
		t.Fatalf("unaligned packed rows: files=%d symbols=%d evidence=%d", len(envelope.Files), len(envelope.Symbols), len(envelope.Evidence))
	}
	relationRows := 0
	for index, row := range envelope.Evidence {
		if row.Rank != index+1 || envelope.Files[index] != row.File || envelope.Symbols[index] != row.ID {
			t.Fatalf("unaligned row %d: %#v", index+1, row)
		}
		if row.Provenance == "direct_caller" || row.Provenance == "direct_callee" {
			relationRows++
		}
	}
	if relationRows == 0 {
		t.Fatal("packed envelope lost every direct relationship row")
	}
	encoded, err := json.Marshal(digest)
	if err != nil || len(encoded) > localizationDigestMaxBytes {
		t.Fatalf("digest size = %d, err=%v", len(encoded), err)
	}
	ready := newLocalizationCompletion(true, "")
	ready = localizationCompletionWithDigest(ready, digest)
	firstReplay, _ := json.Marshal(localizationAnswerReadyResult(ready))
	secondReplay, _ := json.Marshal(localizationAnswerReadyResult(ready))
	if !reflect.DeepEqual(firstReplay, secondReplay) {
		t.Fatal("relationship replay is not byte-identical")
	}
	if !strings.HasSuffix(ready.FinalResponse, localizationAnswerReadyDirective) {
		t.Fatalf("directive is not inside final_response: %q", ready.FinalResponse)
	}
}

func TestRecoveryAllowsInferredConcreteIdentifierOnlyForScopedSymbolEvidence(t *testing.T) {
	retained := mergeLocalizationEvidenceDigest([]localizationDigestRow{
		captureTestRow("repo/registry.go::Registry.Configure", "repo/registry.go"),
	}, nil)
	state := newLocalizationTerminalState()
	completion := newLocalizationRecoveryCompletion()
	completion.digest = retained
	state.armForTask(completion, "investigate registry configuration behavior")
	args := map[string]any{"query": "DerivedPolicy.ApplyRule"}

	blocked, firstToken := state.authorizeWithToken("search", "symbols", args)
	if blocked != nil || firstToken == 0 {
		t.Fatalf("inferred exact identifier authorization = (%#v, %d)", blocked, firstToken)
	}
	zero := state.finishReservedReadTokenWithDigest(firstToken, true, nil, true)
	if zero.State != localizationStateNeedsRecovery || zero.AllowedToolCalls != 1 || zero.digest != retained {
		t.Fatalf("empty typed page consumed recovery: %#v", zero)
	}

	blocked, secondToken := state.authorizeWithToken("search", "symbols", args)
	if blocked != nil || secondToken == 0 || secondToken == firstToken {
		t.Fatalf("restored exact identifier authorization = (%#v, %d)", blocked, secondToken)
	}
	captureCtx := withLocalizationPermittedEvidenceCapture(context.Background(), secondToken)
	node := localizationV8Node("repo/policy.go::DerivedPolicy.ApplyRule", "ApplyRule", "repo/policy.go")
	captureLocalizationSearchSymbols(captureCtx, []*graph.Node{node})
	rows, recorded := localizationEvidenceForPermittedCall(captureCtx, "search", "symbols", secondToken)
	ready := state.finishReservedReadTokenWithDigest(secondToken, true, rows, recorded)
	if ready.State != localizationStateAnswerReady || ready.digest == nil || ready.digest.Evidence[0].ID != node.ID {
		t.Fatalf("nonempty typed symbol page did not terminalize current-first: %#v", ready)
	}

	textState := newLocalizationTerminalState()
	textState.armForTask(newLocalizationRecoveryCompletion(), "investigate registry configuration behavior")
	if corrective, token := textState.authorizeWithToken("search", "text", args); corrective == nil || token != 0 {
		t.Fatalf("inferred identifier loosened search.text: (%#v, %d)", corrective, token)
	}
	plainState := newLocalizationTerminalState()
	plainState.armForTask(newLocalizationRecoveryCompletion(), "investigate registry configuration behavior")
	if corrective, token := plainState.authorizeWithToken("search", "symbols", map[string]any{"query": "neighbor"}); corrective == nil || token != 0 {
		t.Fatalf("plain unrelated word became an exact identifier: (%#v, %d)", corrective, token)
	}
}

func TestTypedReservedReadStopsOnlyWhenEvidenceAlignsWithConcreteTaskAnchor(t *testing.T) {
	makeState := func(task, symbol string, refinement bool) (*localizationTerminalState, uint64) {
		state := newLocalizationTerminalState()
		retained := mergeLocalizationEvidenceDigest([]localizationDigestRow{
			captureTestRow("repo/router.go::Router.Route", "repo/router.go"),
		}, nil)
		if refinement {
			state.armRefinementForTask(task, symbol, []string{symbol}, retained)
		} else {
			completion := newLocalizationExactReadCompletion(symbol, false)
			completion.digest = retained
			state.armForTask(completion, task)
		}
		blocked, token := state.authorizeWithToken("read", "source", map[string]any{
			"target": map[string]any{"symbol": symbol},
		})
		if blocked != nil || token == 0 {
			t.Fatalf("reserved read authorization = (%#v, %d)", blocked, token)
		}
		return state, token
	}

	for _, refinement := range []bool{false, true} {
		name := "exact"
		if refinement {
			name = "refinement"
		}
		t.Run(name+" aligned", func(t *testing.T) {
			symbol := "repo/policy.go::PolicyRouter.ApplyRule"
			state, token := makeState("trace PolicyRouter.ApplyRule decisions", symbol, refinement)
			row := captureTestRow(symbol, "repo/policy.go")
			row.Name = "ApplyRule"
			row.QualName = "PolicyRouter.ApplyRule"
			ready := state.finishReservedReadTokenWithDigest(token, true, []localizationDigestRow{row}, true)
			if ready.State != localizationStateAnswerReady || ready.Enforceable || ready.digest == nil || len(ready.digest.Evidence) == 0 {
				t.Fatalf("aligned typed read did not converge advisory answer_ready: %#v", ready)
			}
		})
		t.Run(name+" low alignment", func(t *testing.T) {
			symbol := "repo/cache.go::Cache.Flush"
			state, token := makeState("investigate payment timeout policy", symbol, refinement)
			row := captureTestRow(symbol, "repo/cache.go")
			row.Name = "Flush"
			row.QualName = "Cache.Flush"
			open := state.finishReservedReadTokenWithDigest(token, true, []localizationDigestRow{row}, true)
			if open.State != localizationStateNeedsRecovery || open.AllowedToolCalls != 1 {
				t.Fatalf("low-alignment typed read skipped recovery: %#v", open)
			}
		})
	}
}

func TestSearchTextCaptureResolvesInScopeOwnerThenFileEvidence(t *testing.T) {
	ownerGraph := graph.New()
	file := &graph.Node{ID: "repo/config.go", Name: "config.go", Kind: graph.KindFile, FilePath: "repo/config.go", StartLine: 1, EndLine: 60}
	owner := &graph.Node{ID: "repo/config.go::FormatterRegistry", Name: "FormatterRegistry", QualName: "FormatterRegistry", Kind: graph.KindType, FilePath: "repo/config.go", StartLine: 10, EndLine: 40}
	ownerGraph.AddNode(file)
	ownerGraph.AddNode(owner)
	ownerServer := &Server{graph: ownerGraph}
	ctx := withLocalizationPermittedEvidenceCapture(context.Background(), 71)
	ownerServer.captureLocalizationSearchText(ctx, []enrichedTextMatch{{Path: "repo/config.go", Line: 24, Text: "register marker"}})
	rows, recorded := localizationEvidenceForPermittedCall(ctx, "search", "text", 71)
	if !recorded || len(rows) != 1 || rows[0].ID != owner.ID || rows[0].Line != 24 || rows[0].Provenance != "permitted_search_text_owner" {
		t.Fatalf("file-level text owner capture = %#v, recorded=%v", rows, recorded)
	}

	fileGraph := graph.New()
	fileOnly := &graph.Node{ID: "repo/root.go", Name: "root.go", Kind: graph.KindFile, FilePath: "repo/root.go", StartLine: 1, EndLine: 20}
	fileGraph.AddNode(fileOnly)
	fileServer := &Server{graph: fileGraph}
	fileCtx := withLocalizationPermittedEvidenceCapture(context.Background(), 72)
	fileServer.captureLocalizationSearchText(fileCtx, []enrichedTextMatch{{Path: "repo/root.go", Line: 3, Text: "package marker"}})
	rows, recorded = localizationEvidenceForPermittedCall(fileCtx, "search", "text", 72)
	if !recorded || len(rows) != 1 || rows[0].ID != fileOnly.ID || rows[0].File != fileOnly.FilePath || rows[0].Line != 3 || rows[0].Provenance != "permitted_search_text_file" {
		t.Fatalf("file text evidence capture = %#v, recorded=%v", rows, recorded)
	}

	missingCtx := withLocalizationPermittedEvidenceCapture(context.Background(), 73)
	fileServer.captureLocalizationSearchText(missingCtx, []enrichedTextMatch{{Path: "repo/missing.go", Line: 8, Text: "unattributed marker"}})
	if rows, recorded = localizationEvidenceForPermittedCall(missingCtx, "search", "text", 73); !recorded || len(rows) != 0 {
		t.Fatalf("unvalidated text path became evidence: %#v, recorded=%v", rows, recorded)
	}
}

func TestFacadeAuthArgumentIsStrippedAndPublishesTypedTerminalReceipts(t *testing.T) {
	t.Setenv("GORTEX_HOOK_SESSION_DIR", t.TempDir())
	registry := newFacadeRegistry()
	var server *Server
	handlerSawAuth := false
	registry.capture(mcpgo.NewTool("explore", mcpgo.WithString("task", mcpgo.Required())), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if arguments, ok := req.Params.Arguments.(map[string]any); ok {
			_, handlerSawAuth = arguments[localizationauth.ArgumentKey]
		}
		completion := newLocalizationCompletion(true, "")
		completion.digest = testEvidenceDigest()
		server.localizationFor(ctx).armForTask(completion, req.GetString("task", ""))
		return localizationAnswerReadyResult(completion), nil
	})
	server = &Server{facades: registry, localization: newLocalizationTerminalState(), sessions: newSessionMap()}

	newToken := func(t *testing.T) string {
		t.Helper()
		token, ok := localizationauth.NewToken()
		if !ok {
			t.Fatal("could not create localization auth token")
		}
		return token
	}
	requireReceipt := func(t *testing.T, token string, result *mcpgo.CallToolResult) localizationauth.Receipt {
		t.Helper()
		if result == nil || result.Meta == nil {
			t.Fatalf("terminal result has no typed metadata: %#v", result)
		}
		host, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
		if !ok {
			t.Fatalf("typed host envelope = %T", result.Meta.AdditionalFields[localizationHostMetaKey])
		}
		receipt, ok := localizationauth.Consume(token)
		if !ok {
			t.Fatal("terminal receipt was not published")
		}
		if receipt.FinalResponse != host.Contract.Completion.FinalResponse || receipt.ContractVersion != host.Contract.Completion.ContractVersion || receipt.Enforceable != host.Contract.Completion.Enforceable {
			t.Fatalf("receipt %#v != typed host completion %#v", receipt, host.Contract.Completion)
		}
		return receipt
	}

	ctx := WithSessionID(context.Background(), "auth_direct_answer_ready")
	directToken := newToken(t)
	directArgs := map[string]any{
		"operation":                  "localize",
		"task":                       "locate registry configuration",
		localizationauth.ArgumentKey: directToken,
	}
	direct, err := server.handleFacade(ctx, "explore", mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: "explore", Arguments: directArgs}})
	if err != nil || direct == nil || direct.IsError {
		t.Fatalf("direct answer_ready = (%#v, %v)", direct, err)
	}
	if handlerSawAuth {
		t.Fatal("reserved auth argument reached the legacy handler")
	}
	if _, exists := directArgs[localizationauth.ArgumentKey]; exists {
		t.Fatal("reserved auth argument survived facade entry")
	}
	directReceipt := requireReceipt(t, directToken, direct)

	replayToken := newToken(t)
	replayArgs := map[string]any{"operation": "symbols", "query": "AnyIdentifier", localizationauth.ArgumentKey: replayToken}
	replay, err := server.handleFacade(ctx, "search", mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: "search", Arguments: replayArgs}})
	if err != nil || replay == nil || replay.IsError {
		t.Fatalf("authenticated replay = (%#v, %v)", replay, err)
	}
	replayReceipt := requireReceipt(t, replayToken, replay)
	if replayReceipt.FinalResponse != directReceipt.FinalResponse {
		t.Fatal("authenticated replay receipt diverged from direct answer_ready")
	}

	errorCtx := WithSessionID(context.Background(), "auth_terminal_error")
	errorCompletion := newLocalizationRecoveryCompletion()
	errorCompletion.digest = testEvidenceDigest()
	server.localizationFor(errorCtx).armForTask(errorCompletion, "locate registry configuration")
	errorToken := newToken(t)
	errorArgs := map[string]any{"operation": "unsupported_operation", "query": "Registry.Configure", localizationauth.ArgumentKey: errorToken}
	terminalError, err := server.handleFacade(errorCtx, "search", mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: "search", Arguments: errorArgs}})
	if err != nil || terminalError == nil || !terminalError.IsError {
		t.Fatalf("authenticated terminal error = (%#v, %v)", terminalError, err)
	}
	requireReceipt(t, errorToken, terminalError)
}
