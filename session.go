// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Session is used to wrap a reliable ordered connection and to
// multiplex it into multiple streams.
type Session struct {
	// remoteGoAway indicates the remote side does
	// not want futher connections. Must be first for alignment.
	remoteGoAway int32

	// localGoAway indicates that we should stop
	// accepting futher connections. Must be first for alignment.
	localGoAway int32

	// nextStreamID is the next stream we should
	// send. This depends if we are a client/server.
	nextStreamID uint32

	// config holds our configuration
	config *Config

	// logger is used for our logs
	logger Logger

	// conn is the underlying connection
	conn io.ReadWriteCloser

	// bufRead is a buffered reader
	bufRead *bufio.Reader

	// pings is used to track inflight pings
	pings    map[uint32]chan struct{}
	pingID   uint32
	pingLock sync.Mutex

	// streams maps a stream id to a stream, and inflight has an entry
	// for any outgoing stream that has not yet been established. Both are
	// protected by streamLock.
	streams    map[uint32]*Stream
	inflight   map[uint32]struct{}
	streamLock sync.Mutex

	// synCh acts like a semaphore. It is sized to the AcceptBacklog which
	// is assumed to be symmetric between the client and server. This allows
	// the client to avoid exceeding the backlog and instead blocks the open.
	synCh chan struct{}

	// acceptCh is used to pass ready streams to the client
	acceptCh chan *Stream

	// extProbeDone is closed once the stream metadata capability
	// negotiation has completed (or was never started).
	extProbeDone chan struct{}

	// extProbeRespCh delivers the capability probe result to the
	// negotiation goroutine.
	extProbeRespCh chan bool

	// streamMetaEnabled is 1 once the stream metadata extension has
	// been negotiated with the peer.
	streamMetaEnabled int32

	// pendingAdmissions tracks inbound streams waiting for an admission
	// decision. Protected by streamLock.
	pendingAdmissions int

	// sendCh is used to mark a stream as ready to send,
	// or to send a header out directly.
	sendCh chan *sendReady

	// recvDoneCh is closed when recv() exits to avoid a race
	// between stream registration and stream shutdown
	recvDoneCh chan struct{}
	sendDoneCh chan struct{}

	// shutdown is used to safely close a session
	shutdown        bool
	shutdownErr     error
	shutdownCh      chan struct{}
	shutdownLock    sync.Mutex
	shutdownErrLock sync.Mutex
}

// sendReady is used to either mark a stream as ready
// or to directly send a header
type sendReady struct {
	Hdr  []byte
	mu   sync.Mutex // Protects Body from unsafe reads.
	Body []byte
	Err  chan error
}

// newSession is used to construct a new session
func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	logger := config.Logger
	if logger == nil {
		logger = log.New(config.LogOutput, "", log.LstdFlags)
	}

	s := &Session{
		config:         config,
		logger:         logger,
		conn:           conn,
		bufRead:        bufio.NewReader(conn),
		pings:          make(map[uint32]chan struct{}),
		streams:        make(map[uint32]*Stream),
		inflight:       make(map[uint32]struct{}),
		synCh:          make(chan struct{}, config.AcceptBacklog),
		acceptCh:       make(chan *Stream, config.AcceptBacklog),
		extProbeDone:   make(chan struct{}),
		extProbeRespCh: make(chan bool, 1),
		sendCh:         make(chan *sendReady, 64),
		recvDoneCh:     make(chan struct{}),
		sendDoneCh:     make(chan struct{}),
		shutdownCh:     make(chan struct{}),
	}
	if client {
		s.nextStreamID = 1
	} else {
		s.nextStreamID = 2
	}
	go s.recv()
	go s.send()
	if config.EnableKeepAlive {
		go s.keepalive()
	}
	if config.EnableStreamMeta {
		go s.negotiateStreamMeta()
	} else {
		close(s.extProbeDone)
	}
	return s
}

// IsClosed does a safe check to see if we have shutdown
func (s *Session) IsClosed() bool {
	select {
	case <-s.shutdownCh:
		return true
	default:
		return false
	}
}

// CloseChan returns a read-only channel which is closed as
// soon as the session is closed.
func (s *Session) CloseChan() <-chan struct{} {
	return s.shutdownCh
}

// NumStreams returns the number of currently open streams
func (s *Session) NumStreams() int {
	s.streamLock.Lock()
	num := len(s.streams)
	s.streamLock.Unlock()
	return num
}

