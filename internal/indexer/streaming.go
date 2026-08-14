package indexer

import (
	"fmt"
	"os"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// extractStreaming runs a StreamingExtractor over a file by handle so the whole
// file is never read into memory. It opens the file as an io.ReaderAt, hands it
// to the extractor with an emit sink that accumulates the produced nodes/edges,
// and recovers any per-document panic (mirroring safeExtract) so a malformed
// asset isolates to a failed file rather than crashing the pass.
func (idx *Indexer) extractStreaming(se parser.StreamingExtractor, path, relPath string) (res *parser.ExtractionResult, err error) {
	releaseLifecycle, admissionErr := idx.extractionLifecycle.admit()
	if admissionErr != nil {
		return nil, admissionErr
	}
	defer releaseLifecycle()

	f, oerr := os.Open(path)
	if oerr != nil {
		return nil, oerr
	}
	defer func() { _ = f.Close() }()
	fi, serr := f.Stat()
	if serr != nil {
		return nil, serr
	}

	res = &parser.ExtractionResult{}
	defer func() {
		if rec := recover(); rec != nil {
			res = nil
			err = fmt.Errorf("streaming extractor panicked on %s: %v", relPath, rec)
		}
	}()

	emit := func(n *graph.Node, edges []*graph.Edge) {
		if n != nil {
			res.Nodes = append(res.Nodes, n)
		}
		if len(edges) > 0 {
			res.Edges = append(res.Edges, edges...)
		}
	}
	if eerr := se.ExtractStream(relPath, f, fi.Size(), emit); eerr != nil {
		return res, eerr
	}
	return res, nil
}
