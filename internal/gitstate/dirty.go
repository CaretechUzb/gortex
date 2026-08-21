package gitstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/gitcmd"
)

// ErrDirtyUnavailable reports that a checkout's dirty state could not be
// sampled — the directory is not a working tree, or the status call
// failed. As with ErrInventoryUnavailable and ErrHEADUnavailable, the
// zero DirtySnapshot returned alongside it carries no information: a
// caller diffing against a previous snapshot MUST NOT read the empty
// entry list as "the checkout went clean".
var ErrDirtyUnavailable = errors.New("git dirty state unavailable")

// DirtyKind names what kind of difference one path carries.
type DirtyKind string

const (
	// DirtyModified is a content change to a path that exists on both
	// sides of the comparison. A conflicted (unmerged) path is reported
	// this way too.
	DirtyModified DirtyKind = "modified"
	// DirtyAdded is a path the index holds and HEAD does not.
	DirtyAdded DirtyKind = "added"
	// DirtyDeleted is a path that HEAD holds and the index or the
	// worktree does not.
	DirtyDeleted DirtyKind = "deleted"
	// DirtyUntracked is a path on disk that the index does not hold.
	// Ignored paths are never reported as untracked.
	DirtyUntracked DirtyKind = "untracked"
	// DirtyModeChanged is a path whose octal file mode changed while the
	// path stayed present on both sides — the executable bit flipping is
	// the usual case.
	DirtyModeChanged DirtyKind = "mode_changed"
	// DirtySymlinkChanged is a path whose file mode changed between
	// 120000 and anything else — a symlink replaced a file, or a file
	// replaced a symlink.
	DirtySymlinkChanged DirtyKind = "symlink_changed"
	// DirtyRenamedFrom is the destination half of a rename. OldPath
	// names where the content came from.
	DirtyRenamedFrom DirtyKind = "renamed_from"
)

// DirtyEntry is one path's difference from HEAD.
type DirtyEntry struct {
	// Path is the path relative to the worktree root, exactly as git
	// spells it. It may contain spaces and newlines.
	Path string
	// Kind is what kind of difference this is.
	Kind DirtyKind
	// Staged is true when the index differs from HEAD at this path.
	Staged bool
	// Unstaged is true when the worktree differs from the index at this
	// path. An untracked path is unstaged.
	Unstaged bool
	// OldPath is the source path of a rename, set only on a
	// DirtyRenamedFrom entry. It is diagnostic: the vanished source is
	// reported as its own DirtyDeleted entry, so a caller that only
	// diffs paths never has to read it.
	OldPath string
	// Submodule is true when the path is a submodule rather than a file.
	Submodule bool
}

// DirtySnapshot is one sample of everything a checkout differs from
// HEAD by.
type DirtySnapshot struct {
	// HeadRef is the full ref HEAD points at ("refs/heads/main"), empty
	// when HEAD is detached.
	HeadRef string
	// HeadCommit is the commit HEAD resolves to, empty when the branch
	// is unborn.
	HeadCommit string
	// HeadTree is that commit's tree, empty when the branch is unborn.
	HeadTree string
	// Entries are the differing paths, ordered by path and then kind.
	Entries []DirtyEntry
	// Fingerprint is a hex sha256 that changes whenever the snapshot
	// does. See SampleDirty for what it covers and what bounds it.
	Fingerprint string
}

// Octal file modes git prints in the porcelain mode columns. Only these
// two need naming: 000000 marks a side where the path is absent, and
// 120000 marks a symlink.
const (
	absentMode  = "000000"
	symlinkMode = "120000"
)

