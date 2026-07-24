package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestWeakReadAllowsOneBoundedSearchRecoveryThenTerminates(t *testing.T) {
	server := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "weak_read_search_recovery")
	terminal := server.localizationFor(ctx)
	preferred := "repo/search.go::findCandidate"
	terminal.armRefinementForTask("find candidate resolution", preferred, []string{preferred}, nil)

	readSpec, ok := server.facades.operation("read", "source")
	if !ok {
		t.Fatal("read.source facade operation is missing")
	}
	searchSpec, ok := server.facades.operation("search", "text")
	if !ok {
		t.Fatal("search.text facade operation is missing")
	}
	readCalls := 0
	searchCalls := 0
	server.facades.capture(mcpgo.NewTool(readSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		readCalls++
		return mcpgo.NewToolResultText(`{"source":"func findCandidate() {}"}`), nil
	})
	server.facades.capture(mcpgo.NewTool(searchSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		searchCalls++
		return mcpgo.NewToolResultText(`{"matches":[{"symbol":"repo/search.go::resolveCandidate"}]}`), nil
	})

	readResult, err := server.handleFacade(ctx, "read", localizationRecoveryRequest("read", "source", map[string]any{
		"target": map[string]any{"symbol": preferred},
	}))
	if err != nil || readResult == nil || readResult.IsError || readCalls != 1 {
		t.Fatalf("weak preferred read = (%#v, %v), calls=%d", readResult, err, readCalls)
	}
	requireLocalizationResultStateEqual(t, terminal, readResult, localizationStateNeedsRecovery, false, 1)

	searchResult, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "resolveCandidate",
	}))
	if err != nil || searchResult == nil || searchResult.IsError || searchCalls != 1 {
		t.Fatalf("bounded recovery search = (%#v, %v), calls=%d", searchResult, err, searchCalls)
	}
	requireLocalizationResultStateEqual(t, terminal, searchResult, localizationStateAnswerReady, true, 0)

	extra, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "another anchor",
	}))
	if err != nil {
		t.Fatalf("post-recovery call returned transport error: %v", err)
	}
	requireLocalizationTerminalReplay(t, extra, "search", "text")
	if searchCalls != 1 {
		t.Fatalf("post-recovery search reached handler: calls=%d", searchCalls)
	}
}

func TestRecoveryRejectsUnrelatedAnchorWithoutConsumingAllowance(t *testing.T) {
	server := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "unrelated_recovery_anchor")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationRecoveryCompletion(), "--multiline with --replace duplicates printer output")

	searchSpec, ok := server.facades.operation("search", "text")
	if !ok {
		t.Fatal("search.text facade operation is missing")
	}
	calls := 0
	server.facades.capture(mcpgo.NewTool(searchSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		return mcpgo.NewToolResultText(`{"matches":[{"text":"replace output"}]}`), nil
	})

	result, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "fn sink_matched",
	}))
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("unrelated recovery = (%#v, %v), want corrective tool error", result, err)
	}
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("corrective result content = %#v, want one text block", result.Content)
	}
	var corrective struct {
		ErrorCode ErrorCode `json:"error_code"`
		Retriable bool      `json:"retriable"`
		Data      struct {
			Contract localizationTerminalContract `json:"contract"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &corrective); err != nil {
		t.Fatalf("decode corrective error %q: %v", text, err)
	}
	if corrective.ErrorCode != ErrCodeLocalizationComplete || !corrective.Retriable {
		t.Fatalf("corrective error = %#v", corrective)
	}
	if corrective.Data.Contract.Terminal ||
		corrective.Data.Contract.Completion.State != localizationStateNeedsRecovery ||
		corrective.Data.Contract.Completion.AllowedToolCalls != 1 {
		t.Fatalf("corrective contract = %#v", corrective.Data.Contract)
	}
	terminal.mu.Lock()
	stored := terminal.completionLocked()
	terminal.mu.Unlock()
	if stored.State != localizationStateNeedsRecovery || stored.AllowedToolCalls != 1 {
		t.Fatalf("stored recovery after rejected anchor = %#v", stored)
	}
	if calls != 0 {
		t.Fatalf("unrelated recovery reached handler: calls=%d", calls)
	}

	retry, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "--replace",
	}))
	if err != nil || retry == nil || retry.IsError || calls != 1 {
		t.Fatalf("corrected recovery = (%#v, %v), calls=%d", retry, err, calls)
	}
	requireLocalizationResultStateEqual(t, terminal, retry, localizationStateAnswerReady, true, 0)
}

func TestRecoveryAcceptsTaskAlignedIdentifierAndCompactLiteralAnchors(t *testing.T) {
	tests := []struct {
		name  string
		task  string
		query string
	}{
		{name: "identifier segment", task: "find candidate resolution", query: "resolveCandidate"},
		{name: "flag", task: "--multiline with --replace duplicates output", query: "--replace"},
		{name: "compact literal", task: `register the locale code "ku"`, query: "ku"},
		{name: "specific VCS path", task: "confirm the completed VCS default exclusion change", query: `".jj/"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !localizationRecoveryQueryAligned(tt.task, tt.query) {
				t.Fatalf("query %q should align with task %q", tt.query, tt.task)
			}
		})
	}
	if localizationRecoveryQueryAligned("--multiline with --replace duplicates output", "fn sink_matched") {
		t.Fatal("generic adjacent declaration unexpectedly aligned with task")
	}
}