// Open is used to create a new stream as a net.Conn
func (s *Session) Open() (net.Conn, error) {
	conn, err := s.OpenStream()
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// OpenWithMeta is used to create a new stream as a net.Conn, attaching
// bounded service metadata via the stream metadata extension.
func (s *Session) OpenWithMeta(ctx context.Context, meta []byte) (net.Conn, error) {
	conn, err := s.OpenStreamWithMeta(ctx, meta)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// OpenStream is used to create a new stream
func (s *Session) OpenStream() (*Stream, error) {
	return s.openStream(nil)
}

// OpenStreamWithMeta is used to create a new stream, attaching bounded
// service metadata (up to 1024 bytes) that the peer can inspect in its
// AdmitStream callback before accepting or rejecting the stream. It
// requires the optional stream metadata extension to be enabled on both
// ends; the given context is only used while waiting for the capability
// negotiation to complete. If the peer did not negotiate the extension,
// ErrStreamMetaUnsupported is returned and no unknown frames are sent.
func (s *Session) OpenStreamWithMeta(ctx context.Context, meta []byte) (*Stream, error) {
	if len(meta) == 0 {
		return s.OpenStream()
	}
	if len(meta) > maxStreamMetaSize {
		return nil, ErrStreamMetaTooLarge
	}
	if !s.config.EnableStreamMeta {
		return nil, ErrStreamMetaUnsupported
	}

	// Wait for the capability negotiation to conclude. We must never
	// send metadata frames to a peer that has not opted in.
	select {
	case <-s.extProbeDone:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.shutdownCh:
		return nil, ErrSessionShutdown
	}
	if atomic.LoadInt32(&s.streamMetaEnabled) == 0 {
		return nil, ErrStreamMetaUnsupported
	}
	return s.openStream(meta)
}

// openStream is used to create a new stream, optionally announcing it
// with service metadata first.
func (s *Session) openStream(meta []byte) (*Stream, error) {
	if s.IsClosed() {
		return nil, ErrSessionShutdown
	}
	if atomic.LoadInt32(&s.remoteGoAway) == 1 {
		return nil, ErrRemoteGoAway
	}

	// Block if we have too many inflight SYNs
	select {
	case s.synCh <- struct{}{}:
	case <-s.shutdownCh:
		return nil, ErrSessionShutdown
	}

GET_ID:
	// Get an ID, and check for stream exhaustion
	id := atomic.LoadUint32(&s.nextStreamID)
	if id >= math.MaxUint32-1 {
		return nil, ErrStreamsExhausted
	}
	if !atomic.CompareAndSwapUint32(&s.nextStreamID, id, id+2) {
		goto GET_ID
	}

	// Register the stream
	stream := newStream(s, id, streamInit)
	s.streamLock.Lock()
	s.streams[id] = stream
	s.inflight[id] = struct{}{}
	s.streamLock.Unlock()

	if s.config.StreamOpenTimeout > 0 {
		go s.setOpenTimeout(stream)
	}

	// Announce the stream metadata before the SYN so the peer can run
	// its admission decision before surfacing the stream.
	if len(meta) > 0 {
		if err := s.sendStreamMeta(id, meta); err != nil {
			select {
			case <-s.synCh:
			default:
				s.logger.Printf("[ERR] yamux: aborted stream open without inflight syn semaphore")
			}
			return nil, err
		}
	}

	// Send the window update to create
	if err := stream.sendWindowUpdate(); err != nil {
		select {
		case <-s.synCh:
		default:
			s.logger.Printf("[ERR] yamux: aborted stream open without inflight syn semaphore")
		}
		return nil, err
	}
	return stream, nil
}

// sendStreamMeta sends a stream metadata frame announcing a new stream.
func (s *Session) sendStreamMeta(id uint32, meta []byte) error {
	hdr := header(make([]byte, headerSize))
	hdr.encode(typeStreamMeta, 0, id, uint32(len(meta)))
	return s.waitForSend(hdr, meta)
}

// negotiateStreamMeta probes the peer for support of the stream
// metadata extension. A peer that understands the probe answers with
// pingIDStreamMetaAck; any other peer echoes the probe ID back, which
// disables the extension locally.
func (s *Session) negotiateStreamMeta() {
	defer close(s.extProbeDone)

	hdr := header(make([]byte, headerSize))
	hdr.encode(typePing, flagSYN, 0, pingIDStreamMetaProbe)
	if err := s.sendNoWait(hdr); err != nil {
		return
	}

	t := timerPool.Get()
	timer := t.(*time.Timer)
	timer.Reset(s.config.ConnectionWriteTimeout)
	defer func() {
		timer.Stop()
		select {
		case <-timer.C:
		default:
		}
		timerPool.Put(t)
	}()

	select {
	case supported := <-s.extProbeRespCh:
		if supported {
			atomic.StoreInt32(&s.streamMetaEnabled, 1)
		}
	case <-timer.C:
		s.logger.Printf("[WARN] yamux: stream metadata capability probe timed out, extension disabled")
	case <-s.shutdownCh:
	}
}

// setOpenTimeout implements a timeout for streams that are opened but not established.
// If the StreamOpenTimeout is exceeded we assume the peer is unable to ACK,
// and close the session.
// The number of running timers is bounded by the capacity of the synCh.
func (s *Session) setOpenTimeout(stream *Stream) {
	timer := time.NewTimer(s.config.StreamOpenTimeout)
	defer timer.Stop()

	select {
	case <-stream.establishCh:
		return
	case <-s.shutdownCh:
		return
	case <-timer.C:
		// Timeout reached while waiting for ACK.
		// Close the session to force connection re-establishment.
		s.logger.Printf("[ERR] yamux: aborted stream open (destination=%s): %v", s.RemoteAddr().String(), ErrTimeout.err)
		_ = s.Close()
	}
}

// Accept is used to block until the next available stream
// is ready to be accepted.
func (s *Session) Accept() (net.Conn, error) {
	conn, err := s.AcceptStream()
	if err != nil {
		return nil, err
	}
	return conn, err
}

// AcceptStream is used to block until the next available stream
// is ready to be accepted.
func (s *Session) AcceptStream() (*Stream, error) {
	select {
	case stream := <-s.acceptCh:
		if err := stream.sendWindowUpdate(); err != nil {
			return nil, err
		}
		return stream, nil
	case <-s.shutdownCh:
		return nil, s.shutdownErr
	}
}

// AcceptStream is used to block until the next available stream
// is ready to be accepted.
func (s *Session) AcceptStreamWithContext(ctx context.Context) (*Stream, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case stream := <-s.acceptCh:
		if err := stream.sendWindowUpdate(); err != nil {
			return nil, err
		}
		return stream, nil
	case <-s.shutdownCh:
		return nil, s.shutdownErr
	}
}

// Close is used to close the session and all streams.
// Attempts to send a GoAway before closing the connection.
func (s *Session) Close() error {
	s.shutdownLock.Lock()
	defer s.shutdownLock.Unlock()

	if s.shutdown {
		return nil
	}
	s.shutdown = true

	s.shutdownErrLock.Lock()
	if s.shutdownErr == nil {
		s.shutdownErr = ErrSessionShutdown
	}
	s.shutdownErrLock.Unlock()

	close(s.shutdownCh)

	_ = s.conn.Close()
	<-s.recvDoneCh

	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	for _, stream := range s.streams {
		stream.forceClose()
	}
	<-s.sendDoneCh
	return nil
}

// exitErr is used to handle an error that is causing the
// session to terminate.
func (s *Session) exitErr(err error) {
	s.shutdownErrLock.Lock()
	if s.shutdownErr == nil {
		s.shutdownErr = err
	}
	s.shutdownErrLock.Unlock()
	_ = s.Close()
}

// GoAway can be used to prevent accepting further
// connections. It does not close the underlying conn.
func (s *Session) GoAway() error {
	return s.waitForSend(s.goAway(goAwayNormal), nil)
}

// goAway is used to send a goAway message
func (s *Session) goAway(reason uint32) header {
	atomic.SwapInt32(&s.localGoAway, 1)
	hdr := header(make([]byte, headerSize))
	hdr.encode(typeGoAway, 0, 0, reason)
	return hdr
}

// Ping is used to measure the RTT response time
func (s *Session) Ping() (time.Duration, error) {
	// Get a channel for the ping
	ch := make(chan struct{})

	// Get a new ping id, mark as pending
	s.pingLock.Lock()
	id := s.pingID
	s.pingID++
	if s.pingID >= pingIDStreamMetaAck {
		// The top of the ID space is reserved for capability probes.
		s.pingID = 0
	}
	s.pings[id] = ch
	s.pingLock.Unlock()

	// Send the ping request
	hdr := header(make([]byte, headerSize))
	hdr.encode(typePing, flagSYN, 0, id)
	if err := s.waitForSend(hdr, nil); err != nil {
		return 0, err
	}

	// Wait for a response
	start := time.Now()
	select {
	case <-ch:
	case <-time.After(s.config.ConnectionWriteTimeout):
		s.pingLock.Lock()
		delete(s.pings, id) // Ignore it if a response comes later.
		s.pingLock.Unlock()
		return 0, ErrTimeout
	case <-s.shutdownCh:
		return 0, ErrSessionShutdown
	}

	// Compute the RTT
	return time.Since(start), nil
}

// keepalive is a long running goroutine that periodically does
// a ping to keep the connection alive.
func (s *Session) keepalive() {
	for {
		select {
		case <-time.After(s.config.KeepAliveInterval):
			_, err := s.Ping()
			if err != nil {
				if err != ErrSessionShutdown {
					s.logger.Printf("[ERR] yamux: keepalive failed: %v", err)
					s.exitErr(ErrKeepAliveTimeout)
				}
				return
			}
		case <-s.shutdownCh:
			return
		}
	}
}

// waitForSendErr waits to send a header, checking for a potential shutdown
func (s *Session) waitForSend(hdr header, body []byte) error {
	errCh := make(chan error, 1)
	return s.waitForSendErr(hdr, body, errCh)
}

// waitForSendErr waits to send a header with optional data, checking for a
// potential shutdown. Since there's the expectation that sends can happen
// in a timely manner, we enforce the connection write timeout here.
func (s *Session) waitForSendErr(hdr header, body []byte, errCh chan error) error {
	t := timerPool.Get()
	timer := t.(*time.Timer)
	timer.Reset(s.config.ConnectionWriteTimeout)
	defer func() {
		timer.Stop()
		select {
		case <-timer.C:
		default:
		}
		timerPool.Put(t)
	}()

	ready := &sendReady{Hdr: hdr, Body: body, Err: errCh}
	select {
	case s.sendCh <- ready:
	case <-s.shutdownCh:
		return ErrSessionShutdown
	case <-timer.C:
		return ErrConnectionWriteTimeout
	}

	bodyCopy := func() {
		if body == nil {
			return // A nil body is ignored.
		}

		// In the event of session shutdown or connection write timeout,
		// we need to prevent `send` from reading the body buffer after
		// returning from this function since the caller may re-use the
		// underlying array.
		ready.mu.Lock()
		defer ready.mu.Unlock()

		if ready.Body == nil {
			return // Body was already copied in `send`.
		}
		newBody := make([]byte, len(body))
		copy(newBody, body)
		ready.Body = newBody
	}

	select {
	case err := <-errCh:
		return err
	case <-s.shutdownCh:
		bodyCopy()
		return ErrSessionShutdown
	case <-timer.C:
		bodyCopy()
		return ErrConnectionWriteTimeout
	}
}

// sendNoWait does a send without waiting. Since there's the expectation that
// the send happens right here, we enforce the connection write timeout if we
// can't queue the header to be sent.
func (s *Session) sendNoWait(hdr header) error {
	t := timerPool.Get()
	timer := t.(*time.Timer)
	timer.Reset(s.config.ConnectionWriteTimeout)
	defer func() {
		timer.Stop()
		select {
		case <-timer.C:
		default:
		}
		timerPool.Put(t)
	}()

	select {
	case s.sendCh <- &sendReady{Hdr: hdr}:
		return nil
	case <-s.shutdownCh:
		return ErrSessionShutdown
	case <-timer.C:
		return ErrConnectionWriteTimeout
	}
}

// send is a long running goroutine that sends data
func (s *Session) send() {
	if err := s.sendLoop(); err != nil {
		s.exitErr(err)
	}
}

func (s *Session) sendLoop() error {
	defer close(s.sendDoneCh)
	var bodyBuf bytes.Buffer
	for {
		bodyBuf.Reset()

		select {
		case ready := <-s.sendCh:
			// Send a header if ready
			if ready.Hdr != nil {
				_, err := s.conn.Write(ready.Hdr)
				if err != nil {
					s.logger.Printf("[ERR] yamux: Failed to write header: %v", err)
					asyncSendErr(ready.Err, err)
					return err
				}
			}

			ready.mu.Lock()
			if ready.Body != nil {
				// Copy the body into the buffer to avoid
				// holding a mutex lock during the write.
				_, err := bodyBuf.Write(ready.Body)
				if err != nil {
					ready.Body = nil
					ready.mu.Unlock()
					s.logger.Printf("[ERR] yamux: Failed to copy body into buffer: %v", err)
					asyncSendErr(ready.Err, err)
					return err
				}
				ready.Body = nil
			}
			ready.mu.Unlock()

			if bodyBuf.Len() > 0 {
				// Send data from a body if given
				_, err := s.conn.Write(bodyBuf.Bytes())
				if err != nil {
					s.logger.Printf("[ERR] yamux: Failed to write body: %v", err)
					asyncSendErr(ready.Err, err)
					return err
				}
			}

			// No error, successful send
			asyncSendErr(ready.Err, nil)
		case <-s.shutdownCh:
			return nil
		}
	}
}

// recv is a long running goroutine that accepts new data
func (s *Session) recv() {
	if err := s.recvLoop(); err != nil {
		s.exitErr(err)
	}
}

// Ensure that the index of the handler (typeData/typeWindowUpdate/etc) matches the message type
var (
	handlers = []func(*Session, header) error{
		typeData:         (*Session).handleStreamMessage,
		typeWindowUpdate: (*Session).handleStreamMessage,
		typePing:         (*Session).handlePing,
		typeGoAway:       (*Session).handleGoAway,
		typeStreamMeta:   (*Session).handleStreamMeta,
	}
)

// recvLoop continues to receive data until a fatal error is encountered
func (s *Session) recvLoop() error {
	defer close(s.recvDoneCh)
	hdr := header(make([]byte, headerSize))
	for {
		// Read the header
		if _, err := io.ReadFull(s.bufRead, hdr); err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "closed") && !strings.Contains(err.Error(), "reset by peer") {
				s.logger.Printf("[ERR] yamux: Failed to read header: %v", err)
			}
			return err
		}

		// Verify the version
		if hdr.Version() != protoVersion {
			s.logger.Printf("[ERR] yamux: Invalid protocol version: %d", hdr.Version())
			return ErrInvalidVersion
		}

		mt := hdr.MsgType()
		if mt < typeData || mt > typeStreamMeta {
			return ErrInvalidMsgType
		}
		if mt == typeStreamMeta && !s.config.EnableStreamMeta {
			s.logger.Printf("[ERR] yamux: unexpected stream metadata frame, extension disabled")
			return ErrInvalidMsgType
		}

		if err := handlers[mt](s, hdr); err != nil {
			return err
		}
	}
}

