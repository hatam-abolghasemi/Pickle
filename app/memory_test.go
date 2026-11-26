package main

import (
	rtmetrics "runtime/metrics"
	"testing"
	"time"
)

var memorySink [][]byte

func heapReleased() uint64 {
	s := []rtmetrics.Sample{{Name: "/memory/classes/heap/released:bytes"}}
	rtmetrics.Read(s)
	return s[0].Value.Uint64()
}

// Garbage left by a burst goes back to the OS once things are quiet, not
// while a burst is in flight, and without waiting for a collection.
func TestReleaseAfterQuiet(t *testing.T) {
	for range 64 {
		memorySink = append(memorySink, make([]byte, 1<<20))
	}
	memorySink = nil
	before := heapReleased()
	done := working()
	soon()
	time.Sleep(quietAfter + time.Second)
	if heapReleased() > before+16<<20 {
		t.Fatal("released while a burst was in flight")
	}
	done()
	time.Sleep(200 * time.Millisecond)
	if heapReleased() < before+32<<20 {
		t.Fatalf("garbage not released: %d -> %d bytes", before, heapReleased())
	}
	if armed.Load() {
		t.Fatal("a check is still scheduled")
	}
}

func TestSetDropDelay(t *testing.T) {
	setDropDelay(time.Hour)
	if dropDelay <= 0 {
		t.Fatalf("dropDelay = %s", dropDelay)
	}
}