func TestRecoveryRejectsDigestOnlyTermFromRG2095Incident(t *testing.T) {
	terminal := newLocalizationTerminalState()
	task := "--multiline with --replace causes duplicate output when a match spans multiple lines"
	digest := &localizationEvidenceDigest{Evidence: []localizationDigestRow{{
		ID:       "ripgrep/crates/printer/src/standard.rs::StandardSink.replacer",
		Name:     "replacer",
		QualName: "StandardSink.replacer",
		File:     "crates/printer/src/standard.rs",
	}}}
	completion := newLocalizationRecoveryCompletion()
	completion.digest = digest
	terminal.armForTask(completion, task)

	blocked, reserved := terminal.authorize("search", "text", map[string]any{"query": "fn sink_matched"})
	if blocked == nil {
		t.Fatal("digest-only generic sink term re-admitted the RG2095 recovery failure")
	}
	if reserved {
		t.Fatal("rejected recovery unexpectedly reserved the one-shot allowance")
	}
}

func TestRecoveryFailureRestoresOnceAndTerminalizesSameResponse(t *testing.T) {
	server := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "weak_recovery_failure")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationRecoveryCompletion(), "find candidate resolution")

	searchSpec, ok := server.facades.operation("search", "symbols")
	if !ok {
		t.Fatal("search.symbols facade operation is missing")
	}
	calls := 0
	server.facades.capture(mcpgo.NewTool(searchSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		return nil, errors.New("recovery backend unavailable")
	})
	request := localizationRecoveryRequest("search", "symbols", map[string]any{"query": "resolveCandidate"})

	first, err := server.handleFacade(ctx, "search", request)
	if err != nil || first == nil || !first.IsError || calls != 1 {
		t.Fatalf("first failed recovery = (%#v, %v), calls=%d", first, err, calls)
	}
	requireLocalizationResultStateEqual(t, terminal, first, localizationStateNeedsRecovery, false, 1)

	second, err := server.handleFacade(ctx, "search", request)
	if err != nil || second == nil || !second.IsError || calls != 2 {
		t.Fatalf("second failed recovery = (%#v, %v), calls=%d", second, err, calls)
	}
	requireLocalizationResultStateEqual(t, terminal, second, localizationStateAnswerReady, true, 0)

	third, err := server.handleFacade(ctx, "search", request)
	if err != nil {
		t.Fatalf("post-exhaustion search returned transport error: %v", err)
	}
	requireLocalizationTerminalReplay(t, third, "search", "symbols")
	if calls != 2 {
		t.Fatalf("recovery allowance restored more than once: calls=%d", calls)
	}
}

