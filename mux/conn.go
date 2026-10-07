// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package mux

import (
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// packet is a single queued datagram with its source address.
type packet struct {
	data []byte
	addr net.Addr
}

// Conn is a per-protocol sub-connection of a Mux. It has an independent
// bounded queue, read deadline and close state; datagram boundaries and
// source addresses are preserved.
type Conn struct {
	proto     Protocol
	queue     chan packet
	policy    OverflowPolicy
	mux       *Mux
	done      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool
	deadline  atomic.Int64
	received  atomic.Uint64
	dropped   atomic.Uint64
}

func newConn(m *Mux, proto Protocol, queueSize int, policy OverflowPolicy) *Conn {
	return &Conn{
		proto:  proto,
		queue:  make(chan packet, queueSize),
		policy: policy,
		mux:    m,
		done:   make(chan struct{}),
	}
}

// Protocol returns the protocol this sub-connection carries.
func (c *Conn) Protocol() Protocol {
	return c.proto
}

// Stats returns a snapshot of the packet counters.
func (c *Conn) Stats() Stats {
	return Stats{Received: c.received.Load(), Dropped: c.dropped.Load()}
}

// Queued returns the number of datagrams currently waiting in the queue.
func (c *Conn) Queued() int {
	return len(c.queue)
}

// ReadFrom reads a single datagram into b, returning its source address.
// If b is smaller than the datagram, the excess is silently discarded,
// mirroring UDP semantics. It blocks until a datagram is available, the
// read deadline is exceeded, or the sub-connection is closed.
func (c *Conn) ReadFrom(b []byte) (int, net.Addr, error) {
	if c.closed.Load() {
		return 0, nil, net.ErrClosed
	}

	var timeout <-chan time.Time
	var timer *time.Timer
	if at := c.deadline.Load(); at > 0 {
		wait := time.Until(time.Unix(0, at))
		if wait <= 0 {
			return 0, nil, errTimeout
		}
		timer = time.NewTimer(wait)
		defer timer.Stop()
		timeout = timer.C
	}

	select {
	case p := <-c.queue:
		return copy(b, p.data), p.addr, nil
	case <-c.done:
		return 0, nil, net.ErrClosed
	case <-timeout:
		return 0, nil, errTimeout
	}
}

// WriteTo sends a datagram through the shared socket, serialized with
// writes from the other sub-connections.
func (c *Conn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	c.mux.writeMu.Lock()
	defer c.mux.writeMu.Unlock()

	return c.mux.conn.WriteTo(b, addr)
}

// Close closes the sub-connection. Pending and future ReadFrom calls
// return net.ErrClosed; incoming datagrams for this protocol are dropped
// and counted in Stats.Dropped. The shared socket stays open.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.done)
	})

	return nil
}

// SetReadDeadline sets the deadline for future ReadFrom calls. A zero
// value disables the deadline.
func (c *Conn) SetReadDeadline(t time.Time) error {
	if t.IsZero() {
		c.deadline.Store(0)
	} else {
		c.deadline.Store(t.UnixNano())
	}

	return nil
}

// LocalAddr returns the local address of the shared socket.
func (c *Conn) LocalAddr() net.Addr {
	return c.mux.conn.LocalAddr()
}

// enqueue offers p to the queue without ever blocking the shared read
// loop, applying the configured overflow policy when the queue is full.
func (c *Conn) enqueue(p packet) {
	if c.closed.Load() {
		c.dropped.Add(1)

		return
	}

	select {
	case c.queue <- p:
		c.received.Add(1)
	default:
		c.dropped.Add(1)
		if c.policy != DropOldest {
			return
		}
		select {
		case <-c.queue:
		default:
		}
		select {
		case c.queue <- p:
			c.received.Add(1)
		default:
		}
	}
}

func (c *Conn) close() {
	_ = c.Close()
}

var errTimeout error = timeoutError{} //nolint:gochecknoglobals,err113

// timeoutError is a net.Error returned when a read deadline is exceeded.
type timeoutError struct{}

func (timeoutError) Error() string   { return "mux: read deadline exceeded" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
func (timeoutError) Unwrap() error   { return os.ErrDeadlineExceeded }
