package stun

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// SocketOwnership declares who owns the packet socket passed to NewMux.
type SocketOwnership int

const (
	// DoesNotOwnSocket means Close does not close the underlying socket.
	DoesNotOwnSocket SocketOwnership = iota
	// OwnsSocket means Close closes the underlying socket.
	OwnsSocket
)

// OverflowPolicy defines what happens when a sub-connection queue is full.
type OverflowPolicy uint8

const (
	// OverflowDropNewest drops the datagram which could not be enqueued.
	OverflowDropNewest OverflowPolicy = iota
	// OverflowDropOldest removes the oldest queued datagram and enqueues the
	// newest datagram.
	OverflowDropOldest
)

// PacketDropReason reports why a classified datagram was not retained.
type PacketDropReason uint8

const (
	// PacketDropNone means the datagram was enqueued.
	PacketDropNone PacketDropReason = iota
	// PacketDropQueueFull means the bounded queue rejected or displaced it.
	PacketDropQueueFull
	// PacketDropConsumerClosed means the consumer was already closed.
	PacketDropConsumerClosed
)

// QueueStats contains point-in-time statistics for one sub-connection queue.
type QueueStats struct {
	Protocol              Protocol
	RemoteAddr            string
	Capacity              int
	Length                int
	OverflowPolicy        OverflowPolicy
	Received              uint64
	DroppedQueueFull      uint64
	DroppedConsumerClosed uint64
	Closed                bool
}

// Stats contains Mux and per-sub-connection queue statistics.
type Stats struct {
	ReceivedPackets  uint64
	MalformedPackets uint64
	ReadErrors       uint64
	PendingDropped   uint64
	ActiveConns      int
	PendingConns     int
	Queues           []QueueStats
}

// Option configures a Mux.
type Option func(*muxConfig) error

type queueConfig struct {
	capacity int
	policy   OverflowPolicy
}

type muxConfig struct {
	readBufferSize int
	pendingLimit   int
	defaultQueue   queueConfig
	queues         map[Protocol]queueConfig
}

type streamKey struct {
	protocol Protocol
	network  string
	address  string
}

// WithReadBufferSize sets the size used for each ReadFrom call.
func WithReadBufferSize(size int) Option {
	return func(cfg *muxConfig) error {
		if size <= 0 {
			return errors.New("read buffer size must be positive")
		}

		cfg.readBufferSize = size

		return nil
	}
}

// WithPendingConnectionLimit bounds the number of unaccepted sub-connections.
func WithPendingConnectionLimit(limit int) Option {
	return func(cfg *muxConfig) error {
		if limit <= 0 {
			return errors.New("pending connection limit must be positive")
		}

		cfg.pendingLimit = limit

		return nil
	}
}

// WithQueueCapacity sets the default bounded queue capacity.
func WithQueueCapacity(capacity int) Option {
	return func(cfg *muxConfig) error {
		if capacity <= 0 {
			return errors.New("queue capacity must be positive")
		}

		cfg.defaultQueue.capacity = capacity

		return nil
	}
}

// WithOverflowPolicy sets the default queue overflow policy.
func WithOverflowPolicy(policy OverflowPolicy) Option {
	return func(cfg *muxConfig) error {
		if policy != OverflowDropNewest && policy != OverflowDropOldest {
			return errors.New("unknown overflow policy")
		}

		cfg.defaultQueue.policy = policy

		return nil
	}
}

// WithProtocolQueue sets capacity and overflow policy for one protocol.
func WithProtocolQueue(protocol Protocol, capacity int, policy OverflowPolicy) Option {
	return func(cfg *muxConfig) error {
		if !isRoutableProtocol(protocol) {
			return errors.New("unknown protocol queue")
		}

		if capacity <= 0 {
			return errors.New("queue capacity must be positive")
		}

		if policy != OverflowDropNewest && policy != OverflowDropOldest {
			return errors.New("unknown overflow policy")
		}

		cfg.queues[protocol] = queueConfig{capacity: capacity, policy: policy}

		return nil
	}
}

