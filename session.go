// MIT License
//
// Copyright (c) 2016-2017 xtaci
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package smux

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultAcceptBacklog = 1024
	minShaperNotifySize  = 16
	maxShaperSize        = 1024
	openCloseTimeout     = 30 * time.Second // Timeout for opening/closing streams
	// maxControlWriteBuffer caps payload bytes of queued control frames
	// (SYN/FIN/NOP/UPD). Control frames keep an independent small quota so
	// that a data backlog can never prevent close notifications from being
	// sent.
	maxControlWriteBuffer = 64 * 1024
)

// resultChanPool reduces allocation of result channels
var resultChanPool = sync.Pool{
	New: func() any {
		return make(chan writeResult, 1)
	},
}

// CLASSID represents the class of a frame
type CLASSID int

const (
	CLSCTRL CLASSID = iota // prioritized control signal
	CLSDATA
)

// timeoutError representing timeouts for operations such as accept, read and write
//
// To better cooperate with the standard library, timeoutError should implement the standard library's `net.Error`.
//
// For example, using smux to implement net.Listener and work with http.Server, the keep-alive connection (*smux.Stream) will be unexpectedly closed.
// For more details, see https://github.com/xtaci/smux/pull/99.
type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Temporary() bool { return true }
func (timeoutError) Timeout() bool   { return true }

var (
	ErrInvalidProtocol           = errors.New("invalid protocol")
	ErrConsumed                  = errors.New("peer consumed more than sent")
	ErrGoAway                    = errors.New("stream id overflows, should start a new connection")
	ErrTimeout         net.Error = &timeoutError{}
	ErrWouldBlock                = errors.New("operation would block on IO")
	// ErrSendBufferFull is returned by stream writes when the session-level
	// or per-stream send buffer limit is reached and the session is
	// configured for non-blocking writes (Config.WriteBlock == false).
	ErrSendBufferFull = errors.New("send buffer full")
)

// chargeKind identifies which send budget a writeRequest is charged to.
// Every reservation is released exactly once, either by sendLoop after the
// frame has been processed or by writeFrameInternal when the request never
// enters the send queue.
type chargeKind int

const (
	chargeNone chargeKind = iota
	chargeData
	chargeControl
)

// writeRequest represents a request to write a frame
type writeRequest struct {
	class   CLASSID
	frame   Frame
	seq     uint32
	result  chan writeResult
	charge  chargeKind  // send budget this request has been charged to
	charges int         // payload bytes reserved from the budget
	strm    *stream     // owning stream for per-stream accounting, nil for session frames
	flags   *writeFlags // shared across all copies of the request in channels/queues
}

// writeFlags is shared by every copy of a writeRequest as it travels through
// the shaper channel and queues, so that cancellation from the writer is
// visible to sendLoop and the reservation refund happens exactly once.
type writeFlags struct {
	released int32 // atomic; ensures the reservation is released at most once
	canceled int32 // atomic; set when the writer gives up (deadline/close) before the frame is sent
}

// writeResult represents the result of a write request
type writeResult struct {
	n   int
	err error
}

// Session defines a multiplexed connection for streams
type Session struct {
	conn io.ReadWriteCloser

	config           *Config
	goAway           int32  // flag id exhausted
	nextStreamID     uint32 // next stream identifier
	nextStreamIDLock sync.Mutex

	bucket       int32         // token bucket
	bucketNotify chan struct{} // used for waiting for tokens

	streams    map[uint32]*stream // all streams in this session
	streamLock sync.Mutex         // locks streams

	die     chan struct{} // flag session has died
	dieOnce sync.Once
	closed  int32 // atomic flag for fast IsClosed check

	// socket error handling
	socketReadError      atomic.Value
	socketWriteError     atomic.Value
	chSocketReadError    chan struct{}
	chSocketWriteError   chan struct{}
	socketReadErrorOnce  sync.Once
	socketWriteErrorOnce sync.Once

	// smux protocol errors
	protoError     atomic.Value
	chProtoError   chan struct{}
	protoErrorOnce sync.Once

	chAccepts chan *stream

	sessionIsActive int32        // flag session is active
	acceptDeadline  atomic.Value // deadline for Accept()

	requestID        uint32            // Monotonic increasing write request ID
	shaper           chan writeRequest // a shaper for writing
	sq               *shaperQueue
	chShaperPending  chan struct{}
	chShaperConsumed chan struct{}

	// send-side backpressure accounting. writeBudget tracks payload bytes of
	// queued data frames (PSH), controlBudget tracks payload bytes of queued
	// control frames (SYN/FIN/NOP/UPD). The two budgets are independent so
	// that a saturated data queue never blocks control notifications.
	//
	// budgetLock serializes reservation/refund of both budgets and the
	// per-stream pending counters, eliminating reservation races; waiters
	// sleep on edge-triggered channels while not holding the lock.
	writeBudget   int64
	controlBudget int64
	budgetLock    sync.Mutex
	chBudgetSpace chan struct{} // signaled when session data budget is released
}

