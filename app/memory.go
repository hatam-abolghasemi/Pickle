package main

import (
	"log/slog"
	"os"
	"runtime/debug"
	rtmetrics "runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Memory is held only while it is used.
//
// Go keeps the heap it has freed for reuse and returns it to the OS only
// gradually, and GOMEMLIMIT only makes the GC work harder near the limit.
// Pickle waits for neither: once a burst of work (a watch event, a push, a
// relist, a rediscovery, the backfill) has been quiet for quietAfter and
// nothing is in flight, freed heap over minRelease goes back to the OS at
// once. Under events that never pause, it goes back at least every
// releaseAtMost. Nothing runs between bursts: an idle pickle sits at its
// live heap.

const (
	// defaultGCPercent replaces Go's GOGC=100 unless GOGC is set: at 100
	// the heap may grow to twice the live set after a burst, at 50 to 1.5x.
	defaultGCPercent = 50
	// limitFraction of the cgroup memory limit is GOMEMLIMIT, unless it is
	// set: the limit is soft, so the rest is headroom for a collection's
	// overshoot, stacks and the runtime.
	limitFraction = 0.9
	quietAfter    = 5 * time.Second
	releaseAtMost = 30 * time.Second
	minRelease    = 1 << 20
)

// applyMemoryLimit sets GOGC and GOMEMLIMIT from the container's own cgroup
// limit; variables set by hand win.
func applyMemoryLimit() {
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(defaultGCPercent)
	}
	if os.Getenv("GOMEMLIMIT") != "" {
		return // the runtime already applied it
	}
	limit, ok := cgroupMemoryLimit()
	if !ok {
		slog.Info("no cgroup memory limit found; GOMEMLIMIT not set")
		return
	}
	target := int64(float64(limit) * limitFraction)
	debug.SetMemoryLimit(target)
	slog.Info("memory limit", "cgroup", limit, "gomemlimit", target)
}

// cgroupMemoryLimit is this container's memory limit: cgroup v2, then v1.
// No limit ("max", or v1's huge sentinel) is not ok.
func cgroupMemoryLimit() (int64, bool) {
	for _, f := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || v <= 0 || v > 1<<50 {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

var (
	lastWork    atomic.Int64 // unix nanos of the last soon
	firstWork   atomic.Int64 // unix nanos of the first soon since the last release
	armed       atomic.Bool  // a check is scheduled
	inFlight    atomic.Int64 // bursts running now (working)
	releaseHeld atomic.Bool  // a release waits for inFlight to reach 0
)

// soon says a burst of work just ended. One clock read and one atomic
// store on the hot path; a timer is started only when none is pending.
func soon() {
	now := time.Now().UnixNano()
	lastWork.Store(now)
	if armed.CompareAndSwap(false, true) {
		firstWork.Store(now)
		time.AfterFunc(quietAfter, checkQuiet)
	}
}

// checkQuiet releases once quietAfter has passed since the last soon, or
// releaseAtMost since the first; otherwise it waits for the earlier of the two.
func checkQuiet() {
	now := time.Now().UnixNano()
	wait := min(lastWork.Load()+int64(quietAfter), firstWork.Load()+int64(releaseAtMost)) - now
	if wait > 0 {
		time.AfterFunc(time.Duration(wait), checkQuiet)
		return
	}
	armed.Store(false)
	release()
}

// working says a burst started; call the returned func once when it ends.
// No release runs while any burst is in flight: it runs when the last ends.
func working() (done func()) {
	inFlight.Add(1)
	return func() {
		if inFlight.Add(-1) == 0 && releaseHeld.CompareAndSwap(true, false) {
			go release()
		}
	}
}

// release hands freed heap back to the OS when more than minRelease is held
// for nothing: heap already free, plus objects allocated since the last
// collection beyond what it found live (garbage not yet collected). Reading
// four runtime counters is the whole cost when there is nothing to free.
func release() {
	if inFlight.Load() > 0 {
		releaseHeld.Store(true)
		if inFlight.Load() > 0 || !releaseHeld.CompareAndSwap(true, false) {
			return // the last burst to end runs it
		}
	}
	s := []rtmetrics.Sample{
		{Name: "/memory/classes/heap/free:bytes"},
		{Name: "/memory/classes/heap/unused:bytes"},
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/gc/heap/live:bytes"},
	}
	rtmetrics.Read(s)
	held := s[0].Value.Uint64() + s[1].Value.Uint64()
	if objects, live := s[2].Value.Uint64(), s[3].Value.Uint64(); objects > live {
		held += objects - live
	}
	if held > minRelease {
		debug.FreeOSMemory()
		slog.Debug("released idle heap", "KiB", held/1024)
	}
}