// SampleDirty reports how the working tree at dir differs from HEAD.
//
// HEAD is sampled with SampleHEAD, so an unborn branch is a valid
// result rather than a failure: HeadCommit and HeadTree are empty and
// every path git tracks shows up as an entry, which is exactly what
// `git status` reports in that state.
//
// The listing comes from `git status --porcelain=v2 -z`, with untracked
// files expanded individually and rename detection on. -z is required,
// not preferred: a path may legally contain spaces and newlines, and the
// rename record spells its second path in its own NUL-terminated chunk.
// Ignored paths are absent because --ignored is not passed — this
// package never re-implements git's ignore rules. The status call runs
// under --no-optional-locks so that observing a checkout never writes to
// its index.
//
// Fingerprint is a hex sha256 over a canonical encoding of HeadCommit
// and every entry (path, kind, staged, unstaged, old path) plus, for
// every kind that has bytes behind it, the path's lstat size and
// modification time in nanoseconds. Two samples taken with nothing
// changed in between fingerprint identically; editing content, flipping
// a mode, or staging a change all change it.
//
// The filesystem's modification-time granularity is the sensitivity
// bound. A content change that lands within the same mtime tick and
// leaves the size identical contributes no new stat evidence — such a
// path is still reported, and still fingerprinted, but only through the
// fields git itself reports.
func SampleDirty(ctx context.Context, dir string) (DirtySnapshot, error) {
	abs, err := absDir(dir)
	if err != nil {
		return DirtySnapshot{}, fmt.Errorf("gitstate: resolve %q: %w: %w", dir, ErrDirtyUnavailable, err)
	}

	head, err := SampleHEAD(ctx, abs)
	if err != nil {
		return DirtySnapshot{}, fmt.Errorf("gitstate: read HEAD in %s: %w: %w", abs, ErrDirtyUnavailable, err)
	}

	// Porcelain paths are relative to the worktree root, which is not
	// necessarily the directory that was queried, so the root is needed
	// before any path can be statted.
	root, err := gitcmd.Output(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return DirtySnapshot{}, fmt.Errorf("gitstate: resolve worktree root for %s: %w: %w", abs, ErrDirtyUnavailable, err)
	}

	out, err := gitcmd.Run(ctx, abs, "--no-optional-locks", "status", "--porcelain=v2", "-z", "--untracked-files=all", "--renames")
	if err != nil {
		return DirtySnapshot{}, fmt.Errorf("gitstate: read status in %s: %w: %w", abs, ErrDirtyUnavailable, err)
	}

	snap := DirtySnapshot{
		HeadRef:    head.Ref,
		HeadCommit: head.CommitOID,
		HeadTree:   head.TreeOID,
		Entries:    parseStatusZ(out),
	}
	slices.SortStableFunc(snap.Entries, compareDirtyEntries)
	snap.Fingerprint = fingerprintDirty(root, snap.HeadCommit, snap.Entries)
	return snap, nil
}

// compareDirtyEntries orders entries by path, breaking ties on kind so
// the order stays total even when one path carries more than one entry.
func compareDirtyEntries(a, b DirtyEntry) int {
	if c := strings.Compare(a.Path, b.Path); c != 0 {
		return c
	}
	return strings.Compare(string(a.Kind), string(b.Kind))
}

// parseStatusZ parses `git status --porcelain=v2 -z`.
//
// The stream is a flat sequence of NUL-terminated records, each opened
// by a one-character type: '1' a changed path, '2' a rename or copy, 'u'
// an unmerged path, '?' an untracked path. A '2' record is the only one
// that spans two chunks — its source path follows in the next one.
// Records of any other type (an ignored path, a header a newer git
// adds) are skipped rather than guessed at.
func parseStatusZ(out []byte) []DirtyEntry {
	chunks := bytes.Split(out, []byte{0})
	var entries []DirtyEntry
	for i := 0; i < len(chunks); i++ {
		rec := string(chunks[i])
		switch {
		case strings.HasPrefix(rec, "1 "):
			if e, ok := parseChangedRecord(rec); ok {
				entries = append(entries, e)
			}
		case strings.HasPrefix(rec, "2 "):
			var source string
			if i+1 < len(chunks) {
				source = string(chunks[i+1])
				i++
			}
			entries = append(entries, parseRenameRecord(rec, source)...)
		case strings.HasPrefix(rec, "u "):
			if e, ok := parseUnmergedRecord(rec); ok {
				entries = append(entries, e)
			}
		case strings.HasPrefix(rec, "? "):
			if path := rec[2:]; path != "" {
				entries = append(entries, DirtyEntry{Path: path, Kind: DirtyUntracked, Unstaged: true})
			}
		}
	}
	return entries
}