// Mux demultiplexes packet datagrams according to RFC 7983. The read loop never
// blocks on a protocol consumer; every protocol and source pair has an
// independent bounded queue.
type Mux struct {
	conn         net.PacketConn
	ownsSocket   bool
	readSize     int
	pendingLimit int

	queueConfigs [ProtocolTURN + 1]queueConfig

	stateMu       sync.Mutex
	streams       map[streamKey]*muxConn
	closedStreams map[streamKey]*muxConn
	pending       [ProtocolTURN + 1][]MuxConn
	notify        [ProtocolTURN + 1]chan struct{}
	muxClosed     bool

	received       atomic.Uint64
	malformed      atomic.Uint64
	readError      atomic.Uint64
	pendingDropped atomic.Uint64

	closeOnce sync.Once
	closeMu   sync.Mutex
	closing   bool
	stopped   bool
	closeErr  error
	done      chan struct{}
}

// NewMux creates a Mux and starts its read loop. Socket ownership must be
// explicitly declared: OwnsSocket makes Close close conn, while
// DoesNotOwnSocket leaves conn open and interrupts only the Mux read loop.
func NewMux(conn net.PacketConn, ownership SocketOwnership, options ...Option) (*Mux, error) {
	if conn == nil {
		return nil, errors.New("connection must not be nil")
	}

	if ownership != DoesNotOwnSocket && ownership != OwnsSocket {
		return nil, errors.New("unknown socket ownership")
	}

	cfg := muxConfig{
		readBufferSize: 65535,
		pendingLimit:   1024,
		defaultQueue: queueConfig{
			capacity: 128,
			policy:   OverflowDropNewest,
		},
		queues: make(map[Protocol]queueConfig),
	}

	for _, option := range options {
		if err := option(&cfg); err != nil {
			return nil, err
		}
	}

	mux := &Mux{
		conn:          conn,
		ownsSocket:    ownership == OwnsSocket,
		readSize:      cfg.readBufferSize,
		pendingLimit:  cfg.pendingLimit,
		streams:       make(map[streamKey]*muxConn),
		closedStreams: make(map[streamKey]*muxConn),
		done:          make(chan struct{}),
	}

	for protocol := ProtocolSTUN; protocol <= ProtocolTURN; protocol++ {
		mux.queueConfigs[protocol] = cfg.defaultQueue
		if protocolCfg, ok := cfg.queues[protocol]; ok {
			mux.queueConfigs[protocol] = protocolCfg
		}

		mux.notify[protocol] = make(chan struct{}, 1)
	}

	go mux.run()

	return mux, nil
}

// Accept returns the next sub-connection for a protocol. A sub-connection is
// identified by protocol and source address and owns its queue and deadlines.
func (m *Mux) Accept(protocol Protocol) (MuxConn, error) {
	if !isRoutableProtocol(protocol) {
		return nil, errors.New("unknown protocol queue")
	}

	for {
		m.stateMu.Lock()
		if len(m.pending[protocol]) > 0 {
			stream := m.pending[protocol][0]
			m.pending[protocol] = m.pending[protocol][1:]
			m.stateMu.Unlock()

			return stream, nil
		}

		if m.muxClosed {
			m.stateMu.Unlock()

			return nil, net.ErrClosed
		}

		notify := m.notify[protocol]
		m.stateMu.Unlock()

		select {
		case <-notify:
		case <-m.done:
			return nil, net.ErrClosed
		}
	}
}

// Stats returns global counters and a snapshot of active and closed queues.
func (m *Mux) Stats() Stats {
	m.stateMu.Lock()
	queues := make([]QueueStats, 0, len(m.streams)+len(m.closedStreams))

	activeConns := len(m.streams)
	var pendingConns int
	for _, stream := range m.streams {
		queueStats := stream.Stats()
		queues = append(queues, queueStats)
	}
	for _, pending := range m.pending {
		pendingConns += len(pending)
	}
	for _, stream := range m.closedStreams {
		queues = append(queues, stream.Stats())
	}
	m.stateMu.Unlock()

	return Stats{
		ReceivedPackets:  m.received.Load(),
		MalformedPackets: m.malformed.Load(),
		ReadErrors:       m.readError.Load(),
		PendingDropped:   m.pendingDropped.Load(),
		ActiveConns:      activeConns,
		PendingConns:     pendingConns,
		Queues:           queues,
	}
}

// Done is closed after the read loop has stopped and all consumers have been
// closed.
func (m *Mux) Done() <-chan struct{} {
	return m.done
}

