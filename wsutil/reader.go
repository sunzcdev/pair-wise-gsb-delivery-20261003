package wsutil

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"

	"github.com/gobwas/ws"
)

// ErrNoFrameAdvance means that Reader's Read() method was called without
// preceding NextFrame() call.
var ErrNoFrameAdvance = errors.New("no frame advance")

// ErrFrameTooLarge indicates that a message of length higher than
// MaxFrameSize was being read.
var ErrFrameTooLarge = errors.New("frame too large")

// FrameHandlerFunc handles parsed frame header and its body represented by
// io.Reader.
//
// Note that reader represents already unmasked body.
type FrameHandlerFunc func(ws.Header, io.Reader) error

// Reader is a wrapper around source io.Reader which represents WebSocket
// connection. It contains options for reading messages from source.
//
// Reader implements io.Reader, which Read() method reads payload of incoming
// WebSocket frames. It also takes care on fragmented frames and possibly
// intermediate control frames between them.
//
// Note that Reader's methods are not goroutine safe.
type Reader struct {
	Source io.Reader
	State  ws.State

	// SkipHeaderCheck disables checking header bits to be RFC6455 compliant.
	SkipHeaderCheck bool

	// CheckUTF8 enables UTF-8 checks for text frames payload. If incoming
	// bytes are not valid UTF-8 sequence, ErrInvalidUTF8 returned.
	CheckUTF8 bool

	// Extensions is a list of negotiated extensions for reader Source.
	// It is used to meet the specs and clear appropriate bits in fragment
	// header RSV segment.
	Extensions []RecvExtension

	// MaxFrameSize controls the maximum frame size in bytes
	// that can be read. A message exceeding that size will return
	// a ErrFrameTooLarge to the application.
	//
	// Not setting this field means there is no limit.
	MaxFrameSize int64

	// Observer, if not nil, receives frame lifecycle events: a header that
	// has been read and validated, a frame payload that has been fully
	// consumed, and mid-stream failures. See FrameEvent and FrameObserver
	// for the event and termination semantics.
	Observer FrameObserver

	OnContinuation FrameHandlerFunc
	OnIntermediate FrameHandlerFunc

	opCode ws.OpCode // Used to store message op code on fragmentation.

	frame       io.Reader                  // Used to as frame reader.
	raw         io.LimitedReader           // Used to discard frames without cipher.
	utf8        UTF8Reader                 // Used to check UTF8 sequences if CheckUTF8 is true.
	tmp         [ws.MaxHeaderSize - 2]byte // Used for reading headers.
	cr          *CipherReader              // Used by NextFrame() to unmask frame payload.
	frameIdx    int64                      // Number of next frame on the wire.
	msgIdx      int64                      // Number of next data message.
	curMsg      int64                      // Message index of the current/in-flight data message; -1 if none.
	msgActive   bool                       // Whether a data message is currently in flight.
	cur         FrameEvent                 // Event of the frame being processed.
	consumed    bool                       // Whether current frame payload was reported consumed.
	failed      bool                       // Whether current frame was reported failed.
	frameActive bool                       // Whether a frame payload is currently in flight.
	observerErr error                      // Non-nil once the observer terminated the Reader.
}

// NewReader creates new frame reader that reads from r keeping given state to
// make some protocol validity checks when it needed.
func NewReader(r io.Reader, s ws.State) *Reader {
	return &Reader{
		Source: r,
		State:  s,
	}
}

// NewClientSideReader is a helper function that calls NewReader with r and
// ws.StateClientSide.
func NewClientSideReader(r io.Reader) *Reader {
	return NewReader(r, ws.StateClientSide)
}

// NewServerSideReader is a helper function that calls NewReader with r and
// ws.StateServerSide.
func NewServerSideReader(r io.Reader) *Reader {
	return NewReader(r, ws.StateServerSide)
}

