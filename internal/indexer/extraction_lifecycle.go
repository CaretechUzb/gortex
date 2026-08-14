package indexer

import (
	"errors"
	"sync"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/parser"
)

// ErrIndexerClosed is returned when extraction is attempted after lifecycle
// teardown has begun.
var ErrIndexerClosed = errors.New("indexer is closed")

// extractionLifecycle is the admission/close boundary shared by every parse
// route. Admission and close are serialized under mu so close cannot race a
// WaitGroup Add; close waits for timed-out background extracts as well as
// synchronous and crash-worker requests.
type extractionLifecycle struct {
	mu     sync.Mutex
	cond   *sync.Cond
	closed bool
	active int
}

func (l *extractionLifecycle) admit() (func(), error) {
	l.mu.Lock()
	if l.cond == nil {
		l.cond = sync.NewCond(&l.mu)
	}
	if l.closed {
		l.mu.Unlock()
		return nil, ErrIndexerClosed
	}
	l.active++
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.active--
			if l.active == 0 {
				l.cond.Broadcast()
			}
			l.mu.Unlock()
		})
	}, nil
}

func (l *extractionLifecycle) closeAndWait() {
	l.mu.Lock()
	if l.cond == nil {
		l.cond = sync.NewCond(&l.mu)
	}
	l.closed = true
	for l.active > 0 {
		l.cond.Wait()
	}
	l.mu.Unlock()
}

// initializeExtractionOptions loads repository-local parser configuration once
// per Indexer lifetime. The parser value is immutable and safe for concurrent
// extraction requests.
func (idx *Indexer) initializeExtractionOptions(root string) {
	idx.extractionOptionsOnce.Do(func() {
		opts := parser.NewExtractionOptions(config.LoadLocalTemporalEnvHelpers(root))
		idx.extractionOptions.Store(&opts)
	})
}

func (idx *Indexer) extractionOptionsValue() parser.ExtractionOptions {
	if opts := idx.extractionOptions.Load(); opts != nil {
		return *opts
	}
	return parser.ExtractionOptions{}
}

// ExtractBuffer parses an in-memory file through the same repository options
// and lifecycle admission used by disk indexing. MCP overlays use this path to
// maintain base/overlay parity.
func (idx *Indexer) ExtractBuffer(language, relPath string, src []byte) (*parser.ExtractionResult, error) {
	if idx == nil || idx.registry == nil {
		return nil, errors.New("indexer has no parser registry")
	}
	ext, ok := idx.registry.GetByLanguage(language)
	if !ok || ext == nil {
		return nil, errors.New("indexer has no extractor for language " + language)
	}
	release, err := idx.extractionLifecycle.admit()
	if err != nil {
		return nil, err
	}
	defer release()
	return safeExtractWithOptions(ext, relPath, src, idx.extractionOptionsValue())
}

// Close rejects new extraction admissions, waits for every admitted request,
// then terminates the long-lived crash-isolation workers. It is idempotent.
func (idx *Indexer) Close() {
	if idx == nil {
		return
	}
	idx.extractionLifecycle.closeAndWait()
	idx.closeParsePool()
}