func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	s := new(Session)
	s.die = make(chan struct{})
	s.conn = conn
	s.config = config
	s.streams = make(map[uint32]*stream)
	s.chAccepts = make(chan *stream, defaultAcceptBacklog)
	s.bucket = int32(config.MaxReceiveBuffer)
	s.bucketNotify = make(chan struct{}, 1)
	s.shaper = make(chan writeRequest, maxShaperSize)
	s.chSocketReadError = make(chan struct{})
	s.chSocketWriteError = make(chan struct{})
	s.chProtoError = make(chan struct{})
	s.chShaperPending = make(chan struct{}, 1)
	s.chShaperConsumed = make(chan struct{}, 1)
	s.sq = NewShaperQueue()
	s.chBudgetSpace = make(chan struct{}, 1)

	if client {
		s.nextStreamID = 1
	} else {
		s.nextStreamID = 0
	}

	go s.shaperLoop()
	go s.recvLoop()
	go s.sendLoop()
	if !config.KeepAliveDisabled {
		go s.keepalive()
	}
	return s
}

// OpenStream is used to create a new stream
func (s *Session) OpenStream() (*Stream, error) {
	if s.IsClosed() {
		return nil, io.ErrClosedPipe
	}

	// generate stream id
	s.nextStreamIDLock.Lock()
	if s.goAway > 0 {
		s.nextStreamIDLock.Unlock()
		return nil, ErrGoAway
	}

	// check for stream id overflow
	if s.nextStreamID+2 < s.nextStreamID {
		s.goAway = 1
		s.nextStreamIDLock.Unlock()
		return nil, ErrGoAway
	}

	// allocate next stream id
	s.nextStreamID += 2
	sid := s.nextStreamID
	s.nextStreamIDLock.Unlock()

	stream := newStream(sid, s.config.MaxFrameSize, s)

	if _, err := s.writeControlFrame(newFrame(byte(s.config.Version), cmdSYN, sid)); err != nil {
		return nil, err
	}

	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	select {
	case <-s.chSocketReadError:
		return nil, s.socketReadError.Load().(error)
	case <-s.chSocketWriteError:
		return nil, s.socketWriteError.Load().(error)
	case <-s.die:
		return nil, io.ErrClosedPipe
	default:
		s.streams[sid] = stream
		wrapper := &Stream{stream: stream}
		// NOTE(x): disabled finalizer for issue #997
		/*
			runtime.SetFinalizer(wrapper, func(s *Stream) {
				s.Close()
			})
		*/
		return wrapper, nil
	}
}

// Open returns a generic ReadWriteCloser
func (s *Session) Open() (io.ReadWriteCloser, error) {
	return s.OpenStream()
}

// AcceptStream is used to block until the next available stream
// is ready to be accepted.
func (s *Session) AcceptStream() (*Stream, error) {
	var deadline <-chan time.Time
	if d, ok := s.acceptDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	select {
	case stream := <-s.chAccepts:
		wrapper := &Stream{stream: stream}
		runtime.SetFinalizer(wrapper, func(s *Stream) {
			s.Close()
		})
		return wrapper, nil
	case <-deadline:
		return nil, ErrTimeout
	case <-s.chSocketReadError:
		return nil, s.socketReadError.Load().(error)
	case <-s.chProtoError:
		return nil, s.protoError.Load().(error)
	case <-s.die:
		return nil, io.ErrClosedPipe
	}
}