// Read implements io.Reader. It reads the next message payload into p.
// It takes care on fragmented messages.
//
// The error is io.EOF only if all of message bytes were read.
// If an io.EOF happens during reading some but not all the message bytes
// Read() returns io.ErrUnexpectedEOF.
//
// The error is ErrNoFrameAdvance if no NextFrame() call was made before
// reading next message bytes.
func (r *Reader) Read(p []byte) (n int, err error) {
	if r.observerErr != nil {
		return 0, r.observerTerminated()
	}
	if r.frame == nil {
		if !r.fragmented() {
			// Every new Read() must be preceded by NextFrame() call.
			return 0, ErrNoFrameAdvance
		}
		// Read next continuation or intermediate control frame.
		_, err = r.NextFrame()
		if err != nil {
			return 0, err
		}
		if r.frame == nil {
			// We handled intermediate control and now got nothing to read.
			return 0, nil
		}
	}

	n, err = r.frame.Read(p)
	if err != nil && err != io.EOF {
		if r.raw.N == 0 {
			// The whole frame payload was drained; report actual consumption
			// before the failure (e.g. invalid utf8 final chunk).
			if !r.reportConsumed() {
				return n, r.observerTerminated()
			}
		}
		if !r.reportFailed(err) {
			return n, r.observerTerminated()
		}
		return n, err
	}
	if err == nil && r.raw.N != 0 {
		return n, nil
	}

	// EOF condition (either err is io.EOF or r.raw.N is zero).
	switch {
	case r.raw.N != 0:
		err = io.ErrUnexpectedEOF
		if !r.reportFailed(err) {
			return n, r.observerTerminated()
		}
		return n, err

	case r.fragmented():
		err = nil
		if !r.reportConsumed() {
			return n, r.observerTerminated()
		}
		r.resetFragment()

	case r.CheckUTF8 && !r.utf8.Valid():
		// NOTE: check utf8 only when full message received, since partial
		// reads may be invalid.
		n = r.utf8.Accepted()
		if !r.reportConsumed() {
			return n, r.observerTerminated()
		}
		err = ErrInvalidUTF8

	default:
		if !r.reportConsumed() {
			return n, r.observerTerminated()
		}
		r.reset()
		err = io.EOF
	}

	return n, err
}

// Discard discards current message unread bytes.
// It discards all frames of fragmented message.
func (r *Reader) Discard() (err error) {
	if r.observerErr != nil {
		return r.observerTerminated()
	}
	for {
		_, err = io.Copy(ioutil.Discard, &r.raw)
		if err != nil {
			if !r.reportFailed(err) {
				return r.observerTerminated()
			}
			break
		}
		if r.frameActive && !r.reportConsumed() {
			return r.observerTerminated()
		}
		if !r.fragmented() {
			break
		}
		if _, err = r.NextFrame(); err != nil {
			break
		}
	}
	r.reset()
	return err
}

// Reset abandons the current in-flight frame or fragmented message and
// restarts frame and message numbering. It does not change Source, State or
// Observer.
//
// All observer events are delivered synchronously from NextFrame(), Read()
// and Discard(), so after Reset no event referring to a frame seen before the
// reset can ever be delivered. A previous observer termination is also
// cleared.
func (r *Reader) Reset() {
	r.reset()
	r.frameIdx = 0
	r.msgIdx = 0
	r.cur = FrameEvent{}
	r.consumed = false
	r.failed = false
	r.frameActive = false
	r.observerErr = nil
}

// NextFrame prepares r to read next message. It returns received frame header
// and non-nil error on failure.
//
// Note that next NextFrame() call must be done after receiving or discarding
// all current message bytes.
func (r *Reader) NextFrame() (hdr ws.Header, err error) {
	if r.observerErr != nil {
		return hdr, r.observerTerminated()
	}

	hdr, err = r.readHeader(r.Source)
	if err == io.EOF && r.fragmented() {
		// If we are in fragmented state EOF means that is was totally
		// unexpected.
		//
		// NOTE: This is necessary to prevent callers such that
		// ioutil.ReadAll to receive some amount of bytes without an error.
		// ReadAll() ignores an io.EOF error, thus caller may think that
		// whole message fetched, but actually only part of it.
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		// A clean io.EOF with zero header bytes means the connection ended
		// between frames; there is no frame to report. Any other error is a
		// mid-stream failure: account for the missing frame position, but do
		// not count it as a frame on the wire or start a new data message.
		if err != io.EOF {
			r.beginFrame(hdr, r.peekMessageIndex(hdr), false)
			if !r.reportFailed(err) {
				return hdr, r.observerTerminated()
			}
		}
		return hdr, err
	}

	// Header bytes were parsed: this is a frame on the wire, even if its
	// semantics are rejected below.
	r.beginFrame(hdr, r.peekMessageIndex(hdr), true)

	if !r.SkipHeaderCheck {
		if err = ws.CheckHeader(hdr, r.State); err != nil {
			if !r.reportFailed(err) {
				return hdr, r.observerTerminated()
			}
			return hdr, err
		}
	}

	if n := r.MaxFrameSize; n > 0 && hdr.Length > n {
		if !r.reportFailed(ErrFrameTooLarge) {
			return hdr, r.observerTerminated()
		}
		return hdr, ErrFrameTooLarge
	}

	// Save raw reader to use it on discarding frame without ciphering and
	// other streaming checks.
	r.raw = io.LimitedReader{
		R: r.Source,
		N: hdr.Length,
	}

	frame := io.Reader(&r.raw)
	if hdr.Masked {
		if r.cr == nil {
			r.cr = NewCipherReader(frame, hdr.Mask)
		} else {
			r.cr.Reset(frame, hdr.Mask)
		}
		frame = r.cr
	}

	for _, x := range r.Extensions {
		hdr, err = x.UnsetBits(hdr)
		if err != nil {
			r.cur.Header = hdr
			r.cur.WireLength = frameWireLength(hdr)
			if !r.reportFailed(err) {
				return hdr, r.observerTerminated()
			}
			return hdr, err
		}
	}
	r.cur.Header = hdr
	r.cur.WireLength = frameWireLength(hdr)
	// A fully validated data frame commits a new data message; continuation
	// frames and intermediate control frames reuse the in-flight index.
	r.cur.MessageIndex = r.messageIndex(hdr)

	// The header has passed every enabled check; report it before any payload
	// byte may be consumed by the callbacks below.
	if !r.reportHeaderChecked() {
		return hdr, r.observerTerminated()
	}

	if r.fragmented() {
		if hdr.OpCode.IsControl() {
			if cb := r.OnIntermediate; cb != nil {
				err = cb(hdr, frame)
			}
			if err == nil {
				// Ensure that src is empty.
				_, err = io.Copy(ioutil.Discard, &r.raw)
			}
			if err != nil {
				if !r.reportFailed(err) {
					return hdr, r.observerTerminated()
				}
			} else {
				if !r.reportConsumed() {
					return hdr, r.observerTerminated()
				}
			}
			return hdr, err
		}
	} else {
		r.opCode = hdr.OpCode
	}
	if r.CheckUTF8 && (hdr.OpCode == ws.OpText || (r.fragmented() && r.opCode == ws.OpText)) {
		r.utf8.Source = frame
		frame = &r.utf8
	}

	// Save reader with ciphering and other streaming checks.
	r.frame = frame

	if hdr.OpCode == ws.OpContinuation {
		if cb := r.OnContinuation; cb != nil {
			if cbErr := cb(hdr, frame); cbErr != nil {
				return hdr, cbErr
			}
		}
	}

	if hdr.Fin {
		r.State = r.State.Clear(ws.StateFragmented)
	} else {
		r.State = r.State.Set(ws.StateFragmented)
	}

	return hdr, err
}

