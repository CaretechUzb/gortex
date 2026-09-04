package store_sqlite

import (
	"bytes"
	"database/sql"
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// synthesizedByProbe is the key the meta codecs write for a synthesizer stamp.
// It is a PREFILTER only: a blob without these bytes cannot carry the stamp,
// but one that has them still has to survive a real decode, because the same
// byte sequence can occur inside a value. Measured on a 4,585,606-edge store
// the probe over-selects by exactly one row out of 1,162,057 — cheap enough to
// pay on every row, and never the thing that decides.
//
// It matches both codecs: the flat encoder writes keys raw, and the JSON
// fallback writes them quoted, so the substring is present either way.
var synthesizedByProbe = []byte("synthesized_by")

var (
	metaKeySynthesizedBy = []byte("synthesized_by")
	metaKeyProvenance    = []byte("provenance")
	metaKeyVia           = []byte("via")
)

// SynthesizedEdgesSeq streams every edge carrying a synthesizer stamp.
//
// This is a deliberate FULL TABLE SCAN with no kind predicate. Restricting it
// to the kinds that carry the stamp today (references, extends, imports, reads,
// composes, overrides, calls, renders_child) would be faster and would silently
// under-report the moment a synthesizer stamps a ninth kind — the exact failure
// this projection exists to remove. Measured at 24.5s over 4.6M edges against
// the ~59s tool deadline, which buys the correctness outright.
//
// No ORDER BY: a rollup does not care about edge order, and sorting 4.6M rows
// to discard the ordering is pure cost. (The framework census orders by id
// because its consumers refetch by identity; this one aggregates and stops.)
func (s *Store) SynthesizedEdgesSeq() (iter.Seq[graph.SynthesizedEdge], func() error) {
	// Seq-local, not a field on Store: two callers ranging concurrently must
	// not observe each other's aborts.
	var scanErr error
	seq := func(yield func(graph.SynthesizedEdge) bool) {
		scanErr = nil
		// Bound to this handle's generation. Without the predicate the census
		// counts every generation's copy of a synthesized edge as a separate
		// firing, which is both a wrong total and a cross-view read.
		rows, err := s.db.Query(
			`SELECT kind, from_id, to_id, meta FROM edges WHERE view_gen = ?`, s.viewGen)
		if err != nil {
			scanErr = err
			panicOnFatal(err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var kind, from, to string
			var meta sql.RawBytes
			if err := rows.Scan(&kind, &from, &to, &meta); err != nil {
				scanErr = err
				panicOnFatal(err)
				return
			}
			if !bytes.Contains(meta, synthesizedByProbe) {
				continue
			}
			row := graph.SynthesizedEdge{From: from, To: to, Kind: graph.EdgeKind(kind)}
			if err := decodeSynthesizedMeta(meta, &row); err != nil {
				scanErr = err
				panicOnFatal(err)
				return
			}
			if row.SynthesizedBy == "" {
				continue
			}
			if !yield(row) {
				return
			}
		}
		// A driver failure part-way through the cursor ends the loop exactly
		// like a clean exhaust. panicOnFatal deliberately swallows a closed
		// store as a benign teardown race, so without recording it first the
		// caller would receive a truncated census and no way to know.
		if err := rows.Err(); err != nil {
			scanErr = err
			panicOnFatal(err)
		}
	}
	return seq, func() error { return scanErr }
}

// decodeSynthesizedMeta pulls the three census fields out of a meta blob,
// cursor-locally: the blob is never retained and no map is built for the flat
// codec, which is what keeps a 4.6M-row scan inside the deadline.
func decodeSynthesizedMeta(blob []byte, row *graph.SynthesizedEdge) error {
	if len(blob) == 0 || row == nil {
		return nil
	}
	if !isFlatMeta(blob) {
		meta, err := decodeMeta(blob)
		if err != nil {
			return err
		}
		row.SynthesizedBy, _ = meta["synthesized_by"].(string)
		row.Provenance, _ = meta["provenance"].(string)
		row.Via, _ = meta["via"].(string)
		return nil
	}

	d := &metaDecoder{buf: blob[2:]}
	count, err := d.readCount()
	if err != nil {
		return err
	}
	for i := 0; i < count; i++ {
		key, err := d.readRawString()
		if err != nil {
			return err
		}
		var field *string
		switch {
		case bytes.Equal(key, metaKeySynthesizedBy):
			field = &row.SynthesizedBy
		case bytes.Equal(key, metaKeyProvenance):
			field = &row.Provenance
		case bytes.Equal(key, metaKeyVia):
			field = &row.Via
		}
		if field == nil {
			if err := d.skipValue(); err != nil {
				return err
			}
			continue
		}
		value, err := d.readValue()
		if err != nil {
			return err
		}
		*field, _ = value.(string)
	}
	return nil
}

var _ graph.SynthesizedEdgeSequencer = (*Store)(nil)