// Accept Returns a generic ReadWriteCloser instead of smux.Stream
func (s *Session) Accept() (io.ReadWriteCloser, error) {
	return s.AcceptStream()
}

// Close is used to close the session and all streams.
func (s *Session) Close() error {
	var once bool
	s.dieOnce.Do(func() {
		atomic.StoreInt32(&s.closed, 1)
		close(s.die)
		once = true
	})

	if !once {
		return io.ErrClosedPipe
	}

	s.streamLock.Lock()
	for k := range s.streams {
		s.streams[k].sessionClose()
	}
	s.streamLock.Unlock()
	return s.conn.Close()
}

// CloseChan can be used by someone who wants to be notified immediately when this
// session is closed
func (s *Session) CloseChan() <-chan struct{} {
	return s.die
}

// notifyBucket notifies recvLoop that bucket is available
func (s *Session) notifyBucket() {
	select {
	case s.bucketNotify <- struct{}{}:
	default:
	}
}

func (s *Session) notifyReadError(err error) {
	s.socketReadErrorOnce.Do(func() {
		s.socketReadError.Store(err)
		close(s.chSocketReadError)
	})
}

func (s *Session) notifyWriteError(err error) {
	s.socketWriteErrorOnce.Do(func() {
		s.socketWriteError.Store(err)
		close(s.chSocketWriteError)
	})
}

func (s *Session) notifyProtoError(err error) {
	s.protoErrorOnce.Do(func() {
		s.protoError.Store(err)
		close(s.chProtoError)
	})
}

// IsClosed does a safe check to see if we have shutdown
func (s *Session) IsClosed() bool {
	return atomic.LoadInt32(&s.closed) != 0
}

// NumStreams returns the number of currently open streams
func (s *Session) NumStreams() int {
	if s.IsClosed() {
		return 0
	}
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	return len(s.streams)
}

// SetDeadline sets a deadline used by Accept* calls.
// A zero time value disables the deadline.
func (s *Session) SetDeadline(t time.Time) error {
	s.acceptDeadline.Store(t)
	return nil
}

// LocalAddr satisfies net.Conn interface
func (s *Session) LocalAddr() net.Addr {
	if ts, ok := s.conn.(interface {
		LocalAddr() net.Addr
	}); ok {
		return ts.LocalAddr()
	}
	return nil
}

// RemoteAddr satisfies net.Conn interface
func (s *Session) RemoteAddr() net.Addr {
	if ts, ok := s.conn.(interface {
		RemoteAddr() net.Addr
	}); ok {
		return ts.RemoteAddr()
	}
	return nil
}

// notify the session that a stream has closed
func (s *Session) streamClosed(sid uint32) {
	s.streamLock.Lock()
	defer s.streamLock.Unlock()

	stream, ok := s.streams[sid]
	if !ok {
		return
	}

	if n := stream.recycleTokens(); n > 0 {
		// return remaining tokens to the bucket
		if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
			s.notifyBucket()
		}
	}
	delete(s.streams, sid)
}

// returnTokens is called by stream to return token after read
func (s *Session) returnTokens(n int) {
	if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
		s.notifyBucket()
	}
}

// notifyBudgetSpace wakes up writers waiting for send budget to become
// available. The single-buffered channel is edge triggered, so a notification
// is coalesced when one is already pending.
func (s *Session) notifyBudgetSpace() {
	select {
	case s.chBudgetSpace <- struct{}{}:
	default:
	}
}

