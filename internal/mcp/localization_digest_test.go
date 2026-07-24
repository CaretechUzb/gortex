package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func testEvidenceDigest() *localizationEvidenceDigest {
	return newLocalizationEvidenceDigest(localizationExploreEnvelope{
		Files:   []string{"repo/storage/disk.go", "repo/storage/cloud.go"},
		Symbols: []string{"repo/storage/disk.go::DiskStorage.Load", "repo/storage/cloud.go::CloudStorage.Load"},
		Evidence: []localizationEvidence{
			{Rank: 1, ID: "repo/storage/disk.go::DiskStorage.Load", Name: "Load", File: "repo/storage/disk.go", Line: 42, Signature: "func (s *DiskStorage) Load(key string) ([]byte, error)"},
			{Rank: 2, ID: "repo/storage/cloud.go::CloudStorage.Load", Name: "Load", File: "repo/storage/cloud.go", Line: 17},
		},
	})
}

func requireLocalizationTerminalError(t *testing.T, result *mcpgo.CallToolResult, facade, operation string) localizationTerminalContract {
	t.Helper()
	if result == nil || !result.IsError {
		t.Fatalf("terminal result = %#v, want typed MCP error", result)
	}
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("terminal result content = %#v, want one text block", result.Content)
	}
	var payload struct {
		ErrorCode ErrorCode `json:"error_code"`
		Message   string    `json:"message"`
		Retriable bool      `json:"retriable"`
		Data      struct {
			Contract  localizationTerminalContract `json:"contract"`
			Facade    string                       `json:"facade"`
			Operation string                       `json:"operation"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("decode terminal error %q: %v", text, err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &wire); err != nil {
		t.Fatalf("decode terminal error wire %q: %v", text, err)
	}
	if raw, exists := wire["retriable"]; !exists || string(raw) != "false" {
		t.Fatalf("terminal error must explicitly encode retriable=false: %s", text)
	}
	if payload.ErrorCode != ErrCodeLocalizationTerminal || payload.Message == "" || payload.Retriable {
		t.Fatalf("terminal error = %#v", payload)
	}
	if payload.Data.Facade != facade || payload.Data.Operation != operation {
		t.Fatalf("terminal error route = %q/%q, want %q/%q", payload.Data.Facade, payload.Data.Operation, facade, operation)
	}
	contract := payload.Data.Contract
	if !contract.Terminal || contract.Completion.State != localizationStateAnswerReady ||
		contract.Completion.ContractVersion != localizationTerminalContractV2 ||
		contract.Completion.AllowedToolCalls != 0 {
		t.Fatalf("terminal contract = %#v", contract)
	}
	return contract
}

func requireLocalizationTerminalReplay(t *testing.T, result *mcpgo.CallToolResult, _, _ string) localizationTerminalContract {
	t.Helper()
	if result == nil || result.IsError {
		t.Fatalf("terminal replay = %#v, want successful MCP result", result)
	}
	text, ok := singleTextContent(result)
	if !ok || !strings.Contains(text, localizationAnswerReadyDirective) {
		t.Fatalf("terminal replay content = %#v", result.Content)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("terminal replay structured content = %#v", result.StructuredContent)
	}
	encoded, err := json.Marshal(structured)
	if err != nil {
		t.Fatalf("marshal terminal replay: %v", err)
	}
	var contract localizationTerminalContract
	if err := json.Unmarshal(encoded, &contract); err != nil {
		t.Fatalf("decode terminal replay contract: %v", err)
	}
	if !contract.Terminal || contract.Completion.State != localizationStateAnswerReady ||
		contract.Completion.ContractVersion != localizationTerminalContractV2 ||
		contract.Completion.AllowedToolCalls != 0 || contract.Completion.FinalResponse == "" {
		t.Fatalf("terminal replay contract = %#v", contract)
	}
	if structured["final_response"] != contract.Completion.FinalResponse ||
		structured["directive"] != localizationAnswerReadyDirective {
		t.Fatalf("terminal replay stable keys = %#v", structured)
	}
	if text != contract.Completion.FinalResponse {
		t.Fatalf("terminal replay text diverged from final_response: %q", text)
	}
	if !strings.HasSuffix(contract.Completion.FinalResponse, localizationAnswerReadyDirective) {
		t.Fatalf("terminal convergence directive is outside final_response: %q", contract.Completion.FinalResponse)
	}
	return contract
}

func TestPostTerminalNavigationReturnsByteIdenticalSuccessfulReplay(t *testing.T) {
	state := &localizationTerminalState{}
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	state.armForTask(completion, "find the storage load implementations")

	var canonical []byte
	for _, facade := range []string{"explore", "search", "read", "relations", "trace", "analyze"} {
		for repeat := 0; repeat < 3; repeat++ {
			result, reserved := state.authorize(facade, "any_operation", nil)
			if reserved {
				t.Fatalf("%s repeat %d reserved a handler call", facade, repeat)
			}
			contract := requireLocalizationTerminalReplay(t, result, facade, "any_operation")
			if contract.Completion.Enforceable {
				t.Fatalf("%s repeat %d unexpectedly upgraded advisory evidence", facade, repeat)
			}
			visible, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal %s result: %v", facade, err)
			}
			if !strings.Contains(string(visible), "repo/storage/disk.go") {
				t.Fatalf("%s replay omitted retained evidence: %s", facade, visible)
			}
			if canonical == nil {
				canonical = append([]byte(nil), visible...)
			} else if !reflect.DeepEqual(visible, canonical) {
				t.Fatalf("%s repeat %d replay changed by route", facade, repeat)
			}
		}
	}
	for _, facade := range []string{"change", "edit", "refactor", "workspace", "session", "recall", "remember", "capabilities"} {
		if result, reserved := state.authorize(facade, "any_operation", nil); result != nil || reserved {
			t.Fatalf("%s must remain dispatchable after answer_ready: result=%#v reserved=%v", facade, result, reserved)
		}
	}
}

func TestRepeatLocalizeAgainstTerminalContractReturnsCompactStop(t *testing.T) {
	state := &localizationTerminalState{}
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	state.armForTask(completion, "find the storage load implementations")

	token, blocked := state.beginLocalize("find the storage load implementations again", false)
	if token != 0 {
		t.Fatal("repeat localize must not reserve the handler slot")
	}
	requireLocalizationTerminalReplay(t, blocked, "explore", "localize")
}

func TestRefinementPromotionRetainsDigestForReplay(t *testing.T) {
	state := &localizationTerminalState{}
	candidate := "repo/storage/disk.go::DiskStorage.Load"
	state.armRefinementRoutesForTask(
		"find the storage load implementations",
		candidate,
		[]string{candidate},
		map[string]localizationRefinementRoute{candidate: {enforceable: true}},
		testEvidenceDigest(),
	)

	args := map[string]any{"target": map[string]any{"symbol": candidate}}
	if blocked, reserved := state.authorize("read", "source", args); blocked != nil || !reserved {
		t.Fatalf("permitted refinement read = (%v, %v), want reservation", blocked, reserved)
	}
	state.finishReservedRead(true)

	terminal, reserved := state.authorize("search", "symbols", nil)
	if reserved {
		t.Fatal("post-promotion navigation reserved a handler")
	}
	contract := requireLocalizationTerminalReplay(t, terminal, "search", "symbols")
	if !strings.Contains(contract.Completion.FinalResponse, "#1 repo/storage/disk.go") ||
		!strings.Contains(contract.Completion.FinalResponse, "#2 repo/storage/cloud.go") {
		t.Fatalf("promotion lost retained aligned evidence: %q", contract.Completion.FinalResponse)
	}
}

func TestRefinementAllowsOneAlternateRankedCandidateRead(t *testing.T) {
	state := &localizationTerminalState{}
	preferred := "repo/storage/disk.go::DiskStorage.Load"
	alternate := "repo/storage/cloud.go::CloudStorage.Load"
	state.armRefinementRoutesForTask(
		"find the storage load implementations",
		preferred,
		[]string{preferred, alternate},
		map[string]localizationRefinementRoute{
			preferred: {enforceable: true},
			alternate: {enforceable: true},
		},
		testEvidenceDigest(),
	)

	if completion := state.completionLocked(); !strings.Contains(completion.RequiredAction, "recommended") ||
		!strings.Contains(completion.RequiredAction, "any returned candidate") {
		t.Fatalf("required action does not explain alternate candidate authorization: %q", completion.RequiredAction)
	}
	args := map[string]any{"target": map[string]any{"symbol": alternate}}
	if blocked, reserved := state.authorize("read", "source", args); blocked != nil || !reserved {
		t.Fatalf("alternate ranked candidate read = (%v, %v), want reservation", blocked, reserved)
	}
	state.finishReservedRead(true)
	if result, reserved := state.authorize("read", "source", args); reserved {
		t.Fatal("second read reserved a handler")
	} else {
		requireLocalizationTerminalReplay(t, result, "read", "source")
	}
}

func TestDigestLifecycleAndLegacyFallback(t *testing.T) {
	state := &localizationTerminalState{}
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	state.armForTask(completion, "find the storage load implementations")
	state.keepOpenForTask("new unrelated work")
	if blocked, _ := state.authorize("search", "symbols", nil); blocked != nil {
		t.Fatalf("inactive state must not block, got %+v", blocked)
	}
	if state.digest != nil {
		t.Fatal("keepOpenForTask must clear the retained digest")
	}

	withoutDigest := &localizationTerminalState{}
	withoutDigest.armForTask(newLocalizationCompletion(true, ""), "task without digest")
	blocked, _ := withoutDigest.authorize("search", "symbols", nil)
	requireLocalizationTerminalReplay(t, blocked, "search", "symbols")
}

func TestDigestByteCapShedsEvidenceTail(t *testing.T) {
	envelope := localizationExploreEnvelope{}
	for i := 0; i < 400; i++ {
		envelope.Evidence = append(envelope.Evidence, localizationEvidence{
			Rank:      i + 1,
			ID:        fmt.Sprintf("repo/big/file.go::Sym%03d", i),
			Name:      strings.Repeat("n", 40),
			File:      "repo/big/file.go",
			Line:      i,
			Signature: strings.Repeat("s", 2000),
		})
	}
	digest := newLocalizationEvidenceDigest(envelope)
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatalf("marshal digest: %v", err)
	}
	if len(encoded) > localizationDigestMaxBytes {
		t.Fatalf("digest = %d bytes, want <= %d", len(encoded), localizationDigestMaxBytes)
	}
	if len(digest.Evidence) == 0 || len(digest.Evidence) >= localizationReplayEvidenceLimit {
		t.Fatalf("byte cap retained %d rows, want a non-empty shed prefix", len(digest.Evidence))
	}
	if len(digest.Files) != len(digest.Evidence) || len(digest.Symbols) != len(digest.Evidence) {
		t.Fatalf("files=%d symbols=%d evidence=%d, want positional rows", len(digest.Files), len(digest.Symbols), len(digest.Evidence))
	}
	for index, file := range digest.Files {
		if file != "repo/big/file.go" || digest.Evidence[index].Rank != index+1 {
			t.Fatalf("unaligned compacted row %d: file=%q evidence=%#v", index, file, digest.Evidence[index])
		}
	}
	if len(digest.finalResponse) > localizationFinalResponseMaxBytes {
		t.Fatalf("final_response = %d bytes, want <= %d", len(digest.finalResponse), localizationFinalResponseMaxBytes)
	}
}

func TestDigestByteCapRetainsSingleMandatoryRowAfterSheddingOptionalFields(t *testing.T) {
	envelope := localizationExploreEnvelope{Evidence: []localizationEvidence{{
		Rank:      1,
		ID:        "repo/registry.go::Registry.Configure",
		Name:      strings.Repeat("n", 1000),
		QualName:  strings.Repeat("q", 3000),
		Kind:      "method",
		File:      "repo/registry.go",
		Line:      17,
		Signature: strings.Repeat("s", 8000),
		Callers:   []string{strings.Repeat("caller", 1000)},
		Callees:   []string{strings.Repeat("callee", 1000)},
	}}}

	digest := newLocalizationEvidenceDigest(envelope)
	if len(digest.Evidence) != 1 {
		t.Fatalf("mandatory evidence rows = %d, want 1", len(digest.Evidence))
	}
	row := digest.Evidence[0]
	if row.ID != envelope.Evidence[0].ID || row.File != envelope.Evidence[0].File || row.Line != envelope.Evidence[0].Line {
		t.Fatalf("mandatory row identity changed while shedding: %#v", row)
	}
	if row.Signature != "" || row.QualName != "" || len(row.Callers) != 0 || len(row.Callees) != 0 {
		t.Fatalf("largest optional fields were retained after the digest fit: %#v", row)
	}
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatalf("marshal digest: %v", err)
	}
	if len(encoded) > localizationDigestMaxBytes {
		t.Fatalf("shed digest = %d bytes, want <= %d", len(encoded), localizationDigestMaxBytes)
	}
}

func TestDigestPathologicalIdentityFallsBackToBoundedEmptyEvidence(t *testing.T) {
	oversized := strings.Repeat("x", localizationDigestMaxBytes+1)
	digest := newLocalizationEvidenceDigest(localizationExploreEnvelope{Evidence: []localizationEvidence{{
		Rank: 1, ID: oversized, File: oversized, Line: 1,
	}}})
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatalf("marshal pathological digest: %v", err)
	}
	if len(encoded) > localizationDigestMaxBytes {
		t.Fatalf("pathological digest = %d bytes, want <= %d", len(encoded), localizationDigestMaxBytes)
	}
	if len(digest.Evidence) != 0 || len(digest.Files) != 0 || len(digest.Symbols) != 0 {
		t.Fatalf("oversized irreducible row survived fallback: %#v", digest)
	}
	want := renderLocalizationFinalResponse(nil)
	if digest.finalResponse != want {
		t.Fatalf("pathological fallback final_response = %q, want %q", digest.finalResponse, want)
	}
	completion := newLocalizationCompletion(true, "")
	completion.digest = digest
	result := localizationAnswerReadyResult(completion)
	contract := requireLocalizationTerminalReplay(t, result, "search", "symbols")
	if contract.Completion.FinalResponse != want {
		t.Fatalf("pathological replay final_response = %q, want %q", contract.Completion.FinalResponse, want)
	}
}

func TestPostTerminalReadsAreIntercepted(t *testing.T) {
	state := &localizationTerminalState{}
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	state.armForTask(completion, "find the storage load implementations")

	blocked, reserved := state.authorize("read", "source", map[string]any{
		"target": map[string]any{"symbol": "repo/storage/disk.go::DiskStorage.Load"},
	})
	if reserved {
		t.Fatal("post-terminal read reserved a handler")
	}
	requireLocalizationTerminalReplay(t, blocked, "read", "source")
}

func TestLocalizationDigestKeepsOnlyConcreteBoundedEvidence(t *testing.T) {
	envelope := localizationExploreEnvelope{
		Files:   []string{"pkg/unsupported.go", "pkg/a.go", "pkg/b.go", "pkg/c.go", "pkg/d.go", "pkg/e.go", "pkg/f.go"},
		Symbols: []string{"repo/pkg/unsupported.go::Unsupported", "repo/pkg/a.go::A", "repo/pkg/b.go::B", "repo/pkg/c.go::C", "repo/pkg/d.go::D", "repo/pkg/e.go::E", "repo/pkg/f.go::F"},
		Evidence: []localizationEvidence{
			{Rank: 1, ID: "repo/pkg/a.go::A", Name: "A", Kind: "function", File: "pkg/a.go", Line: 10, Callers: []string{"repo/pkg/caller.go::CallA"}},
			{Rank: 2, ID: "repo/pkg/b.go::B", Name: "B", Kind: "method", File: "pkg/b.go", Line: 20},
			{Rank: 3, ID: "repo/pkg/c.go::C", Name: "C", Kind: "method", File: "pkg/c.go", Line: 30},
			{Rank: 4, ID: "repo/pkg/d.go::D", Name: "D", Kind: "method", File: "pkg/d.go", Line: 40},
			{Rank: 5, ID: "repo/pkg/e.go::E", Name: "E", Kind: "method", File: "pkg/e.go", Line: 50},
			{Rank: 6, ID: "repo/pkg/f.go::F", Name: "F", Kind: "method", File: "pkg/f.go", Line: 60},
			{Rank: 7, ID: "repo/pkg/missing.go::Missing", Name: "Missing"},
		},
	}

	digest := newLocalizationEvidenceDigest(envelope)
	if len(digest.Evidence) != 6 {
		t.Fatalf("retained evidence = %d, want 6", len(digest.Evidence))
	}
	wantFiles := []string{"pkg/a.go", "pkg/b.go", "pkg/c.go", "pkg/d.go", "pkg/e.go", "pkg/f.go"}
	wantSymbols := []string{"repo/pkg/a.go::A", "repo/pkg/b.go::B", "repo/pkg/c.go::C", "repo/pkg/d.go::D", "repo/pkg/e.go::E", "repo/pkg/f.go::F"}
	if !reflect.DeepEqual(digest.Files, wantFiles) {
		t.Fatalf("digest files = %#v, want %#v", digest.Files, wantFiles)
	}
	if !reflect.DeepEqual(digest.Symbols, wantSymbols) {
		t.Fatalf("digest symbols = %#v, want %#v", digest.Symbols, wantSymbols)
	}
	if got := digest.Evidence[0].Callers; !reflect.DeepEqual(got, []string{"repo/pkg/caller.go::CallA"}) {
		t.Fatalf("causal provenance was dropped: %#v", got)
	}
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatalf("marshal digest: %v", err)
	}
	if strings.Contains(string(encoded), "final_response") || strings.Contains(string(encoded), "unsupported") {
		t.Fatalf("digest retained an unsupported or prewritten answer field: %s", encoded)
	}
}

func TestLocalizationFinalResponseKeepsDuplicateFilesPositionallyAligned(t *testing.T) {
	digest := newLocalizationEvidenceDigest(localizationExploreEnvelope{Evidence: []localizationEvidence{
		{Rank: 7, ID: "repo/pkg/shared.go::First", File: "pkg/shared.go", Line: 11, Source: "first body"},
		{Rank: 9, ID: "repo/pkg/shared.go::Second", File: "pkg/shared.go", Line: 27, Source: "second body"},
		{Rank: 12, ID: "repo/pkg/other.go::Third", File: "pkg/other.go", Line: 3, Source: "third body"},
	}})
	wantFiles := []string{"pkg/shared.go", "pkg/shared.go", "pkg/other.go"}
	wantSymbols := []string{"repo/pkg/shared.go::First", "repo/pkg/shared.go::Second", "repo/pkg/other.go::Third"}
	if !reflect.DeepEqual(digest.Files, wantFiles) || !reflect.DeepEqual(digest.Symbols, wantSymbols) {
		t.Fatalf("positional digest = files %#v symbols %#v", digest.Files, digest.Symbols)
	}
	for index, row := range digest.Evidence {
		marker := fmt.Sprintf("#%d %s", index+1, row.ID)
		if row.Rank != index+1 || !strings.Contains(digest.finalResponse, marker) {
			t.Fatalf("row %d not aligned in final_response:\n%s", index, digest.finalResponse)
		}
	}
	encoded, err := json.Marshal(digest)
	if err != nil || strings.Contains(string(encoded), "body") || strings.Contains(digest.finalResponse, "body") {
		t.Fatalf("retained digest leaked source: %s (%v)", encoded, err)
	}
}

func TestLocalizationAnswerReadyResultReplaysAlignedStableContract(t *testing.T) {
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	result := localizationAnswerReadyResult(completion)
	contract := requireLocalizationTerminalReplay(t, result, "", "")
	wantRows := []string{
		"FILES:\n#1 repo/storage/disk.go\n#2 repo/storage/cloud.go",
		"SYMBOLS:\n#1 repo/storage/disk.go::DiskStorage.Load\n#2 repo/storage/cloud.go::CloudStorage.Load",
		"EVIDENCE:\n#1 repo/storage/disk.go:42 — repo/storage/disk.go::DiskStorage.Load\n#2 repo/storage/cloud.go:17 — repo/storage/cloud.go::CloudStorage.Load",
	}
	for _, want := range wantRows {
		if !strings.Contains(contract.Completion.FinalResponse, want) {
			t.Fatalf("final_response omitted aligned rows %q:\n%s", want, contract.Completion.FinalResponse)
		}
	}
	visible, _ := singleTextContent(result)
	if strings.Contains(strings.ToLower(visible), "source") || len(contract.Completion.FinalResponse) > localizationFinalResponseMaxBytes {
		t.Fatalf("terminal replay leaked source or exceeded cap: %q", visible)
	}
	if result.Meta == nil || result.Meta.AdditionalFields == nil {
		t.Fatal("terminal result omitted host-only metadata")
	}
	host, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok || host.Evidence == nil || host.FallbackFormat == "" || host.Evidence.Evidence[0].File != "repo/storage/disk.go" {
		t.Fatalf("host-only fallback envelope = %#v", result.Meta.AdditionalFields[localizationHostMetaKey])
	}
	if host.Contract.Completion.FinalResponse != contract.Completion.FinalResponse {
		t.Fatalf("host and visible final_response disagree: host=%q visible=%q", host.Contract.Completion.FinalResponse, contract.Completion.FinalResponse)
	}
}

func TestAttachLocalizationHostEnvelopePreservesPreterminalStructuredContent(t *testing.T) {
	original := map[string]any{
		"payload":    map[string]any{"value": "kept"},
		"completion": map[string]any{"legacy": true},
		"terminal":   "unchanged",
	}
	before, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal original structured content: %v", err)
	}
	result := mcpgo.NewToolResultText("preterminal payload")
	result.StructuredContent = original
	completion := newLocalizationRecoveryCompletion()
	attached := attachLocalizationHostEnvelope(result, completion, testEvidenceDigest())
	after, err := json.Marshal(attached.StructuredContent)
	if err != nil {
		t.Fatalf("marshal attached structured content: %v", err)
	}
	if !reflect.DeepEqual(attached.StructuredContent, original) || !reflect.DeepEqual(after, before) {
		t.Fatalf("preterminal structured content changed: before=%s after=%s shape=%#v", before, after, attached.StructuredContent)
	}
	host, ok := attached.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok {
		t.Fatalf("preterminal host envelope missing: %#v", attached.Meta)
	}
	if host.Contract.Terminal || host.Contract.Completion.State != localizationStateNeedsRecovery || host.Contract.Completion.FinalResponse != "" {
		t.Fatalf("preterminal host contract was terminally enriched: %#v", host.Contract)
	}
}

func TestDecorateLocalizationReadResultPreservesMultiContentStructuredAndMeta(t *testing.T) {
	structured := map[string]any{"source": "func Run() {}", "count": 2, "terminal": false}
	result := &mcpgo.CallToolResult{
		Content: []mcpgo.Content{
			mcpgo.NewTextContent(`{"source":"func Run() {}","completion":{"state":"stale"},"terminal":false}`),
			mcpgo.NewTextContent("secondary payload"),
		},
		StructuredContent: structured,
	}
	result.Meta = mcpgo.NewMetaFromMap(map[string]any{
		"existing":              "kept",
		localizationHostMetaKey: "repository-controlled-spoof",
	})

	decorated := decorateLocalizationReadResult(result, newLocalizationCompletion(true, ""))
	if decorated != result {
		t.Fatal("decorator must preserve the result object and its non-text payload")
	}
	if len(decorated.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(decorated.Content))
	}
	first, ok := mcpgo.AsTextContent(decorated.Content[0])
	if !ok {
		t.Fatal("first content block stopped being text")
	}
	var firstPayload map[string]any
	if err := json.Unmarshal([]byte(first.Text), &firstPayload); err != nil {
		t.Fatalf("decorated first content is invalid JSON: %v", err)
	}
	if firstPayload["source"] != "func Run() {}" || firstPayload["completion"] == nil || firstPayload["terminal"] != true {
		t.Fatalf("decorated first payload = %#v", firstPayload)
	}
	if strings.Count(first.Text, `"completion"`) != 1 {
		t.Fatalf("decorated first payload contains duplicate completion keys: %s", first.Text)
	}
	second, ok := mcpgo.AsTextContent(decorated.Content[1])
	if !ok || second.Text != "secondary payload" {
		t.Fatalf("secondary content was lost: %#v", decorated.Content[1])
	}
	decoratedStructured, ok := decorated.StructuredContent.(map[string]any)
	if !ok || decoratedStructured["source"] != structured["source"] || decoratedStructured["completion"] == nil || decoratedStructured["terminal"] != true {
		t.Fatalf("structured payload was lost: %#v", decorated.StructuredContent)
	}
	if _, mutated := structured["completion"]; mutated {
		t.Fatal("decorator mutated the handler's structured map")
	}
	if decorated.Meta == nil || decorated.Meta.AdditionalFields["existing"] != "kept" {
		t.Fatalf("existing metadata was lost: %#v", decorated.Meta)
	}
	host, ok := decorated.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok || !host.Contract.Terminal || host.Contract.Completion.ContractVersion != localizationTerminalContractV2 {
		t.Fatalf("authoritative metadata was not replaced: %#v", decorated.Meta.AdditionalFields[localizationHostMetaKey])
	}
}

func TestDecorateLocalizationReadResultPreservesStructuredOnlyPayload(t *testing.T) {
	payload := []any{"first", map[string]any{"second": true}}
	result := &mcpgo.CallToolResult{StructuredContent: payload}

	decorated := decorateLocalizationReadResult(result, newLocalizationCompletion(true, ""))
	if len(decorated.Content) != 1 {
		t.Fatalf("completion content blocks = %d, want 1", len(decorated.Content))
	}
	completionText, ok := mcpgo.AsTextContent(decorated.Content[0])
	if !ok || !strings.Contains(completionText.Text, `"state":"answer_ready"`) {
		t.Fatalf("structured-only result omitted visible completion: %#v", decorated.Content)
	}
	wrapped, ok := decorated.StructuredContent.(map[string]any)
	if !ok || !reflect.DeepEqual(wrapped["payload"], payload) || wrapped["completion"] == nil {
		t.Fatalf("structured-only payload was lost: %#v", decorated.StructuredContent)
	}
}

func TestDecorateLocalizationReadResultMirrorsContentOnlyPayloadForHosts(t *testing.T) {
	completion := newLocalizationCompletion(true, "")

	plain := mcpgo.NewToolResultText("func Load() {}")
	decoratedPlain := decorateLocalizationReadResult(plain, completion)
	plainStructured, ok := decoratedPlain.StructuredContent.(map[string]any)
	if !ok || plainStructured["text"] != "func Load() {}" || plainStructured["completion"] == nil {
		t.Fatalf("plain content was not mirrored for structured-first hosts: %#v", decoratedPlain.StructuredContent)
	}
	plainText, ok := singleTextContent(decoratedPlain)
	if !ok || !strings.Contains(plainText, "func Load() {}") {
		t.Fatalf("plain content block was lost: %#v", decoratedPlain.Content)
	}

	multi := &mcpgo.CallToolResult{Content: []mcpgo.Content{
		mcpgo.NewTextContent("primary"),
		mcpgo.NewTextContent("secondary"),
	}}
	decoratedMulti := decorateLocalizationReadResult(multi, completion)
	multiStructured, ok := decoratedMulti.StructuredContent.(map[string]any)
	mirrored, mirroredOK := multiStructured["content"].([]mcpgo.Content)
	if !ok || !mirroredOK || len(mirrored) != 2 || multiStructured["completion"] == nil {
		t.Fatalf("multi-content payload was not mirrored: %#v", decoratedMulti.StructuredContent)
	}
	if len(decoratedMulti.Content) != 2 {
		t.Fatalf("multi-content blocks = %d, want 2", len(decoratedMulti.Content))
	}
}

func TestPermittedRefinementReadInvokesHandlerAndPreservesPayload(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "refinement_read_payload")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1.0"}}}`)
	if reply := srv.MCPServer().HandleMessage(ctx, initFrame); reply == nil {
		t.Fatal("initialize returned nil")
	}

	readSpec, ok := srv.facades.operation("read", "source")
	if !ok {
		t.Fatal("read.source facade operation is missing")
	}
	searchSpec, ok := srv.facades.operation("search", "symbols")
	if !ok {
		t.Fatal("search.symbols facade operation is missing")
	}

	const symbol = "repo/storage/disk.go::DiskStorage.Load"
	readCalls := 0
	searchCalls := 0
	srv.facades.capture(mcpgo.NewTool(readSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		readCalls++
		result := mcpgo.NewToolResultText(`{"source":"func (s *DiskStorage) Load() {}","language":"go"}`)
		result.Meta = mcpgo.NewMetaFromMap(map[string]any{"handler": "kept"})
		return result, nil
	})
	srv.facades.capture(mcpgo.NewTool(searchSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		searchCalls++
		return mcpgo.NewToolResultText("unexpected search result"), nil
	})
	srv.localizationFor(ctx).armRefinementRoutesForTask(
		"find the storage load implementation",
		symbol,
		[]string{symbol},
		map[string]localizationRefinementRoute{symbol: {enforceable: true}},
		testEvidenceDigest(),
	)

	readRequest := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name: "read",
		Arguments: map[string]any{
			"operation": "source",
			"target":    map[string]any{"symbol": symbol},
		},
	}}
	result, err := srv.handleFacade(ctx, "read", readRequest)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("permitted refinement read = (%+v, %v)", result, err)
	}
	if readCalls != 1 {
		t.Fatalf("read handler calls = %d, want 1", readCalls)
	}
	if len(result.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(result.Content))
	}
	first, ok := mcpgo.AsTextContent(result.Content[0])
	if !ok || !strings.Contains(first.Text, `"source":"func (s *DiskStorage) Load() {}"`) ||
		!strings.Contains(first.Text, `"state":"answer_ready"`) {
		t.Fatalf("decorated first payload = %#v", result.Content[0])
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok || structured["source"] != "func (s *DiskStorage) Load() {}" ||
		structured["language"] != "go" || structured["completion"] == nil {
		t.Fatalf("structured source payload was lost: %#v", result.StructuredContent)
	}
	if result.Meta == nil || result.Meta.AdditionalFields["handler"] != "kept" {
		t.Fatalf("handler metadata was lost: %#v", result.Meta)
	}
	// This completed read is the answer_ready PostToolUse event observed by a
	// host. Capture its exact terminal payload before deliberately issuing one
	// more navigation call below.
	initialEncoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal initial terminal contract: %v", err)
	}
	var initialContract localizationTerminalContract
	if err := json.Unmarshal(initialEncoded, &initialContract); err != nil {
		t.Fatalf("decode initial terminal contract: %v", err)
	}
	initialHost, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok || !initialContract.Terminal || initialContract.Completion.FinalResponse == "" {
		t.Fatalf("answer_ready PostToolUse envelope = contract %#v host %#v", initialContract, result.Meta)
	}
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal MCP result: %v", err)
	}
	var roundTrip struct {
		Content           []map[string]any `json:"content"`
		StructuredContent map[string]any   `json:"structuredContent"`
	}
	if err := json.Unmarshal(wire, &roundTrip); err != nil {
		t.Fatalf("unmarshal MCP result: %v", err)
	}
	if len(roundTrip.Content) != 1 || roundTrip.StructuredContent["source"] != "func (s *DiskStorage) Load() {}" ||
		roundTrip.StructuredContent["language"] != "go" || roundTrip.StructuredContent["completion"] == nil {
		t.Fatalf("wire result lost source payload: %s", wire)
	}

	searchRequest := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name:      "search",
		Arguments: map[string]any{"operation": "  SyMbOlS  ", "query": "Load"},
	}}
	terminal, err := srv.handleFacade(ctx, "search", searchRequest)
	if err != nil {
		t.Fatalf("post-refinement navigation error = %v", err)
	}
	if searchCalls != 0 {
		t.Fatalf("search handler calls after answer_ready = %d, want 0", searchCalls)
	}
	replayContract := requireLocalizationTerminalReplay(t, terminal, "search", "symbols")
	replayHost, ok := terminal.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok {
		t.Fatalf("post-terminal replay host envelope missing: %#v", terminal.Meta)
	}
	if replayContract.Completion.FinalResponse != initialContract.Completion.FinalResponse ||
		replayHost.Contract.Completion.FinalResponse != initialHost.Contract.Completion.FinalResponse ||
		!reflect.DeepEqual(replayHost.Evidence, initialHost.Evidence) {
		t.Fatalf("host-ignored terminal replay diverged: initial=%#v/%#v replay=%#v/%#v", initialContract, initialHost, replayContract, replayHost)
	}
}

