package stun

import (
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type queuedPacket struct {
	data []byte
	addr net.Addr
}

type packetQueue struct {
	mu       sync.Mutex
	slots    []queuedPacket
	head     int
	size     int
	notify   chan struct{}
	closed   bool
	capacity int
	policy   OverflowPolicy
	protocol Protocol

	received              atomic.Uint64
	droppedQueueFull      atomic.Uint64
	droppedConsumerClosed atomic.Uint64
}

func newPacketQueue(capacity int, policy OverflowPolicy) *packetQueue {
	return &packetQueue{
		slots:    make([]queuedPacket, capacity),
		notify:   make(chan struct{}, 1),
		capacity: capacity,
		policy:   policy,
	}
}

func (q *packetQueue) enqueue(packet queuedPacket) PacketDropReason {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.received.Add(1)

	if q.closed {
		q.droppedConsumerClosed.Add(1)

		return PacketDropConsumerClosed
	}

	if q.size < q.capacity {
		index := (q.head + q.size) % q.capacity
		q.slots[index] = packet
		q.size++
		q.signalLocked()

		return PacketDropNone
	}

	if q.policy == OverflowDropOldest {
		index := (q.head + q.size) % q.capacity
		q.slots[q.head] = queuedPacket{}
		q.head = (q.head + 1) % q.capacity
		q.slots[index] = packet
		q.signalLocked()
	}

	q.droppedQueueFull.Add(1)

	return PacketDropQueueFull
}

func (q *packetQueue) signalLocked() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *packetQueue) dequeue() (queuedPacket, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.size == 0 {
		return queuedPacket{}, false
	}

	packet := q.slots[q.head]
	q.slots[q.head] = queuedPacket{}
	q.head = (q.head + 1) % q.capacity
	q.size--

	return packet, true
}

func (q *packetQueue) notifyChannel() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.notify
}

func (q *packetQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return
	}

	q.closed = true
	q.droppedConsumerClosed.Add(uint64(q.size))
	q.slots = nil
	q.size = 0
	q.head = 0
	close(q.notify)
}

func (q *packetQueue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.closed
}

func (q *packetQueue) stats() QueueStats {
	q.mu.Lock()
	defer q.mu.Unlock()

	return QueueStats{
		Protocol:              q.protocol,
		Capacity:              q.capacity,
		Length:                q.size,
		OverflowPolicy:        q.policy,
		Received:              q.received.Load(),
		DroppedQueueFull:      q.droppedQueueFull.Load(),
		DroppedConsumerClosed: q.droppedConsumerClosed.Load(),
		Closed:                q.closed,
	}
}

// MuxConn is one protocol-specific view of the multiplexed packet socket.
type MuxConn interface {
	net.PacketConn
	io.Reader
	io.Writer

	// Protocol returns the protocol carried by this connection.
	Protocol() Protocol

	// RemoteAddr returns the source address represented by this sub-connection.
	RemoteAddr() net.Addr

	// Stats returns a point-in-time snapshot of this connection's queue.
	Stats() QueueStats

	// Closed reports whether this protocol consumer is closed.
	Closed() bool
}

type muxConn struct {
	protocol   Protocol
	remoteAddr net.Addr
	conn       net.PacketConn
	mux        *Mux
	queue      *packetQueue
	key        streamKey

	deadlineMu   sync.Mutex
	readDeadline time.Time
	readWake     chan struct{}
	readTimer    *time.Timer
}

var _ Connection = (*muxConn)(nil)

func newMuxConn(key streamKey, remoteAddr net.Addr, mux *Mux, queue *packetQueue) *muxConn {
	return &muxConn{
		protocol:   key.protocol,
		remoteAddr: remoteAddr,
		conn:       mux.conn,
		mux:        mux,
		queue:      queue,
		key:        key,
		readWake:   make(chan struct{}),
	}
}

func (c *muxConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if len(p) == 0 {
		return 0, nil, nil
	}

	for {
		packet, ok := c.queue.dequeue()
		if ok {
			return copy(p, packet.data), packet.addr, nil
		}

		if c.queue.isClosed() {
			return 0, nil, net.ErrClosed
		}

		notify, wake, expired := c.readWait()
		if expired {
			return 0, nil, os.ErrDeadlineExceeded
		}

		select {
		case <-notify:
		case <-wake:
		}
	}
}

func (c *muxConn) readWait() (<-chan struct{}, <-chan struct{}, bool) {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()

	if !c.readDeadline.IsZero() && time.Until(c.readDeadline) <= 0 {
		return nil, nil, true
	}

	return c.queue.notifyChannel(), c.readWake, false
}

func (c *muxConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if c.queue.isClosed() {
		return 0, net.ErrClosed
	}

	return c.conn.WriteTo(p, addr)
}

func (c *muxConn) Close() error {
	c.deadlineMu.Lock()
	if c.readTimer != nil {
		c.readTimer.Stop()
		c.readTimer = nil
	}
	c.deadlineMu.Unlock()

	c.mux.closeStream(c)

	return nil
}

func (c *muxConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *muxConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}

	return c.SetWriteDeadline(t)
}

func (c *muxConn) SetReadDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()

	if c.readTimer != nil {
		c.readTimer.Stop()
		c.readTimer = nil
	}

	c.readDeadline = t
	oldWake := c.readWake
	c.readWake = make(chan struct{})
	close(oldWake)

	if t.IsZero() {
		return nil
	}

	wait := time.Until(t)
	if wait <= 0 {
		close(c.readWake)

		return nil
	}

	wake := c.readWake
	c.readTimer = time.AfterFunc(wait, func() {
		c.deadlineMu.Lock()
		defer c.deadlineMu.Unlock()

		if c.readWake == wake {
			close(wake)
		}
	})

	return nil
}

// SetWriteDeadline configures the shared underlying socket. The deadline is
// socket-global rather than isolated by protocol sub-connection.
func (c *muxConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

func (c *muxConn) Protocol() Protocol {
	return c.protocol
}

func (c *muxConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (c *muxConn) Read(p []byte) (int, error) {
	n, _, err := c.ReadFrom(p)

	return n, err
}

func (c *muxConn) Write(p []byte) (int, error) {
	return c.WriteTo(p, c.remoteAddr)
}

func (c *muxConn) Stats() QueueStats {
	stats := c.queue.stats()
	stats.RemoteAddr = c.remoteAddr.String()

	return stats
}

func (c *muxConn) Closed() bool {
	return c.queue.isClosed()
}
