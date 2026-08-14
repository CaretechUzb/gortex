package indexer

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/crashpool"
)

// maxAdHocExtractSourceBytes is a hard ceiling for caller-owned, out-of-index
// validation. Normal indexing may admit larger files under configuration, but
// interactive recovery must not turn one MCP request into an unbounded parse.
const maxAdHocExtractSourceBytes = 1 << 20 // 1 MiB

// ExtractSource parses caller-owned bytes through the indexer's normal
// admission, minified-content, timeout, panic-recovery, and optional
// crash-isolation path without mutating graph state. The caller owns the
// returned result and must call ReleaseTree when it is no longer needed.
func (idx *Indexer) ExtractSource(ctx context.Context, filePath string, src []byte) (*parser.ExtractionResult, error) {
	if idx == nil || idx.registry == nil {
		return nil, fmt.Errorf("indexer source extraction is unavailable")
	}
	if ctx == nil {
		return nil, fmt.Errorf("indexer source extraction requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := int64(maxAdHocExtractSourceBytes)
	if idx.config.MaxFileSize > 0 && idx.config.MaxFileSize < limit {
		limit = idx.config.MaxFileSize
	}
	if int64(len(src)) > limit {
		return nil, fmt.Errorf("source exceeds bounded extraction limit (%d bytes)", limit)
	}

	language, ok := idx.registry.DetectLanguageContent(filePath, src)
	if !ok {
		return nil, fmt.Errorf("unsupported source language for %s", filePath)
	}
	extractor, ok := idx.registry.GetByLanguage(language)
	if !ok {
		return nil, fmt.Errorf("no extractor registered for %s", language)
	}
	prepared := parser.ApplyPreParse(extractor, src)
	if int64(len(prepared)) > limit {
		return nil, fmt.Errorf("prepared source exceeds bounded extraction limit (%d bytes)", limit)
	}

	var pool *crashpool.Pool
	var quarantine *crashpool.Quarantine
	if idx.crashIsolationEnabled() {
		pool, quarantine = idx.sharedParsePool()
	}
	nativeAdmission := newNativeParseExtractionAdmission(0, nil, idx.nativeParseAdmission.Load())
	path := filePath
	if !filepath.IsAbs(path) && idx.rootPath != "" {
		path = filepath.Join(idx.rootPath, filepath.FromSlash(filePath))
	}
	result, skipped, err := idx.extractFileCtx(
		ctx, nativeAdmission, pool, quarantine,
		path, filePath, language, extractor, prepared,
	)
	if err != nil {
		if result != nil {
			result.ReleaseTree()
		}
		return nil, err
	}
	if skipped {
		if result != nil {
			result.ReleaseTree()
		}
		return nil, fmt.Errorf("source extraction refused by index safeguards")
	}
	if err := ctx.Err(); err != nil {
		if result != nil {
			result.ReleaseTree()
		}
		return nil, err
	}
	return result, nil
}
