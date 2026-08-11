package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// exactAwareController records which of the two status paths ran.
type exactAwareController struct {
	*fakeController
	exactCalls int
}

func (c *exactAwareController) StatusExact(ctx context.Context) (StatusResponse, error) {
	c.exactCalls++
	return c.Status(ctx)
}

// The routine poll must never take the scanning path. That is the point of
// serving status from maintained counters: a burst of polls, from agent hooks
// or a watch loop, cannot start a corpus scan.
func TestControlStatusDefaultsToTheNonScanningPath(t *testing.T) {
	ctrl := &exactAwareController{fakeController: &fakeController{}}
	_, socket := newDaemon(t, ctrl)

	c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
	require.NoError(t, err)
	defer c.Close()

	for _, params := range []any{nil, StatusParams{}, StatusParams{Exact: false}} {
		resp, err := c.Control(ControlStatus, params)
		require.NoError(t, err)
		require.True(t, resp.OK, resp.ErrorMsg)
	}
	require.Zero(t, ctrl.exactCalls, "a routine status poll must not trigger a recount")
}

func TestControlStatusExactRoutesToTheRecountingPath(t *testing.T) {
	ctrl := &exactAwareController{fakeController: &fakeController{}}
	_, socket := newDaemon(t, ctrl)

	c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
	require.NoError(t, err)
	defer c.Close()

	resp, err := c.Control(ControlStatus, StatusParams{Exact: true})
	require.NoError(t, err)
	require.True(t, resp.OK, resp.ErrorMsg)
	require.Equal(t, 1, ctrl.exactCalls)
}

// A controller whose backend cannot recount — the in-memory graph maintains
// its counters on every mutation, so it has nothing to reconcile against —
// answers the ordinary way instead of failing the request.
func TestControlStatusExactDegradesWhenUnsupported(t *testing.T) {
	_, socket := newDaemon(t, &fakeController{})

	c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
	require.NoError(t, err)
	defer c.Close()

	resp, err := c.Control(ControlStatus, StatusParams{Exact: true})
	require.NoError(t, err)
	require.True(t, resp.OK, "exact status without an audit path must degrade, not fail")
}

// An older client — or any caller sending a payload this handler does not
// recognise — must still get status rather than an error.
func TestControlStatusIgnoresUnparseableParams(t *testing.T) {
	_, socket := newDaemon(t, &fakeController{})

	c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
	require.NoError(t, err)
	defer c.Close()

	resp, err := c.Control(ControlStatus, "not-an-object")
	require.NoError(t, err)
	require.True(t, resp.OK, "an unrecognised status payload must not fail the request")
}