// handleStreamMessage handles either a data or window update frame
func (s *Session) handleStreamMessage(hdr header) error {
	// Check for a new stream creation
	id := hdr.StreamID()
	flags := hdr.Flags()
	if flags&flagSYN == flagSYN {
		if err := s.incomingStream(id); err != nil {
			return err
		}
	}

	// Get the stream
	s.streamLock.Lock()
	stream := s.streams[id]
	s.streamLock.Unlock()

	// If we do not have a stream, likely we sent a RST
	if stream == nil {
		// Drain any data on the wire
		if hdr.MsgType() == typeData && hdr.Length() > 0 {
			s.logger.Printf("[WARN] yamux: Discarding data for stream: %d", id)
			if _, err := io.CopyN(io.Discard, s.bufRead, int64(hdr.Length())); err != nil {
				s.logger.Printf("[ERR] yamux: Failed to discard data: %v", err)
				return nil
			}
		} else {
			s.logger.Printf("[WARN] yamux: frame for missing stream: %v", hdr)
		}
		return nil
	}

	// Check if this is a window update
	if hdr.MsgType() == typeWindowUpdate {
		if err := stream.incrSendWindow(hdr, flags); err != nil {
			if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
				s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
			}
			return err
		}
		return nil
	}

	// Read the new data
	if err := stream.readData(hdr, flags, s.bufRead); err != nil {
		if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
			s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
		}
		return err
	}
	return nil
}