// notifyStreamSpace wakes writers blocked on a single stream's send limit.
func notifyStreamSpace(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// dataBudget returns the session-wide queued data payload bytes (test and
// diagnostics helper; the value is maintained under budgetLock).
func (s *Session) dataBudget() int64 {
	s.budgetLock.Lock()
	defer s.budgetLock.Unlock()
	return s.writeBudget
}

// ctrlBudget returns the queued control payload bytes.
func (s *Session) ctrlBudget() int64 {
	s.budgetLock.Lock()
	defer s.budgetLock.Unlock()
	return s.controlBudget
}

// pending returns this stream's queued data payload bytes.
func (s *stream) pending() int64 {
	s.sess.budgetLock.Lock()
	defer s.sess.budgetLock.Unlock()
	return s.pendingBytes
}

// waitSignal blocks until one of fire channels is signaled, the deadline
// fires or the session shuts down. A nil deadline waits indefinitely.
func (s *Session) waitSignal(fire []<-chan struct{}, deadline <-chan time.Time) error {
	cases := make([]reflect.SelectCase, 0, len(fire)+4)
	for _, ch := range fire {
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)})
	}
	cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(s.die)})
	cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(s.chSocketWriteError)})
	if deadline != nil {
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(deadline)})
	}

	chosen, _, _ := reflect.Select(cases)
	switch {
	case chosen < len(fire):
		return nil
	case chosen == len(fire):
		return io.ErrClosedPipe
	case chosen == len(fire)+1:
		return s.socketWriteError.Load().(error)
	default:
		return ErrTimeout
	}
}

// reserveData attempts to reserve n payload bytes from both the session-wide
// data send budget and the per-stream budget. When block is true it waits for
// space (or the deadline) and retries; when false it returns
// ErrSendBufferFull without waiting.
//
// A successful reservation must be released exactly once via releaseRequest.
func (s *Session) reserveData(strm *stream, n int, block bool, deadline <-chan time.Time) error {
	limit := int64(s.config.MaxWriteBuffer)
	streamLimit := int64(s.config.MaxStreamWriteBuffer)

	for {
		var fire []<-chan struct{}

		s.budgetLock.Lock()
		var streamPending int64
		if strm != nil {
			streamPending = strm.pendingBytes
		}
		sessionFull := s.writeBudget+int64(n) > limit
		streamFull := strm != nil && streamPending+int64(n) > streamLimit

		if !sessionFull && !streamFull {
			s.writeBudget += int64(n)
			if strm != nil {
				strm.pendingBytes += int64(n)
			}
			s.budgetLock.Unlock()
			return nil
		}

		if sessionFull {
			fire = append(fire, s.chBudgetSpace)
		}
		if streamFull {
			fire = append(fire, strm.chSpace)
		}
		s.budgetLock.Unlock()

		if !block {
			return ErrSendBufferFull
		}
		if err := s.waitSignal(fire, deadline); err != nil {
			return err
		}
	}
}

// reserveControl reserves n payload bytes from the independent control frame
// budget. Control frames always block: the small quota drains quickly and
// close notifications must not be dropped just because data is backed up.
func (s *Session) reserveControl(n int, deadline <-chan time.Time) error {
	limit := int64(maxControlWriteBuffer)
	for {
		s.budgetLock.Lock()
		if s.controlBudget+int64(n) <= limit {
			s.controlBudget += int64(n)
			s.budgetLock.Unlock()
			return nil
		}
		s.budgetLock.Unlock()

		if err := s.waitSignal([]<-chan struct{}{s.chBudgetSpace}, deadline); err != nil {
			return err
		}
	}
}

// releaseRequest refunds the send budget reserved for a queued write request.
// It is idempotent: cancellation (deadline), stream close and the eventual
// sendLoop completion may all race for the same reservation, but the bytes
// are refunded at most once.
func (s *Session) releaseRequest(req *writeRequest) {
	if req.flags == nil || !atomic.CompareAndSwapInt32(&req.flags.released, 0, 1) {
		return
	}

	s.budgetLock.Lock()
	switch req.charge {
	case chargeData:
		s.writeBudget -= int64(req.charges)
		if req.strm != nil {
			req.strm.pendingBytes -= int64(req.charges)
		}
	case chargeControl:
		s.controlBudget -= int64(req.charges)
	default:
		s.budgetLock.Unlock()
		return
	}
	s.budgetLock.Unlock()

	s.notifyBudgetSpace()
	if req.charge == chargeData && req.strm != nil {
		notifyStreamSpace(req.strm.chSpace)
	}
}

