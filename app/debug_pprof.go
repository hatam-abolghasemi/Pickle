//go:build pprof

package main

import (
	"log/slog"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/ on the default mux
	"os"
)

// Built with -tags pprof, pickle serves Go's profiling endpoints on
// DEBUG_PPROF_ADDR (e.g. 127.0.0.1:6060) when it is set. Release images are
// built without the tag: the endpoints aren't in them.
func init() {
	if addr := os.Getenv("DEBUG_PPROF_ADDR"); addr != "" {
		go func() {
			slog.Info("pprof listening", "addr", addr)
			if err := http.ListenAndServe(addr, nil); err != nil {
				slog.Warn("pprof stopped", "err", err)
			}
		}()
	}
}