func (r *Reader) fragmented() bool {
	return r.State.Fragmented()
}

func (r *Reader) resetFragment() {
	r.raw = io.LimitedReader{}
	r.frame = nil
	// Reset source of the UTF8Reader, but not the state.
	r.utf8.Source = nil
	r.consumed = false
	r.failed = false
	r.frameActive = false
	r.cur = FrameEvent{}
}

func (r *Reader) reset() {
	r.raw = io.LimitedReader{}
	r.frame = nil
	r.utf8 = UTF8Reader{}
	r.opCode = 0
	r.msgActive = false
	r.consumed = false
	r.failed = false
	r.frameActive = false
	r.cur = FrameEvent{}
}

// beginFrame prepares per-frame observer state for the frame described by h.
// If onWire is false, the header was not fully parsed (read failure), so the
// frame is not counted and its frame index points to the position the frame
// would occupy; wire length is reported as zero.
func (r *Reader) beginFrame(h ws.Header, msgIdx int64, onWire bool) {
	r.consumed = false
	r.failed = false
	r.frameActive = onWire
	r.cur = FrameEvent{
		Header:       h,
		FrameIndex:   r.frameIdx,
		MessageIndex: msgIdx,
	}
	if onWire {
		r.cur.WireLength = frameWireLength(h)
		r.frameIdx++
	}
}

// peekMessageIndex resolves the message index h would belong to without
// committing a new data message.
func (r *Reader) peekMessageIndex(h ws.Header) int64 {
	if h.OpCode.IsControl() && r.msgActive {
		return r.curMsg
	}
	if h.OpCode.IsControl() {
		return -1
	}
	if r.msgActive && (h.OpCode == ws.OpContinuation || r.fragmented()) {
		return r.curMsg
	}
	return r.msgIdx
}

// messageIndex resolves and commits the message index to report for h.
//
// A new data frame starts a new data message; continuation frames and
// intermediate control frames reuse the in-flight message index; a control
// frame received outside of any data message reports -1.
func (r *Reader) messageIndex(h ws.Header) int64 {
	if h.OpCode.IsControl() {
		if r.msgActive {
			return r.curMsg
		}
		return -1
	}
	if r.msgActive && (h.OpCode == ws.OpContinuation || r.fragmented()) {
		return r.curMsg
	}
	// New data message.
	idx := r.msgIdx
	r.msgIdx++
	r.curMsg = idx
	r.msgActive = true
	return idx
}

// reportHeaderChecked delivers an EventHeaderChecked for the current frame.
// It reports false when the observer has terminated the Reader.
func (r *Reader) reportHeaderChecked() bool {
	r.cur.Kind = EventHeaderChecked
	return r.emit(r.cur)
}

