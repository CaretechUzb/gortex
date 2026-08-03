package store_sqlite

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/zzet/gortex/internal/graph"
)

// JSONB bulk ingest.
//
// The placeholder AddBatch writer binds every column of every row as its own
// SQL variable; on the modernc driver each bind copies its argument into the
// VM (conn.bind + memmove dominate cold-load ingest CPU). This path replaces
// thousands of binds per statement with exactly two bounded payloads — a
// JSONB array of scalar rows plus a raw metadata-BLOB arena the statement
// slices with substr() — while sharing the production encoders, conflict
// clauses, and skip predicates, so the resulting rows are byte-identical to
// the placeholder writer's (asserted by the reopen-parity test).
//
// When mutation receipts are active, bounded RETURNING clauses collect the
// exact changed identities from the same JSONB statements. The runtime must
// expose jsonb(); GORTEX_SQLITE_JSONB_INGEST=0 forces the placeholder path.
const (
	jsonbIngestMaxPayload = sqliteBatchMaxBoundBytes
	jsonbIngestNodeRows   = 4096
	jsonbIngestEdgeRows   = 8192
)

const jsonbNodeIngestSQL = `INSERT INTO nodes (` + nodeInsertColumns + `)
SELECT
	json_extract(row.value, '$[0]'), json_extract(row.value, '$[1]'),
	json_extract(row.value, '$[2]'), json_extract(row.value, '$[3]'),
	json_extract(row.value, '$[4]'), json_extract(row.value, '$[5]'),
	json_extract(row.value, '$[6]'), json_extract(row.value, '$[7]'),
	json_extract(row.value, '$[8]'), json_extract(row.value, '$[9]'),
	json_extract(row.value, '$[10]'), json_extract(row.value, '$[11]'),
	json_extract(row.value, '$[12]'), json_extract(row.value, '$[13]'),
	json_extract(row.value, '$[14]'), json_extract(row.value, '$[15]'),
	json_extract(row.value, '$[16]'), json_extract(row.value, '$[17]'),
	json_extract(row.value, '$[18]'), json_extract(row.value, '$[19]'),
	json_extract(row.value, '$[20]'), json_extract(row.value, '$[21]'),
	json_extract(row.value, '$[22]'), json_extract(row.value, '$[23]'),
	json_extract(row.value, '$[24]'), json_extract(row.value, '$[25]'),
	json_extract(row.value, '$[26]'), json_extract(row.value, '$[27]'),
	json_extract(row.value, '$[28]'),
	CASE WHEN json_type(row.value, '$[29]') = 'null' THEN NULL
		ELSE substr(?2, json_extract(row.value, '$[29]') + 1, json_extract(row.value, '$[30]')) END,
	json_extract(row.value, '$[31]'), json_extract(row.value, '$[32]'),
	json_extract(row.value, '$[33]'), json_extract(row.value, '$[34]'),
	json_extract(row.value, '$[35]')
FROM jsonb_each(jsonb(?1)) AS row
WHERE true` + nodeUpsertClause

const jsonbEdgeIngestSQL = `INSERT OR IGNORE INTO edges (` + edgeInsertColumns + `)
SELECT
	json_extract(row.value, '$[0]'), json_extract(row.value, '$[1]'),
	json_extract(row.value, '$[2]'), json_extract(row.value, '$[3]'),
	json_extract(row.value, '$[4]'), json_extract(row.value, '$[5]'),
	json_extract(row.value, '$[6]'), json_extract(row.value, '$[7]'),
	json_extract(row.value, '$[8]'), json_extract(row.value, '$[9]'),
	CASE WHEN json_type(row.value, '$[10]') = 'null' THEN NULL
		ELSE substr(?2, json_extract(row.value, '$[10]') + 1, json_extract(row.value, '$[11]')) END,
	json_extract(row.value, '$[12]'), json_extract(row.value, '$[13]'),
	json_extract(row.value, '$[14]')
FROM jsonb_each(jsonb(?1)) AS row`

// jsonbIngestEnabled is the operator kill switch: GORTEX_SQLITE_JSONB_INGEST=0
// (or "false") forces the placeholder writer everywhere.
func jsonbIngestEnabled() bool {
	v := os.Getenv("GORTEX_SQLITE_JSONB_INGEST")
	return v != "0" && !strings.EqualFold(v, "false")
}

// jsonbIngestSupport caches the process-wide jsonb() availability probe:
// 0 = unprobed, 1 = supported, -1 = unsupported. The bundled SQLite build is
// a process constant, so one probe answers for every store.
var jsonbIngestSupport atomic.Int32

