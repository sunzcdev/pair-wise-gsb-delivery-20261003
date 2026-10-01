// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type streamState int

const (
	streamInit streamState = iota
	streamSYNSent
	streamSYNReceived
	streamEstablished
	streamLocalClose
	streamRemoteClose
	streamClosed
	streamReset
)

// Stream is used to represent a logical stream within a session. Methods on
// Stream are safe to call concurrently with one another, but all Read calls
// must be on the same goroutine and all Write calls must be on the same
// goroutine.
type Stream struct {
	recvWindow uint32
	sendWindow uint32

	id      uint32
	session *Session

	state     streamState
	stateLock sync.Mutex

	recvBuf  *bytes.Buffer
	recvLock sync.Mutex

	// recvCommitted tracks receive memory committed for this stream against
	// the session budget: bytes buffered in recvBuf plus receive credit
	// advertised to the peer (recvWindow) that the peer may still consume.
	// The field is accounted under the session budget lock, but updates are
	// serialized by recvLock. releaseRecvBudget returns the outstanding
	// commitment exactly once when the stream is torn down.
	recvCommitted  uint32
	budgetReleased bool

	controlHdr     header
	controlErr     chan error
	controlHdrLock sync.Mutex

	sendHdr  header
	sendErr  chan error
	sendLock sync.Mutex

	recvNotifyCh chan struct{}
	sendNotifyCh chan struct{}

	readDeadline  atomic.Value // time.Time
	writeDeadline atomic.Value // time.Time

	// establishCh is notified if the stream is established or being closed.
	establishCh chan struct{}

	// closeTimer is set with stateLock held to honor the StreamCloseTimeout
	// setting on Session.
	closeTimer *time.Timer
}

// newStream is used to construct a new stream within
// a given session for an ID
func newStream(session *Session, id uint32, state streamState) *Stream {
	s := &Stream{
		id:           id,
		session:      session,
		state:        state,
		controlHdr:   header(make([]byte, headerSize)),
		controlErr:   make(chan error, 1),
		sendHdr:      header(make([]byte, headerSize)),
		sendErr:      make(chan error, 1),
		recvWindow:   initialStreamWindow,
		sendWindow:   initialStreamWindow,
		recvNotifyCh: make(chan struct{}, 1),
		sendNotifyCh: make(chan struct{}, 1),
		establishCh:  make(chan struct{}, 1),
	}
	s.readDeadline.Store(time.Time{})
	s.writeDeadline.Store(time.Time{})
	return s
}

// Session returns the associated stream session
func (s *Stream) Session() *Session {
	return s.session
}

// StreamID returns the ID of this stream
func (s *Stream) StreamID() uint32 {
	return s.id
}

// Read is used to read from the stream. It is safe to call Write, Read, and/or
// Close concurrently with each other, but calls to Read are not reentrant and
// should not be called from multiple goroutines. Multiple Read goroutines would
// receive different chunks of data from the Stream and be unable to reassemble
// them in order or along message boundaries, and may encounter deadlocks.
func (s *Stream) Read(b []byte) (n int, err error) {
	defer asyncNotify(s.recvNotifyCh)
	var released uint32
START:
	released = 0

	// If the stream is closed and there's no data buffered, return EOF
	s.stateLock.Lock()
	switch s.state {
	case streamLocalClose:
		// LocalClose only prohibits further local writes. Handle reads normally.
	case streamRemoteClose:
		fallthrough
	case streamClosed:
		s.recvLock.Lock()
		if s.recvBuf == nil || s.recvBuf.Len() == 0 {
			s.recvLock.Unlock()
			s.stateLock.Unlock()
			return 0, io.EOF
		}
		s.recvLock.Unlock()
	case streamReset:
		s.stateLock.Unlock()
		return 0, ErrConnectionReset
	}
	s.stateLock.Unlock()

	// If there is no data available, block
	s.recvLock.Lock()
	if s.recvBuf == nil || s.recvBuf.Len() == 0 {
		s.recvLock.Unlock()
		goto WAIT
	}

	// Read any bytes
	n, _ = s.recvBuf.Read(b)
	// Buffered data consumed by the application frees committed receive
	// memory. Skip accounting once the stream's budget was already returned
	// on teardown (checked under recvLock, which releaseRecvBudget also
	// holds while setting budgetReleased).
	if !s.budgetReleased {
		s.recvCommitted -= uint32(n)
		released = uint32(n)
	}
	s.recvLock.Unlock()
	if released > 0 {
		s.session.recvBudget.release(released)
	}

	// Send a window update potentially
	err = s.sendWindowUpdate()
	if err == ErrSessionShutdown {
		err = nil
	}
	return n, err

WAIT:
	var timeout <-chan time.Time
	var timer *time.Timer
	readDeadline := s.readDeadline.Load().(time.Time)
	if !readDeadline.IsZero() {
		delay := time.Until(readDeadline)
		timer = time.NewTimer(delay)
		timeout = timer.C
	}
	select {
	case <-s.session.shutdownCh:
	case <-s.recvNotifyCh:
	case <-timeout:
		return 0, ErrTimeout
	}
	if timer != nil {
		if !timer.Stop() {
			<-timeout
		}
	}
	goto START
}

