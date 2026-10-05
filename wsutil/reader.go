package wsutil

import (
	"encoding/binary"
	"errors"
	"io"
	"io/ioutil"

	"github.com/gobwas/ws"
)

// ErrNoFrameAdvance means that Reader's Read() method was called without
// preceding NextFrame() call.
var ErrNoFrameAdvance = errors.New("no frame advance")

// ErrReaderTerminated means that Reader's observer returned an error on a
// previous call and the Reader will not process any more frames or events.
var ErrReaderTerminated = errors.New("reader terminated by observer error")

// ErrFrameTooLarge indicates that a message of length higher than
// MaxFrameSize was being read.
var ErrFrameTooLarge = errors.New("frame too large")

// FrameEvent describes lifecycle progress of a single WebSocket frame on the
// wire.
//
// FrameSeq is a 1-based ordinal of the frame within the connection among the
// frames whose headers were successfully received and validated. It is 0 for
// failures that happened before a frame header could be established.
//
// MessageSeq is a 1-based ordinal of the data message that the frame belongs
// to. Control frames that interleave a fragmented data message carry that
// in-progress message ordinal; a control frame received while no data message
// is in progress carries 0, because control frames do not open data messages.
//
// WireLength is the number of on-the-wire bytes belonging to this frame that
// have been consumed at the moment the event is reported. For a header event
// it is the frame header size; for a payload or a failure event it also
// includes the payload bytes actually consumed so far. It is relative to the
// frame start, so a completed frame reports its full wire size (the value
// needed to account for per-frame overhead).
type FrameEvent struct {
	FrameSeq   int
	MessageSeq int
	OpCode     ws.OpCode
	Length     int64
	WireLength int64
}

// FrameObserver receives optional notifications about frame lifecycle.
//
// Every successfully validated frame produces exactly one HeaderEvent,
// followed later by either one PayloadEvent (when its whole payload was
// actually consumed) or one FailureEvent (when reading or validating the frame
// failed before its payload was fully consumed). No PayloadEvent is reported
// while any payload bytes remain unread.
//
// Control frames interleaved between fragments of a data message are reported
// as independent frames (with their own FrameSeq), but they do not open a new
// data message, so their MessageSeq refers to the in-progress message.
//
// If any of the observer methods returns a non-nil error, the Reader is
// terminated: the returned error is propagated to the caller, no further
// frames are processed and no further events (including events belonging to a
// reset or a reuse of the Reader) are delivered. Subsequent Reader method
// calls return ErrReaderTerminated.
type FrameObserver interface {
	OnFrameHeader(FrameEvent) error
	OnFramePayload(FrameEvent) error
	OnFrameFailure(FrameEvent, error) error
}

// FrameObserverFunc is an adapter to allow the use of ordinary functions as
// independent frame event hooks. Any nil function is treated as a no-op hook.
type FrameObserverFunc struct {
	Header  func(FrameEvent) error
	Payload func(FrameEvent) error
	Failure func(FrameEvent, error) error
}

// OnFrameHeader implements FrameObserver.
func (f FrameObserverFunc) OnFrameHeader(e FrameEvent) error {
	if f.Header == nil {
		return nil
	}
	return f.Header(e)
}

// OnFramePayload implements FrameObserver.
func (f FrameObserverFunc) OnFramePayload(e FrameEvent) error {
	if f.Payload == nil {
		return nil
	}
	return f.Payload(e)
}

