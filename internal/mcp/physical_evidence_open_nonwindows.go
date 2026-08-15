//go:build !windows

package mcp

import (
	"os"
	"syscall"
)

// openPhysicalEvidenceFile prevents a repository-local FIFO or device from
// blocking the MCP request if the path changes after the pre-open Lstat. The
// caller still verifies the opened handle with f.Stat before reading.
func openPhysicalEvidenceFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
