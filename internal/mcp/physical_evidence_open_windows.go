//go:build windows

package mcp

import "os"

// Windows repository paths do not expose POSIX FIFOs. The caller performs
// Lstat before open and f.Stat immediately afterward to reject non-regular
// filesystem objects and replacement races.
func openPhysicalEvidenceFile(path string) (*os.File, error) {
	return os.Open(path)
}
