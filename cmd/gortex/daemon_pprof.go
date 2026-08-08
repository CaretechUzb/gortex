package main

import (
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux
	"os"
	"sync/atomic"

	"go.uber.org/zap"
)

// pprofAddr holds the bound address of the daemon's pprof listener
// (empty when pprof is disabled). Read by controller.Status so the
// active address is reported to clients; written exactly once from
// startPProfIfEnabled. atomic.Value guards against the data race that
// would otherwise exist between Status calls and startup.
var pprofAddr atomic.Value

func init() {
	pprofAddr.Store("")
}

// daemonPProfAddr returns the currently-bound pprof listener address,
// or an empty string when no listener is running.
func daemonPProfAddr() string {
	v, _ := pprofAddr.Load().(string)
	return v
}

// startPProfIfEnabled opens an HTTP pprof listener when the user has
// set GORTEX_DAEMON_PPROF_ADDR (e.g. "127.0.0.1:6060"). Opt-in by
// design — leaving a pprof endpoint on by default would expose the
// daemon's internal state to any process on the machine. The listener
// runs in its own goroutine; failures are logged but don't block
// daemon startup.
func startPProfIfEnabled(logger *zap.Logger) {
	addr := os.Getenv("GORTEX_DAEMON_PPROF_ADDR")
	if addr == "" {
		return
	}
	// pprof serves the DefaultServeMux with no auth, and /debug/pprof/cmdline
	// echoes the daemon's argv — which carries --http-auth-token. The heap it
	// dumps holds indexed source from every tracked repository. Keep it on
	// loopback: an operator who wants it reachable can port-forward.
	if !isLocalhostBind(addr) {
		logger.Warn("daemon: refusing non-loopback pprof bind",
			zap.String("addr", addr),
			zap.String("hint", "pprof is unauthenticated and exposes argv + heap; bind 127.0.0.1 and port-forward"))
		return
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Warn("daemon: pprof listener failed",
			zap.String("addr", addr), zap.Error(err))
		return
	}
	bound := ln.Addr().String()
	pprofAddr.Store(bound)
	logger.Info("daemon: pprof endpoint open",
		zap.String("addr", bound),
		zap.String("hint", "go tool pprof -http=: http://"+bound+"/debug/pprof/heap"))
	go func() {
		// Serve on the DefaultServeMux which net/http/pprof registers on.
		if err := http.Serve(ln, nil); err != nil && err != http.ErrServerClosed {
			logger.Warn("daemon: pprof serve exited", zap.Error(err))
		}
	}()
}