// handlePing is invokde for a typePing frame
func (s *Session) handlePing(hdr header) error {
	flags := hdr.Flags()
	pingID := hdr.Length()

	// Check if this is a query, respond back in a separate context so we
	// don't interfere with the receiving thread blocking for the write.
	if flags&flagSYN == flagSYN {
		// Intercept stream metadata capability probes. A peer that
		// supports the extension answers with pingIDStreamMetaAck;
		// anything else (including a plain echo, which is what a peer
		// without the extension produces) means it must not be used.
		if pingID == pingIDStreamMetaProbe {
			resp := pingIDStreamMetaProbe
			if s.config.EnableStreamMeta {
				resp = pingIDStreamMetaAck
			}
			go func() {
				hdr := header(make([]byte, headerSize))
				hdr.encode(typePing, flagACK, 0, resp)
				if err := s.sendNoWait(hdr); err != nil {
					s.logger.Printf("[WARN] yamux: failed to send capability reply: %v", err)
				}
			}()
			return nil
		}
		go func() {
			hdr := header(make([]byte, headerSize))
			hdr.encode(typePing, flagACK, 0, pingID)
			if err := s.sendNoWait(hdr); err != nil {
				s.logger.Printf("[WARN] yamux: failed to send ping reply: %v", err)
			}
		}()
		return nil
	}

	// Handle a capability probe response.
	if pingID == pingIDStreamMetaProbe || pingID == pingIDStreamMetaAck {
		select {
		case s.extProbeRespCh <- pingID == pingIDStreamMetaAck:
		default:
		}
		return nil
	}

	// Handle a response
	s.pingLock.Lock()
	ch := s.pings[pingID]
	if ch != nil {
		delete(s.pings, pingID)
		close(ch)
	}
	s.pingLock.Unlock()
	return nil
}

