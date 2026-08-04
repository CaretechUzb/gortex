package llm

import (
	"strings"
	"testing"
)

func TestFailureSnippet(t *testing.T) {
	cases := []struct {
		name           string
		stdout, stderr string
		want           string
	}{
		{"prefers stderr", "some output", "auth required", "auth required"},
		{"falls back to stdout", "Error: Reached max turns (1)", "", "Error: Reached max turns (1)"},
		{"whitespace-only stderr is empty", "Error: boom", "  \n", "Error: boom"},
		{"both empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FailureSnippet([]byte(tc.stdout), []byte(tc.stderr)); got != tc.want {
				t.Errorf("FailureSnippet=%q want %q", got, tc.want)
			}
		})
	}
}

func TestExtractJSON_Variants(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  string
		okErr bool // true means "expect not-found"
	}{
		{"raw", `{"a":1}`, `{"a":1}`, false},
		{"with prose", "Here:\n{\"a\":1}\nend", `{"a":1}`, false},
		{"markdown fence", "```json\n{\"a\":1}\n```", `{"a":1}`, false},
		{"plain fence", "```\n{\"a\":1}\n```", `{"a":1}`, false},
		{"array", `[1,2,3]`, `[1,2,3]`, false},
		{"nested braces in strings", `{"k":"a{b}c","v":1}`, `{"k":"a{b}c","v":1}`, false},
		{"no JSON", "I cannot help with that.", "", true},
		{"truncated", `{"a":1,`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ExtractJSON(tc.in)
			if tc.okErr && ok {
				t.Fatalf("ExtractJSON should have failed; got %q", got)
			}
			if !tc.okErr && !ok {
				t.Fatalf("ExtractJSON unexpectedly failed for %q", tc.in)
			}
			if !tc.okErr && got != tc.want {
				t.Errorf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestAppendSchemaInstruction_KeepsExistingSystem(t *testing.T) {
	out := AppendSchemaInstruction("be terse", ShapeExpandTerms, nil)
	if !strings.HasPrefix(out, "be terse") {
		t.Errorf("rider must follow original system: %q", out)
	}
	if !strings.Contains(out, "JSON Schema") {
		t.Errorf("rider must reference JSON Schema: %q", out)
	}
	if !strings.Contains(out, "terms") {
		t.Errorf("rider must embed shape property name: %q", out)
	}
}

func TestAppendSchemaInstruction_Freeform(t *testing.T) {
	if out := AppendSchemaInstruction("base", ShapeFreeform, nil); out != "base" {
		t.Errorf("ShapeFreeform must leave the prompt untouched: %q", out)
	}
}

func TestAppendSchemaInstruction_EmptyPrompt(t *testing.T) {
	out := AppendSchemaInstruction("", ShapeRerankOrder, nil)
	if strings.HasPrefix(out, "\n") {
		t.Errorf("empty prompt must not leave a leading separator: %q", out)
	}
	if !strings.Contains(out, "order") {
		t.Errorf("rider must embed shape property name: %q", out)
	}
}

func TestSnippet_TruncatesLong(t *testing.T) {
	long := strings.Repeat("x", 1000)
	out := Snippet([]byte(long))
	if len(out) < 100 {
		t.Errorf("Snippet=%d chars; want a truncated payload", len(out))
	}
	if !strings.HasSuffix(out, "…") {
		t.Error("long Snippet must end with the truncation marker")
	}
}

func TestSnippet_ShortUnchanged(t *testing.T) {
	if out := Snippet([]byte("  short  ")); out != "short" {
		t.Errorf("Snippet must trim and pass short input through: %q", out)
	}
}