func TestEnforceableAnswerReadyLocksBeforeHandler(t *testing.T) {
	server := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "strong_answer_ready_lock")
	completion := newLocalizationCompletion(true, "")
	completion.Enforceable = true
	server.localizationFor(ctx).armForTask(completion, "find candidate resolution")

	searchSpec, ok := server.facades.operation("search", "text")
	if !ok {
		t.Fatal("search.text facade operation is missing")
	}
	calls := 0
	server.facades.capture(mcpgo.NewTool(searchSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		return mcpgo.NewToolResultText("unexpected"), nil
	})

	result, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "resolveCandidate",
	}))
	if err != nil {
		t.Fatalf("strong terminal search returned transport error: %v", err)
	}
	requireLocalizationTerminalReplay(t, result, "search", "text")
	if calls != 0 {
		t.Fatalf("enforceable answer_ready reached handler: calls=%d", calls)
	}
}

func TestUnsupportedRecoveryAttemptTerminatesBeforeSchemaDispatch(t *testing.T) {
	server := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "unsupported_recovery")
	server.localizationFor(ctx).armForTask(newLocalizationRecoveryCompletion(), "find candidate resolution")

	result, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "not_an_operation", map[string]any{
		"query": "resolveCandidate",
	}))
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("unsupported recovery = (%#v, %v), want terminal tool error", result, err)
	}
	requireLocalizationTerminalError(t, result, "search", "not_an_operation")

	valid, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "resolveCandidate",
	}))
	if err != nil {
		t.Fatalf("post-rejection recovery returned transport error: %v", err)
	}
	requireLocalizationTerminalReplay(t, valid, "search", "text")
}

func TestSchemaInvalidAllowedRecoveryTerminatesBeforeHandler(t *testing.T) {
	server := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "schema_invalid_recovery")
	terminal := server.localizationFor(ctx)
	terminal.armForTask(newLocalizationRecoveryCompletion(), "find candidate resolution")

	searchSpec, ok := server.facades.operation("search", "text")
	if !ok {
		t.Fatal("search.text facade operation is missing")
	}
	calls := 0
	server.facades.capture(mcpgo.NewTool(searchSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls++
		return mcpgo.NewToolResultText("unexpected"), nil
	})
	invalid, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query":   "resolveCandidate",
		"options": "not-an-object",
	}))
	if err != nil || invalid == nil || !invalid.IsError {
		t.Fatalf("schema-invalid recovery = (%#v, %v), want terminal tool error", invalid, err)
	}
	requireLocalizationResultStateEqual(t, terminal, invalid, localizationStateAnswerReady, true, 0)
	if calls != 0 {
		t.Fatalf("schema-invalid recovery reached handler: calls=%d", calls)
	}

	valid, err := server.handleFacade(ctx, "search", localizationRecoveryRequest("search", "text", map[string]any{
		"query": "resolveCandidate",
	}))
	if err != nil {
		t.Fatalf("post-invalid recovery returned transport error: %v", err)
	}
	requireLocalizationTerminalReplay(t, valid, "search", "text")
	if calls != 0 {
		t.Fatalf("recovery allowance survived invalid schema: calls=%d", calls)
	}
}

func TestStaleInvalidRecoveryTicketCannotConsumeNewTaskState(t *testing.T) {
	state := &localizationTerminalState{}
	state.armForTask(newLocalizationRecoveryCompletion(), "old anchor task")
	blocked, oldGeneration := state.interceptAnswerReady("search", "text", map[string]any{"query": "old anchor"})
	if blocked != nil || oldGeneration == 0 {
		t.Fatalf("old invalid-recovery preflight = (%#v, %d)", blocked, oldGeneration)
	}

	state.armForTask(newLocalizationRecoveryCompletion(), "new anchor task")
	if completion, consumed := state.consumeInvalidRecovery("search", "text", oldGeneration); consumed {
		t.Fatalf("stale invalid request consumed new task: %#v", completion)
	}
	state.mu.Lock()
	stored := state.completionLocked()
	state.mu.Unlock()
	if stored.State != localizationStateNeedsRecovery || stored.AllowedToolCalls != 1 {
		t.Fatalf("new task completion after stale invalid request = %#v", stored)
	}

	blocked, newGeneration := state.interceptAnswerReady("search", "text", map[string]any{"query": "new anchor"})
	if blocked != nil || newGeneration == 0 || newGeneration == oldGeneration {
		t.Fatalf("new invalid-recovery preflight = (%#v, %d), old generation=%d", blocked, newGeneration, oldGeneration)
	}
	completion, consumed := state.consumeInvalidRecovery("search", "text", newGeneration)
	if !consumed || completion.State != localizationStateAnswerReady {
		t.Fatalf("current invalid request consumption = (%#v, %v)", completion, consumed)
	}
}

