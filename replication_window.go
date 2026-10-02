// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"sync"
	"time"

	"github.com/hashicorp/go-metrics"
)

const (
	defaultInitialWindow    = 16
	defaultMinWindow        = 1
	defaultMaxWindow        = 64
	defaultMaxInflightBytes = 8 << 20 // 8 MiB

	// windowRTTSlack is the absolute margin added to the RTT slowdown check so
	// that sub-millisecond jitter on fast links to nearby followers does not
	// trigger spurious window reductions.
	windowRTTSlack = 5 * time.Millisecond
)

// WindowDiagnostics is a point-in-time snapshot of the adaptive replication
// window state for a single follower. It is safe to consume concurrently.
type WindowDiagnostics struct {
	// Generation identifies the current replication connection. It is
	// incremented every time a new pipeline is established to the follower.
	Generation uint64

	// Window is the current maximum number of in-flight AppendEntries
	// requests allowed to the follower.
	Window uint64

	// Inflight is the number of pipelined requests currently awaiting a
	// response on the current connection.
	Inflight uint64

	// InflightBytes is the total number of log entry bytes currently in
	// flight on the current connection.
	InflightBytes uint64

	// GrowEvents counts how many times the window has grown.
	GrowEvents uint64

	// ShrinkEvents counts how many times the window has shrunk.
	ShrinkEvents uint64

	// StaleCompletions counts responses received for a previous connection
	// (generation). These are deliberately ignored so that late completions
	// from before a reconnect cannot adjust the new window.
	StaleCompletions uint64

	// MinWindowObserved and MaxWindowObserved record the extreme window sizes to
	// aid diagnostics.
	MinWindowObserved uint64
	MaxWindowObserved uint64

	// MinRTT is the smallest round trip time observed on the current
	// connection and LastRTT the most recent one to feed the controller.
	MinRTT  time.Duration
	LastRTT time.Duration
}

// adaptiveWindow bounds the number of in-flight AppendEntries requests and
// log bytes to a single follower during pipeline replication, adapting the
// request limit to the connection's observed latency and failures.
//
// The window is scoped to a follower, but feedback is scoped to a connection:
// each new pipeline bumps the generation, and completions tagged with an
// older generation are ignored so that late responses from a previous
// connection cannot adjust the new window.
//
// The zero value is not usable; construct with newAdaptiveWindow. All methods
// are safe for concurrent use.
type adaptiveWindow struct {
	mu sync.Mutex

	peer ServerID
	cfg  ReplicationWindowConfig

	window        uint64
	inflight      uint64
	inflightBytes uint64
	generation    uint64

	minRTT  time.Duration
	lastRTT time.Duration

	growCredits       uint64
	growEvents        uint64
	shrinkEvents      uint64
	staleCompletions  uint64
	minWindowObserved uint64
	maxWindowObserved uint64

	// waitCh is closed and replaced to broadcast that a slot may have become
	// available to waiters in acquire.
	waitCh chan struct{}
}

func newAdaptiveWindow(peer ServerID, cfg ReplicationWindowConfig) *adaptiveWindow {
	cfg = cfg.withDefaults()
	w := &adaptiveWindow{
		peer:              peer,
		cfg:               cfg,
		window:            cfg.InitialWindow,
		minWindowObserved: cfg.InitialWindow,
		maxWindowObserved: cfg.InitialWindow,
		waitCh:            make(chan struct{}),
	}
	return w
}

// begin starts a new replication connection and returns its generation.
// In-flight accounting is reset, while the window size and RTT baseline
// carry over so a reconnecting follower does not restart from scratch and a
// connection that opens while the link is degraded is still compared against
// the follower's historical baseline. Completions tagged with an older
// generation are ignored from now on.
func (w *adaptiveWindow) begin() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.generation++
	w.inflight = 0
	w.inflightBytes = 0
	w.growCredits = 0
	w.broadcastLocked()
	w.emitGaugeLocked()
	return w.generation
}

