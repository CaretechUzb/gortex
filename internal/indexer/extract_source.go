package indexer

import (
	"fmt"

	"github.com/zzet/gortex/internal/parser"
)

// ExtractSource parses caller-owned bytes with the repository's configured
// language extractor without mutating the graph. Callers use it for bounded
// validation when a file deliberately has no indexed symbols.
func (idx *Indexer) ExtractSource(filePath string, src []byte) (*parser.ExtractionResult, error) {
	if idx == nil || idx.registry == nil {
		return nil, fmt.Errorf("source extractor is unavailable")
	}
	language, ok := idx.registry.DetectLanguageContent(filePath, src)
	if !ok {
		return nil, fmt.Errorf("no extractor for %q", filePath)
	}
	extractor, ok := idx.registry.GetByLanguage(language)
	if !ok {
		return nil, fmt.Errorf("extractor %q is unavailable", language)
	}
	return extractor.Extract(filePath, parser.ApplyPreParse(extractor, src))
}
