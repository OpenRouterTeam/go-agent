package agent

// Goroutine-leak detection for the whole package test run.
//
// This package spawns a goroutine per CallModel run plus one per stream
// subscriber, so a missed Complete() or an unclosed subscriber leaks goroutines
// that outlive their run. That leak is invisible to a normal `go test`: the
// tests still pass, and a long-lived process just grows.
//
// Deliberately dependency-free rather than using uber-go/goleak: this is a
// load-bearing SDK whose contract pins its dependency graph, and a test-only
// dependency still lands in go.mod for every consumer of the package.
//
// The check is intentionally forgiving -- it allows a margin and re-checks with
// backoff -- because the runtime and the testing package keep their own
// goroutines, and finished goroutines are reaped asynchronously. It is meant to
// catch a systematic leak (every run leaks one), not to be a precise accounting.

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
)

// leakMargin tolerates goroutines the runtime/testing machinery keeps around.
// A real per-run leak scales with the number of tests and clears this easily.
const leakMargin = 12

func TestMain(m *testing.M) {
	before := runtime.NumGoroutine()

	code := m.Run()

	// Only police leaks on an otherwise-green run: a failed or panicking test
	// abandons goroutines by design, and reporting that as a leak would bury
	// the actual failure.
	if code != 0 {
		os.Exit(code)
	}

	if leaked, after := waitForGoroutinesToSettle(before + leakMargin); leaked {
		fmt.Fprintf(os.Stderr,
			"\ngoroutine leak: %d goroutines before tests, %d after (margin %d).\n"+
				"A stream was likely left uncompleted or a subscriber unclosed.\n\n",
			before, after, leakMargin)
		buf := make([]byte, 1<<20)
		fmt.Fprintf(os.Stderr, "%s\n", buf[:runtime.Stack(buf, true)])
		os.Exit(1)
	}

	os.Exit(code)
}

// waitForGoroutinesToSettle polls until the goroutine count drops to limit,
// giving the runtime time to reap finished goroutines. It reports whether the
// count was still above limit when the budget ran out.
func waitForGoroutinesToSettle(limit int) (leaked bool, count int) {
	delay := time.Millisecond
	for i := 0; i < 10; i++ {
		count = runtime.NumGoroutine()
		if count <= limit {
			return false, count
		}
		runtime.GC() // nudge finalizers/reaping along
		time.Sleep(delay)
		delay *= 2
	}
	return true, runtime.NumGoroutine()
}
