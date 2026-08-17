package localizationauth

import (
	"encoding/json"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTakeArgumentStripsReservedField(t *testing.T) {
	token, ok := NewToken()
	if !ok {
		t.Fatal("NewToken failed")
	}
	arguments := map[string]any{"operation": "localize", ArgumentKey: token}
	if got := TakeArgument(arguments); got != token {
		t.Fatalf("TakeArgument = %q, want %q", got, token)
	}
	if _, exists := arguments[ArgumentKey]; exists {
		t.Fatal("reserved auth argument was not stripped")
	}
	if arguments["operation"] != "localize" {
		t.Fatal("ordinary arguments were mutated")
	}
	arguments[ArgumentKey] = "forged"
	if got := TakeArgument(arguments); got != "" {
		t.Fatalf("malformed token accepted: %q", got)
	}
	if _, exists := arguments[ArgumentKey]; exists {
		t.Fatal("malformed reserved argument was not stripped")
	}
}

func TestReceiptRoundTripPreservesFinalResponseVerbatim(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	token, ok := NewToken()
	if !ok {
		t.Fatal("NewToken failed")
	}
	want := Receipt{
		FinalResponse:   "FILES\n#1 exact.go\n\nSYMBOLS\n#1 Exact.Call\n\nEVIDENCE\n#1 exact.go:9 — unchanged\n",
		ContractVersion: 2,
		Enforceable:     true,
	}
	if !Publish(token, want) {
		t.Fatal("Publish failed")
	}
	got, ok := Consume(token)
	if !ok {
		t.Fatal("Consume failed")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("receipt mismatch\n got: %#v\nwant: %#v", got, want)
	}
	if _, ok := Consume(token); ok {
		t.Fatal("receipt was consumed more than once")
	}
}

func TestReceiptRejectsUntrustedOrInvalidRecords(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	valid, ok := NewToken()
	if !ok {
		t.Fatal("NewToken failed")
	}
	for name, token := range map[string]string{
		"empty":     "",
		"too_short": "abcd",
		"uppercase": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"non_hex":   "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
	} {
		t.Run(name, func(t *testing.T) {
			if Publish(token, Receipt{FinalResponse: "answer", ContractVersion: 2}) {
				t.Fatal("accepted invalid token")
			}
		})
	}
	if Publish(valid, Receipt{FinalResponse: "", ContractVersion: 2}) {
		t.Fatal("accepted empty final response")
	}
	if Publish(valid, Receipt{FinalResponse: "answer", ContractVersion: 1}) {
		t.Fatal("accepted legacy contract")
	}
	if _, ok := Consume(valid); ok {
		t.Fatal("invalid publish left a consumable receipt")
	}
}

func TestReceiptConcurrentConsumeHasSingleWinner(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	token, ok := NewToken()
	if !ok {
		t.Fatal("NewToken failed")
	}
	if !Publish(token, Receipt{FinalResponse: "answer", ContractVersion: 2}) {
		t.Fatal("Publish failed")
	}

	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if receipt, ok := Consume(token); ok && receipt.FinalResponse == "answer" {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("consume winners = %d, want 1", got)
	}
}

func TestReceiptStorageIsBounded(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	for range maxReceiptFiles + 24 {
		token, ok := NewToken()
		if !ok {
			t.Fatal("NewToken failed")
		}
		if !Publish(token, Receipt{FinalResponse: "answer", ContractVersion: 2}) {
			t.Fatal("Publish failed")
		}
	}
	entries, err := os.ReadDir(receiptRoot())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(entries); got > maxReceiptFiles {
		t.Fatalf("receipt files = %d, hard cap = %d", got, maxReceiptFiles)
	}
}

func TestReceiptRejectsStaleEnvelope(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	token, ok := NewToken()
	if !ok {
		t.Fatal("NewToken failed")
	}
	if !Publish(token, Receipt{FinalResponse: "answer", ContractVersion: 2}) {
		t.Fatal("Publish failed")
	}
	path, digest, ok := receiptPath(token)
	if !ok {
		t.Fatal("receiptPath failed")
	}
	stale := receiptEnvelope{
		Version:         receiptVersion,
		TokenDigest:     digest,
		Receipt:         Receipt{FinalResponse: "answer", ContractVersion: 2},
		CreatedUnixNano: time.Now().Add(-receiptTTL - time.Minute).UnixNano(),
	}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := Consume(token); ok {
		t.Fatal("accepted stale receipt")
	}
}
