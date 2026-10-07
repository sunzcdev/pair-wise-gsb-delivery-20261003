// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

// Package mux demultiplexes STUN, DTLS, TURN ChannelData, RTP and RTCP
// datagrams received on a single UDP socket into independent
// per-protocol connections, following RFC 7983.
package mux

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// OverflowPolicy decides what happens when a sub-connection queue is full.
type OverflowPolicy int

const (
	// DropNewest discards the incoming datagram when the queue is full.
	DropNewest OverflowPolicy = iota
	// DropOldest evicts the oldest queued datagram to make room for the
	// incoming one.
	DropOldest
)

// Stats is a snapshot of the packet counters of a sub-connection.
type Stats struct {
	// Received counts datagrams accepted into the queue.
	Received uint64
	// Dropped counts datagrams discarded because the queue was full or
	// the sub-connection was already closed.
	Dropped uint64
}

// Config controls a Mux. The zero values of QueueSize, Overflow and
// ReadBufferSize select the documented defaults.
type Config struct {
	// Conn is the shared UDP socket.
	Conn net.PacketConn
	// OwnConn declares ownership of Conn: when true, Close closes Conn.
	// When false, Close only stops the read loop and Conn stays open.
	OwnConn bool
	// QueueSize bounds each sub-connection queue. Defaults to 64.
	QueueSize int
	// Overflow selects the queue overflow policy. Defaults to DropNewest.
	Overflow OverflowPolicy
	// ReadBufferSize bounds the datagram size read from the socket.
	// Defaults to the maximum UDP datagram size.
	ReadBufferSize int
}

const (
	defaultQueueSize   = 64
	maxUDPDatagramSize = 1<<16 - 1
	subConnectionCount = 5
)

var errNilConn = errors.New("mux: nil PacketConn") //nolint:gochecknoglobals

// Mux demultiplexes datagrams read from a single UDP socket into
// per-protocol sub-connections. A slow consumer of one protocol never
// blocks the others: dispatch is non-blocking and overflows are handled
// according to the configured OverflowPolicy.
type Mux struct {
	conn      net.PacketConn
	ownConn   bool
	writeMu   sync.Mutex
	conns     [subConnectionCount]*Conn
	malformed atomic.Uint64
	closed    atomic.Bool
	loopDone  chan struct{}
	closeOnce sync.Once
}

// NewMux creates a Mux and starts its read loop on cfg.Conn.
func NewMux(cfg Config) (*Mux, error) {
	if cfg.Conn == nil {
		return nil, errNilConn
	}
	queueSize := cfg.QueueSize
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	bufSize := cfg.ReadBufferSize
	if bufSize <= 0 {
		bufSize = maxUDPDatagramSize
	}

	m := &Mux{
		conn:     cfg.Conn,
		ownConn:  cfg.OwnConn,
		loopDone: make(chan struct{}),
	}
	for _, proto := range []Protocol{ProtocolSTUN, ProtocolDTLS, ProtocolTURN, ProtocolRTP, ProtocolRTCP} {
		m.conns[proto-1] = newConn(m, proto, queueSize, cfg.Overflow)
	}

	go m.readLoop(bufSize)

	return m, nil
}

// Conn returns the sub-connection carrying the given protocol.
// It returns nil for ProtocolUnknown.
func (m *Mux) Conn(proto Protocol) *Conn {
	if proto <= ProtocolUnknown || int(proto) > subConnectionCount {
		return nil
	}

	return m.conns[proto-1]
}

// Malformed returns the count of datagrams that failed RFC 7983
// classification or the per-protocol header checks.
func (m *Mux) Malformed() uint64 {
	return m.malformed.Load()
}

// LocalAddr returns the local address of the shared socket.
func (m *Mux) LocalAddr() net.Addr {
	return m.conn.LocalAddr()
}

// Close closes all sub-connections and stops the read loop. The shared
// socket is closed only if ownership was declared with Config.OwnConn.
func (m *Mux) Close() error {
	var err error
	m.closeOnce.Do(func() {
		m.closed.Store(true)
		for _, conn := range m.conns {
			conn.close()
		}
		if m.ownConn {
			err = m.conn.Close()
		} else {
			// Interrupt the read loop without taking the socket.
			err = m.conn.SetReadDeadline(time.Now())
		}
		<-m.loopDone
		if !m.ownConn {
			_ = m.conn.SetReadDeadline(time.Time{})
		}
	})

	return err
}

func (m *Mux) readLoop(bufSize int) {
	defer close(m.loopDone)
	buf := make([]byte, bufSize)
	for {
		n, addr, err := m.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		// Copy to preserve the datagram beyond the next ReadFrom call.
		data := make([]byte, n)
		copy(data, buf[:n])

		proto := classify(data)
		if proto == ProtocolUnknown {
			m.malformed.Add(1)

			continue
		}
		m.conns[proto-1].enqueue(packet{data: data, addr: addr})
	}
}
