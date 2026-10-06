package rpc2

// InboundConfig configures an InboundPool.
type InboundConfig struct {
	// MaxConcurrent is the maximum number of handlers executing at the
	// same time. Values less than 1 are treated as 1.
	MaxConcurrent int

	// QueueSize is the maximum number of requests waiting for an
	// execution slot. Negative values are treated as 0.
	QueueSize int
}

// InboundPool bounds the resources spent on executing inbound requests.
// It limits how many handlers run concurrently and how many requests may
// wait for an execution slot, so a slow method cannot make the peer spawn
// an unbounded number of goroutines.
//
// A pool may be shared by several methods (a common limit) or dedicated to
// a single method (a per-method limit).
type InboundPool struct {
	run   chan struct{}
	queue chan struct{}
}

// NewInboundPool returns a new InboundPool with the given limits.
func NewInboundPool(cfg InboundConfig) *InboundPool {
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 1
	}
	if cfg.QueueSize < 0 {
		cfg.QueueSize = 0
	}
	return &InboundPool{
		run:   make(chan struct{}, cfg.MaxConcurrent),
		queue: make(chan struct{}, cfg.MaxConcurrent+cfg.QueueSize),
	}
}

// tryEnqueue admits task for execution, returning false immediately if the
// pool is full. Admitted tasks start once an execution slot is free.
// Tasks still waiting when stop is closed are released without running.
func (p *InboundPool) tryEnqueue(stop <-chan struct{}, task func()) bool {
	select {
	case p.queue <- struct{}{}:
	default:
		return false
	}
	go func() {
		defer func() { <-p.queue }()
		select {
		case p.run <- struct{}{}:
			defer func() { <-p.run }()
			task()
		case <-stop:
		}
	}()
	return true
}

// Running returns the number of tasks currently executing.
func (p *InboundPool) Running() int {
	return len(p.run)
}

// Waiting returns the number of admitted tasks waiting for an execution slot.
func (p *InboundPool) Waiting() int {
	return len(p.queue) - len(p.run)
}