// recvLoop keeps on reading from underlying connection if tokens are available
func (s *Session) recvLoop() {
	var hdr rawHeader
	var updHdr updHeader

	for {
		// Wait until we have tokens or session is closed.
		for atomic.LoadInt32(&s.bucket) <= 0 && !s.IsClosed() {
			select {
			case <-s.bucketNotify:
			case <-s.die:
				// If it returns here, Accept() and OpenStream() are unblocked with io.ErrClosedPipe,
				// causing recvLoop to exit gracefully. If recvLoop is blocked in io.ReadFull, however,
				// it will be unblocked by a socket read error instead.
				return
			}
		}

		// As long as we have tokens, try to read frames.
		// read header first
		_, err := io.ReadFull(s.conn, hdr[:])
		if err != nil {
			s.notifyReadError(err)
			return
		}

		// Mark the session as active
		atomic.StoreInt32(&s.sessionIsActive, 1)

		// validate protocol version
		if hdr.Version() != byte(s.config.Version) {
			s.notifyProtoError(ErrInvalidProtocol)
			return
		}

		// handle different command types
		sid := hdr.StreamID()
		switch hdr.Cmd() {
		case cmdNOP:
			if hdr.Length() != 0 {
				s.notifyProtoError(ErrInvalidProtocol)
				return
			}
		case cmdSYN: // stream opening
			if hdr.Length() != 0 {
				s.notifyProtoError(ErrInvalidProtocol)
				return
			}
			var accepted *stream
			s.streamLock.Lock()
			if _, ok := s.streams[sid]; !ok {
				stream := newStream(sid, s.config.MaxFrameSize, s)
				s.streams[sid] = stream
				accepted = stream
			}
			s.streamLock.Unlock()

			if accepted != nil {
				select {
				case s.chAccepts <- accepted:
				case <-s.die:
				}
			}

		case cmdFIN: // stream closing
			if hdr.Length() != 0 {
				s.notifyProtoError(ErrInvalidProtocol)
				return
			}
			s.streamLock.Lock()
			st := s.streams[sid]
			s.streamLock.Unlock()
			if st != nil {
				st.fin() // fin unblocks the readers and writers
			}

		case cmdPSH: // data frame
			if hdr.Length() == 0 {
				continue
			}

			// read payload from the underlying connection
			pNewbuf := defaultAllocator.Get(int(hdr.Length()))
			written, err := io.ReadFull(s.conn, *pNewbuf)
			if err != nil {
				s.notifyReadError(err)

				// recycle the buffer immediately.
				defaultAllocator.Put(pNewbuf)
				return
			}

			// push data to the corresponding stream
			s.streamLock.Lock()
			if stream, ok := s.streams[sid]; ok {
				stream.pushBytes(pNewbuf)
				// deduct tokens from the bucket
				atomic.AddInt32(&s.bucket, -int32(written))
				stream.wakeupReader()
			} else {
				// data directed to a missing/closed stream, recycle the buffer immediately.
				defaultAllocator.Put(pNewbuf)
			}
			s.streamLock.Unlock()

		case cmdUPD: // a window update signal (v2 only)
			if s.config.Version != 2 {
				s.notifyProtoError(ErrInvalidProtocol)
				return
			}
			if hdr.Length() != szCmdUPD {
				s.notifyProtoError(ErrInvalidProtocol)
				return
			}

			_, err := io.ReadFull(s.conn, updHdr[:])
			if err != nil {
				s.notifyReadError(err)
				return
			}

			// update the window size for the corresponding stream
			s.streamLock.Lock()
			st := s.streams[sid]
			s.streamLock.Unlock()
			if st != nil {
				st.update(updHdr.Consumed(), updHdr.Window())
			}

		default:
			s.notifyProtoError(ErrInvalidProtocol)
			return
		}
	}
}

// keepalive sends NOP frames periodically to keep the connection alive
func (s *Session) keepalive() {
	tickerPing := time.NewTicker(s.config.KeepAliveInterval)
	tickerTimeout := time.NewTicker(s.config.KeepAliveTimeout)
	defer tickerPing.Stop()
	defer tickerTimeout.Stop()
	for {
		select {
		case <-tickerPing.C:
			s.writeFrameInternal(newFrame(byte(s.config.Version), cmdNOP, 0), tickerPing.C, CLSCTRL, nil)
			s.notifyBucket() // force a wakeup signal to the recvLoop
		case <-tickerTimeout.C:
			if !atomic.CompareAndSwapInt32(&s.sessionIsActive, 1, 0) {
				// recvLoop may block while bucket is 0, in this case,
				// session should not be closed.
				if atomic.LoadInt32(&s.bucket) > 0 {
					s.Close()
					return
				}
			}
		case <-s.die:
			return
		}
	}
}