// OnFrameFailure implements FrameObserver.
func (f FrameObserverFunc) OnFrameFailure(e FrameEvent, err error) error {
	if f.Failure == nil {
		return nil
	}
	return f.Failure(e, err)
}

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

	// Observer, if non-nil, receives frame lifecycle events. See FrameObserver
	// documentation for the event ordering and termination rules.
	Observer FrameObserver

	OnContinuation FrameHandlerFunc
	OnIntermediate FrameHandlerFunc

	opCode ws.OpCode                  // Used to store message op code on fragmentation.
	frame  io.Reader                  // Used to as frame reader.
	raw    io.LimitedReader           // Used to discard frames without cipher.
	utf8   UTF8Reader                 // Used to check UTF8 sequences if CheckUTF8 is true.
	tmp    [ws.MaxHeaderSize - 2]byte // Used for reading headers.
	cr     *CipherReader              // Used by NextFrame() to unmask frame payload.

	wire       countingReader // Counts bytes consumed from Source.
	wireStart  int64          // Wire offset at which current frame header began.
	frameSeq   int            // Ordinal of the current frame (0 before first).
	messageSeq int            // Ordinal of the current data message (0 before first).
	cur        FrameEvent     // Metadata of the frame being processed.
	terminated bool           // Whether the observer terminated the Reader.
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
	if err = r.alive(); err != nil {
		return 0, err
	}
	if r.frame == nil {
		if !r.fragmented() {
			// Every new Read() must be preceded by NextFrame() call.
			return 0, ErrNoFrameAdvance
		}
		// Read next continuation or intermediate control frame.
		_, err := r.NextFrame()
		if err != nil {
			return 0, err
		}
		if err = r.alive(); err != nil {
			return 0, err
		}
		if r.frame == nil {
			// We handled intermediate control and now got nothing to read.
			return 0, nil
		}
	}

	n, err = r.frame.Read(p)
	if err != nil && err != io.EOF {
		return n, r.failCurrent(err)
	}
	if err == nil && r.raw.N != 0 {
		return n, nil
	}

	// EOF condition (either err is io.EOF or r.raw.N is zero).
	switch {
	case r.raw.N != 0:
		err = io.ErrUnexpectedEOF
		err = r.failCurrent(err)

	case r.fragmented():
		err = nil
		err = r.payloadDone()
		r.resetFragment()

	case r.CheckUTF8 && !r.utf8.Valid():
		// NOTE: check utf8 only when full message received, since partial
		// reads may be invalid.
		n = r.utf8.Accepted()
		err = ErrInvalidUTF8
		err = r.failCurrent(err)

	default:
		if fail := r.payloadDone(); fail != nil {
			return n, fail
		}
		r.reset()
		err = io.EOF
	}

	return n, err
}

// Discard discards current message unread bytes.
// It discards all frames of fragmented message.
func (r *Reader) Discard() (err error) {
	if err = r.alive(); err != nil {
		return err
	}
	for {
		_, err = io.Copy(ioutil.Discard, &r.raw)
		if err != nil {
			return r.failCurrent(err)
		}
		if fail := r.payloadDone(); fail != nil {
			return fail
		}
		if !r.fragmented() {
			break
		}
		if _, err = r.NextFrame(); err != nil {
			return err
		}
	}
	r.reset()
	return nil
}

// NextFrame prepares r to read next message. It returns received frame header
// and non-nil error on failure.
//
// Note that next NextFrame() call must be done after receiving or discarding
// all current message bytes.
func (r *Reader) NextFrame() (hdr ws.Header, err error) {
	if err = r.alive(); err != nil {
		return hdr, err
	}

	r.wire.R = r.Source
	before := r.wire.n
	r.wireStart = before
	hdr, err = r.readHeader(&r.wire)
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
	if err == nil && !r.SkipHeaderCheck {
		err = ws.CheckHeader(hdr, r.State)
	}
	if err != nil {
		// A clean EOF with no bytes consumed and no message in progress is a
		// normal end of the connection: it is not a frame failure. Any other
		// header read error (truncated header, unexpected EOF mid-message) is
		// reported without associating a new frame ordinal.
		if err != io.EOF || r.fragmented() || r.wire.n != before {
			return hdr, r.fail(r.eventFor(hdr, r.inProgressMessageSeq(), before), err)
		}
		return hdr, err
	}

	if n := r.MaxFrameSize; n > 0 && hdr.Length > n {
		return hdr, r.fail(r.eventFor(hdr, r.inProgressMessageSeq(), before), ErrFrameTooLarge)
	}

	r.beginFrame(hdr)
	if err = r.notifyHeader(); err != nil {
		return hdr, err
	}

	// Save raw reader to use it on discarding frame without ciphering and
	// other streaming checks.
	r.raw = io.LimitedReader{
		R: &r.wire,
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
			return hdr, r.failCurrent(err)
		}
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
				return hdr, r.failCurrent(err)
			}
			if err = r.payloadDone(); err != nil {
				return hdr, err
			}
			r.resetFrame()
			return hdr, nil
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
				return hdr, r.failCurrent(cbErr)
			}
		}
	}

	if hdr.Fin {
		r.State = r.State.Clear(ws.StateFragmented)
	} else {
		r.State = r.State.Set(ws.StateFragmented)
	}

	return hdr, nil
}

