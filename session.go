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
)

// writeRequest represents a request to write a frame
type writeRequest struct {
	class  CLASSID
	frame  Frame
	seq    uint32
	result chan writeResult
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
	coalescer        *writeCoalescer // optional send-side frame coalescer
}

func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	return newSessionWithClock(config, conn, client, realClock{})
}

func newSessionWithClock(config *Config, conn io.ReadWriteCloser, client bool, clk clock) *Session {
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
	if config.WriteCoalesceBytes > 0 && config.WriteCoalesceDelay > 0 {
		s.coalescer = newWriteCoalescer(config.WriteCoalesceBytes, config.WriteCoalesceDelay, clk)
	}

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
	if s.coalescer != nil {
		s.sendLoopCoalesced()
		return
	}

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

// sendLoopCoalesced is the sendLoop variant used when write coalescing is
// enabled. PSH data frames are accumulated into a single write committed by
// byte threshold or by the coalesce timer; FIN and other control frames force
// an immediate commit of the pending batch first, then go out on their own.
func (s *Session) sendLoopCoalesced() {
	co := s.coalescer

	for {
		select {
		case <-s.die:
			// Pending frames can no longer reach the connection; report the
			// undelivered data as an error so blocked writers do not hang.
			if co.pendingLen() > 0 {
				co.fail(io.ErrClosedPipe)
			}
			return
		case <-co.timer.Ch():
			// A stale tick for an already empty batch is simply ignored.
			if co.pendingLen() > 0 {
				if err := s.flushBatch(); err != nil {
					s.notifyWriteError(err)
					return
				}
			}
		case <-s.chShaperPending:
			for {
				request, ok := s.sq.Pop()
				if !ok {
					// notify shaperLoop to accept new requests
					s.notifyShaperConsumed()
					break
				}

				// FIN and control frames must be delivered immediately and
				// must not overtake earlier frames (the shaper pops frames
				// in per-stream order, so flushing the batch first keeps a
				// FIN behind that stream's earlier data).
				if request.frame.cmd != cmdPSH {
					if co.pendingLen() > 0 {
						if err := s.flushBatch(); err != nil {
							s.notifyWriteError(err)
							return
						}
					}
					if err := s.sendOne(request); err != nil {
						s.notifyWriteError(err)
						return
					}
					continue
				}

				co.append(request)
				if co.full() {
					if err := s.flushBatch(); err != nil {
						s.notifyWriteError(err)
						return
					}
				}
			}
		}
	}
}

// writeAll writes the whole of b to the underlying connection, retrying on
// short writes that report no error. It returns the number of bytes accepted
// up to the failure; io.ErrShortWrite is reported when the connection makes
// no progress (0 bytes, no error) or returns an invalid byte count, so a
// misbehaving connection cannot stall sendLoop forever.
func (s *Session) writeAll(b []byte) (int, error) {
	written := 0
	for len(b) > 0 {
		n, err := s.conn.Write(b)
		if n < 0 || n > len(b) {
			return written, io.ErrShortWrite
		}
		written += n
		b = b[n:]
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

// sendOne serializes a single frame (control frame or FIN) and writes it
// immediately, delivering its write result to the requesting stream.
func (s *Session) sendOne(request writeRequest) error {
	var hdr rawHeader
	hdr[0] = request.frame.ver
	hdr[1] = request.frame.cmd
	binary.LittleEndian.PutUint16(hdr[2:], uint16(len(request.frame.data)))
	binary.LittleEndian.PutUint32(hdr[4:], request.frame.sid)

	buf := make([]byte, headerSize+len(request.frame.data))
	copy(buf, hdr[:])
	copy(buf[headerSize:], request.frame.data)

	// A control/FIN frame is only acknowledged as a whole. A partial write
	// tears the connection down, so report no payload as committed.
	_, err := s.writeAll(buf)
	if err != nil {
		request.result <- writeResult{n: 0, err: err}
	} else {
		request.result <- writeResult{n: len(request.frame.data), err: nil}
	}
	return err
}

// flushBatch commits the frames currently held by the coalescer in one
// underlying write. On a short write or write failure the batch is split at
// frame boundaries when accounting per-request results: a frame is reported
// successful only when all of its bytes (header included) were written.
func (s *Session) flushBatch() error {
	co := s.coalescer
	buf, reqs := co.take()
	written, err := s.writeAll(buf)

	pos := 0
	results := make([]writeResult, len(reqs))
	for i := range reqs {
		req := reqs[i]
		frameLen := headerSize + len(req.frame.data)
		if err == nil || pos+frameLen <= written {
			results[i] = writeResult{n: len(req.frame.data), err: nil}
		} else {
			results[i] = writeResult{n: 0, err: err}
		}
		pos += frameLen
	}

	// Reset before unblocking writers: once a writer returns, all traces of
	// its frame must already be gone from the coalescer state.
	co.reset()
	for i := range reqs {
		reqs[i].result <- results[i]
	}

	return err
}

// writeControlFrame writes the control frame to the underlying connection
// and returns the number of bytes written if successful
func (s *Session) writeControlFrame(f Frame) (n int, err error) {
	timer := time.NewTimer(openCloseTimeout)
	defer timer.Stop()

	return s.writeFrameInternal(f, timer.C, CLSCTRL)
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
