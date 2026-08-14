package parser

import (
	"reflect"
	"testing"
)

type optionsTestExtractor struct {
	seen []string
}

func (e *optionsTestExtractor) Language() string     { return "test" }
func (e *optionsTestExtractor) Extensions() []string { return []string{".test"} }
func (e *optionsTestExtractor) Extract(string, []byte) (*ExtractionResult, error) {
	return &ExtractionResult{}, nil
}
func (e *optionsTestExtractor) ExtractWithOptions(_ string, _ []byte, opts ExtractionOptions) (*ExtractionResult, error) {
	e.seen = opts.TemporalEnvHelpers()
	return &ExtractionResult{}, nil
}

type baseTestExtractor struct {
	called bool
}

func (e *baseTestExtractor) Language() string     { return "base" }
func (e *baseTestExtractor) Extensions() []string { return []string{".base"} }
func (e *baseTestExtractor) Extract(string, []byte) (*ExtractionResult, error) {
	e.called = true
	return &ExtractionResult{}, nil
}

func TestExtractionOptionsNormalizeAndDefend(t *testing.T) {
	input := []string{" HelperB ", "HelperA", "HelperB", "", "helpera"}
	opts := NewExtractionOptions(input)
	input[0] = "mutated"

	want := []string{"HelperA", "HelperB", "helpera"}
	got := opts.TemporalEnvHelpers()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TemporalEnvHelpers() = %#v, want %#v", got, want)
	}
	got[0] = "mutated"
	if again := opts.TemporalEnvHelpers(); !reflect.DeepEqual(again, want) {
		t.Fatalf("accessor exposed mutable state: %#v", again)
	}
}

func TestExtractDispatchesOptionalCapability(t *testing.T) {
	ext := &optionsTestExtractor{}
	_, err := Extract(ext, "x.test", nil, NewExtractionOptions([]string{"CustomEnv"}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ext.seen, []string{"CustomEnv"}) {
		t.Fatalf("options seen = %#v", ext.seen)
	}
}

func TestExtractFallsBackToBaseExtractor(t *testing.T) {
	ext := &baseTestExtractor{}
	if _, err := Extract(ext, "x.base", nil, NewExtractionOptions([]string{"ignored"})); err != nil {
		t.Fatal(err)
	}
	if !ext.called {
		t.Fatal("base Extract was not called")
	}
}