func TestStaleRecoveryCannotConsumeNewTaskState(t *testing.T) {
	state := &localizationTerminalState{}
	state.armForTask(newLocalizationRecoveryCompletion(), "old anchor task")
	blocked, token := state.authorizeWithToken("search", "text", map[string]any{"query": "old anchor"})
	if blocked != nil || token == 0 {
		t.Fatalf("old recovery reservation = (%#v, %d)", blocked, token)
	}

	state.reset()
	strong := newLocalizationCompletion(true, "")
	strong.Enforceable = true
	state.armForTask(strong, "new task")
	stale := state.finishReservedReadToken(token, true)
	if stale.State != localizationStateInactive {
		t.Fatalf("stale finisher completion = %#v, want inactive", stale)
	}
	if blocked, reserved := state.authorize("search", "text", map[string]any{"query": "new anchor"}); reserved {
		t.Fatal("new strong task reserved a recovery call")
	} else {
		requireLocalizationTerminalReplay(t, blocked, "search", "text")
	}
}

func TestRecoveryEvidenceRejectsLongSymptomSinkEcho(t *testing.T) {
	task := "Storage writes stall during commit"
	row := localizationDigestRow{
		ID:        "repo/storage/report.go::Reporter.ReportStorageFailureAsPending",
		Name:      "ReportStorageFailureAsPending",
		QualName:  "Reporter.ReportStorageFailureAsPending",
		Kind:      "method",
		File:      "repo/storage/report.go",
		Signature: "storage writes stall during commit",
	}
	if localizationRecoveryEvidenceAlignedWithLead(task, localizationTaskLead(task), "storage", "search.text", []localizationDigestRow{row}) {
		t.Fatalf("long symptom sink became confident through body echoes: %#v", row)
	}
}

func TestRecoveryEvidenceAcceptsTwoSegmentImplementation(t *testing.T) {
	task := "Storage writes stall during commit"
	row := localizationDigestRow{
		ID:       "repo/storage/flush.go::Storage.Flush",
		Name:     "StorageFlush",
		QualName: "Storage.Flush",
		Kind:     "method",
		File:     "repo/storage/flush.go",
	}
	if !localizationRecoveryEvidenceAlignedWithLead(task, localizationTaskLead(task), "", "read.source", []localizationDigestRow{row}) {
		t.Fatalf("two-segment implementation did not satisfy proportional lead confidence: %#v", row)
	}
}

func TestRecoveryEvidenceAcceptsLongTechnicalIdentifierWithShortLeadTerms(t *testing.T) {
	task := "JWT auth failures block requests"
	row := localizationDigestRow{
		ID:        "repo/auth/policy.go::Policy.ApplyJWTAuthPolicy",
		Name:      "ApplyJWTAuthPolicy",
		QualName:  "Policy.ApplyJWTAuthPolicy",
		Kind:      "method",
		File:      "repo/auth/policy.go",
		Signature: "jwt auth request failure",
	}
	if !localizationRecoveryEvidenceAlignedWithLead(task, localizationTaskLead(task), "", "read.source", []localizationDigestRow{row}) {
		t.Fatalf("short technical lead terms did not establish long identifier coverage: %#v", row)
	}
}

func TestRecoveryEvidenceRejectsGenericCallableWithSignatureOnlyOverlap(t *testing.T) {
	task := "Storage commits stall during flush"
	row := localizationDigestRow{
		ID:        "repo/storage/handler.go::Handler.Handle",
		Name:      "Handle",
		QualName:  "Handler.Handle",
		Kind:      "method",
		File:      "repo/storage/handler.go",
		Signature: "func Handle() storage commits stall during flush",
	}
	if localizationRecoveryEvidenceAlignedWithLead(task, localizationTaskLead(task), "storage", "search.text", []localizationDigestRow{row}) {
		t.Fatalf("generic callable borrowed confidence from its signature: %#v", row)
	}
}