func TestAnswerReadyNavigationDispatchNeverInvokesLegacyHandler(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "terminal_handler_intercept")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1.0"}}}`)
	if reply := srv.MCPServer().HandleMessage(ctx, initFrame); reply == nil {
		t.Fatal("initialize returned nil")
	}

	spec, ok := srv.facades.operation("search", "symbols")
	if !ok {
		t.Fatal("search.symbols facade operation is missing")
	}
	readSpec, ok := srv.facades.operation("read", "source")
	if !ok {
		t.Fatal("read.source facade operation is missing")
	}
	changeSpec, ok := srv.facades.operation("change", "detect")
	if !ok {
		t.Fatal("change.detect facade operation is missing")
	}
	handlerCalls := 0
	changeCalls := 0
	srv.facades.capture(mcpgo.NewTool(spec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		handlerCalls++
		return mcpgo.NewToolResultText(strings.Repeat("expensive", 10_000)), nil
	})
	srv.facades.capture(mcpgo.NewTool(readSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		handlerCalls++
		return mcpgo.NewToolResultText(strings.Repeat("source", 10_000)), nil
	})
	srv.facades.capture(mcpgo.NewTool(changeSpec.Legacy), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		changeCalls++
		return mcpgo.NewToolResultText(`{"changed":[]}`), nil
	})
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	srv.localizationFor(ctx).armForTask(completion, "find the storage load implementations")

	request := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name: "search",
		Arguments: map[string]any{
			"operation": "  SyMbOlS  ",
			"query":     "Load",
		},
	}}
	for repeat := 0; repeat < 10; repeat++ {
		result, err := srv.handleFacade(ctx, "search", request)
		if err != nil {
			t.Fatalf("repeat %d result = (%+v, %v)", repeat, result, err)
		}
		requireLocalizationTerminalReplay(t, result, "search", "symbols")
	}
	if handlerCalls != 0 {
		t.Fatalf("legacy handler invoked %d times after answer_ready", handlerCalls)
	}
	readRequest := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name: "read",
		Arguments: map[string]any{
			"operation": "source",
			"target":    map[string]any{"symbol": "repo/storage/disk.go::DiskStorage.Load"},
		},
	}}
	readResult, err := srv.handleFacade(ctx, "read", readRequest)
	if err != nil {
		t.Fatalf("post-terminal read = (%+v, %v)", readResult, err)
	}
	requireLocalizationTerminalReplay(t, readResult, "read", "source")
	if handlerCalls != 0 {
		t.Fatalf("read handler invoked %d times after answer_ready", handlerCalls)
	}

	// The pre-validation gate also catches malformed recovery attempts instead
	// of spending a turn on schema errors.
	request.Params.Arguments = map[string]any{"operation": "not_an_operation"}
	result, err := srv.handleFacade(ctx, "search", request)
	if err != nil {
		t.Fatalf("malformed post-terminal request = (%+v, %v)", result, err)
	}
	requireLocalizationTerminalReplay(t, result, "search", "not_an_operation")

	changeRequest := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name:      "change",
		Arguments: map[string]any{"operation": "detect"},
	}}
	changeResult, err := srv.handleFacade(ctx, "change", changeRequest)
	if err != nil || changeResult == nil || changeResult.IsError || changeCalls != 1 {
		t.Fatalf("post-terminal change.detect = (%+v, %v), calls=%d", changeResult, err, changeCalls)
	}
	if text, _ := singleTextContent(changeResult); strings.Contains(text, localizationAnswerReadyDirective) {
		t.Fatal("post-terminal change.detect was incorrectly intercepted")
	}

	// Capabilities has a dedicated handler and remains usable after localization.
	capabilitiesFrame := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"capabilities","arguments":{}}}`)
	raw, err := json.Marshal(srv.MCPServer().HandleMessage(ctx, capabilitiesFrame))
	if err != nil {
		t.Fatalf("marshal capabilities response: %v", err)
	}
	var called struct {
		Error  any                   `json:"error"`
		Result *mcpgo.CallToolResult `json:"result"`
	}
	if err := json.Unmarshal(raw, &called); err != nil {
		t.Fatalf("decode capabilities response: %v", err)
	}
	if called.Error != nil || called.Result == nil || called.Result.IsError {
		t.Fatalf("terminal capabilities response = error %#v result %#v", called.Error, called.Result)
	}
	if text, _ := singleTextContent(called.Result); strings.Contains(text, localizationAnswerReadyDirective) {
		t.Fatalf("capabilities was incorrectly intercepted: %q", text)
	}
}
