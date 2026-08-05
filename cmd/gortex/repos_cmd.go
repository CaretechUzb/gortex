package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/tui"
)

var reposJSON bool

// reposCacheDir is the persistence-store directory `gortex repos`
// inspects for index freshness. Empty resolves to the default
// (~/.gortex/cache/) — the same slot `gortex server` / `gortex mcp`
// persist to. Overridable so tests can point at a temp store.
var reposCacheDir string

// reposBackendPath is the on-disk SQLite backend file `gortex repos`
// reads index-freshness provenance (the repo_index_state table) from.
// Empty resolves to the daemon's default store (~/.gortex/store/store.sqlite).
// Overridable so tests can point at an isolated store.
var reposBackendPath string

var reposCmd = &cobra.Command{
	Use:   "repos",
	Short: "List every tracked repository with its git head and index freshness",
	Long: `Lists the repositories registered in the global config
(~/.gortex/config.yaml).

For each repo the command reports the current git HEAD commit and an
index-freshness indicator: when the persisted index was last built and
whether that index still matches HEAD. A repo is "stale" when HEAD has
moved past the commit the cached index was built from, or when no index
has been persisted yet.

The default output is a table; --json emits the same data as a JSON
array suitable for scripting.`,
	RunE: runRepos,
}

func init() {
	reposCmd.Flags().BoolVar(&reposJSON, "json", false, "emit machine-readable JSON instead of a table")
	rootCmd.AddCommand(reposCmd)
}

// repoStatus is one repository's entry in the `gortex repos` output —
// identity plus the git head commit and index-freshness facts. It is
// the JSON shape emitted under --json; the table renderer projects the
// same struct into columns.
type repoStatus struct {
	// Name is the repo's configured name, falling back to the path
	// basename when the global config declares no explicit name.
	Name string `json:"name"`
	// Path is the absolute on-disk path of the repository.
	Path string `json:"path"`
	// Workspace is the workspace slug the repo's nodes are keyed on
	// (the global-config override; empty when none is declared).
	Workspace string `json:"workspace,omitempty"`
	// HeadCommit is the current git HEAD commit SHA, or empty when
	// the path is not a git repository / git is unavailable.
	HeadCommit string `json:"head_commit"`
	// Branch is the current git branch, empty for a detached HEAD.
	Branch string `json:"branch,omitempty"`
	// IndexedCommit is the commit SHA the persisted index was built
	// from. Empty when no index snapshot exists yet.
	IndexedCommit string `json:"indexed_commit,omitempty"`
	// LastIndexed is the timestamp the persisted index was built.
	// Nil (omitted from JSON) when the repo has never been indexed.
	LastIndexed *time.Time `json:"last_indexed,omitempty"`
	// Stale is true when the persisted index does not match the
	// current HEAD — either HEAD moved past IndexedCommit or no
	// index has been persisted at all.
	Stale bool `json:"stale"`
	// Indexed is true when a persisted index snapshot was found for
	// the repo's current branch slot.
	Indexed bool `json:"indexed"`
	// IndexedDirty is true when the recorded index was built from a
	// working tree with uncommitted changes (the repo_index_state.dirty
	// flag). Omitted from JSON when false / unknown. The index still
	// counts as fresh when its commit matches HEAD — this is provenance,
	// not a staleness signal.
	IndexedDirty bool `json:"indexed_dirty,omitempty"`
	// Missing is true when Path no longer names a directory on disk.
	// Such an entry outlives the checkout it points at and can never
	// go fresh again, so it is reported as its own state rather than
	// collapsing into "stale" behind an empty HEAD (#312).
	Missing bool `json:"missing,omitempty"`
}

func runRepos(cmd *cobra.Command, _ []string) error {
	repos, err := loadGlobalRepos()
	if err != nil {
		return err
	}

	// Primary freshness source: the daemon's on-disk SQLite backend
	// records one repo_index_state row per repo at the end of every
	// (re)index. Read it once, read-only, so a single open serves the
	// whole list. An unavailable / empty store yields an empty map and
	// we fall back to the snapshot store below.
	indexStates := loadRepoIndexStates()

	// The persistence store is the legacy fallback freshness source —
	// the embedded `gortex mcp --index` path persists a per-repo
	// snapshot here on shutdown. An empty cache dir resolves to the
	// default (~/.gortex/cache/).
	store, err := persistence.NewFileStore(reposCacheDir, version)
	if err != nil {
		return fmt.Errorf("open persistence store: %w", err)
	}
	defer store.Close()

	entries := make([]repoStatus, 0, len(repos))
	for _, r := range repos {
		entries = append(entries, describeRepo(store, indexStates, len(repos), r))
	}
	// Stable order regardless of config-file ordering so scripted
	// diffs and the table stay deterministic.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].Path < entries[j].Path
	})

	if reposJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}

	return renderReposTable(cmd, entries)
}