// shaperLoop implements a priority queue and bandwidth shaping for write requests.
// Eg: Control messages are prioritized over data messages, and shaper tries
// its best to keep fair bandwidth among streams.
func (s *Session) shaperLoop() {
	chShaper := s.shaper

	for {
		select {
		case <-s.die:
			return
		case r := <-chShaper:
			s.sq.Push(r)
			// batch drain: collect more requests if available
			for len(chShaper) > 0 && s.sq.Len() < maxShaperSize {
				select {
				case r := <-chShaper:
					s.sq.Push(r)
				default:
				}
			}
			// notify sendLoop there are pending requests
			s.notifyShaperPending()

			if s.sq.Len() >= maxShaperSize {
				// stop accepting new requests temporarily if shaper queue is full
				chShaper = nil
			}
		case <-s.chShaperConsumed:
			// sendLoop drained (part of) the queue. Re-enable the shaper
			// channel and proactively move any buffered requests into the
			// queue; otherwise a writer blocked on a full shaper channel and
			// a coalesced consumed notification could stall indefinitely.
			chShaper = s.shaper
			s.drainShaperChannel()
		}
	}
}

// drainShaperChannel moves requests buffered in the shaper channel into the
// priority queue up to its capacity, and notifies sendLoop of new pendings.
func (s *Session) drainShaperChannel() {
	moved := false
	for len(s.shaper) > 0 && s.sq.Len() < maxShaperSize {
		select {
		case r := <-s.shaper:
			s.sq.Push(r)
			moved = true
		default:
		}
	}
	if moved {
		s.notifyShaperPending()
	}
}

// notifyShaperPending notifies sendLoop that there are pending requests
func (s *Session) notifyShaperPending() {
	select {
	case s.chShaperPending <- struct{}{}:
	default:
	}
}

// notifyShaperConsumed notifies when shaper queue is being consumed
func (s *Session) notifyShaperConsumed() {
	select {
	case s.chShaperConsumed <- struct{}{}:
	default:
	}
}

// sendLoop sends frames over the underlying connection
func (s *Session) sendLoop() {
	var buf []byte
	var n int
	var err error
	var vec [][]byte // vector for writeBuffers

	bw, ok := s.conn.(interface {
		WriteBuffers(v [][]byte) (n int, err error)
	})

	if ok {
		buf = make([]byte, headerSize)
		vec = make([][]byte, 2)
	} else {
		buf = make([]byte, (1<<16)+headerSize)
	}

EVENT_LOOP:
	for {
		select {
		case <-s.die:
			// Refund reservations still held in the shaper queue. Their
			// writers are unblocked by session shutdown, while requests left
			// in the shaper channel are owned by their blocked writers and
			// released on those exit paths.
			for {
				request, ok := s.sq.Pop()
				if !ok {
					return
				}
				s.releaseRequest(&request)
			}
		case <-s.chShaperPending:
			for {
				request, ok := s.sq.Pop()
				if !ok {
					// notify shaperLoop to accept new requests
					s.notifyShaperConsumed()
					goto EVENT_LOOP
				}

				// The writer gave up waiting (write deadline or session
				// close). Refund the reservation and skip the wire write so
				// that a single timed-out write never corrupts the ordering
				// or contents of other queued streams.
				if atomic.LoadInt32(&request.flags.canceled) != 0 {
					s.releaseRequest(&request)
					// The writer has gone; never block the sendLoop.
					select {
					case request.result <- writeResult{n: 0, err: ErrTimeout}:
					default:
					}
					continue
				}

				buf[0] = request.frame.ver
				buf[1] = request.frame.cmd
				binary.LittleEndian.PutUint16(buf[2:], uint16(len(request.frame.data)))
				binary.LittleEndian.PutUint32(buf[4:], request.frame.sid)

				// support for scatter-gather I/O
				if len(vec) > 0 {
					vec[0] = buf[:headerSize]
					vec[1] = request.frame.data
					n, err = bw.WriteBuffers(vec)
				} else {
					copy(buf[headerSize:], request.frame.data)
					n, err = s.conn.Write(buf[:headerSize+len(request.frame.data)])
				}

				n -= headerSize
				if n < 0 {
					n = 0
				}

				result := writeResult{
					n:   n,
					err: err,
				}

				// Refund the send budget before reporting the result so that
				// a writer woken by the result (or by a deadline/close racing
				// with this completion) always observes free space. The
				// reservation is released exactly once here.
				s.releaseRequest(&request)
				if atomic.LoadInt32(&request.flags.canceled) != 0 {
					// The writer timed out or left while the write was in
					// flight. The reservation has been refunded; deliver the
					// result best-effort to avoid blocking other streams.
					select {
					case request.result <- result:
					default:
					}
				} else {
					request.result <- result
				}

				// store conn error
				if err != nil {
					// Refund every reservation still sitting in the shaper
					// queue before tearing down. Requests trapped in the
					// shaper channel are released by their own blocked
					// writers via chSocketWriteError.
					for {
						pending, ok := s.sq.Pop()
						if !ok {
							break
						}
						s.releaseRequest(&pending)
					}
					s.notifyWriteError(err)
					return
				}
			}
		}
	}
}

