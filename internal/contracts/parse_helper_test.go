package contracts

import "testing"

func TestParseTreeForLang_HCL(t *testing.T) {
	src := []byte(`resource "aws_lambda_function" "processor" {}`)
	tree := ParseTreeForLang("hcl", src)
	defer tree.Release()
	if tree == nil || tree.Tree() == nil {
		t.Fatalf("expected a non-nil hcl tree")
	}
	if got := tree.Lang(); got != "hcl" {
		t.Errorf("expected Lang()==\"hcl\", got %q", got)
	}
}

func TestParseTreeForLang_UnsupportedLanguage(t *testing.T) {
	if tree := ParseTreeForLang("python", []byte("x = 1")); tree != nil {
		t.Errorf("expected nil for an unsupported language, got %+v", tree)
	}
}

func TestParseTreeForLang_EmptySource(t *testing.T) {
	if tree := ParseTreeForLang("hcl", nil); tree != nil {
		t.Errorf("expected nil for empty source, got %+v", tree)
	}
}