func (r *Reader) fragmented() bool {
	return r.State.Fragmented()
}

func (r *Reader) resetFragment() {
	r.raw = io.LimitedReader{}
	r.frame = nil
	// Reset source of the UTF8Reader, but not the state.
	r.utf8.Source = nil
}

func (r *Reader) reset() {
	r.raw = io.LimitedReader{}
	r.frame = nil
	r.utf8 = UTF8Reader{}
	r.opCode = 0
	r.cur = FrameEvent{}
}

func (r *Reader) resetFrame() {
	r.raw = io.LimitedReader{}
	r.frame = nil
	r.cur = FrameEvent{}
}

// alive reports whether the Reader may still process frames. Once the
// observer terminates the Reader every public method is a no-op.
func (r *Reader) alive() error {
	if r.terminated {
		return ErrReaderTerminated
	}
	return nil
}

// inProgressMessageSeq returns the ordinal of the data message currently
// being received, or 0 when no data message is in progress.
func (r *Reader) inProgressMessageSeq() int {
	if r.fragmented() {
		return r.messageSeq
	}
	return 0
}

// beginFrame assigns frame and message ordinals for a freshly validated
// header and prepares the current event metadata.
func (r *Reader) beginFrame(h ws.Header) {
	r.frameSeq++

	messageSeq := r.inProgressMessageSeq()
	if !r.fragmented() && h.OpCode.IsData() {
		// A fresh non-control frame that is not a continuation starts a new
		// data message.
		r.messageSeq++
		messageSeq = r.messageSeq
	}

	r.cur = FrameEvent{
		FrameSeq:   r.frameSeq,
		MessageSeq: messageSeq,
		OpCode:     h.OpCode,
		Length:     h.Length,
		WireLength: r.wire.n - r.wireStart,
	}
}

// eventFor builds an event for a header without assigning a new frame
// ordinal. It is used when validation fails before a frame is established.
func (r *Reader) eventFor(h ws.Header, messageSeq int, start int64) FrameEvent {
	return FrameEvent{
		FrameSeq:   0,
		MessageSeq: messageSeq,
		OpCode:     h.OpCode,
		Length:     h.Length,
		WireLength: r.wire.n - start,
	}
}

func (r *Reader) snapshotEvent() FrameEvent {
	e := r.cur
	e.WireLength = r.wire.n - r.wireStart
	return e
}

// notifyHeader reports the validated header event.
func (r *Reader) notifyHeader() error {
	if r.Observer == nil {
		return nil
	}
	e := r.snapshotEvent()
	if err := r.Observer.OnFrameHeader(e); err != nil {
		return r.terminate(err)
	}
	return nil
}

// payloadDone reports that the whole payload of the current frame was
// actually consumed. It must be called only when r.raw.N is zero.
func (r *Reader) payloadDone() error {
	if r.Observer == nil || r.cur.FrameSeq == 0 {
		return nil
	}
	e := r.snapshotEvent()
	if err := r.Observer.OnFramePayload(e); err != nil {
		return r.terminate(err)
	}
	return nil
}

// failCurrent reports a failure for the frame currently being processed and
// terminates the Reader.
func (r *Reader) failCurrent(cause error) error {
	return r.fail(r.snapshotEvent(), cause)
}

// terminate marks the Reader as terminated due to an error returned by an
// observer hook. It does not emit a failure event: an observer error aborts
// observation rather than describes a frame processing failure.
func (r *Reader) terminate(err error) error {
	r.terminated = true
	return err
}

// fail reports a failure event and, regardless of whether the observer
// returns an error, terminates the Reader so that no stale or later events
// are delivered. The error returned to the caller is the cause unless the
// observer replaces it with its own non-nil error.
func (r *Reader) fail(e FrameEvent, cause error) error {
	if r.Observer == nil {
		// Without an observer the Reader behaves as before: errors are
		// propagated plainly and do not impose a terminal state.
		return cause
	}
	if r.terminated {
		return ErrReaderTerminated
	}
	r.terminated = true

	if obsErr := r.Observer.OnFrameFailure(e, cause); obsErr != nil {
		return obsErr
	}
	return cause
}

// countingReader wraps the source while counting every byte consumed. It
// lets events report the wire length without copying payload bytes.
type countingReader struct {
	R io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (n int, err error) {
	n, err = c.R.Read(p)
	c.n += int64(n)
	return n, err
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
