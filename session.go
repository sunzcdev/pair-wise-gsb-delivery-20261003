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
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
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
	// errFrameCanceled is delivered by sendLoop for a frame whose submission
	// was withdrawn by OpenStreamContext before it reached the wire.
	errFrameCanceled = errors.New("frame write canceled")
)

// writeRequest represents a request to write a frame
type writeRequest struct {
	class  CLASSID
	frame  Frame
	seq    uint32
	result chan writeResult
	cancel *frameCancel // optional cancellation coordination (SYN frames)
}

// frameCancel coordinates cancellation of a single frame between the caller
// and the sendLoop. The lifecycle is:
//
//	queued  - waiting in the shaper queue, the frame has not touched the wire
//	writing - sendLoop is about to call Write, so the frame is committed
//
// A frame is considered committed as soon as sendLoop transitions the state
// to "writing"; at that point the Write may complete even if the caller no
// longer waits for the result.
type frameCancel struct {
	mu    sync.Mutex
	state frameState
}

type frameState int

const (
	frameQueued frameState = iota
	frameWriting
	frameWithdrawn
)

// newFrameCancel creates the cancellation coordinator for one frame.
func newFrameCancel() *frameCancel {
	return &frameCancel{state: frameQueued}
}

// beginWrite is invoked by sendLoop right before writing the frame.
// It reports true when the frame was withdrawn while still queued, in which
// case the Write must be skipped.
func (fc *frameCancel) beginWrite() (withdrawn bool) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.state == frameWithdrawn {
		return true
	}
	fc.state = frameWriting
	return false
}

// tryWithdraw attempts to remove a queued frame before it reaches the wire.
// It reports true when the frame is guaranteed not to have been written.
func (fc *frameCancel) tryWithdraw() bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.state != frameQueued {
		return false
	}
	fc.state = frameWithdrawn
	return true
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
	sid, err := s.allocStreamID()
	if err != nil {
		return nil, err
	}

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

// OpenStreamContext is used to create a new stream. It behaves like
// OpenStream but abandons the open request when ctx is canceled before the
// stream is returned to the caller.
//
// The cancellation point matters:
//
//   - If the SYN frame has not been submitted to the wire, no frame is sent
//     and no remote stream is created.
//   - If the SYN frame has been submitted but the stream has not been
//     returned yet, a FIN frame is sent to release the stream that is now
//     visible to the peer.
//   - Once the stream has been returned, ctx is no longer consulted; the
//     caller owns the returned stream and closes it as usual.
//
// A stream identifier is consumed on every attempt and is never reused.
// The call returns exactly one result: it never returns both a stream and
// an error.
func (s *Session) OpenStreamContext(ctx context.Context) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.IsClosed() {
		return nil, io.ErrClosedPipe
	}

	sid, err := s.allocStreamID()
	if err != nil {
		return nil, err
	}

	stream := newStream(sid, s.config.MaxFrameSize, s)

	committed, werr := s.writeControlFrameContext(newFrame(byte(s.config.Version), cmdSYN, sid), ctx)
	if werr != nil {
		if committed {
			// The peer has observed the SYN; release the remote stream with
			// FIN so it does not outlive the abandoned local open request.
			s.abandonOpenedStream(sid)
		}
		if werr == errFrameCanceled {
			// The SYN was withdrawn before the write; surface the context
			// or deadline reason that triggered the withdrawal.
			if e := ctx.Err(); e != nil {
				werr = e
			} else {
				werr = ErrTimeout
			}
		}
		return nil, werr
	}

	// The SYN is on the wire but the stream has not been returned yet.
	// A cancellation in this tiny window must still release the remote stream.
	select {
	case <-ctx.Done():
		s.abandonOpenedStream(sid)
		return nil, ctx.Err()
	default:
	}

	select {
	case <-s.chSocketReadError:
		s.abandonOpenedStream(sid)
		return nil, s.socketReadError.Load().(error)
	case <-s.chSocketWriteError:
		s.abandonOpenedStream(sid)
		return nil, s.socketWriteError.Load().(error)
	case <-s.die:
		s.abandonOpenedStream(sid)
		return nil, io.ErrClosedPipe
	default:
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
		return wrapper, nil
	}
}

// allocStreamID reserves the next odd/even stream identifier. Allocated
// identifiers are never reused, even when the open request is abandoned.
func (s *Session) allocStreamID() (uint32, error) {
	if s.IsClosed() {
		return 0, io.ErrClosedPipe
	}

	s.nextStreamIDLock.Lock()
	defer s.nextStreamIDLock.Unlock()
	if s.goAway > 0 {
		return 0, ErrGoAway
	}

	// check for stream id overflow
	if s.nextStreamID+2 < s.nextStreamID {
		s.goAway = 1
		return 0, ErrGoAway
	}

	// allocate next stream id
	s.nextStreamID += 2
	return s.nextStreamID, nil
}