func TestRecoveryWeakResultPreservesAllowanceForEveryOperation(t *testing.T) {
	longSink := localizationDigestRow{
		ID:        "repo/storage/report.go::Reporter.ReportStorageFailureAsPending",
		Name:      "ReportStorageFailureAsPending",
		QualName:  "Reporter.ReportStorageFailureAsPending",
		Kind:      "method",
		File:      "repo/storage/report.go",
		Signature: "storage writes stall during commit",
	}
	implementation := localizationDigestRow{
		ID:       "repo/storage/flush.go::Storage.Flush",
		Name:     "StorageFlush",
		QualName: "Storage.Flush",
		Kind:     "method",
		File:     "repo/storage/flush.go",
	}
	tests := []struct {
		name           string
		facade         string
		operation      string
		arguments      map[string]any
		retryArguments map[string]any
	}{
		{
			name: "text search", facade: "search", operation: "text",
			arguments: map[string]any{"query": "storage"}, retryArguments: map[string]any{"query": "storage"},
		},
		{
			name: "symbol search", facade: "search", operation: "symbols",
			arguments: map[string]any{"query": "storage"}, retryArguments: map[string]any{"query": implementation.Name},
		},
		{
			name: "source read", facade: "read", operation: "source",
			arguments:      map[string]any{"target": map[string]any{"symbol": longSink.ID}},
			retryArguments: map[string]any{"target": map[string]any{"symbol": implementation.ID}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newLocalizationTerminalState()
			state.armForTask(newLocalizationRecoveryCompletion(), "Storage writes stall during commit")

			blocked, token := state.authorizeWithToken(tt.facade, tt.operation, tt.arguments)
			if blocked != nil || token == 0 {
				t.Fatalf("first recovery authorization = (%#v, %d)", blocked, token)
			}
			completion := state.finishReservedReadTokenWithDigest(token, true, []localizationDigestRow{longSink}, true)
			if completion.State != localizationStateNeedsRecovery || completion.AllowedToolCalls != 1 {
				t.Fatalf("weak recovery completion = %#v, want preserved allowance", completion)
			}

			blocked, retryToken := state.authorizeWithToken(tt.facade, tt.operation, tt.retryArguments)
			if blocked != nil || retryToken == 0 {
				t.Fatalf("retry recovery authorization = (%#v, %d)", blocked, retryToken)
			}
			completion = state.finishReservedReadTokenWithDigest(retryToken, true, []localizationDigestRow{implementation}, true)
			if completion.State != localizationStateAnswerReady || completion.AllowedToolCalls != 0 {
				t.Fatalf("strong recovery completion = %#v, want answer_ready", completion)
			}
		})
	}
}

func TestPlannedRecoveryDerivesOneProductionCallableFamily(t *testing.T) {
	current := []localizationDigestRow{{
		ID:   "repo/stream.go::stream_multi_line_handler",
		Name: "stream_multi_line_handler",
		Kind: "function",
		File: "repo/stream.go",
	}}
	retained := &localizationEvidenceDigest{Evidence: []localizationDigestRow{
		{
			ID:   "repo/codec.go::Codec.transform_with_capture",
			Name: "transform_with_capture",
			Kind: "method",
			File: "repo/codec.go",
		},
		{
			ID:   "repo/codec.go::Codec.transform_with_capture_at",
			Kind: "method",
			File: "repo/codec.go",
		},
		{
			ID:   "repo/tests/codec_test.go::transform_via_fixture",
			Name: "transform_via_fixture",
			Kind: "function",
			File: "repo/tests/codec_test.go",
		},
	}}
	operation, anchor, ok := localizationPlannedRecoveryForWeakRefinement(
		"combining --multi-line with --transform duplicates output",
		current,
		retained,
	)
	if !ok || operation != "search.symbols" || anchor != "transform_with" {
		t.Fatalf("planned recovery = (%q, %q, %v), want search.symbols transform_with", operation, anchor, ok)
	}
}

