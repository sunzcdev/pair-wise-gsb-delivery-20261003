package message

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"

	"github.com/emersion/go-message/textproto"
)

// ErrBufferExceeded is returned by SafeWriter when the encoded message
// exceeds the buffering budget and spilling to temporary files is disabled
// (SafeWriterOptions.TempDir is empty).
var ErrBufferExceeded = errors.New("message: safe writer buffer budget exceeded")

const (
	defaultSafeMaxBufferBytes        = 32 << 20
	defaultSafeMaxBoundaryCandidates = 1000
)

// SafeWriterOptions are options for CreateSafeWriter.
type SafeWriterOptions struct {
	// MaxBufferBytes is the maximum total number of encoded (transport) bytes
	// held in memory while the message is staged. Once exceeded, data spills
	// to temporary files if TempDir is set, otherwise writes fail with
	// ErrBufferExceeded. Zero means a default of 32 MiB.
	MaxBufferBytes int64

	// TempDir is the directory used to stage encoded data that doesn't fit
	// in the memory budget. If empty, spilling is disabled and exceeding
	// MaxBufferBytes results in ErrBufferExceeded. Temporary files are
	// removed when the root SafeWriter is closed.
	TempDir string

	// GenerateBoundary generates boundary candidates. Candidates are tried
	// in order and the first one that doesn't collide with the encoded
	// message content is used. If nil, random boundaries are generated.
	GenerateBoundary func() string

	// MaxBoundaryCandidates limits how many boundary candidates are tried
	// before giving up with an error. Zero means a default of 1000.
	MaxBoundaryCandidates int
}

func (o *SafeWriterOptions) withDefaults() *SafeWriterOptions {
	var out SafeWriterOptions
	if o != nil {
		out = *o
	}
	if out.MaxBufferBytes == 0 {
		out.MaxBufferBytes = defaultSafeMaxBufferBytes
	}
	if out.MaxBoundaryCandidates == 0 {
		out.MaxBoundaryCandidates = defaultSafeMaxBoundaryCandidates
	}
	return &out
}

// safeState is shared by a SafeWriter and all of its descendant parts.
type safeState struct {
	opts    *SafeWriterOptions
	memUsed int64
	files   []*os.File
	err     error // sticky error, poisons the whole writer tree
}

func (s *safeState) cleanup() {
	for _, f := range s.files {
		name := f.Name()
		f.Close()
		os.Remove(name)
	}
	s.files = nil
}

// staging buffers the encoded transport bytes of a single entity. Since the
// body source is not seekable and the boundary (and thus the header) can only
// be chosen once the whole body has been seen, the encoded bytes must be
// staged until Close.
type staging struct {
	state *safeState
	mem   bytes.Buffer
	file  *os.File
}

func (st *staging) Write(p []byte) (int, error) {
	if st.state.err != nil {
		return 0, st.state.err
	}
	n, err := st.write(p)
	if err != nil {
		st.state.err = err
	}
	return n, err
}

func (st *staging) write(p []byte) (int, error) {
	if st.file != nil {
		return st.file.Write(p)
	}
	if st.state.memUsed+int64(len(p)) > st.state.opts.MaxBufferBytes {
		if st.state.opts.TempDir == "" {
			return 0, ErrBufferExceeded
		}
		if err := st.spill(); err != nil {
			return 0, err
		}
		return st.file.Write(p)
	}
	st.state.memUsed += int64(len(p))
	return st.mem.Write(p)
}