// writeControlFrame writes the control frame to the underlying connection
// and returns the number of bytes written if successful
func (s *Session) writeControlFrame(f Frame) (n int, err error) {
	timer := time.NewTimer(openCloseTimeout)
	defer timer.Stop()

	return s.writeFrameInternal(f, timer.C, CLSCTRL, nil)
}

// internal writeFrame version to support deadline used in keepalive
func (s *Session) writeFrameInternal(f Frame, deadline <-chan time.Time, class CLASSID, strm *stream) (int, error) {
	// PSH payloads are charged to the session/stream data budgets; every other
	// command is a control frame charged to the independent control budget.
	var charge chargeKind
	switch f.cmd {
	case cmdPSH:
		charge = chargeData
	default:
		charge = chargeControl
	}

	if charge == chargeData {
		if err := s.reserveData(strm, len(f.data), s.config.WriteBlock, deadline); err != nil {
			return 0, err
		}
	} else {
		if err := s.reserveControl(len(f.data), deadline); err != nil {
			return 0, err
		}
	}

	// get result channel from pool
	resultCh := resultChanPool.Get().(chan writeResult)

	req := writeRequest{
		class:   class,
		frame:   f,
		seq:     atomic.AddUint32(&s.requestID, 1),
		result:  resultCh,
		charge:  charge,
		charges: len(f.data),
		strm:    strm,
		flags:   &writeFlags{},
	}
	select {
	case s.shaper <- req:
	case <-s.die:
		s.releaseRequest(&req)
		resultChanPool.Put(resultCh)
		return 0, io.ErrClosedPipe
	case <-s.chSocketWriteError:
		s.releaseRequest(&req)
		resultChanPool.Put(resultCh)
		return 0, s.socketWriteError.Load().(error)
	case <-deadline:
		s.releaseRequest(&req)
		resultChanPool.Put(resultCh)
		return 0, ErrTimeout
	}

	select {
	case result := <-resultCh:
		// sendLoop is responsible for releasing the reservation.
		resultChanPool.Put(resultCh)
		return result.n, result.err
	case <-s.die:
		// Cancel the queued frame: release the reservation here and let
		// sendLoop skip the wire write. The channel is not recycled because
		// sendLoop may still deliver a result to it.
		atomic.StoreInt32(&req.flags.canceled, 1)
		s.releaseRequest(&req)
		return 0, io.ErrClosedPipe
	case <-s.chSocketWriteError:
		// sendLoop exits on the write error; release the reservation here.
		// The channel is not recycled because sendLoop may race with us.
		atomic.StoreInt32(&req.flags.canceled, 1)
		s.releaseRequest(&req)
		return 0, s.socketWriteError.Load().(error)
	case <-deadline:
		// Write deadline: cancel the frame so it is skipped (unless it was
		// already on the wire), refunding the reservation exactly once.
		// Other streams' queued frames stay intact. The channel is not
		// recycled because sendLoop may still deliver a result to it.
		atomic.StoreInt32(&req.flags.canceled, 1)
		s.releaseRequest(&req)
		return 0, ErrTimeout
	}
}
