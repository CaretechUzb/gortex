package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

func prescribedBodyTargets(source string) (string, []exploreTarget) {
	const symbol = "storage/disk.go::DiskStorage.Load"
	primary := &graph.Node{
		ID: symbol, Name: "DiskStorage.Load", Kind: graph.KindMethod,
		FilePath: "storage/disk.go", StartLine: 12,
	}
	sibling := &graph.Node{
		ID: "storage/cache.go::Cache.Warm", Name: "Cache.Warm", Kind: graph.KindMethod,
		FilePath: "storage/cache.go", StartLine: 40,
	}
	return symbol, []exploreTarget{
		{node: primary, score: 1, source: source},
		{node: sibling, score: 0.4, source: "func (c *Cache) Warm() error { return nil }"},
	}
}

func decodeLocalizationEnvelope(t *testing.T, result *mcpgo.CallToolResult) localizationExploreEnvelope {
	t.Helper()
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("expected one text result: %#v", result)
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode localization envelope: %v\n%s", err, text)
	}
	return envelope
}

// Packing the prescribed body is worth its bytes only if the page can then
// drop the instruction to go and read it — inlining while still commanding the
// read leaves the caller doing both. So a refinement that cannot retire its
// read carries no body, exactly as before.
func TestARefinementThatCannotRetireItsReadCarriesNoBody(t *testing.T) {
	const task = "loading a stored record leaves the path empty"
	symbol, targets := prescribedBodyTargets(
		`func (d *DiskStorage) Load(path string) ([]byte, error) { return os.ReadFile(path) }`)

	result, _, _, finalized := buildLocalizationExploreResultForTaskFinalized(
		newLocalizationRefinementCompletion(symbol), task, targets, 1600)
	envelope := decodeLocalizationEnvelope(t, result)

	if finalized.State == localizationStateAnswerReady {
		t.Fatalf("this fixture is supposed to leave the read standing: %#v", finalized)
	}
	for _, evidence := range envelope.Evidence {
		if strings.TrimSpace(evidence.Source) != "" {
			t.Fatalf("a page that still commands the read also paid for the body: %#v", evidence)
		}
	}
}

// The prescribed body is the only body a retiring page adds: a refinement that
// grew by every candidate's source would spend its whole budget restating what
// the one bounded read was going to fetch.
func TestARetiringPageAddsThePrescribedBodyAndNoOther(t *testing.T) {
	symbol, targets := prescribedBodyTargets(
		`func (d *DiskStorage) Load(path string) ([]byte, error) { return os.ReadFile(path) }`)
	envelope := localizationExploreEnvelope{
		Completion: newLocalizationRefinementCompletion(symbol),
		Evidence: []localizationEvidence{
			{Rank: 1, ID: symbol, Name: "DiskStorage.Load", Kind: "method", File: "storage/disk.go", Line: 12},
			{Rank: 2, ID: "storage/cache.go::Cache.Warm", Name: "Cache.Warm", Kind: "method", File: "storage/cache.go", Line: 40},
		},
	}
	packed := localizationEnvelopePackingPrescribedBody(envelope, targets, []int{0, 1}, symbol, 6400)

	var prescribed, others int
	for _, evidence := range packed.Evidence {
		if strings.TrimSpace(evidence.Source) == "" {
			continue
		}
		if evidence.ID == symbol {
			prescribed++
			continue
		}
		others++
	}
	if prescribed != 1 {
		t.Fatalf("prescribed body was not packed: %#v", packed.Evidence)
	}
	if others != 0 {
		t.Fatalf("packing added %d body/bodies beyond the prescribed one", others)
	}
}

// Inlining the body is only half the fix. A page that ships the body and still
// commands the read leaves the caller doing both, so the instruction has to go
// at the same moment the body arrives.
func TestAPageCarryingThePrescribedBodyStopsCommandingTheRead(t *testing.T) {
	const symbol = "repo/storage/disk.go::DiskStorage.Load"
	const task = "DiskStorage.Load returns an empty path when loading a stored record"
	body := "func (s *DiskStorage) Load(path string) ([]byte, error) { return s.read(path) }"

	for _, prescription := range []struct {
		name       string
		completion localizationCompletion
	}{
		{name: "exact read", completion: newLocalizationExactReadCompletion(symbol, false)},
		{name: "refinement", completion: newLocalizationRefinementCompletion(symbol)},
	} {
		t.Run(prescription.name, func(t *testing.T) {
			envelope := localizationExploreEnvelope{
				Completion: prescription.completion,
				Evidence: []localizationEvidence{
					firstCallEvidence(symbol, "DiskStorage.Load", "storage/disk.go", body),
				},
			}
			prescribed := localizationPrescribedSymbol(envelope.Completion)
			if prescribed != symbol {
				t.Fatalf("prescribed symbol = %q, want %q", prescribed, symbol)
			}
			if !localizationPrescribedReadSatisfiedByEnvelope(task, prescribed, envelope) {
				t.Fatal("a packed, aligned body is exactly what the prescribed read would return")
			}

			retired := localizationCompletionRetiringPrescribedRead(envelope.Completion)
			if retired.State != localizationStateAnswerReady {
				t.Fatalf("a satisfied prescription stayed non-terminal: %#v", retired)
			}
			if retired.RequiredAction != "respond" || retired.AllowedToolCalls != 0 {
				t.Fatalf("a satisfied prescription still prescribes a call: %#v", retired)
			}
			if strings.Contains(retired.Instruction, "read(") {
				t.Fatalf("a satisfied prescription still instructs a read: %q", retired.Instruction)
			}
			if retired.ExactSymbol != "" || retired.refinementSymbol != "" {
				t.Fatalf("a retired prescription still names a symbol to read: %#v", retired)
			}
		})
	}
}

// A body that is not there cannot retire anything, whichever state prescribed
// the read.
func TestAnAbsentBodyNeverRetiresThePrescription(t *testing.T) {
	const symbol = "repo/storage/disk.go::DiskStorage.Load"
	const task = "DiskStorage.Load returns an empty path when loading a stored record"
	envelope := localizationExploreEnvelope{
		Completion: newLocalizationRefinementCompletion(symbol),
		Evidence: []localizationEvidence{
			firstCallEvidence(symbol, "DiskStorage.Load", "storage/disk.go", ""),
		},
	}
	if localizationPrescribedReadSatisfiedByEnvelope(task, symbol, envelope) {
		t.Fatal("a signature-only row still owes the caller the body")
	}
}

// The read is retired only when its answer is genuinely in hand. A body that
// never fit the budget leaves the prescription standing.
func TestAnUnpackedPrescriptionStillAsksForTheRead(t *testing.T) {
	const task = "DiskStorage.Load returns an empty path when loading a stored record"
	symbol, targets := prescribedBodyTargets(strings.Repeat("x", 64_000))
	completion := newLocalizationRefinementCompletion(symbol)

	_, _, _, finalized := buildLocalizationExploreResultForTaskFinalized(completion, task, targets, 1000)
	if finalized.State == localizationStateAnswerReady {
		t.Fatalf("a prescription was retired without its body: %#v", finalized)
	}
}