func jsonbIngestSupported(tx *sql.Tx) bool {
	switch jsonbIngestSupport.Load() {
	case 1:
		return true
	case -1:
		return false
	}
	var kind string
	if err := tx.QueryRow(`SELECT typeof(jsonb('[]'))`).Scan(&kind); err != nil || kind == "" {
		jsonbIngestSupport.Store(-1)
		return false
	}
	jsonbIngestSupport.Store(1)
	return true
}

// jsonbIngestValue normalizes the driver-facing argument types produced by
// appendNodeInsertArgs / appendEdgeInsertArgs into their JSON equivalents so
// json_extract yields the same SQLite storage classes the placeholder binds
// produce.
func jsonbIngestValue(value any) any {
	switch value := value.(type) {
	case sql.NullString:
		if !value.Valid {
			return nil
		}
		return value.String
	case sql.NullBool:
		if !value.Valid {
			return nil
		}
		return value.Bool
	case sql.NullInt64:
		if !value.Valid {
			return nil
		}
		return value.Int64
	default:
		return value
	}
}

// appendJSONBIngestRow encodes one row's args into the JSON payload, swapping
// the single BLOB argument at metaIndex for an (offset, length) pair into the
// raw blob arena. Returns false (without consuming the row) when adding it
// would exceed the bounded payload; a first row is always admitted so cursor
// progress is guaranteed.
func appendJSONBIngestRow(payload, blobs *bytes.Buffer, row []any, metaIndex, rows int) (bool, error) {
	meta, ok := row[metaIndex].([]byte)
	if row[metaIndex] != nil && !ok {
		return false, fmt.Errorf("metadata argument %d has type %T, want []byte", metaIndex, row[metaIndex])
	}
	metaPresent := meta != nil
	metaOffset := blobs.Len()

	row = append(row, nil)
	copy(row[metaIndex+2:], row[metaIndex+1:len(row)-1])
	if metaPresent {
		row[metaIndex] = metaOffset
		row[metaIndex+1] = len(meta)
	} else {
		row[metaIndex] = nil
		row[metaIndex+1] = nil
	}
	for i := range row {
		if i == metaIndex || i == metaIndex+1 {
			continue
		}
		row[i] = jsonbIngestValue(row[i])
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		return false, err
	}
	separator := 0
	if rows > 0 {
		separator = 1
	}
	boundBytes := payload.Len() + separator + len(encoded) + 1 + blobs.Len() + len(meta)
	if rows > 0 && boundBytes > jsonbIngestMaxPayload {
		return false, nil
	}
	if rows > 0 {
		payload.WriteByte(',')
	}
	payload.Write(encoded)
	if metaPresent {
		blobs.Write(meta)
	}
	return true, nil
}

func nextJSONBNodePayload(nodes []*graph.Node, start int) (jsonPayload, blobPayload []byte, next, rows int, err error) {
	var payload, blobs bytes.Buffer
	payload.Grow(256 << 10)
	blobs.Grow(128 << 10)
	payload.WriteByte('[')
	// Keep the raw-BLOB bind non-NULL even for a valid zero-length blob. Row
	// offsets are zero-based into this buffer; SQLite substr is one-based.
	blobs.WriteByte(0)
	pos := start
	for pos < len(nodes) && rows < jsonbIngestNodeRows {
		node := nodes[pos]
		if node == nil || node.ID == "" || graph.IsProxyNode(node) {
			pos++
			continue
		}
		args, appendErr := appendNodeInsertArgs(nil, node)
		if appendErr != nil {
			return nil, nil, start, 0, appendErr
		}
		added, appendErr := appendJSONBIngestRow(&payload, &blobs, args, 29, rows)
		if appendErr != nil {
			return nil, nil, start, 0, appendErr
		}
		if !added {
			break
		}
		pos++
		rows++
	}
	payload.WriteByte(']')
	return payload.Bytes(), blobs.Bytes(), pos, rows, nil
}

func nextJSONBEdgePayload(edges []*graph.Edge, start int) (jsonPayload, blobPayload []byte, next, rows int, err error) {
	var payload, blobs bytes.Buffer
	payload.Grow(256 << 10)
	blobs.Grow(128 << 10)
	payload.WriteByte('[')
	blobs.WriteByte(0)
	pos := start
	for pos < len(edges) && rows < jsonbIngestEdgeRows {
		edge := edges[pos]
		if edge == nil || graph.IsProxyID(edge.From) || graph.IsProxyID(edge.To) {
			pos++
			continue
		}
		args, appendErr := appendEdgeInsertArgs(nil, edge)
		if appendErr != nil {
			return nil, nil, start, 0, appendErr
		}
		added, appendErr := appendJSONBIngestRow(&payload, &blobs, args, 10, rows)
		if appendErr != nil {
			return nil, nil, start, 0, appendErr
		}
		if !added {
			break
		}
		pos++
		rows++
	}
	payload.WriteByte(']')
	return payload.Bytes(), blobs.Bytes(), pos, rows, nil
}

