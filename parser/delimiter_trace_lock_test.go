package parser

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuin/goldmark/v2/util"
)

// User trace callbacks must never run while the parser holds an internal
// lock. The callback re-acquires traceMu to replace itself, which deadlocks if
// traceMu is held during dispatch.
func TestDelimiterTraceCallbackNotHeldUnderLock(t *testing.T) {
	p := New()
	setter, ok := p.(DelimiterTraceSetter)
	if !ok {
		t.Fatalf("default parser must implement DelimiterTraceSetter, got %T", p)
	}

	var replaced atomic.Bool
	setter.SetDelimiterTrace(func(DelimiterTraceEvent) {
		setter.SetDelimiterTrace(func(DelimiterTraceEvent) {
			replaced.Store(true)
		})
	})

	done := make(chan struct{})
	go func() {
		// Deliberately many sources: whether a particular input takes the
		// emphasis fast path or the full delimiter-stack path, every trace event
		// must be delivered lock-free.
		for _, source := range []string{"***a*b**", "*x*", "**y**", "a *b **c** d*"} {
			p.Parse(util.StringToReadOnlyBytes(source))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: trace callback invoked while parser lock was held")
	}

	// The callback installed reentrantly from inside a callback must be live on
	// subsequent parses.
	p.Parse(util.StringToReadOnlyBytes("***e*f**"))
	if !replaced.Load() {
		t.Fatal("reentrant trace replacement did not take effect")
	}
}