func TestPlannedRecoveryRequiresUniqueComplementaryFamily(t *testing.T) {
	current := []localizationDigestRow{{
		ID: "repo/stream.go::multi_line_handler", Name: "multi_line_handler", Kind: "function", File: "repo/stream.go",
	}}
	tests := []struct {
		name     string
		task     string
		current  []localizationDigestRow
		retained []localizationDigestRow
	}{
		{
			name:    "ambiguous families",
			task:    "combining --multi-line with --transform duplicates output",
			current: current,
			retained: []localizationDigestRow{
				{ID: "repo/codec.go::transform_with_capture", Name: "transform_with_capture", Kind: "function", File: "repo/codec.go"},
				{ID: "repo/codec.go::transform_via_buffer", Name: "transform_via_buffer", Kind: "function", File: "repo/codec.go"},
			},
		},
		{
			name:     "current read covers no explicit concept",
			task:     "combining --multi-line with --transform duplicates output",
			current:  []localizationDigestRow{{ID: "repo/sink.go::output_sink", Name: "output_sink", Kind: "function", File: "repo/sink.go"}},
			retained: []localizationDigestRow{{ID: "repo/codec.go::transform_with_capture", Name: "transform_with_capture", Kind: "function", File: "repo/codec.go"}},
		},
		{
			name:     "only one explicit concept",
			task:     "--transform duplicates output",
			current:  current,
			retained: []localizationDigestRow{{ID: "repo/codec.go::transform_with_capture", Name: "transform_with_capture", Kind: "function", File: "repo/codec.go"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if operation, anchor, ok := localizationPlannedRecoveryForWeakRefinement(
				tt.task,
				tt.current,
				&localizationEvidenceDigest{Evidence: tt.retained},
			); ok {
				t.Fatalf("unexpected planned recovery = (%q, %q)", operation, anchor)
			}
		})
	}
}

func TestPlannedRecoveryReplaceFamilyRegression(t *testing.T) {
	current := []localizationDigestRow{{
		ID:   "crates/searcher/src/sink.rs::sink_slow_multi_line_only_matching",
		Name: "sink_slow_multi_line_only_matching",
		Kind: "function",
		File: "crates/searcher/src/sink.rs",
	}}
	retained := &localizationEvidenceDigest{Evidence: []localizationDigestRow{
		current[0],
		{
			ID:       "crates/searcher/src/glue.rs::Matcher.replace_with_captures",
			Name:     "replace_with_captures",
			QualName: "Matcher.replace_with_captures",
			Kind:     "method",
			File:     "crates/searcher/src/glue.rs",
		},
		{
			ID:       "crates/searcher/src/glue.rs::Matcher.replace_with_captures_at",
			Name:     "replace_with_captures_at",
			QualName: "Matcher.replace_with_captures_at",
			Kind:     "method",
			File:     "crates/searcher/src/glue.rs",
		},
	}}
	operation, anchor, ok := localizationPlannedRecoveryForWeakRefinement(
		"--multiline with --replace duplicates output while --only-matching spans lines",
		current,
		retained,
	)
	if !ok || operation != "search.symbols" || anchor != "replace_with" {
		t.Fatalf("planned recovery = (%q, %q, %v), want search.symbols replace_with", operation, anchor, ok)
	}
}

func TestPlannedRecoveryIsExactRetriableAndFallsBackToGeneric(t *testing.T) {
	task := "Printed records are duplicated unexpectedly\ncombining --multiline with --replace while --only-matching spans lines"
	preferred := "crates/searcher/src/sink.rs::sink_slow_multi_line_only_matching"
	current := []localizationDigestRow{{
		ID: preferred, Name: "sink_slow_multi_line_only_matching", Kind: "function", File: "crates/searcher/src/sink.rs",
	}}
	retained := &localizationEvidenceDigest{Evidence: []localizationDigestRow{
		current[0],
		{ID: "crates/searcher/src/glue.rs::Matcher.replace_with_captures", Name: "replace_with_captures", Kind: "method", File: "crates/searcher/src/glue.rs"},
		{ID: "crates/searcher/src/glue.rs::Matcher.replace_with_captures_at", Name: "replace_with_captures_at", Kind: "method", File: "crates/searcher/src/glue.rs"},
	}}
	state := newLocalizationTerminalState()
	state.armRefinementForTask(task, preferred, []string{preferred}, retained)
	blocked, token := state.authorizeWithToken("read", "source", map[string]any{"target": map[string]any{"symbol": preferred}})
	if blocked != nil || token == 0 {
		t.Fatalf("refinement authorization = (%#v, %d)", blocked, token)
	}
	completion := state.finishReservedReadTokenWithDigest(token, true, current, true)
	if completion.State != localizationStateNeedsRecovery || completion.RequiredAction != `search.symbols("replace_with")` {
		t.Fatalf("planned completion = %#v", completion)
	}
	if len(completion.AllowedOperations) != 1 || completion.AllowedOperations[0] != "search.symbols" {
		t.Fatalf("planned allowed operations = %#v", completion.AllowedOperations)
	}
	wantInstruction := `Call Gortex MCP search(operation:"symbols", query:"replace_with"); then respond from the accepted result and follow its completion.`
	if completion.Instruction != wantInstruction {
		t.Fatalf("planned instruction = %q, want %q", completion.Instruction, wantInstruction)
	}
	encoded, err := json.Marshal(completion)
	if err != nil {
		t.Fatalf("marshal planned completion: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("decode planned completion: %v", err)
	}
	if _, exists := wire["recoveryOperation"]; exists {
		t.Fatalf("session-only recovery operation leaked onto wire: %s", encoded)
	}
	if _, exists := wire["recoveryAnchor"]; exists {
		t.Fatalf("session-only recovery anchor leaked onto wire: %s", encoded)
	}

	wrong, generation := state.interceptAnswerReady("read", "file", map[string]any{"target": map[string]any{"file": "CHANGELOG.md"}})
	if wrong == nil || !wrong.IsError || generation != 0 {
		t.Fatalf("wrong planned navigation = (%#v, %d), want retriable block", wrong, generation)
	}
	text, ok := singleTextContent(wrong)
	if !ok {
		t.Fatalf("wrong planned navigation content = %#v", wrong.Content)
	}
	var mismatch struct {
		Retriable bool `json:"retriable"`
	}
	if err := json.Unmarshal([]byte(text), &mismatch); err != nil || !mismatch.Retriable {
		t.Fatalf("planned mismatch = (%#v, %v)", mismatch, err)
	}
	state.mu.Lock()
	stored := state.completionLocked()
	state.mu.Unlock()
	if stored.RequiredAction != completion.RequiredAction || stored.AllowedToolCalls != 1 {
		t.Fatalf("planned allowance changed after wrong navigation: %#v", stored)
	}

	wrong, token = state.authorizeWithToken("search", "symbols", map[string]any{"query": "replace"})
	if wrong == nil || token != 0 {
		t.Fatalf("wrong planned anchor = (%#v, %d)", wrong, token)
	}
	blocked, token = state.authorizeWithToken("search", "symbols", map[string]any{"query": "replace_with"})
	if blocked != nil || token == 0 {
		t.Fatalf("exact planned recovery authorization = (%#v, %d)", blocked, token)
	}
	weak := []localizationDigestRow{{
		ID:   "crates/searcher/src/glue.rs::Matcher.replace_with_captures",
		Name: "replace_with_captures", Kind: "method", File: "crates/searcher/src/glue.rs",
	}}
	completion = state.finishReservedReadTokenWithDigest(token, true, weak, true)
	generic := newLocalizationRecoveryCompletion()
	if completion.RequiredAction != generic.RequiredAction || completion.Instruction != generic.Instruction ||
		len(completion.AllowedOperations) != len(generic.AllowedOperations) {
		t.Fatalf("weak planned result did not fall back to generic recovery: %#v", completion)
	}
}

func TestPlannedRecoveryEmptyResultFallsBackToGeneric(t *testing.T) {
	state := newLocalizationTerminalState()
	state.armForTask(newLocalizationPlannedRecoveryCompletion("search.symbols", "transform_with"), "--multi-line with --transform duplicates output")
	blocked, token := state.authorizeWithToken("search", "symbols", map[string]any{"query": "transform_with"})
	if blocked != nil || token == 0 {
		t.Fatalf("planned recovery authorization = (%#v, %d)", blocked, token)
	}
	completion := state.finishReservedReadTokenWithDigest(token, true, nil, true)
	generic := newLocalizationRecoveryCompletion()
	if completion.State != localizationStateNeedsRecovery || completion.RequiredAction != generic.RequiredAction ||
		completion.Instruction != generic.Instruction || len(completion.AllowedOperations) != len(generic.AllowedOperations) {
		t.Fatalf("empty planned result did not fall back to generic recovery: %#v", completion)
	}
}

func localizationRecoveryRequest(facade, operation string, arguments map[string]any) mcpgo.CallToolRequest {
	arguments["operation"] = operation
	return mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: facade, Arguments: arguments}}
}

