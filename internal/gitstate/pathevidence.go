package gitstate

import (
	"os"
	"path/filepath"
)

// Volume kinds reported by PathEvidence.
const (
	// VolumeKindUnixDev means the volume token is a Unix device number.
	// Two paths with the same token live on the same mounted
	// filesystem.
	VolumeKindUnixDev = "unix-dev"
	// VolumeKindUnsupported means the platform exposes no volume
	// identity this package knows how to read; the tokens are empty and
	// carry no meaning.
	VolumeKindUnsupported = "unsupported"
)

// PathEvidence is filesystem evidence about a worktree root, gathered
// so that a later claim of "this checkout is gone" can be told apart
// from "the volume it lived on is not mounted right now".
//
// Identity is sampled without following symlinks: a symlink standing
// where a directory used to be has its own identity, not its target's.
type PathEvidence struct {
	// RootExists is true when the root itself could be statted.
	RootExists bool
	// RootIdentity is an opaque token that stays the same for the same
	// filesystem object and changes when the object is replaced. Empty
	// when the root does not exist or the platform is unsupported.
	RootIdentity string
	// VolumeKind names how VolumeToken was derived, empty when the root
	// does not exist.
	VolumeKind string
	// VolumeToken is an opaque per-volume token. Two paths on the same
	// mounted filesystem share it. Empty when the root does not exist
	// or the platform is unsupported.
	VolumeToken string
	// AncestorPath is the nearest existing strict ancestor of the root.
	// Empty only when nothing above the root could be statted.
	AncestorPath string
	// AncestorVolumeKind names how AncestorVolumeToken was derived.
	AncestorVolumeKind string
	// AncestorVolumeToken is the ancestor's volume token. When the root
	// is missing but this token matches what it was while the root
	// existed, the volume is still mounted and the absence is real.
	AncestorVolumeToken string
}

// SamplePathEvidence gathers filesystem evidence about root.
//
// It never fails: an unreachable root simply yields RootExists false,
// and the walk up to the nearest existing ancestor still records which
// volume that ancestor sits on.
func SamplePathEvidence(root string) PathEvidence {
	var ev PathEvidence
	abs, err := absDir(root)
	if err != nil {
		return ev
	}

	if fi, statErr := os.Lstat(abs); statErr == nil {
		ev.RootExists = true
		ev.VolumeKind, ev.VolumeToken, ev.RootIdentity = pathIdentity(fi)
	}

	parent := filepath.Dir(abs)
	for {
		if fi, statErr := os.Lstat(parent); statErr == nil {
			ev.AncestorPath = parent
			ev.AncestorVolumeKind, ev.AncestorVolumeToken, _ = pathIdentity(fi)
			break
		}
		next := filepath.Dir(parent)
		if next == parent {
			break
		}
		parent = next
	}
	return ev
}
