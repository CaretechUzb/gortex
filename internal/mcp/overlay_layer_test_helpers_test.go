package mcp

import (
	"context"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

type overlayLayerFileCall struct {
	path  string
	scope graph.LocalizationNodeScope
	limit int
}

type overlayLayerNameCall struct {
	name  string
	scope graph.LocalizationNodeScope
	limit int
}

type overlayLayerTestStore struct {
	graph.Store

	mu              sync.Mutex
	fileCalls       []overlayLayerFileCall
	nameCalls       []overlayLayerNameCall
	legacyFileReads int
	legacyNameReads int
	fileFn          func(context.Context, string, graph.LocalizationNodeScope, int) (graph.BoundedNodeProjection, error)
	nameFn          func(context.Context, string, graph.LocalizationNodeScope, int) (graph.BoundedNodeProjection, error)
}

func (s *overlayLayerTestStore) FindFileNodesBounded(
	ctx context.Context,
	filePath string,
	scope graph.LocalizationNodeScope,
	limit int,
) (graph.BoundedNodeProjection, error) {
	s.mu.Lock()
	s.fileCalls = append(s.fileCalls, overlayLayerFileCall{path: filePath, scope: scope, limit: limit})
	fn := s.fileFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, filePath, scope, limit)
	}
	reader, ok := s.Store.(graph.BoundedFileNodeReader)
	if !ok {
		return graph.BoundedNodeProjection{}, graph.ErrBoundedLocalizationUnavailable
	}
	return reader.FindFileNodesBounded(ctx, filePath, scope, limit)
}

func (s *overlayLayerTestStore) FindNodesByNameBounded(
	ctx context.Context,
	name string,
	scope graph.LocalizationNodeScope,
	limit int,
) (graph.BoundedNodeProjection, error) {
	s.mu.Lock()
	s.nameCalls = append(s.nameCalls, overlayLayerNameCall{name: name, scope: scope, limit: limit})
	fn := s.nameFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, name, scope, limit)
	}
	reader, ok := s.Store.(graph.BoundedExactNameReader)
	if !ok {
		return graph.BoundedNodeProjection{}, graph.ErrBoundedLocalizationUnavailable
	}
	return reader.FindNodesByNameBounded(ctx, name, scope, limit)
}

func (s *overlayLayerTestStore) GetFileNodes(filePath string) []*graph.Node {
	s.mu.Lock()
	s.legacyFileReads++
	s.mu.Unlock()
	return s.Store.GetFileNodes(filePath)
}

func (s *overlayLayerTestStore) FindNodesByName(name string) []*graph.Node {
	s.mu.Lock()
	s.legacyNameReads++
	s.mu.Unlock()
	return s.Store.FindNodesByName(name)
}

func (s *overlayLayerTestStore) snapshotFileCalls() ([]overlayLayerFileCall, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]overlayLayerFileCall(nil), s.fileCalls...), s.legacyFileReads
}

func (s *overlayLayerTestStore) snapshotNameCalls() []overlayLayerNameCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]overlayLayerNameCall(nil), s.nameCalls...)
}

func (s *overlayLayerTestStore) snapshotLegacyReads() (fileReads, nameReads int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.legacyFileReads, s.legacyNameReads
}

type overlayLayerLegacyStore struct {
	graph.Store
}

type overlayLayerTestExtractor struct {
	mu      sync.Mutex
	calls   int
	extract func(int, string, []byte) (*parser.ExtractionResult, error)
}

func (e *overlayLayerTestExtractor) Language() string     { return "go" }
func (e *overlayLayerTestExtractor) Extensions() []string { return []string{".go"} }

func (e *overlayLayerTestExtractor) Extract(filePath string, content []byte) (*parser.ExtractionResult, error) {
	e.mu.Lock()
	call := e.calls
	e.calls++
	fn := e.extract
	e.mu.Unlock()
	if fn == nil {
		return &parser.ExtractionResult{}, nil
	}
	return fn(call, filePath, content)
}

func (e *overlayLayerTestExtractor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}