// Close stops the read loop. It closes the underlying socket only when ownership
// was declared as OwnsSocket.
func (m *Mux) Close() error {
	m.closeOnce.Do(func() {
		m.closeMu.Lock()
		if m.stopped {
			m.closeMu.Unlock()

			return
		}

		m.closing = true

		if m.ownsSocket {
			m.closeErr = m.conn.Close()
			m.closeMu.Unlock()

			return
		}

		m.closeErr = m.conn.SetReadDeadline(time.Unix(1, 0))
		m.closeMu.Unlock()
	})

	<-m.done

	m.closeMu.Lock()
	defer m.closeMu.Unlock()

	return m.closeErr
}

func (m *Mux) run() {
	defer close(m.done)

	buffer := make([]byte, m.readSize)

	for {
		n, addr, err := m.conn.ReadFrom(buffer)
		if err != nil {
			m.stop(err)

			return
		}

		m.received.Add(1)

		payload := buffer[:n]
		protocol, ok := Classify(payload)
		if !ok {
			m.malformed.Add(1)

			continue
		}

		data := make([]byte, n)
		copy(data, payload)
		m.deliver(queuedPacket{data: data, addr: cloneAddr(addr)}, protocol)
	}
}

func (m *Mux) deliver(packet queuedPacket, protocol Protocol) {
	key := streamKey{
		protocol: protocol,
		network:  packet.addr.Network(),
		address:  packet.addr.String(),
	}

	m.stateMu.Lock()
	stream, ok := m.streams[key]
	if !ok && !m.muxClosed {
		pendingCount := 0
		for protocol := ProtocolSTUN; protocol <= ProtocolTURN; protocol++ {
			pendingCount += len(m.pending[protocol])
		}

		if pendingCount >= m.pendingLimit {
			m.stateMu.Unlock()
			m.pendingDropped.Add(1)

			return
		}

		delete(m.closedStreams, key)
		cfg := m.queueConfigs[protocol]
		stream = newMuxConn(key, packet.addr, m, newPacketQueue(cfg.capacity, cfg.policy))
		m.streams[key] = stream
		m.pending[protocol] = append(m.pending[protocol], stream)

		select {
		case m.notify[protocol] <- struct{}{}:
		default:
		}
	}
	m.stateMu.Unlock()

	if stream != nil {
		stream.queue.enqueue(packet)
	}
}

func (m *Mux) stop(err error) {
	m.closeMu.Lock()
	closing := m.closing
	m.closing = true
	m.stopped = true
	if !closing {
		m.closeErr = err
		m.readError.Add(1)
	}
	m.closeMu.Unlock()

	m.stateMu.Lock()
	m.muxClosed = true
	streams := make([]*muxConn, 0, len(m.streams))
	for _, stream := range m.streams {
		m.closedStreams[stream.key] = stream
		streams = append(streams, stream)
	}
	m.streams = make(map[streamKey]*muxConn)
	m.pending = [ProtocolTURN + 1][]MuxConn{}
	for protocol := ProtocolSTUN; protocol <= ProtocolTURN; protocol++ {
		select {
		case m.notify[protocol] <- struct{}{}:
		default:
		}
	}
	m.stateMu.Unlock()

	for _, stream := range streams {
		stream.queue.close()
	}

	if !closing && m.ownsSocket {
		_ = m.conn.Close()
	}

	if closing && !m.ownsSocket {
		_ = m.conn.SetReadDeadline(time.Time{})
	}
}

func (m *Mux) closeStream(stream *muxConn) {
	m.stateMu.Lock()
	delete(m.streams, stream.key)
	m.closedStreams[stream.key] = stream
	m.removePendingLocked(stream.key.protocol, stream)
	m.stateMu.Unlock()

	stream.queue.close()
}

func (m *Mux) removePendingLocked(protocol Protocol, target MuxConn) {
	pending := m.pending[protocol][:0]
	for _, stream := range m.pending[protocol] {
		if stream != target {
			pending = append(pending, stream)
		}
	}

	m.pending[protocol] = pending
}

func isRoutableProtocol(protocol Protocol) bool {
	return protocol >= ProtocolSTUN && protocol <= ProtocolTURN
}

func cloneAddr(addr net.Addr) net.Addr {
	switch value := addr.(type) {
	case *net.UDPAddr:
		clone := *value
		clone.IP = append(net.IP(nil), value.IP...)

		return &clone
	default:
		return addr
	}
}
