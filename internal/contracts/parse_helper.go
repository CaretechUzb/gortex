package contracts

import (
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/hcl"
	gosrc "github.com/zzet/gortex/internal/parser/tsitter/golang"
)

// ParseTreeForLang produces a fresh ParseTree for src using the
// grammar registered for lang. Returns nil if the language doesn't
// have a grammar Phase 1 supports — currently "go" and "hcl". Used by
// the indexer's incremental re-walk path (extractContracts) which
// doesn't have a tree from the language extractor.
//
// The caller must Release() the returned tree when done.
func ParseTreeForLang(lang string, src []byte) *parser.ParseTree {
	if len(src) == 0 {
		return nil
	}
	var grammar *sitter.Language
	switch lang {
	case "go":
		grammar = gosrc.GetLanguage()
	case "hcl":
		grammar = hcl.GetLanguage()
	default:
		return nil
	}
	tree, err := parser.ParseFile(src, grammar)
	if err != nil || tree == nil {
		return nil
	}
	return parser.NewParseTree(tree, src, lang)
}