func requireLocalizationResultStateEqual(
	t *testing.T,
	state *localizationTerminalState,
	result *mcpgo.CallToolResult,
	wantState string,
	wantTerminal bool,
	wantAllowed int,
) {
	t.Helper()
	if result == nil {
		t.Fatal("localization result is nil")
	}
	payload, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content = %T, want map", result.StructuredContent)
	}
	wire := decodeLocalizationCompletion(t, payload["completion"])
	terminal, ok := payload["terminal"].(bool)
	if !ok || terminal != wantTerminal {
		t.Fatalf("structured terminal = %#v, want %v", payload["terminal"], wantTerminal)
	}
	if wire.State != wantState || wire.AllowedToolCalls != wantAllowed {
		t.Fatalf("wire completion = %#v, want state=%q allowed=%d", wire, wantState, wantAllowed)
	}
	if wantState == localizationStateNeedsRecovery {
		if wire.RequiredAction != "recover_once" || len(wire.AllowedOperations) != len(localizationRecoveryOperations) {
			t.Fatalf("recovery completion is not directional/machine-readable: %#v", wire)
		}
		wantInstruction := `Make one accepted, bounded Gortex MCP recovery call: search(operation:"text" or "symbols", query:<specific task anchor>) or read(operation:"source", target:{symbol:<exact id>}). If Gortex explicitly rejects an overbroad request and preserves the recovery allowance, correct it using only Gortex MCP search or read; the rejected request does not count as the accepted recovery. Do not call host Read, Grep, Glob, Bash, or any other non-Gortex tool. Then respond from the accepted result and follow its completion.`
		if wire.Instruction != wantInstruction {
			t.Fatalf("recovery instruction = %q, want %q", wire.Instruction, wantInstruction)
		}
	}

	if result.Meta == nil || result.Meta.AdditionalFields == nil {
		t.Fatal("localization host metadata is missing")
	}
	host, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok {
		t.Fatalf("localization host metadata = %T, want localizationHostEnvelope", result.Meta.AdditionalFields[localizationHostMetaKey])
	}
	state.mu.Lock()
	stored := state.completionLocked()
	state.mu.Unlock()
	requireLocalizationCompletionJSONEqual(t, wire, host.Contract.Completion, "wire/meta")
	requireLocalizationCompletionJSONEqual(t, wire, stored, "wire/state")
	if host.Contract.Terminal != wantTerminal {
		t.Fatalf("host terminal = %v, want %v", host.Contract.Terminal, wantTerminal)
	}
}

func decodeLocalizationCompletion(t *testing.T, value any) localizationCompletion {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal completion: %v", err)
	}
	var completion localizationCompletion
	if err := json.Unmarshal(encoded, &completion); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	return completion
}

func requireLocalizationCompletionJSONEqual(t *testing.T, left, right localizationCompletion, label string) {
	t.Helper()
	leftJSON, err := json.Marshal(left)
	if err != nil {
		t.Fatalf("marshal left %s completion: %v", label, err)
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		t.Fatalf("marshal right %s completion: %v", label, err)
	}
	if string(leftJSON) != string(rightJSON) {
		t.Fatalf("%s completion mismatch:\nwire=%s\nother=%s", label, leftJSON, rightJSON)
	}
}
