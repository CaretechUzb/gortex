package resolver

import (
	"bufio"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/zzet/gortex/internal/graph"
)

const (
	// Keep at most one compact replay page in memory. Larger legacy frontiers
	// spill behind the same identity boundary instead of paying temp-file and
	// gob setup for every small in-memory graph.
	resolveSpoolBufferBytes = 256 << 10
	resolveSpoolInlineBytes = 256 << 10
)

type unresolvedEdgeRecord struct {
	From     string
	To       string
	Kind     graph.EdgeKind
	FilePath string
	Line     int
}

// unresolvedLegacySpool gives stores without a native high-water pager the
// same stable pass boundary without a pointer snapshot. The initial predicate
// iterator is drained into compact identities before mutations begin; replay
// rehydrates each bounded page with one batched site lookup. One bounded page
// stays inline; larger frontiers spill to disk before resolution starts.
type unresolvedLegacySpool struct {
	store   graph.Store
	records []unresolvedEdgeRecord
	readAt  int
	bytes   int
	file    *os.File
	writer  *bufio.Writer
	encoder *gob.Encoder
	decoder *gob.Decoder
	count   int
}

func newUnresolvedLegacySpoolContext(ctx context.Context, store graph.Store) (*unresolvedLegacySpool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	spool := &unresolvedLegacySpool{store: store}
	for edge := range store.EdgesWithUnresolvedTarget() {
		if err := ctx.Err(); err != nil {
			spool.close()
			return nil, err
		}
		if edge == nil {
			continue
		}
		record := unresolvedEdgeRecord{
			From: edge.From, To: edge.To, Kind: edge.Kind,
			FilePath: edge.FilePath, Line: edge.Line,
		}
		if err := spool.append(record); err != nil {
			spool.close()
			return nil, err
		}
	}
	if spool.file != nil {
		if err := spool.beginRead(); err != nil {
			spool.close()
			return nil, err
		}
	}
	return spool, nil
}

func unresolvedEdgeRecordSize(record unresolvedEdgeRecord) int {
	return 96 + len(record.From) + len(record.To) + len(record.Kind) + len(record.FilePath)
}

func (s *unresolvedLegacySpool) append(record unresolvedEdgeRecord) error {
	recordBytes := unresolvedEdgeRecordSize(record)
	if s.file == nil && len(s.records) < resolvePendingPageRows && s.bytes+recordBytes <= resolveSpoolInlineBytes {
		s.records = append(s.records, record)
		s.bytes += recordBytes
		s.count++
		return nil
	}
	if s.file == nil {
		if err := s.spill(); err != nil {
			return err
		}
	}
	if err := s.encoder.Encode(&record); err != nil {
		return err
	}
	s.count++
	return nil
}

func (s *unresolvedLegacySpool) spill() error {
	file, err := os.CreateTemp("", "gortex-unresolved-*")
	if err != nil {
		return err
	}
	s.file = file
	s.writer = bufio.NewWriterSize(file, resolveSpoolBufferBytes)
	s.encoder = gob.NewEncoder(s.writer)
	for i := range s.records {
		if err := s.encoder.Encode(&s.records[i]); err != nil {
			return err
		}
	}
	s.records = nil
	s.bytes = 0
	return nil
}

func (s *unresolvedLegacySpool) beginRead() error {
	if err := s.writer.Flush(); err != nil {
		return err
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	s.decoder = gob.NewDecoder(bufio.NewReaderSize(s.file, resolveSpoolBufferBytes))
	return nil
}

func (s *unresolvedLegacySpool) close() {
	if s == nil {
		return
	}
	s.records = nil
	if s.file != nil {
		if s.writer != nil {
			_ = s.writer.Flush()
		}
		name := s.file.Name()
		_ = s.file.Close()
		_ = os.Remove(name)
		s.file = nil
	}
}

func (s *unresolvedLegacySpool) nextPage(maxRows, maxBytes int) ([]*graph.Edge, bool, error) {
	if maxRows <= 0 {
		maxRows = resolvePendingPageRows
	}
	if maxBytes <= 0 {
		maxBytes = resolvePendingPageBytes
	}
	if s.readAt >= s.count {
		return nil, true, nil
	}
	if s.file == nil {
		start := s.readAt
		bytesUsed := 0
		for s.readAt < len(s.records) && s.readAt-start < maxRows && (s.readAt == start || bytesUsed < maxBytes) {
			bytesUsed += unresolvedEdgeRecordSize(s.records[s.readAt])
			s.readAt++
		}
		return s.rehydrate(s.records[start:s.readAt]), s.readAt == s.count, nil
	}
	remaining := s.count - s.readAt
	targetRows := min(maxRows, remaining)
	records := make([]unresolvedEdgeRecord, 0, targetRows)
	bytesUsed := 0
	for len(records) < targetRows && bytesUsed < maxBytes {
		var record unresolvedEdgeRecord
		if err := s.decoder.Decode(&record); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, false, fmt.Errorf("unresolved edge spool ended after %d of %d records: %w", s.readAt+len(records), s.count, io.ErrUnexpectedEOF)
			}
			return nil, false, err
		}
		records = append(records, record)
		bytesUsed += unresolvedEdgeRecordSize(record)
	}
	s.readAt += len(records)
	return s.rehydrate(records), s.readAt == s.count, nil
}

func (s *unresolvedLegacySpool) rehydrate(records []unresolvedEdgeRecord) []*graph.Edge {
	sites := make([]graph.EdgeSite, 0, len(records))
	for _, record := range records {
		sites = append(sites, graph.EdgeSite{From: record.From, Line: record.Line, Kind: record.Kind})
	}
	candidates := s.store.GetEdgeCandidates(nil, sites)
	edges := make([]*graph.Edge, 0, len(records))
	for _, record := range records {
		for _, edge := range candidates.Site(record.From, record.Line, record.Kind) {
			if edge != nil && edge.To == record.To && edge.FilePath == record.FilePath {
				edges = append(edges, edge)
				break
			}
		}
	}
	return edges
}
