package store_sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// openReadOnlyStore opens the SQLite store at path for reading only, without
// going through Open.
//
// Open runs schema migrations, alters columns, starts a checkpoint goroutine,
// and on a version mismatch can refuse to open or rebuild the file. None of
// that is appropriate for a status command that may run while a daemon holds
// the same store — `gortex repos` is expected to answer even when the daemon is
// too busy to serve a control request, which is exactly when readiness matters.
//
// query_only blocks accidental writes; busy_timeout keeps a brief read from
// erroring out if the daemon happens to hold the write lock for a moment. We
// deliberately do NOT set journal_mode: forcing it could try to switch the live
// database's mode out from under the daemon, and inheriting the on-disk WAL
// mode is exactly what a concurrent reader wants.
//
// A missing file is not an error — it means nothing has been indexed yet — and
// is reported by the false bool with a nil db. Every other failure is returned.
func openReadOnlyStore(path string) (*sql.DB, bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat sqlite store %q: %w", path, err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(2000)&_pragma=query_only(1)")
	if err != nil {
		return nil, false, fmt.Errorf("open sqlite store %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return db, true, nil
}

// RepoReadiness is one repo's readiness facts, as the store holds them. It is
// deliberately raw: every field is a recorded number or a presence bit, and no
// verdict is formed here. The verdict is a pure function in the command layer
// so every one of its states is table-testable without a database.
type RepoReadiness struct {
	// Gen and ContentGen are the repo's two counters. ContentGen is what stage
	// stamps are compared against; Gen is provenance. See the repo_graph_gen
	// block in schema.go.
	Gen        int64
	ContentGen int64

	// Derive is the recorded derived-pass completion; DeriveFound is false when
	// the repo has no row, which is a real and reportable state — a repo
	// tracked during daemon warmup is never derived at all.
	Derive      graph.DeriveState
	DeriveFound bool

	// EnrichProviders counts the repo's real provider rows, sentinels excluded.
	// EnrichMinContentGen is the MINIMUM content_gen across them — the minimum,
	// never the maximum, so one fresh provider cannot mask a sibling that never
	// ran. It is meaningless when EnrichProviders is zero.
	EnrichProviders     int
	EnrichMinContentGen int64

	// EnrichNoneDeclared is the __none__ sentinel: the enrichment pass looked
	// and found that no provider applies to this repo. Distinct from having no
	// rows at all, which means nobody has looked yet.
	EnrichNoneDeclared bool

	// EnrichRows is the total row count including sentinels, so a caller can
	// tell "looked at, nothing applies" from "never looked at".
	EnrichRows int
}

// ReadinessStates is what one read-only open of the store yields: index
// freshness plus the two later stages, for every repo, plus which tables were
// there to be read.
type ReadinessStates struct {
	Index map[string]graph.RepoIndexState
	Repos map[string]RepoReadiness

	// StoreFound is false when there is no store file at all.
	StoreFound bool

	// DeriveTable and EnrichTable report whether each table EXISTS, which is a
	// third state distinct from "exists and has no row for this repo". A binary
	// carrying this feature can be run against a store an older daemon wrote and
	// has not yet migrated; every repo there is unknown, not "never derived".
	DeriveTable bool
	EnrichTable bool
}

// ReadReadinessStates opens the store once and reads every table readiness
// needs, keyed by repo prefix.
//
// One open and four queries rather than four sibling readers: they would each
// carry a copy of the pragma reasoning above, three of the copies would rot,
// and one of them would eventually be reading a store the daemon is writing
// with the wrong flags.
//
// A missing store file yields an empty result with a nil error — nothing has
// been indexed yet. A store that exists but cannot be read is an error.
// Degrading that to an empty result printed a confident "never indexed" for
// every repo, which reads as a fact about the repos rather than a failure to
// look.
func ReadReadinessStates(path string) (ReadinessStates, error) {
	out := emptyReadinessStates()

	db, found, err := openReadOnlyStore(path)
	if err != nil {
		return out, err
	}
	if !found {
		return out, nil
	}
	defer db.Close() //nolint:errcheck // read-only handle

	return readReadinessFromDB(db, path)
}

// ReadinessStates answers the same question as ReadReadinessStates, through the
// handle this Store already holds.
//
// It exists because readiness gained a second consumer. `gortex repos` has no
// open store and must not disturb the daemon's, so it opens its own read-only
// handle; the MCP server is the daemon and already has one, and re-opening the
// file per tool call would be both wasteful and a second reader of a database
// it is itself writing.
//
// The two share readReadinessFromDB rather than each spelling the queries out.
// Four queries duplicated across two readers is four chances for one of them to
// be updated and the other not — and the symptom would be two surfaces
// disagreeing about whether a repo is ready, which is precisely what having one
// verdict is supposed to prevent.
//
// s.db is the read-dedicated pool; nothing here writes.
func (s *Store) ReadinessStates() (ReadinessStates, error) {
	if s == nil || s.db == nil {
		return emptyReadinessStates(), nil
	}
	return readReadinessFromDB(s.db, s.Path())
}

// emptyReadinessStates is the zero result both readers start from: maps made,
// StoreFound false. Returned as-is when there is nothing to read, so a caller
// never has to nil-check the maps.
func emptyReadinessStates() ReadinessStates {
	return ReadinessStates{
		Index: map[string]graph.RepoIndexState{},
		Repos: map[string]RepoReadiness{},
	}
}

// readReadinessFromDB is the shared body: one index-state scan and three
// tolerant queries against an already-open handle. path is used only to build
// error messages.
func readReadinessFromDB(db *sql.DB, path string) (ReadinessStates, error) {
	out := emptyReadinessStates()
	out.StoreFound = true

	var err error
	if out.Index, err = scanIndexStates(db, path); err != nil {
		return out, err
	}

	repo := func(prefix string) RepoReadiness { return out.Repos[prefix] }

	// repo_graph_gen. Absent on a pre-v13 store, which leaves every counter at
	// zero and every stage reading current-against-nothing; the derive table's
	// absence is what makes those repos unknown, so this needs no third state.
	if err := queryTolerantOfMissingTable(db, path, `
SELECT repo_prefix, gen, content_gen FROM repo_graph_gen`,
		func(rows *sql.Rows) error {
			var prefix string
			var gen, contentGen int64
			if err := rows.Scan(&prefix, &gen, &contentGen); err != nil {
				return err
			}
			r := repo(prefix)
			r.Gen, r.ContentGen = gen, contentGen
			out.Repos[prefix] = r
			return nil
		}); err != nil {
		return out, err
	}

	present, err := queryTolerantOfMissingTablePresence(db, path, `
SELECT repo_prefix, derived_gen, derived_content_gen, derived_sha, derived_at,
       pass_version, config_hash, scoped, legacy
  FROM derive_state`,
		func(rows *sql.Rows) error {
			var st graph.DeriveState
			var scoped, legacy int
			if err := rows.Scan(&st.RepoPrefix, &st.DerivedGen, &st.DerivedContentGen,
				&st.DerivedSHA, &st.DerivedAt, &st.PassVersion, &st.ConfigHash,
				&scoped, &legacy); err != nil {
				return err
			}
			st.Scoped, st.Legacy = scoped != 0, legacy != 0
			r := repo(st.RepoPrefix)
			r.Derive, r.DeriveFound = st, true
			out.Repos[st.RepoPrefix] = r
			return nil
		})
	if err != nil {
		return out, err
	}
	out.DeriveTable = present

	// One grouped query, not one per repo. MIN over the real provider rows is
	// the enrichment verdict; the counts beside it are what separate "no
	// provider applies" from "no provider has been looked at".
	present, err = queryTolerantOfMissingTablePresence(db, path, `
SELECT repo_prefix,
       COUNT(*),
       SUM(CASE WHEN provider NOT IN (?, ?) THEN 1 ELSE 0 END),
       MIN(CASE WHEN provider NOT IN (?, ?) THEN content_gen END),
       MAX(CASE WHEN provider = ? THEN 1 ELSE 0 END)
  FROM enrichment_state
 GROUP BY repo_prefix`,
		func(rows *sql.Rows) error {
			var prefix string
			var total, providers int
			var minGen sql.NullInt64
			var none int
			if err := rows.Scan(&prefix, &total, &providers, &minGen, &none); err != nil {
				return err
			}
			r := repo(prefix)
			r.EnrichRows = total
			r.EnrichProviders = providers
			r.EnrichMinContentGen = minGen.Int64
			r.EnrichNoneDeclared = none != 0
			out.Repos[prefix] = r
			return nil
		},
		graph.EnrichProviderRepoMarker, graph.EnrichProviderNone,
		graph.EnrichProviderRepoMarker, graph.EnrichProviderNone,
		graph.EnrichProviderNone)
	if err != nil {
		return out, err
	}
	out.EnrichTable = present

	return out, nil
}

// queryTolerantOfMissingTablePresence runs one query, feeding each row to scan,
// and reports whether the table was there at all. A missing table is the
// binary-is-newer-than-the-store window and is information, not a failure;
// anything else — a corrupt file most of all — is returned.
func queryTolerantOfMissingTablePresence(
	db *sql.DB, path, query string, scan func(*sql.Rows) error, args ...any,
) (bool, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		if isMissingTableErr(err) || isMissingColumnErr(err) {
			return false, nil
		}
		return false, fmt.Errorf("read readiness state from %q: %w", path, err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	for rows.Next() {
		if err := scan(rows); err != nil {
			return true, fmt.Errorf("scan readiness state from %q: %w", path, err)
		}
	}
	if err := rows.Err(); err != nil {
		return true, fmt.Errorf("iterate readiness state from %q: %w", path, err)
	}
	return true, nil
}

func queryTolerantOfMissingTable(
	db *sql.DB, path, query string, scan func(*sql.Rows) error, args ...any,
) error {
	_, err := queryTolerantOfMissingTablePresence(db, path, query, scan, args...)
	return err
}

// isMissingColumnErr reports whether err is SQLite refusing a query because a
// column does not exist.
//
// A missing column is the same window as a missing table, one migration
// narrower: this binary is ahead of the store it is reading. It arises when a
// schema version gains a column after some store was already stamped at that
// version — which is exactly what happened to v13 while this feature was being
// built, and which the guarded ADD COLUMN steps in createReadinessStateTables
// repair only on a store that has not yet reached v13.
//
// Tolerated ONLY here, in the readiness reader, and only to the same effect a
// missing table has: the stage reads "unknown". The alternative is that
// `gortex repos` fails outright and reports nothing about any repo, which is a
// far worse answer than "I cannot tell yet" — a status command's whole job is
// to degrade rather than refuse. A genuinely mistyped column would be caught
// immediately by the tests, which run against a freshly migrated store.
func isMissingColumnErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such column")
}