// splitRecord splits a porcelain v2 record into its n leading
// space-separated fields plus the path, which is everything left over
// and may itself contain spaces.
func splitRecord(rec string, n int) (fields []string, path string, ok bool) {
	parts := strings.SplitN(rec, " ", n+1)
	if len(parts) != n+1 || parts[n] == "" {
		return nil, "", false
	}
	return parts[:n], parts[n], true
}

// parseChangedRecord parses a '1' record:
//
//	1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>
func parseChangedRecord(rec string) (DirtyEntry, bool) {
	fields, path, ok := splitRecord(rec, 8)
	if !ok || len(fields[1]) != 2 {
		return DirtyEntry{}, false
	}
	staged, unstaged := fields[1][0], fields[1][1]
	kind := kindFromModes(fields[3], fields[4], fields[5])
	if kind == "" {
		kind = kindFromStatus(staged, unstaged)
	}
	return DirtyEntry{
		Path:      path,
		Kind:      kind,
		Staged:    staged != '.',
		Unstaged:  unstaged != '.',
		Submodule: isSubmoduleField(fields[2]),
	}, true
}

// parseRenameRecord parses a '2' record plus the source path that
// follows it:
//
//	2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <X><score> <path>\0<source>
//
// git reports a rename as one record naming two paths. Callers compare
// snapshots path by path, so it is split back into the two path facts it
// stands for: the source is gone, and the destination is here and
// remembers where its content came from. A copy leaves the source in
// place, so only a rename contributes the deletion half.
func parseRenameRecord(rec, source string) []DirtyEntry {
	fields, path, ok := splitRecord(rec, 9)
	if !ok || len(fields[1]) != 2 || source == "" {
		return nil
	}
	staged, unstaged := fields[1][0], fields[1][1]
	submodule := isSubmoduleField(fields[2])

	var entries []DirtyEntry
	if strings.HasPrefix(fields[8], "R") {
		// The source only vanishes on the side that reported the rename.
		// A worktree modification reported alongside a staged rename
		// ("RM") belongs to the destination, not to the vanished source.
		entries = append(entries, DirtyEntry{
			Path:      source,
			Kind:      DirtyDeleted,
			Staged:    staged == 'R',
			Unstaged:  unstaged == 'R',
			Submodule: submodule,
		})
	}
	return append(entries, DirtyEntry{
		Path:      path,
		Kind:      DirtyRenamedFrom,
		Staged:    staged != '.',
		Unstaged:  unstaged != '.',
		OldPath:   source,
		Submodule: submodule,
	})
}

// parseUnmergedRecord parses a 'u' record:
//
//	u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>
//
// A conflicted path differs from HEAD on both sides at once — the index
// holds three stages of it and the worktree holds whatever the merge
// left behind — so both flags are set regardless of which stage codes
// git printed.
func parseUnmergedRecord(rec string) (DirtyEntry, bool) {
	fields, path, ok := splitRecord(rec, 10)
	if !ok {
		return DirtyEntry{}, false
	}
	return DirtyEntry{
		Path:      path,
		Kind:      DirtyModified,
		Staged:    true,
		Unstaged:  true,
		Submodule: isSubmoduleField(fields[2]),
	}, true
}

// isSubmoduleField reports whether a porcelain <sub> field describes a
// submodule. The field is "N..." for anything else and "S<c><m><u>" for
// a submodule.
func isSubmoduleField(sub string) bool { return strings.HasPrefix(sub, "S") }

// kindFromModes classifies an entry from git's three octal mode columns:
// head to index is the staged transition, index to worktree the unstaged
// one. It returns "" when the modes explain nothing, leaving the status
// codes to decide.
//
// The modes are the only place a mode flip shows up. git reports
// `chmod +x` on otherwise untouched content as a plain 'M', with
// identical blob hashes on both sides — the changed octal column is the
// whole difference.
func kindFromModes(head, index, worktree string) DirtyKind {
	staged := modeTransition(head, index)
	unstaged := modeTransition(index, worktree)
	if staged == DirtySymlinkChanged || unstaged == DirtySymlinkChanged {
		return DirtySymlinkChanged
	}
	if staged == DirtyModeChanged || unstaged == DirtyModeChanged {
		return DirtyModeChanged
	}
	return ""
}