// loadRepoIndexStates reads the daemon's per-repo index-freshness rows
// (the SQLite repo_index_state table) keyed by repo prefix. It opens the
// backend store read-only so it is safe to run while a daemon holds the
// same store. Any failure (no store yet, unreadable cache) degrades to an
// empty map so `gortex repos` falls back to the snapshot store rather than
// erroring out.
func loadRepoIndexStates() map[string]graph.RepoIndexState {
	path := reposBackendPath
	if path == "" {
		path = filepath.Join(platform.StoreDir(), "store.sqlite")
	}
	states, err := store_sqlite.ReadRepoIndexStates(path)
	if err != nil {
		return map[string]graph.RepoIndexState{}
	}
	return states
}

// describeRepo resolves one RepoEntry into a repoStatus by reading the
// repo's current git HEAD and looking up its recorded index freshness.
//
// The authoritative source is the daemon's repo_index_state row, keyed by
// the repo's resolved prefix (config.ResolvePrefix — the entry Name, else
// the path basename); this is what the daemon writes when it tracks or warms up a repo.
// When there is exactly one tracked repo, a lone-repo index keyed under the
// empty prefix counts too. Failing that, the legacy snapshot store (keyed by
// canonical path + branch) written by the embedded `gortex mcp --index` path
// is consulted. A repo is fresh when the recorded commit matches HEAD.
func describeRepo(store persistence.Store, indexStates map[string]graph.RepoIndexState, repoCount int, r config.RepoEntry) repoStatus {
	head := gitCommitHash(r.Path)
	branch := gitBranch(r.Path)

	entry := repoStatus{
		Name:       repoLabel(r),
		Path:       r.Path,
		Workspace:  r.Workspace,
		HeadCommit: head,
		Branch:     branch,
		// Default to stale; cleared below only when a recorded
		// index is found whose commit matches HEAD.
		Stale: true,
		// A deleted checkout is why HEAD came back empty above. Record
		// the cause so the table can say so instead of leaving the user
		// to infer it from "(none)" — the ghost in #312 sat unflagged
		// for eight days on exactly that inference.
		Missing: config.RepoPathMissing(r.Path),
	}

	// Primary: the daemon's freshness row for this repo's prefix.
	prefix := config.ResolvePrefix(r)
	st, ok := indexStates[prefix]
	if !ok && repoCount == 1 {
		// A single-repo (lone) index is keyed under the empty prefix.
		st, ok = indexStates[""]
	}
	if ok {
		entry.Indexed = true
		entry.IndexedCommit = st.IndexedSHA
		entry.IndexedDirty = st.Dirty
		if st.IndexedAt > 0 {
			ts := time.Unix(st.IndexedAt, 0)
			entry.LastIndexed = &ts
		}
		// Fresh only when the recorded index was built from the exact
		// commit HEAD currently points at. An empty HeadCommit (not a
		// git repo) or an empty recorded SHA can never be fresh.
		entry.Stale = head == "" || st.IndexedSHA == "" || st.IndexedSHA != head
		return entry
	}

	// Fallback: the embedded-server snapshot, keyed under the canonical
	// (main) repo path so every worktree of a repo shares a base slot.
	repoKey := canonicalRepo(r.Path)
	snap, loadErr := store.Load(repoKey, branch, head)
	if loadErr != nil || snap == nil {
		// ErrNotFound (or any read error) — treat as never indexed.
		return entry
	}

	entry.Indexed = true
	entry.IndexedCommit = snap.CommitHash
	indexedAt := snap.IndexedAt
	entry.LastIndexed = &indexedAt
	entry.Stale = head == "" || snap.CommitHash != head
	return entry
}

// renderReposTable prints the repo list as an ASCII table — the default
// human-readable form. Columns mirror the repoStatus JSON fields. On a TTY
// we wrap the table in a styled banner + bottom stat strip; on a non-TTY
// (script piping `gortex repos | grep …`) we keep only the bare table so
// parser-shaped scripts don't break.
func renderReposTable(cmd *cobra.Command, entries []repoStatus) error {
	out := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()
	tty := progress.IsTTY(stderr) && !noProgress

	if len(entries) == 0 {
		if tty {
			emitReposBanner(stderr)
			fmt.Fprintln(stderr, "  "+progress.StyleHint.Render("◌  no tracked repos — run `gortex track <path>` to add one"))
			fmt.Fprintln(stderr)
		} else {
			fmt.Fprintln(out, "(no tracked repos)")
		}
		return nil
	}

	if tty {
		emitReposBanner(stderr)
	}

	t := table.NewWriter()
	t.SetOutputMirror(out)
	t.SetStyle(table.StyleLight)
	t.AppendHeader(table.Row{"repo", "head", "indexed", "last indexed", "freshness", "path"})
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft},
		{Number: 2, Align: text.AlignLeft},
		{Number: 3, Align: text.AlignLeft},
		{Number: 4, Align: text.AlignLeft},
		{Number: 5, Align: text.AlignLeft},
		{Number: 6, Align: text.AlignLeft},
	})

	for _, e := range entries {
		t.AppendRow(table.Row{
			e.Name,
			shortSHA(e.HeadCommit),
			shortSHA(e.IndexedCommit),
			lastIndexedCell(e),
			freshnessCell(e, tty),
			e.Path,
		})
	}
	t.Render()

	emitReposMissingHint(stderr, entries)
	if tty {
		emitReposSummary(stderr, entries)
	}
	return nil
}

