//go:build !unix

package gitstate

import "io/fs"

// pathIdentity has no volume or object identity to report on platforms
// whose stat this package does not read. Callers see the unsupported
// kind and empty tokens, and must not draw conclusions from them.
func pathIdentity(fs.FileInfo) (volumeKind, volumeToken, identity string) {
	return VolumeKindUnsupported, "", ""
}