func (st *staging) spill() error {
	f, err := ioutil.TempFile(st.state.opts.TempDir, "go-message-safe-*")
	if err != nil {
		return err
	}
	if _, err := f.Write(st.mem.Bytes()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	st.state.memUsed -= int64(st.mem.Len())
	st.mem.Reset()
	st.file = f
	st.state.files = append(st.state.files, f)
	return nil
}

// reader returns a reader over the staged bytes.
func (st *staging) reader() (io.Reader, error) {
	if st.file != nil {
		if _, err := st.file.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return st.file, nil
	}
	return bytes.NewReader(st.mem.Bytes()), nil
}

// release discards the staged bytes and frees associated resources.
func (st *staging) release() {
	st.state.memUsed -= int64(st.mem.Len())
	st.mem.Reset()
	if st.file != nil {
		name := st.file.Name()
		st.file.Close()
		os.Remove(name)
		for i, f := range st.state.files {
			if f == st.file {
				st.state.files = append(st.state.files[:i], st.state.files[i+1:]...)
				break
			}
		}
		st.file = nil
	}
}

// stagingHasBoundary reports whether the staged transport bytes contain a
// line that a MIME parser could interpret as a delimiter for boundary. The
// check is deliberately conservative: any occurrence of "--boundary" at the
// beginning of a line is treated as a collision, regardless of what follows
// it.
func stagingHasBoundary(st *staging, boundary string) (bool, error) {
	r, err := st.reader()
	if err != nil {
		return false, err
	}
	nlDash := []byte("\n--" + boundary)
	buf := make([]byte, 64*1024)
	// The virtual leading "\n" also matches "--boundary" at offset 0.
	tail := []byte{'\n'}
	for {
		n, err := r.Read(buf)
		if n > 0 {
			search := append(tail, buf[:n]...)
			if bytes.Contains(search, nlDash) {
				return true, nil
			}
			if len(search) >= len(nlDash) {
				tail = append([]byte(nil), search[len(search)-len(nlDash)+1:]...)
			} else {
				tail = append([]byte(nil), search...)
			}
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

func randomSafeBoundary() string {
	var buf [30]byte
	if _, err := io.ReadFull(rand.Reader, buf[:]); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", buf[:])
}

// pickBoundary returns the first generated boundary candidate which doesn't
// collide with the encoded transport bytes of any of the given parts. Nested
// multiparts are covered because their assembled output is part of their
// parent's staged bytes.
func (s *safeState) pickBoundary(parts []*SafeWriter) (string, error) {
	gen := s.opts.GenerateBoundary
	if gen == nil {
		gen = randomSafeBoundary
	}
	for i := 0; i < s.opts.MaxBoundaryCandidates; i++ {
		b := gen()
		mw := textproto.NewMultipartWriter(ioutil.Discard)
		if err := mw.SetBoundary(b); err != nil {
			return "", fmt.Errorf("message: invalid generated boundary: %w", err)
		}
		collision := false
		for _, p := range parts {
			ok, err := stagingHasBoundary(p.st, b)
			if err != nil {
				return "", err
			}
			if ok {
				collision = true
				break
			}
		}
		if !collision {
			return b, nil
		}
	}
	return "", errors.New("message: failed to generate a collision-free boundary")
}

// A SafeWriter writes a message while guaranteeing that multipart boundaries
// never collide with the message content. It is intended for messages whose
// body comes from an untrusted source.
//
// Unlike Writer, a SafeWriter cannot write the message header immediately:
// the boundary parameter of each multipart entity is only known once all of
// its (recursively) encoded parts have been seen, because collisions are
// detected on the final transport bytes, i.e. after Content-Transfer-Encoding
// is applied, not on the decoded content. As a result:
//
//   - Nothing is written to the underlying writer before Close is called on
//     the root SafeWriter. The message header is only determined at that
//     point.
//   - Since body sources are generally not seekable, encoded part bodies are
//     staged internally: in memory up to SafeWriterOptions.MaxBufferBytes,
//     then spilled to temporary files in SafeWriterOptions.TempDir if set.
//   - If the budget is exceeded and spilling is disabled, writes fail with
//     ErrBufferExceeded and nothing is ever written to the underlying
//     writer (the operation is all-or-nothing).
//
// If the message is not multipart, no boundary is needed and the SafeWriter
// behaves like a regular Writer: the header is written immediately and the
// body is streamed without buffering.
//
// Only bodies are protected against boundary collisions. Header fields are
// expected to be trusted.
type SafeWriter struct {
	state     *safeState
	dst       io.Writer // root only
	header    Header
	multipart bool
	root      bool

	st    *staging
	enc   io.WriteCloser // leaf entities only
	parts []*SafeWriter  // multipart entities only

	closed bool
	err    error
}

// CreateSafeWriter creates a new SafeWriter. See the SafeWriter documentation
// for the contract. opts may be nil.
func CreateSafeWriter(w io.Writer, header Header, opts *SafeWriterOptions) (*SafeWriter, error) {
	header = header.Copy()

	if !header.Has("Mime-Version") {
		header.Set("MIME-Version", "1.0")
	}

	state := &safeState{opts: opts.withDefaults()}

	mediaType, _, _ := header.ContentType()
	if strings.HasPrefix(mediaType, "multipart/") {
		sw := &SafeWriter{
			state:     state,
			dst:       w,
			header:    header,
			multipart: true,
			root:      true,
			st:        &staging{state: state},
		}
		sw.initMultipart()
		return sw, nil
	}

	// Non-multipart messages don't need a boundary: the header is final
	// right away and the body can be streamed.
	if err := checkSafeCharset(&header); err != nil {
		return nil, err
	}
	enc, err := encodingWriter(header.Get("Content-Transfer-Encoding"), w)
	if err != nil {
		return nil, err
	}
	if err := textproto.WriteHeader(w, header.Header); err != nil {
		return nil, err
	}
	return &SafeWriter{state: state, header: header, root: true, enc: enc}, nil
}

func checkSafeCharset(h *Header) error {
	_, mediaParams, _ := h.ContentType()
	switch strings.ToLower(mediaParams["charset"]) {
	case "", "us-ascii", "utf-8":
		return nil
	default:
		return fmt.Errorf("unhandled charset %q", mediaParams["charset"])
	}
}

func (w *SafeWriter) initMultipart() {
	mediaType, mediaParams, _ := w.header.ContentType()
	// The boundary is chosen at Close time, once the encoded content is known.
	delete(mediaParams, "boundary")
	w.header.SetContentType(mediaType, mediaParams)
	w.header.Del("Content-Transfer-Encoding")
}

// Write implements io.Writer. It only works for non-multipart entities.
func (w *SafeWriter) Write(b []byte) (int, error) {
	if w.closed {
		return 0, errors.New("message: write to closed SafeWriter")
	}
	if w.multipart {
		return 0, errors.New("message: cannot write directly to a multipart SafeWriter, use CreatePart")
	}
	return w.enc.Write(b)
}

// CreatePart returns a SafeWriter for a new part in this multipart entity. If
// the part is itself multipart, CreatePart can be called recursively on the
// returned SafeWriter; its boundary is chosen collision-free as well.
func (w *SafeWriter) CreatePart(header Header) (*SafeWriter, error) {
	if !w.multipart {
		return nil, errors.New("message: cannot create a part in a non-multipart SafeWriter")
	}
	if w.closed {
		return nil, errors.New("message: SafeWriter is closed")
	}

	header = header.Copy()
	if err := checkSafeCharset(&header); err != nil {
		return nil, err
	}

	pw := &SafeWriter{state: w.state, header: header, st: &staging{state: w.state}}
	mediaType, _, _ := header.ContentType()
	if strings.HasPrefix(mediaType, "multipart/") {
		pw.multipart = true
		pw.initMultipart()
	} else {
		enc, err := encodingWriter(header.Get("Content-Transfer-Encoding"), pw.st)
		if err != nil {
			return nil, err
		}
		pw.enc = enc
	}

	w.parts = append(w.parts, pw)
	return pw, nil
}

// Close finishes the SafeWriter. Parts which are still open are closed
// automatically. For the root SafeWriter of a multipart message, this is the
// point at which boundaries are chosen, the header is determined and the
// whole message is written to the underlying writer.
func (w *SafeWriter) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true

	if w.state.err != nil {
		w.err = w.state.err
	} else if w.multipart {
		w.err = w.closeMultipart()
	} else if w.enc != nil {
		w.err = w.enc.Close()
	}

	if w.root {
		if w.err == nil && w.multipart {
			w.err = w.flush()
		}
		w.state.cleanup()
	}
	if w.err != nil {
		w.state.err = w.err
	}
	return w.err
}

// closeMultipart picks a collision-free boundary and assembles the encoded
// multipart body into w.st.
func (w *SafeWriter) closeMultipart() error {
	for _, p := range w.parts {
		if err := p.Close(); err != nil {
			return err
		}
	}

	boundary, err := w.state.pickBoundary(w.parts)
	if err != nil {
		return err
	}

	mediaType, mediaParams, _ := w.header.ContentType()
	mediaParams["boundary"] = boundary
	w.header.SetContentType(mediaType, mediaParams)

	for i, p := range w.parts {
		if i == 0 {
			fmt.Fprintf(w.st, "--%s\r\n", boundary)
		} else {
			fmt.Fprintf(w.st, "\r\n--%s\r\n", boundary)
		}
		if err := textproto.WriteHeader(w.st, p.header.Header); err != nil {
			return err
		}
		r, err := p.st.reader()
		if err != nil {
			return err
		}
		if _, err := io.Copy(w.st, r); err != nil {
			return err
		}
		p.st.release()
	}
	_, err = fmt.Fprintf(w.st, "\r\n--%s--\r\n", boundary)
	return err
}

// flush writes the final header and the staged body to the underlying writer.
func (w *SafeWriter) flush() error {
	if err := textproto.WriteHeader(w.dst, w.header.Header); err != nil {
		return err
	}
	r, err := w.st.reader()
	if err != nil {
		return err
	}
	if _, err := io.Copy(w.dst, r); err != nil {
		return err
	}
	w.st.release()
	return nil
}