// modeTransition classifies one before/after pair of octal modes. An
// absent side is a creation or a deletion, which the status codes
// already describe, so only a change between two present modes counts.
func modeTransition(before, after string) DirtyKind {
	switch {
	case before == after, before == "", after == "":
		return ""
	case before == absentMode, after == absentMode:
		return ""
	case before == symlinkMode, after == symlinkMode:
		return DirtySymlinkChanged
	default:
		return DirtyModeChanged
	}
}

// kindFromStatus maps a porcelain XY status pair to a kind. The staged
// column wins when it is set, because it says what the index records;
// '.' means that side is unchanged.
//
// Codes describing a type change ('T') never decide anything here: a
// type change always moves an octal mode column, so kindFromModes has
// already classified it. Everything that is neither an add nor a delete
// is therefore a content modification.
func kindFromStatus(staged, unstaged byte) DirtyKind {
	code := staged
	if code == '.' {
		code = unstaged
	}
	switch code {
	case 'A':
		return DirtyAdded
	case 'D':
		return DirtyDeleted
	default:
		return DirtyModified
	}
}

// Canonical encoding.
//
// A fingerprint must be injective: no two distinct snapshots may encode
// to the same bytes. Joining fields with a separator cannot promise
// that, because a path may contain any byte but NUL — including whatever
// separator was picked. So every string is written length-prefixed,
// every flag as a fixed byte, every integer as a fixed 8 bytes, and the
// entry list behind its own count, all under a domain tag. Field
// boundaries are recoverable from the byte stream alone.
const dirtySnapshotTag = "gortex.gitstate.dirty.v1"

// dirtyCanonical accumulates the canonical byte stream.
type dirtyCanonical struct {
	buf []byte
}

// str writes len(s) as a uvarint followed by the raw bytes of s.
func (c *dirtyCanonical) str(s string) {
	c.buf = binary.AppendUvarint(c.buf, uint64(len(s)))
	c.buf = append(c.buf, s...)
}

// count writes the length of a list as a uvarint.
func (c *dirtyCanonical) count(n int) {
	c.buf = binary.AppendUvarint(c.buf, uint64(n))
}

// i64 writes n as 8 big-endian bytes — fixed width, so no value of one
// field can be mistaken for the start of the next.
func (c *dirtyCanonical) i64(n int64) {
	c.buf = binary.BigEndian.AppendUint64(c.buf, uint64(n))
}

// flag writes a bool as one fixed byte.
func (c *dirtyCanonical) flag(b bool) {
	var v byte
	if b {
		v = 1
	}
	c.buf = append(c.buf, v)
}

// fingerprintDirty reduces a sorted entry list to a lowercase hex
// sha256, mixing in the stat evidence behind each entry.
func fingerprintDirty(root, headCommit string, entries []DirtyEntry) string {
	var c dirtyCanonical
	c.str(dirtySnapshotTag)
	c.str(headCommit)
	c.count(len(entries))
	for _, e := range entries {
		c.str(e.Path)
		c.str(string(e.Kind))
		c.flag(e.Staged)
		c.flag(e.Unstaged)
		c.str(e.OldPath)
		size, mtime := statEvidence(root, e)
		c.i64(size)
		c.i64(mtime)
	}
	sum := sha256.Sum256(c.buf)
	return hex.EncodeToString(sum[:])
}

// statEvidence samples the size and modification time behind an entry.
// A deletion has nothing on disk to stat, and a path that vanished
// between the status call and the stat is treated the same way: it still
// fingerprints, it just contributes no stat evidence of its own.
//
// The link is statted, never its target, so swapping a symlink for one
// pointing somewhere else is visible.
func statEvidence(root string, e DirtyEntry) (size, mtimeNanos int64) {
	if e.Kind == DirtyDeleted {
		return 0, 0
	}
	fi, err := os.Lstat(filepath.Join(root, e.Path))
	if err != nil {
		return 0, 0
	}
	return fi.Size(), fi.ModTime().UnixNano()
}
