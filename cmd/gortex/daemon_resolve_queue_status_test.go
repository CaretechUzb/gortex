package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The status row is the other half of making a queued pass visible. The log line
// only lands once the wait is OVER — precisely too late for the person typing
// `gortex daemon status` because the daemon looks idle.

func TestWorkspaceDerivationSuffixIsEmptyWhenNothingIsPendingOrQueued(t *testing.T) {
	assert.Empty(t, formatWorkspaceDerivation(false, 0, ""))
}

func TestWorkspaceDerivationSuffixKeepsItsOriginalWording(t *testing.T) {
	assert.Equal(t,
		"; deriving workspace edges (recently tracked repos not yet fully bound)",
		formatWorkspaceDerivation(true, 0, ""))
}

// A queued pass is reported whether or not a derivation is also pending: a
// resolve blocked behind a semantic apply is worth naming on its own.
func TestAQueuedResolveIsNamedWithItsAge(t *testing.T) {
	assert.Equal(t,
		"; cross-repo resolve waiting on the resolve mutex for 24m45s",
		formatWorkspaceDerivation(false, 1485, "cross-repo resolve"))

	assert.Equal(t,
		"; deriving workspace edges (recently tracked repos not yet fully bound)"+
			"; cross-repo resolve waiting on the resolve mutex for 24m45s",
		formatWorkspaceDerivation(true, 1485, "cross-repo resolve"))
}

// Without a pass name there is nothing to attribute the wait to, so the seconds
// alone must not produce a row.
func TestAnUnattributedWaitIsNotRendered(t *testing.T) {
	assert.Empty(t, formatWorkspaceDerivation(false, 1485, ""))
}