// emitReposMissingHint names every tracked repo whose directory is gone
// and the command that removes it.
//
// On stderr, unconditionally — not gated on a TTY like the banner and
// stat strip. Those are decoration; this is the one thing in the output
// a user has to act on, so a scripted `gortex repos | grep …` must still
// see it, and putting it on stdout would inject `gortex untrack <path>`
// lines into the table that same pipeline parses. Scripted callers read
// the `missing` field from --json instead.
func emitReposMissingHint(w interface{ Write([]byte) (int, error) }, entries []repoStatus) {
	var gone []repoStatus
	for _, e := range entries {
		if e.Missing {
			gone = append(gone, e)
		}
	}
	if len(gone) == 0 {
		return
	}
	subject := "repo no longer exists"
	if len(gone) > 1 {
		subject = "repos no longer exist"
	}
	fmt.Fprintf(w, "\n!! %d tracked %s on disk — the path was deleted, renamed, or unmounted.\n",
		len(gone), subject)
	fmt.Fprintln(w, "   They can never be re-indexed. Drop each from the inventory with:")
	for _, e := range gone {
		fmt.Fprintf(w, "     gortex untrack %s\n", e.Path)
	}
}

// emitReposBanner prints the gortex mesh banner on stderr above the table.
// Keeping the banner on stderr (not stdout) means `gortex repos | grep foo`
// still sees only the table on stdout — the JSON / table is parseable, the
// decoration is purely visual.
func emitReposBanner(w interface{ Write([]byte) (int, error) }) {
	banner := tui.Banner{
		Title:    "gortex repos",
		Subtitle: "Every tracked repository with its git head and index freshness.",
	}.Render()
	fmt.Fprintln(w)
	fmt.Fprintln(w, banner)
	fmt.Fprintln(w)
}

// emitReposSummary appends a stat strip below the table: total / fresh /
// stale / never-indexed counts so the eye gets the headline at a glance.
func emitReposSummary(w interface{ Write([]byte) (int, error) }, entries []repoStatus) {
	fresh, stale, never, missing := 0, 0, 0, 0
	for _, e := range entries {
		switch {
		// Missing wins over every freshness bucket: the entry has no
		// checkout left to be fresh or stale about.
		case e.Missing:
			missing++
		case !e.Indexed:
			never++
		case e.Stale:
			stale++
		default:
			fresh++
		}
	}
	stats := []string{
		progress.Stat(strconv.Itoa(len(entries)), "tracked", progress.StatNeutral),
		progress.Stat(strconv.Itoa(fresh), "fresh", progress.StatGood),
	}
	if stale > 0 {
		stats = append(stats, progress.Stat(strconv.Itoa(stale), "stale", progress.StatWarn))
	}
	if never > 0 {
		stats = append(stats, progress.Stat(strconv.Itoa(never), "never indexed", progress.StatBad))
	}
	if missing > 0 {
		stats = append(stats, progress.Stat(strconv.Itoa(missing), "missing", progress.StatBad))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+progress.StatStrip(stats...))
	fmt.Fprintln(w)
}

// shortSHA abbreviates a 40-char git SHA to its 12-char prefix for the
// table — the full hash stays in the JSON output. Empty in, empty out.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}

// lastIndexedCell renders the last-indexed timestamp for the table, or
// a placeholder when the repo has never been indexed.
func lastIndexedCell(e repoStatus) string {
	if e.LastIndexed == nil {
		return "(never)"
	}
	return e.LastIndexed.Local().Format("2006-01-02 15:04:05")
}

// freshnessCell renders the staleness indicator for the table. On a TTY
// the label is colour-tiered (green/yellow/red) so the eye picks up risk
// from a long list at a glance; non-TTY keeps the plain text so scripts
// that grep for "stale" / "fresh" still match.
func freshnessCell(e repoStatus, tty bool) string {
	label := "fresh"
	style := progress.StyleOK
	switch {
	// Checked first: a deleted checkout is why HEAD is empty, and
	// "stale" would read as "re-index me" for a repo that cannot be
	// re-indexed at all.
	case e.Missing:
		label = "MISSING"
		style = progress.StyleErr
	case !e.Indexed:
		label = "not indexed"
		style = progress.StyleErr
	case e.Stale:
		label = "stale"
		style = progress.StyleHint
	}
	if !tty {
		return label
	}
	return style.Render(label)
}