// abandonOpenedStream releases a stream whose SYN has already been observed
// by the peer but which was never returned to (or registered for) a local
// caller. A FIN closes the stream in protocol terms; no local map entry
// exists, so there is nothing to delete locally.
func (s *Session) abandonOpenedStream(sid uint32) {
	f := newFrame(byte(s.config.Version), cmdFIN, sid)
	// Cleanup runs asynchronously so a blocked writer does not stall the
	// canceled OpenStreamContext caller. The FIN shares the stream's shaper
	// queue, hence it is never transmitted ahead of the SYN. If the session
	// is gone the peer reclaims all streams together with the connection.
	go func() {
		if _, err := s.writeControlFrame(f); err != nil {
			return
		}
	}()
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
			s.writeFrameInternal(newFrame(byte(s.config.Version), cmdNOP, 0), tickerPing.C, CLSCTRL)
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
			// re-enable shaper channel
			chShaper = s.shaper
		}
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
			return
		case <-s.chShaperPending:
			for {
				request, ok := s.sq.Pop()
				if !ok {
					// notify shaperLoop to accept new requests
					s.notifyShaperConsumed()
					goto EVENT_LOOP
				}

				// The frame was withdrawn by its caller while it was still
				// waiting in the queue, so it never reaches the wire.
				if request.cancel != nil && request.cancel.beginWrite() {
					request.result <- writeResult{err: errFrameCanceled}
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

				request.result <- result

				// store conn error
				if err != nil {
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

	return s.writeFrameInternal(f, timer.C, CLSCTRL)
}

// writeControlFrameContext submits a control frame while allowing ctx to
// cancel the submission before the frame reaches the wire. It reports
// whether the frame was committed to the write phase: a committed frame
// may already be visible to the peer and therefore requires protocol-level
// cleanup by the caller on error.
func (s *Session) writeControlFrameContext(f Frame, ctx context.Context) (committed bool, err error) {
	timer := time.NewTimer(openCloseTimeout)
	defer timer.Stop()

	// get result channel from pool
	resultCh := resultChanPool.Get().(chan writeResult)

	fc := newFrameCancel()
	req := writeRequest{
		class:  CLSCTRL,
		frame:  f,
		seq:    atomic.AddUint32(&s.requestID, 1),
		result: resultCh,
		cancel: fc,
	}

	// queue the SYN; any of the abort conditions unblocks the submission
	select {
	case s.shaper <- req:
	case <-ctx.Done():
		committed, err = s.resolveCanceledFrame(fc, resultCh, ctx.Err())
		s.drainAndRecycle(resultCh)
		return committed, err
	case <-timer.C:
		committed, err = s.resolveCanceledFrame(fc, resultCh, ErrTimeout)
		s.drainAndRecycle(resultCh)
		return committed, err
	case <-s.die:
		committed, err = s.resolveCanceledFrame(fc, resultCh, io.ErrClosedPipe)
		s.drainAndRecycle(resultCh)
		return committed, err
	case <-s.chSocketWriteError:
		committed, err = s.resolveCanceledFrame(fc, resultCh, s.socketWriteError.Load().(error))
		s.drainAndRecycle(resultCh)
		return committed, err
	}

	// The frame is queued; wait for the write result or an abort.
	select {
	case result := <-resultCh:
		resultChanPool.Put(resultCh)
		if result.err == errFrameCanceled {
			return false, errFrameCanceled
		}
		return result.err == nil, result.err
	case <-ctx.Done():
		committed, err = s.resolveCanceledFrame(fc, resultCh, ctx.Err())
		s.drainAndRecycle(resultCh)
		return committed, err
	case <-timer.C:
		committed, err = s.resolveCanceledFrame(fc, resultCh, ErrTimeout)
		s.drainAndRecycle(resultCh)
		return committed, err
	case <-s.die:
		committed, err = s.resolveCanceledFrame(fc, resultCh, io.ErrClosedPipe)
		s.drainAndRecycle(resultCh)
		return committed, err
	case <-s.chSocketWriteError:
		committed, err = s.resolveCanceledFrame(fc, resultCh, s.socketWriteError.Load().(error))
		s.drainAndRecycle(resultCh)
		return committed, err
	}
}

// resolveCanceledFrame determines whether a frame hit the wire when its
// waiter aborts. If it can be withdrawn from the queue it is guaranteed not
// to have been written; otherwise the write has reached (or is entering)
// the write phase and the frame is treated as committed.
func (s *Session) resolveCanceledFrame(fc *frameCancel, resultCh chan writeResult, err error) (bool, error) {
	if fc.tryWithdraw() {
		return false, err
	}
	return true, err
}

// drainAndRecycle waits for the sendLoop result after the waiter has left
// and returns the result channel to the pool. It is required even for a
// withdrawn frame, since sendLoop publishes a synthetic result when it
// encounters one.
func (s *Session) drainAndRecycle(resultCh chan writeResult) {
	go func() {
		select {
		case <-resultCh:
			resultChanPool.Put(resultCh)
		case <-s.die:
		case <-s.chSocketWriteError:
		}
	}()
}

// internal writeFrame version to support deadline used in keepalive
func (s *Session) writeFrameInternal(f Frame, deadline <-chan time.Time, class CLASSID) (int, error) {
	// get result channel from pool
	resultCh := resultChanPool.Get().(chan writeResult)

	req := writeRequest{
		class:  class,
		frame:  f,
		seq:    atomic.AddUint32(&s.requestID, 1),
		result: resultCh,
	}
	select {
	case s.shaper <- req:
	case <-s.die:
		resultChanPool.Put(resultCh)
		return 0, io.ErrClosedPipe
	case <-s.chSocketWriteError:
		resultChanPool.Put(resultCh)
		return 0, s.socketWriteError.Load().(error)
	case <-deadline:
		resultChanPool.Put(resultCh)
		return 0, ErrTimeout
	}

	select {
	case result := <-resultCh:
		resultChanPool.Put(resultCh)
		return result.n, result.err
	case <-s.die:
		// Cannot recycle channel here - sendLoop may still write to it
		return 0, io.ErrClosedPipe
	case <-s.chSocketWriteError:
		// Cannot recycle channel here - sendLoop may still write to it
		return 0, s.socketWriteError.Load().(error)
	case <-deadline:
		// Cannot recycle channel here - sendLoop may still write to it
		return 0, ErrTimeout
	}
}