// Write is used to write to the stream. It is safe to call Write, Read, and/or
// Close concurrently with each other, but calls to Write are not reentrant and
// should not be called from multiple goroutines.
func (s *Stream) Write(b []byte) (n int, err error) {
	s.sendLock.Lock()
	defer s.sendLock.Unlock()
	total := 0
	for total < len(b) {
		n, err := s.write(b[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// write is used to write to the stream, may return on
// a short write.
func (s *Stream) write(b []byte) (n int, err error) {
	var flags uint16
	var max uint32
	var body []byte
START:
	s.stateLock.Lock()
	switch s.state {
	case streamLocalClose:
		fallthrough
	case streamClosed:
		s.stateLock.Unlock()
		return 0, ErrStreamClosed
	case streamReset:
		s.stateLock.Unlock()
		return 0, ErrConnectionReset
	}
	s.stateLock.Unlock()

	// If there is no data available, block
	window := atomic.LoadUint32(&s.sendWindow)
	if window == 0 {
		goto WAIT
	}

	// Determine the flags if any
	flags = s.sendFlags()

	// Send up to our send window
	max = min(window, uint32(len(b)))
	body = b[:max]

	// Send the header
	s.sendHdr.encode(typeData, flags, s.id, max)
	if err = s.session.waitForSendErr(s.sendHdr, body, s.sendErr); err != nil {
		if errors.Is(err, ErrSessionShutdown) || errors.Is(err, ErrConnectionWriteTimeout) {
			// Message left in ready queue, header re-use is unsafe.
			s.sendHdr = header(make([]byte, headerSize))
		}
		return 0, err
	}

	// Reduce our send window
	atomic.AddUint32(&s.sendWindow, ^uint32(max-1))

	// Unlock
	return int(max), err

WAIT:
	var timeout <-chan time.Time
	var timer *time.Timer
	writeDeadline := s.writeDeadline.Load().(time.Time)
	if !writeDeadline.IsZero() {
		delay := time.Until(writeDeadline)
		timer = time.NewTimer(delay)
		timeout = timer.C
	}
	select {
	case <-s.session.shutdownCh:
	case <-s.sendNotifyCh:
	case <-timeout:
		return 0, ErrTimeout
	}
	if timer != nil {
		if !timer.Stop() {
			<-timeout
		}
	}
	goto START
}

// sendFlags determines any flags that are appropriate
// based on the current stream state
func (s *Stream) sendFlags() uint16 {
	s.stateLock.Lock()
	defer s.stateLock.Unlock()
	return s.sendFlagsLocked()
}

// sendFlagsLocked is sendFlags for callers that already hold stateLock.
func (s *Stream) sendFlagsLocked() uint16 {
	var flags uint16
	switch s.state {
	case streamInit:
		flags |= flagSYN
		s.state = streamSYNSent
	case streamSYNReceived:
		flags |= flagACK
		s.state = streamEstablished
	}
	return flags
}

// sendWindowUpdate potentially sends a window update enabling
// further writes to take place. Must be invoked with the lock.
func (s *Stream) sendWindowUpdate() error {
	// Determine the delta update
	max := s.session.config.MaxStreamWindowSize
	// Serialize window updates against stream teardown (which also runs under
	// stateLock) and against buffer accounting (recvLock). The lock order is
	// stateLock -> recvLock -> controlHdrLock, matching processFlags and
	// releaseRecvBudget.
	s.stateLock.Lock()
	if s.state == streamClosed || s.state == streamReset {
		s.stateLock.Unlock()
		return nil
	}
	s.recvLock.Lock()

	var bufLen uint32
	if s.recvBuf != nil {
		bufLen = uint32(s.recvBuf.Len())
	}
	want := (max - bufLen) - s.recvWindow

	// Determine the flags if any
	flags := s.sendFlagsLocked()

	// Check if we can omit the update. A frame carrying flags (SYN/ACK/FIN)
	// must still be sent, so reserve at least the available credit for it.
	if want < (max/2) && flags == 0 {
		s.recvLock.Unlock()
		s.stateLock.Unlock()
		return nil
	}

	// Reserve the additional advertised credit against the session budget
	// before announcing it. The grant can be smaller than requested when the
	// session budget is constrained; that only shrinks this stream's window.
	// A zero grant is only acceptable on a flagged frame (e.g. the opening
	// SYN/ACK, which already committed the initial window).
	delta := s.session.recvBudget.tryAcquireNonBlocking(want)
	if delta == 0 && flags == 0 {
		s.recvLock.Unlock()
		s.stateLock.Unlock()
		return nil
	}

	// Update our window and the stream's committed total.
	s.recvWindow += delta
	s.recvCommitted += delta
	s.recvLock.Unlock()
	s.stateLock.Unlock()

	return s.sendWindowUpdateFrame(flags, delta)
}

// sendWindowUpdateFrame encodes and sends a window update frame with the given
// flags and delta. The credit for delta must already have been reserved.
func (s *Stream) sendWindowUpdateFrame(flags uint16, delta uint32) error {
	s.controlHdrLock.Lock()
	defer s.controlHdrLock.Unlock()

	// Send the header
	s.controlHdr.encode(typeWindowUpdate, flags, s.id, delta)
	if err := s.session.waitForSendErr(s.controlHdr, nil, s.controlErr); err != nil {
		if errors.Is(err, ErrSessionShutdown) || errors.Is(err, ErrConnectionWriteTimeout) {
			// Message left in ready queue, header re-use is unsafe.
			s.controlHdr = header(make([]byte, headerSize))
		}
		return err
	}
	return nil
}

// sendClose is used to send a FIN
func (s *Stream) sendClose() error {
	s.stateLock.Lock()
	flags := s.sendFlagsLocked()
	flags |= flagFIN
	s.stateLock.Unlock()

	s.controlHdrLock.Lock()
	defer s.controlHdrLock.Unlock()
	s.controlHdr.encode(typeWindowUpdate, flags, s.id, 0)
	if err := s.session.waitForSendErr(s.controlHdr, nil, s.controlErr); err != nil {
		if errors.Is(err, ErrSessionShutdown) || errors.Is(err, ErrConnectionWriteTimeout) {
			// Message left in ready queue, header re-use is unsafe.
			s.controlHdr = header(make([]byte, headerSize))
		}
		return err
	}
	return nil
}

// Close is used to close the stream. It is safe to call Close concurrently.
func (s *Stream) Close() error {
	closeStream := false
	s.stateLock.Lock()
	switch s.state {
	// Opened means we need to signal a close
	case streamSYNSent:
		fallthrough
	case streamSYNReceived:
		fallthrough
	case streamEstablished:
		s.state = streamLocalClose
		goto SEND_CLOSE

	case streamLocalClose:
	case streamRemoteClose:
		s.state = streamClosed
		closeStream = true
		goto SEND_CLOSE

	case streamClosed:
	case streamReset:
	default:
		panic("unhandled state")
	}
	s.stateLock.Unlock()
	return nil
SEND_CLOSE:
	// This shouldn't happen (the more realistic scenario to cancel the
	// timer is via processFlags) but just in case this ever happens, we
	// cancel the timer to prevent dangling timers.
	if s.closeTimer != nil {
		s.closeTimer.Stop()
		s.closeTimer = nil
	}

	// If we have a StreamCloseTimeout set we start the timeout timer.
	// We do this only if we're not already closing the stream since that
	// means this was a graceful close.
	//
	// This prevents memory leaks if one side (this side) closes and the
	// remote side poorly behaves and never responds with a FIN to complete
	// the close. After the specified timeout, we clean our resources up no
	// matter what.
	if !closeStream && s.session.config.StreamCloseTimeout > 0 {
		s.closeTimer = time.AfterFunc(
			s.session.config.StreamCloseTimeout, s.closeTimeout)
	}

	s.stateLock.Unlock()
	_ = s.sendClose()
	s.notifyWaiting()
	if closeStream {
		s.session.closeStream(s.id)
	}
	return nil
}

// closeTimeout is called after StreamCloseTimeout during a close to
// close this stream.
func (s *Stream) closeTimeout() {
	// Close our side forcibly
	s.forceClose()

	// Free the stream from the session map
	s.session.closeStream(s.id)

	// Send a RST so the remote side closes too.
	s.sendLock.Lock()
	defer s.sendLock.Unlock()
	hdr := header(make([]byte, headerSize))
	hdr.encode(typeWindowUpdate, flagRST, s.id, 0)
	_ = s.session.sendNoWait(hdr)
}

// forceClose is used for when the session is exiting
func (s *Stream) forceClose() {
	s.stateLock.Lock()
	s.state = streamClosed
	s.stateLock.Unlock()
	s.notifyWaiting()
	s.releaseRecvBudget()
}

// releaseRecvBudget returns to the session all receive memory still committed
// for this stream: data buffered for the application plus credit advertised to
// the peer that can no longer arrive. It is idempotent and safe to call from
// every teardown path (graceful close, RST, close timeout, session shutdown).
func (s *Stream) releaseRecvBudget() {
	s.stateLock.Lock()
	if s.budgetReleased {
		s.stateLock.Unlock()
		return
	}
	s.budgetReleased = true
	s.recvLock.Lock()
	committed := s.recvCommitted
	s.recvCommitted = 0
	s.recvLock.Unlock()
	s.stateLock.Unlock()

	s.session.recvBudget.release(committed)
}

// releaseRecvWindowCredit returns the current receive window credit to the
// session while leaving buffered data committed. It is used when the peer
// announces a half or full close (FIN) and can no longer consume the
// advertised credit. It must be called with stateLock held.
func (s *Stream) releaseRecvWindowCredit() {
	s.recvLock.Lock()
	if s.budgetReleased {
		s.recvLock.Unlock()
		return
	}
	credit := s.recvWindow
	s.recvWindow = 0
	if credit <= s.recvCommitted {
		s.recvCommitted -= credit
	} else {
		s.recvCommitted = 0
	}
	s.recvLock.Unlock()

	s.session.recvBudget.release(credit)
}

// processFlags is used to update the state of the stream
// based on set flags, if any. Lock must be held
func (s *Stream) processFlags(flags uint16) error {
	s.stateLock.Lock()
	closeStream := false
	established := false
	var retErr error

	if flags&flagACK == flagACK {
		if s.state == streamSYNSent {
			s.state = streamEstablished
		}
		established = true
	}
	if flags&flagFIN == flagFIN {
		switch s.state {
		case streamSYNSent:
			fallthrough
		case streamSYNReceived:
			fallthrough
		case streamEstablished:
			// The peer will send no more data, so the receive window it
			// could still have consumed can never be used. Return that
			// credit; buffered data the application has not drained stays
			// committed until read or the stream is fully torn down.
			s.releaseRecvWindowCredit()
			s.state = streamRemoteClose
		case streamLocalClose:
			s.releaseRecvWindowCredit()
			s.state = streamClosed
			closeStream = true
		default:
			s.session.logger.Printf("[ERR] yamux: unexpected FIN flag in state %d", s.state)
			retErr = ErrUnexpectedFlag
		}
	}
	if flags&flagRST == flagRST {
		s.state = streamReset
		closeStream = true
	}
	s.stateLock.Unlock()

	// Perform notifications and teardown without holding stateLock.
	// Session.Close holds streamLock while forceClose takes stateLock, so
	// taking streamLock (via closeStream) while holding stateLock would
	// invert that order and deadlock.
	s.notifyWaiting()
	if established {
		s.session.establishStream(s.id)
	}
	if closeStream {
		if s.closeTimer != nil {
			// Stop our close timeout timer since we gracefully closed
			s.closeTimer.Stop()
		}
		s.session.closeStream(s.id)
	}
	return retErr
}

// notifyWaiting notifies all the waiting channels
func (s *Stream) notifyWaiting() {
	asyncNotify(s.recvNotifyCh)
	asyncNotify(s.sendNotifyCh)
	asyncNotify(s.establishCh)
}

// incrSendWindow updates the size of our send window
func (s *Stream) incrSendWindow(hdr header, flags uint16) error {
	if err := s.processFlags(flags); err != nil {
		return err
	}

	// Increase window, unblock a sender
	atomic.AddUint32(&s.sendWindow, hdr.Length())
	asyncNotify(s.sendNotifyCh)
	return nil
}

// readData is used to handle a data frame
func (s *Stream) readData(hdr header, flags uint16, conn io.Reader) error {
	if err := s.processFlags(flags); err != nil {
		return err
	}

	// Check that our recv window is not exceeded
	length := hdr.Length()
	if length == 0 {
		return nil
	}

	// Wrap in a limited reader
	conn = &io.LimitedReader{R: conn, N: int64(length)}

	// Copy into buffer
	s.recvLock.Lock()

	if length > s.recvWindow {
		s.session.logger.Printf("[ERR] yamux: receive window exceeded (stream: %d, remain: %d, recv: %d)", s.id, s.recvWindow, length)
		s.recvLock.Unlock()
		return ErrRecvWindowExceeded
	}

	if s.recvBuf == nil {
		// Allocate the receive buffer just-in-time to fit the full data frame.
		// This way we can read in the whole packet without further allocations.
		s.recvBuf = bytes.NewBuffer(make([]byte, 0, length))
	}
	copiedLength, err := io.Copy(s.recvBuf, conn)
	if err != nil {
		s.session.logger.Printf("[ERR] yamux: Failed to read stream data: %v", err)
		s.recvLock.Unlock()
		return err
	}

	// Decrement the receive window
	s.recvWindow -= uint32(copiedLength)
	// The bytes move from advertised credit into buffered data; the total
	// committed to the session budget is unchanged.
	s.recvLock.Unlock()

	// Unblock any readers
	asyncNotify(s.recvNotifyCh)
	return nil
}

// SetDeadline sets the read and write deadlines
func (s *Stream) SetDeadline(t time.Time) error {
	if err := s.SetReadDeadline(t); err != nil {
		return err
	}
	if err := s.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

// SetReadDeadline sets the deadline for blocked and future Read calls.
func (s *Stream) SetReadDeadline(t time.Time) error {
	s.readDeadline.Store(t)
	asyncNotify(s.recvNotifyCh)
	return nil
}

// SetWriteDeadline sets the deadline for blocked and future Write calls
func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.writeDeadline.Store(t)
	asyncNotify(s.sendNotifyCh)
	return nil
}

// Shrink is used to compact the amount of buffers utilized
// This is useful when using Yamux in a connection pool to reduce
// the idle memory utilization.
func (s *Stream) Shrink() {
	s.recvLock.Lock()
	if s.recvBuf != nil && s.recvBuf.Len() == 0 {
		s.recvBuf = nil
	}
	s.recvLock.Unlock()
}
