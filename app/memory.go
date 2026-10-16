package main

import (
	"runtime/debug"
	"sync/atomic"
	"time"
)

var lastRelease atomic.Int64

// releaseMemory hands freed heap back to the OS right after a burst of work
// (backfill, relist, push, rediscovery) instead of waiting for the background
// scavenger, so an idle pickle sits near its live heap. Calls closer than 10s
// apart are skipped: back-to-back bursts don't need a full GC each.
func releaseMemory() {
	now := time.Now().UnixNano()
	last := lastRelease.Load()
	if now-last < int64(10*time.Second) || !lastRelease.CompareAndSwap(last, now) {
		return
	}
	debug.FreeOSMemory()
}
