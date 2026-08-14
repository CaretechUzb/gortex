package indexer

import (
	"fmt"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/search"
)

const symbolFTSNormalizationRebuildPending = "__gortex_rebuild_pending__"

// setSymbolFTSNormalization persists a mode marker for backends with a durable
// native symbol index. Other backends have no corpus whose mode can drift.
func (idx *Indexer) setSymbolFTSNormalization(target graph.Store, mode string) error {
	state, ok := target.(graph.SymbolFTSNormalizationState)
	if !ok {
		return nil
	}
	if err := state.SetSymbolFTSNormalization(idx.RepoPrefix(), mode); err != nil {
		return fmt.Errorf("persist symbol FTS normalization: %w", err)
	}
	return nil
}

// markSymbolFTSNormalization records the current mode only after the caller has
// completed an authoritative symbol-FTS replacement and finalization.
func (idx *Indexer) markSymbolFTSNormalization(target graph.Store) error {
	return idx.setSymbolFTSNormalization(target, search.FTSNormalizationMode())
}

// markSymbolFTSNormalizationPending invalidates any previously certified mode
// before a destructive replacement starts. The sentinel is deliberately not a
// valid normalization mode, so any failure forces the next warm reconcile to
// repeat the rebuild even when stemming is disabled.
func (idx *Indexer) markSymbolFTSNormalizationPending(target graph.Store) error {
	return idx.setSymbolFTSNormalization(target, symbolFTSNormalizationRebuildPending)
}

// reconcileSymbolFTSNormalization repairs a warm durable corpus whose
// persisted normalization marker differs from this process's immutable mode.
// The marker advances only after populateSymbolFTS atomically replaces the
// repository corpus. A crash between those operations leaves the old marker,
// so the next reconcile safely repeats the rebuild instead of serving a
// silently mismatched index.
func (idx *Indexer) reconcileSymbolFTSNormalization(reporter progress.Reporter) (bool, error) {
	state, ok := idx.graph.(graph.SymbolFTSNormalizationState)
	if !ok {
		return false, nil
	}
	want := search.FTSNormalizationMode()
	got, marked, err := state.GetSymbolFTSNormalization(idx.RepoPrefix())
	if err != nil {
		return false, fmt.Errorf("read symbol FTS normalization: %w", err)
	}
	if marked && got == want {
		return false, nil
	}
	// A missing marker is a corpus written before normalization existed. That
	// historical corpus is already correct for the disabled (empty) mode.
	if marked || want != "" {
		if err := idx.markSymbolFTSNormalizationPending(idx.graph); err != nil {
			return false, err
		}
		if err := idx.populateSymbolFTS(reporter); err != nil {
			return false, err
		}
	}
	if err := idx.markSymbolFTSNormalization(idx.graph); err != nil {
		return false, err
	}
	idx.logger.Info("indexer: symbol FTS normalization reconciled",
		zap.String("repo", idx.RepoPrefix()),
		zap.String("from", got),
		zap.String("to", want),
		zap.Bool("rebuilt", marked || want != ""))
	return marked || want != "", nil
}