// reportConsumed delivers EventPayloadConsumed once for the current frame,
// and only after its payload has actually been fully drained.
func (r *Reader) reportConsumed() bool {
	if r.consumed {
		return true
	}
	if r.raw.N != 0 {
		// Payload is not fully consumed yet: completion must not be reported
		// early.
		return true
	}
	r.consumed = true
	r.cur.Kind = EventPayloadConsumed
	return r.emit(r.cur)
}

// reportFailed delivers EventFailed at most once for the current frame. It
// reports false when the observer has terminated the Reader.
func (r *Reader) reportFailed(err error) bool {
	if r.failed {
		return true
	}
	r.failed = true
	e := r.cur
	e.Kind = EventFailed
	e.Err = err
	return r.emit(e)
}

// emit delivers e to the observer, applying the termination rule on observer
// error. It reports whether the Reader may continue.
func (r *Reader) emit(e FrameEvent) bool {
	if r.Observer == nil {
		return true
	}
	if err := r.Observer.ObserveFrame(e); err != nil {
		r.observerErr = err
		return false
	}
	return true
}

// observerTerminated wraps the original observer error so callers can detect
// the termination via errors.Is(err, ErrObserverTerminated), while preserving
// the error returned by the observer for inspection.
func (r *Reader) observerTerminated() error {
	return fmt.Errorf("%w: %v", ErrObserverTerminated, r.observerErr)
}

// ObserverError returns the error returned by the Observer that terminated
// the Reader, or nil if the Observer has not terminated it.
func (r *Reader) ObserverError() error {
	return r.observerErr
}

// frameWireLength returns total on-wire size of a frame with header h: header
// bytes plus declared payload length.
func frameWireLength(h ws.Header) int64 {
	return int64(ws.HeaderSize(h)) + h.Length
}

// readHeader reads a frame header from in.
func (r *Reader) readHeader(in io.Reader) (h ws.Header, err error) {
	// Make slice of bytes with capacity 12 that could hold any header.
	//
	// The maximum header size is 14, but due to the 2 hop reads,
	// after first hop that reads first 2 constant bytes, we could reuse 2 bytes.
	// So 14 - 2 = 12.
	bts := r.tmp[:2]

	// Prepare to hold first 2 bytes to choose size of next read.
	_, err = io.ReadFull(in, bts)
	if err != nil {
		return h, err
	}
	const bit0 = 0x80

	h.Fin = bts[0]&bit0 != 0
	h.Rsv = (bts[0] & 0x70) >> 4
	h.OpCode = ws.OpCode(bts[0] & 0x0f)

	var extra int

	if bts[1]&bit0 != 0 {
		h.Masked = true
		extra += 4
	}

	length := bts[1] & 0x7f
	switch {
	case length < 126:
		h.Length = int64(length)

	case length == 126:
		extra += 2

	case length == 127:
		extra += 8

	default:
		err = ws.ErrHeaderLengthUnexpected
		return h, err
	}

	if extra == 0 {
		return h, err
	}

	// Increase len of bts to extra bytes need to read.
	// Overwrite first 2 bytes that was read before.
	bts = bts[:extra]
	_, err = io.ReadFull(in, bts)
	if err != nil {
		// The first 2 header bytes were already received, so an EOF while
		// reading the remaining header bytes is an unexpected truncation.
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return h, err
	}

	switch {
	case length == 126:
		h.Length = int64(binary.BigEndian.Uint16(bts[:2]))
		bts = bts[2:]

	case length == 127:
		if bts[0]&0x80 != 0 {
			err = ws.ErrHeaderLengthMSB
			return h, err
		}
		h.Length = int64(binary.BigEndian.Uint64(bts[:8]))
		bts = bts[8:]
	}

	if h.Masked {
		copy(h.Mask[:], bts)
	}

	return h, nil
}

// NextReader prepares next message read from r. It returns header that
// describes the message and io.Reader to read message's payload. It returns
// non-nil error when it is not possible to read message's initial frame.
//
// Note that next NextReader() on the same r should be done after reading all
// bytes from previously returned io.Reader. For more performant way to discard
// message use Reader and its Discard() method.
//
// Note that it will not handle any "intermediate" frames, that possibly could
// be received between text/binary continuation frames. That is, if peer sent
// text/binary frame with fin flag "false", then it could send ping frame, and
// eventually remaining part of text/binary frame with fin "true" – with
// NextReader() the ping frame will be dropped without any notice. To handle
// this rare, but possible situation (and if you do not know exactly which
// frames peer could send), you could use Reader with OnIntermediate field set.
func NextReader(r io.Reader, s ws.State) (ws.Header, io.Reader, error) {
	rd := &Reader{
		Source: r,
		State:  s,
	}
	header, err := rd.NextFrame()
	if err != nil {
		return header, nil, err
	}
	return header, rd, nil
}
