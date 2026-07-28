//go:build embeddings_gomlx

package embedding

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"

	"github.com/zzet/gortex/internal/platform"
)

const gomlxModelName = "sentence-transformers/all-MiniLM-L6-v2"

// GoMLXProvider uses Hugot with the XLA/GoMLX backend for transformer embeddings.
// XLA/PJRT plugin auto-downloads on first use (~100MB).
type GoMLXProvider struct {
	session   *hugot.Session
	pipeline  *pipelines.FeatureExtractionPipeline
	dims      int
	truncator *tokenTruncator
	mu        sync.Mutex
}

func newGoMLXProvider() (Provider, error) {
	session, err := hugot.NewXLASession(context.Background())
	if err != nil {
		return nil, fmt.Errorf("gomlx/xla session: %w", err)
	}

	modelPath, err := ensureGoMLXModel()
	if err != nil {
		_ = session.Destroy()
		return nil, fmt.Errorf("gomlx model: %w", err)
	}

	config := hugot.FeatureExtractionConfig{
		ModelPath: modelPath,
		Name:      "gortex-embeddings-gomlx",
		Options: []hugot.FeatureExtractionOption{
			pipelines.WithNormalization(),
		},
	}

	pipeline, err := hugot.NewPipeline(session, config)
	if err != nil {
		_ = session.Destroy()
		return nil, fmt.Errorf("gomlx pipeline: %w", err)
	}

	// Belt-and-suspenders token truncation: the XLA path's Rust tokenizer
	// already truncates, but keeping the exact client-side cap here matches the
	// pure-Go provider and preserves the same hard sequence bound. See hugot.go.
	truncator, terr := newTokenTruncator(modelPath)
	if truncator == nil {
		_ = session.Destroy()
		return nil, fmt.Errorf("gomlx token truncator: %w", terr)
	}
	if terr != nil {
		fmt.Fprintf(os.Stderr, "[gortex embedding] %v\n", terr)
	}

	return &GoMLXProvider{
		session:   session,
		pipeline:  pipeline,
		dims:      384,
		truncator: truncator,
	}, nil
}

func (p *GoMLXProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := p.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("gomlx returned no embeddings")
	}
	return vecs[0], nil
}

func (p *GoMLXProvider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	texts = p.truncator.TruncateAll(texts)

	output, err := p.pipeline.RunPipeline(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("gomlx run: %w", err)
	}
	if err := validateBatch("gomlx", texts, output.Embeddings, p.dims); err != nil {
		return nil, err
	}
	return output.Embeddings, nil
}

func (p *GoMLXProvider) Dimensions() int { return p.dims }

func (p *GoMLXProvider) Close() error {
	if p.session != nil {
		return p.session.Destroy()
	}
	return nil
}

func ensureGoMLXModel() (string, error) {
	dest := platform.ModelsDir()
	modelDir := filepath.Join(dest, "sentence-transformers_all-MiniLM-L6-v2")

	if _, err := os.Stat(filepath.Join(modelDir, "tokenizer.json")); err == nil {
		return modelDir, nil
	}

	path, err := hugot.DownloadModel(context.Background(), gomlxModelName, dest, hugot.NewDownloadOptions())
	if err != nil {
		return "", fmt.Errorf("download model: %w", err)
	}
	return path, nil
}
