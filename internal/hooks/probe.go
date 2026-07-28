package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

func unmarshalResult(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// probeViaDaemon dials the local daemon over its unix socket and runs one
// search_symbols control RPC. The whole exchange (dial + handshake + RPC)
// must fit inside timeout; otherwise errProbeTimeout is returned and the
// caller falls through to soft guidance.
//
// Returns errDaemonUnreachable when the daemon isn't running — the hook
// distinguishes "no signal" from "probed and missed" so telemetry stays
// clean.
func probeViaDaemon(pattern string, timeout time.Duration) ([]grepSymbolHit, error) {
	deadline := time.Now().Add(timeout)
	type probeResult struct {
		hits []grepSymbolHit
		err  error
	}
	done := make(chan probeResult, 1)

	go func() {
		client, err := daemon.Dial(daemon.Handshake{
			Mode:       daemon.ModeControl,
			ClientName: "gortex-hook",
		})
		if err != nil {
			if errors.Is(err, daemon.ErrDaemonUnavailable) {
				done <- probeResult{err: errDaemonUnreachable}
				return
			}
			done <- probeResult{err: fmt.Errorf("dial daemon: %w", err)}
			return
		}
		defer client.Close()

		// Cap the round trip at the remaining budget so a stuck daemon
		// can't pin the goroutine past timeout. Passed explicitly rather
		// than left to Control's default: a hook runs on the agent's
		// critical path and its budget is far tighter than the default.
		//
		// The clamp matters: a non-positive budget means "no bound" to
		// ControlWithTimeout, so handing it a window the dial already
		// consumed would turn the tightest caller in the tree into the
		// only unbounded one.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			done <- probeResult{err: fmt.Errorf("daemon probe budget exhausted before the search rpc")}
			return
		}
		resp, err := client.ControlWithTimeout(daemon.ControlSearchSymbols, daemon.SearchSymbolsParams{
			Query: pattern,
			Limit: 10,
		}, remaining)
		if err != nil {
			done <- probeResult{err: fmt.Errorf("control rpc: %w", err)}
			return
		}
		if !resp.OK {
			done <- probeResult{err: fmt.Errorf("daemon rejected search [%s]: %s", resp.ErrorCode, resp.ErrorMsg)}
			return
		}

		var result daemon.SearchSymbolsResult
		if err := unmarshalResult(resp.Result, &result); err != nil {
			done <- probeResult{err: fmt.Errorf("decode result: %w", err)}
			return
		}
		hits := make([]grepSymbolHit, 0, len(result.Hits))
		for _, h := range result.Hits {
			hits = append(hits, grepSymbolHit{
				Name:     h.Name,
				Kind:     h.Kind,
				FilePath: h.FilePath,
				Line:     h.Line,
			})
		}
		done <- probeResult{hits: hits}
	}()

	select {
	case r := <-done:
		return r.hits, r.err
	case <-time.After(time.Until(deadline)):
		return nil, errProbeTimeout
	}
}