// acquire blocks until the request fits within the window's in-flight request
// and byte limits for the given generation, then accounts for it and returns
// true. It returns false if the generation is stale or either abort channel
// fires while waiting. A request made while nothing is in flight is always
// admitted, even if it alone exceeds the byte limit, so that replication can
// never deadlock.
func (w *adaptiveWindow) acquire(generation uint64, bytes uint64, abort1, abort2 <-chan struct{}) bool {
	w.mu.Lock()
	for {
		if generation != w.generation {
			w.mu.Unlock()
			return false
		}
		bytesOK := w.inflightBytes+bytes <= w.cfg.MaxInflightBytes || w.inflight == 0
		if w.inflight < w.window && bytesOK {
			w.inflight++
			w.inflightBytes += bytes
			w.mu.Unlock()
			return true
		}
		wait := w.waitCh
		w.mu.Unlock()
		select {
		case <-wait:
		case <-abort1:
			return false
		case <-abort2:
			return false
		}
		w.mu.Lock()
	}
}

// complete releases the slot held by a finished request and feeds the
// observed round trip time back into the controller. Completions carrying a
// stale generation (from a previous connection) are counted and ignored.
func (w *adaptiveWindow) complete(generation uint64, bytes uint64, rtt time.Duration, failed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if generation != w.generation {
		w.staleCompletions++
		w.emitCounterLocked("stale")
		return
	}
	if w.inflight > 0 {
		w.inflight--
	}
	if w.inflightBytes >= bytes {
		w.inflightBytes -= bytes
	} else {
		w.inflightBytes = 0
	}
	w.lastRTT = rtt
	switch {
	case failed:
		w.shrinkLocked()
	case w.slowLocked(rtt):
		w.shrinkLocked()
	default:
		// Additive increase: one slot per window's worth of successful
		// completions, so the window grows by roughly one per round trip.
		w.growCredits++
		if w.growCredits >= w.window {
			w.growCredits = 0
			w.growLocked()
		}
	}
	w.broadcastLocked()
}

// slowLocked reports whether the observed RTT indicates the connection has
// slowed down relative to the best RTT seen on this connection, and updates
// the baseline otherwise.
func (w *adaptiveWindow) slowLocked(rtt time.Duration) bool {
	if w.minRTT == 0 || rtt < w.minRTT {
		w.minRTT = rtt
		return false
	}
	return rtt > 2*w.minRTT && rtt-w.minRTT > windowRTTSlack
}

func (w *adaptiveWindow) growLocked() {
	if w.window >= w.cfg.MaxWindow {
		return
	}
	w.window++
	w.growEvents++
	if w.window > w.maxWindowObserved {
		w.maxWindowObserved = w.window
	}
	w.emitGaugeLocked()
	w.emitCounterLocked("grow")
}

func (w *adaptiveWindow) shrinkLocked() {
	if w.window <= w.cfg.MinWindow {
		return
	}
	if half := w.window / 2; half > w.cfg.MinWindow {
		w.window = half
	} else {
		w.window = w.cfg.MinWindow
	}
	w.shrinkEvents++
	if w.window < w.minWindowObserved {
		w.minWindowObserved = w.window
	}
	w.emitGaugeLocked()
	w.emitCounterLocked("shrink")
}

func (w *adaptiveWindow) broadcastLocked() {
	close(w.waitCh)
	w.waitCh = make(chan struct{})
}

func (w *adaptiveWindow) emitGaugeLocked() {
	labels := []metrics.Label{{Name: "peer_id", Value: string(w.peer)}}
	metrics.SetGaugeWithLabels([]string{"raft", "replication", "window"}, float32(w.window), labels)
}

func (w *adaptiveWindow) emitCounterLocked(event string) {
	labels := []metrics.Label{{Name: "peer_id", Value: string(w.peer)}}
	metrics.IncrCounterWithLabels([]string{"raft", "replication", "window", event}, 1, labels)
}

// diagnostics returns a snapshot of the window state.
func (w *adaptiveWindow) diagnostics() WindowDiagnostics {
	w.mu.Lock()
	defer w.mu.Unlock()
	return WindowDiagnostics{
		Generation:        w.generation,
		Window:            w.window,
		Inflight:          w.inflight,
		InflightBytes:     w.inflightBytes,
		GrowEvents:        w.growEvents,
		ShrinkEvents:      w.shrinkEvents,
		StaleCompletions:  w.staleCompletions,
		MinWindowObserved: w.minWindowObserved,
		MaxWindowObserved: w.maxWindowObserved,
		MinRTT:            w.minRTT,
		LastRTT:           w.lastRTT,
	}
}

// entriesBytes returns the total size of the log entry payloads.
func entriesBytes(logs []*Log) uint64 {
	var total uint64
	for _, l := range logs {
		total += uint64(len(l.Data))
	}
	return total
}
