package rpc2

import "sync"

// ErrBusy is the error text sent back to the caller when an inbound call is
// rejected because the handler execution limit and the waiting queue are
// both full. On the caller side it is delivered as a ServerError, so it can
// be recognized with errors.As or by comparing against ServerError(ErrBusy).
const ErrBusy = "rpc2: inbound limit reached, server busy"

// InboundLimit configures admission control for inbound requests handled by
// a Client. It bounds the number of handlers executing concurrently and the
// number of requests waiting for a slot, so a slow method cannot make the
// connection spawn an unbounded number of goroutines.
type InboundLimit struct {
	// MaxConcurrent is the maximum number of handlers running at once.
	MaxConcurrent int
	// MaxQueue is the maximum number of requests waiting for a slot.
	// Requests beyond the queue are rejected (calls) or dropped
	// (notifications).
	MaxQueue int
}

// InboundStats reports admission statistics for inbound request handling.
type InboundStats struct {
	Running  int    // handlers currently executing
	Queued   int    // requests waiting for a slot
	Rejected uint64 // calls rejected with ErrBusy
	Dropped  uint64 // notifications dropped by the drop policy
}

type inboundTask struct {
	seq uint64 // 0 means notification
	run func()
}

// inboundGate is a shared or per-method admission gate. Admission is always
// non-blocking so the read loop never stalls on a full gate; that keeps the
// response path flowing and avoids deadlocking bi-directional calls.
type inboundGate struct {
	mu       sync.Mutex
	running  int
	maxRun   int
	queue    []inboundTask
	maxQueue int
	closed   bool
	rejected uint64
	dropped  uint64
}

func newInboundGate(l InboundLimit) *inboundGate {
	maxRun := l.MaxConcurrent
	if maxRun < 1 {
		maxRun = 1
	}
	maxQueue := l.MaxQueue
	if maxQueue < 0 {
		maxQueue = 0
	}
	return &inboundGate{maxRun: maxRun, maxQueue: maxQueue}
}

// admit reports whether the task was accepted. Accepted tasks either start
// immediately on a new goroutine or wait in the queue for a slot.
func (g *inboundGate) admit(t inboundTask) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	if g.running < g.maxRun {
		g.running++
		go g.runTask(t)
		return true
	}
	if len(g.queue) < g.maxQueue {
		g.queue = append(g.queue, t)
		return true
	}
	return false
}

func (g *inboundGate) runTask(t inboundTask) {
	t.run()
	g.mu.Lock()
	defer g.mu.Unlock()
	for !g.closed && len(g.queue) > 0 {
		next := g.queue[0]
		g.queue[0] = inboundTask{}
		g.queue = g.queue[1:]
		go g.runTask(next)
		return
	}
	g.running--
}

func (g *inboundGate) incRejected() {
	g.mu.Lock()
	g.rejected++
	g.mu.Unlock()
}

func (g *inboundGate) incDropped() {
	g.mu.Lock()
	g.dropped++
	g.mu.Unlock()
}

// close releases every queued task. Queued notifications are counted as
// dropped; queued calls are abandoned because the connection is gone and
// their error response can no longer be delivered.
func (g *inboundGate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	for _, t := range g.queue {
		if t.seq == 0 {
			g.dropped++
		}
	}
	g.queue = nil
}

func (g *inboundGate) stats() InboundStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return InboundStats{
		Running:  g.running,
		Queued:   len(g.queue),
		Rejected: g.rejected,
		Dropped:  g.dropped,
	}
}