// handleGoAway is invokde for a typeGoAway frame
func (s *Session) handleGoAway(hdr header) error {
	code := hdr.Length()
	switch code {
	case goAwayNormal:
		atomic.SwapInt32(&s.remoteGoAway, 1)
	case goAwayProtoErr:
		s.logger.Printf("[ERR] yamux: received protocol error go away")
		return fmt.Errorf("yamux protocol error")
	case goAwayInternalErr:
		s.logger.Printf("[ERR] yamux: received internal error go away")
		return fmt.Errorf("remote yamux internal error")
	default:
		s.logger.Printf("[ERR] yamux: received unexpected go away")
		return fmt.Errorf("unexpected go away received")
	}
	return nil
}

// handleStreamMeta handles a stream metadata frame. The frame announces
// a new stream and carries bounded service metadata; the stream stays
// pending until the admission callback accepts or rejects it.
func (s *Session) handleStreamMeta(hdr header) error {
	id := hdr.StreamID()
	length := hdr.Length()
	if length > maxStreamMetaSize {
		s.logger.Printf("[ERR] yamux: stream metadata too large: %d", length)
		if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
			s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
		}
		return ErrStreamMetaTooLarge
	}

	meta := make([]byte, length)
	if _, err := io.ReadFull(s.bufRead, meta); err != nil {
		s.logger.Printf("[ERR] yamux: failed to read stream metadata: %v", err)
		return err
	}

	// Reject immediately if we are doing a go away
	if atomic.LoadInt32(&s.localGoAway) == 1 {
		hdr := header(make([]byte, headerSize))
		hdr.encode(typeWindowUpdate, flagRST, id, 0)
		return s.sendNoWait(hdr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stream := newStream(s, id, streamSYNReceived)
	stream.meta = meta
	stream.admitCancel = cancel
	stream.metaAnnounced = true

	s.streamLock.Lock()
	if _, ok := s.streams[id]; ok {
		// Metadata must arrive before the SYN and only once.
		s.streamLock.Unlock()
		cancel()
		s.logger.Printf("[WARN] yamux: unexpected stream metadata for stream: %d", id)
		return nil
	}
	if s.pendingAdmissions >= s.config.AcceptBacklog {
		s.streamLock.Unlock()
		cancel()
		s.logger.Printf("[WARN] yamux: admission backlog exceeded, forcing connection reset")
		hdr := header(make([]byte, headerSize))
		hdr.encode(typeWindowUpdate, flagRST, id, 0)
		return s.sendNoWait(hdr)
	}
	s.streams[id] = stream
	s.pendingAdmissions++
	s.streamLock.Unlock()

	s.runAdmission(ctx, stream, meta)
	return nil
}

// runAdmission evaluates the admission callback for a pending stream in
// a dedicated goroutine, so that a slow or cancelled callback can never
// stall the receive loop and the data or control frames of other
// streams.
func (s *Session) runAdmission(ctx context.Context, stream *Stream, meta []byte) {
	go func() {
		accepted := true
		if s.config.AdmitStream != nil {
			accepted = s.config.AdmitStream(ctx, meta)
		}
		s.completeAdmission(stream, accepted)
	}()
}

// completeAdmission hands an accepted stream over to the application
// via the accept queue, or resets a rejected one.
func (s *Session) completeAdmission(stream *Stream, accepted bool) {
	s.streamLock.Lock()
	s.pendingAdmissions--
	if s.streams[stream.id] != stream {
		// The stream was reset or the session shut down while the
		// admission callback was running.
		s.streamLock.Unlock()
		return
	}
	if accepted {
		select {
		case s.acceptCh <- stream:
			s.streamLock.Unlock()
			return
		default:
			// Backlog exceeded! RST the stream
			s.logger.Printf("[WARN] yamux: backlog exceeded, forcing connection reset")
			delete(s.streams, stream.id)
			s.streamLock.Unlock()
			stream.forceClose()
			hdr := header(make([]byte, headerSize))
			hdr.encode(typeWindowUpdate, flagRST, stream.id, 0)
			_ = s.sendNoWait(hdr)
			return
		}
	}
	delete(s.streams, stream.id)
	s.streamLock.Unlock()

	stream.forceClose()
	hdr := header(make([]byte, headerSize))
	hdr.encode(typeWindowUpdate, flagRST, stream.id, 0)
	if err := s.sendNoWait(hdr); err != nil {
		s.logger.Printf("[WARN] yamux: failed to reset rejected stream: %v", err)
	}
}

// incomingStream is used to create a new incoming stream
func (s *Session) incomingStream(id uint32) error {
	// Reject immediately if we are doing a go away
	if atomic.LoadInt32(&s.localGoAway) == 1 {
		hdr := header(make([]byte, headerSize))
		hdr.encode(typeWindowUpdate, flagRST, id, 0)
		return s.sendNoWait(hdr)
	}

	// Allocate a new stream
	stream := newStream(s, id, streamSYNReceived)

	s.streamLock.Lock()
	defer s.streamLock.Unlock()

	// Check if stream already exists
	if existing, ok := s.streams[id]; ok {
		if existing.metaAnnounced && !existing.synReceived {
			// The stream was announced by an earlier metadata frame;
			// this SYN completes its handshake and needs no further
			// action here.
			existing.synReceived = true
			return nil
		}
		s.logger.Printf("[ERR] yamux: duplicate stream declared")
		if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
			s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
		}
		return ErrDuplicateStream
	}

	// Register the stream
	s.streams[id] = stream

	// Check if we've exceeded the backlog
	select {
	case s.acceptCh <- stream:
		return nil
	default:
		// Backlog exceeded! RST the stream
		s.logger.Printf("[WARN] yamux: backlog exceeded, forcing connection reset")
		delete(s.streams, id)
		hdr := header(make([]byte, headerSize))
		hdr.encode(typeWindowUpdate, flagRST, id, 0)
		return s.sendNoWait(hdr)
	}
}

// closeStream is used to close a stream once both sides have
// issued a close. If there was an in-flight SYN and the stream
// was not yet established, then this will give the credit back.
func (s *Session) closeStream(id uint32) {
	s.streamLock.Lock()
	if stream, ok := s.streams[id]; ok && stream.admitCancel != nil {
		stream.admitCancel()
	}
	if _, ok := s.inflight[id]; ok {
		select {
		case <-s.synCh:
		default:
			s.logger.Printf("[ERR] yamux: SYN tracking out of sync")
		}
	}
	delete(s.streams, id)
	s.streamLock.Unlock()
}

// establishStream is used to mark a stream that was in the
// SYN Sent state as established.
func (s *Session) establishStream(id uint32) {
	s.streamLock.Lock()
	if _, ok := s.inflight[id]; ok {
		delete(s.inflight, id)
	} else {
		s.logger.Printf("[ERR] yamux: established stream without inflight SYN (no tracking entry)")
	}
	select {
	case <-s.synCh:
	default:
		s.logger.Printf("[ERR] yamux: established stream without inflight SYN (didn't have semaphore)")
	}
	s.streamLock.Unlock()
}