// insertNodeChunksJSONBTx is the JSONB counterpart of
// insertNodeChunksTxLimited. When returnChanged is true, RETURNING supplies the
// exact per-ID multiplicities needed by mutation receipts without abandoning
// the bounded two-bind JSONB statements.
func insertNodeChunksJSONBTx(
	tx *sql.Tx,
	nodes []*graph.Node,
	returnChanged bool,
) (rowsChanged, statements int, changedIDs map[string]int, err error) {
	query := jsonbNodeIngestSQL
	if returnChanged {
		query += " RETURNING id"
		changedIDs = make(map[string]int)
	}
	stmt, err := tx.Prepare(query)
	if err != nil {
		return 0, 0, changedIDs, err
	}
	defer stmt.Close()
	for pos := 0; pos < len(nodes); {
		payload, blobs, next, rows, encodeErr := nextJSONBNodePayload(nodes, pos)
		if encodeErr != nil {
			return rowsChanged, statements, changedIDs, encodeErr
		}
		pos = next
		if rows == 0 {
			continue
		}
		if returnChanged {
			returned, queryErr := stmt.Query(payload, blobs)
			statements++
			if queryErr != nil {
				return rowsChanged, statements, changedIDs, queryErr
			}
			for returned.Next() {
				var id string
				if scanErr := returned.Scan(&id); scanErr != nil {
					_ = returned.Close()
					return rowsChanged, statements, changedIDs, scanErr
				}
				rowsChanged++
				changedIDs[id]++
			}
			if rowsErr := returned.Err(); rowsErr != nil {
				_ = returned.Close()
				return rowsChanged, statements, changedIDs, rowsErr
			}
			if closeErr := returned.Close(); closeErr != nil {
				return rowsChanged, statements, changedIDs, closeErr
			}
			continue
		}
		result, execErr := stmt.Exec(payload, blobs)
		statements++
		if execErr != nil {
			return rowsChanged, statements, changedIDs, execErr
		}
		changed, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return rowsChanged, statements, changedIDs, rowsErr
		}
		rowsChanged += int(changed)
	}
	return rowsChanged, statements, changedIDs, nil
}

// insertEdgeChunksJSONBTx is the JSONB counterpart of
// insertEdgeChunksTxLimited. When returnInserted is true, RETURNING supplies
// exact edge identities for mutation receipts while retaining bounded JSONB
// payloads and INSERT OR IGNORE semantics.
func insertEdgeChunksJSONBTx(
	tx *sql.Tx,
	edges []*graph.Edge,
	returnInserted bool,
) (rowsInserted, statements int, insertedKeys map[sqliteEdgeIdentity]int, err error) {
	query := jsonbEdgeIngestSQL
	if returnInserted {
		query += " RETURNING from_id, to_id, kind, file_path, line"
		insertedKeys = make(map[sqliteEdgeIdentity]int)
	}
	stmt, err := tx.Prepare(query)
	if err != nil {
		return 0, 0, insertedKeys, err
	}
	defer stmt.Close()
	for pos := 0; pos < len(edges); {
		payload, blobs, next, rows, encodeErr := nextJSONBEdgePayload(edges, pos)
		if encodeErr != nil {
			return rowsInserted, statements, insertedKeys, encodeErr
		}
		pos = next
		if rows == 0 {
			continue
		}
		if returnInserted {
			returned, queryErr := stmt.Query(payload, blobs)
			statements++
			if queryErr != nil {
				return rowsInserted, statements, insertedKeys, queryErr
			}
			for returned.Next() {
				var key sqliteEdgeIdentity
				if scanErr := returned.Scan(&key.from, &key.to, &key.kind, &key.filePath, &key.line); scanErr != nil {
					_ = returned.Close()
					return rowsInserted, statements, insertedKeys, scanErr
				}
				rowsInserted++
				insertedKeys[key]++
			}
			if rowsErr := returned.Err(); rowsErr != nil {
				_ = returned.Close()
				return rowsInserted, statements, insertedKeys, rowsErr
			}
			if closeErr := returned.Close(); closeErr != nil {
				return rowsInserted, statements, insertedKeys, closeErr
			}
			continue
		}
		result, execErr := stmt.Exec(payload, blobs)
		statements++
		if execErr != nil {
			return rowsInserted, statements, insertedKeys, execErr
		}
		inserted, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return rowsInserted, statements, insertedKeys, rowsErr
		}
		rowsInserted += int(inserted)
	}
	return rowsInserted, statements, insertedKeys, nil
}
