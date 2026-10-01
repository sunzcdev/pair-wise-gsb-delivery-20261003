package message

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/emersion/go-message/textproto"
)

// defaultBoundaryMemoryBytes is the default per-part memory budget used when
// spooling part bodies for collision-free boundary generation.
const defaultBoundaryMemoryBytes = 1 << 20 // 1 MB

// maxBoundaryAttempts is the maximum number of boundary candidates tried
// before giving up.
const maxBoundaryAttempts = 16

// ErrBoundaryBufferExceeded is returned when a part body exceeds the memory
// budget configured in BoundaryOptions.MaxMemoryBytes and no temporary file
// spillover has been configured with BoundaryOptions.TempFilePattern.
var ErrBoundaryBufferExceeded = errors.New("message: part body exceeds boundary buffer budget")

// BoundaryOptions are options for CreateCollisionFreeWriter.
type BoundaryOptions struct {
	// NextBoundary generates boundary candidates. It is called repeatedly
	// until a candidate that doesn't collide with any part body is found.
	// If nil, random boundaries are generated.
	//
	// This is primarily useful to make tests deterministic.
	NextBoundary func() string

	// MaxMemoryBytes is the maximum number of bytes held in memory while
	// spooling a single part body. Once exceeded, the body is spilled to a
	// temporary file if TempFilePattern is set, otherwise
	// ErrBoundaryBufferExceeded is returned.
	//
	// Set to 0 for the default value (1 MB), set to a negative value to
	// keep bodies in memory without any limit.
	MaxMemoryBytes int64

	// TempDir is the directory where temporary files are created when a
	// part body exceeds MaxMemoryBytes. If empty, os.TempDir is used.
	// It is only used when TempFilePattern is set.
	TempDir string

	// TempFilePattern is the pattern used to create temporary files when a
	// part body exceeds MaxMemoryBytes, see os.CreateTemp. If empty,
	// spilling to disk is disabled and ErrBoundaryBufferExceeded is
	// returned instead.
	TempFilePattern string
}

func (o *BoundaryOptions) withDefaults() *BoundaryOptions {
	var out BoundaryOptions
	if o != nil {
		out = *o
	}
	if out.NextBoundary == nil {
		out.NextBoundary = randomBoundary
	}
	if out.MaxMemoryBytes == 0 {
		out.MaxMemoryBytes = defaultBoundaryMemoryBytes
	}
	return &out
}

func randomBoundary() string {
	var buf [30]byte
	if _, err := io.ReadFull(rand.Reader, buf[:]); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", buf[:])
}

// CreateCollisionFreeWriter creates a new message writer for a multipart
// entity, guaranteeing that the boundary doesn't collide with any of the
// part bodies. It is designed for messages whose body content comes from an
// untrusted source: a random boundary could be guessed by the sender and
// used to forge part structure.
//
// Collisions are detected on the final transfer bytes, i.e. the exact bytes
// written to the wire after Content-Transfer-Encoding is applied, not on the
// decoded content.
//
// Unlike CreateWriter, nothing is written to w until Close is called: the
// header can only be determined once all part bodies have been produced and
// scanned, because it carries the boundary parameter. Part bodies are
// spooled to memory (and optionally to temporary files, see BoundaryOptions)
// in the meantime, which also allows non-seekable body sources. If a body
// exceeds the configured budget, an error matching
// errors.Is(err, ErrBoundaryBufferExceeded) is returned.
//
// The header must have a multipart/* Content-Type. Any pre-existing boundary
// parameter is discarded. Write is not supported on the returned Writer:
// parts must be created with CreatePart. Each part Writer (including nested
// multipart ones) must be closed before Close is called on this Writer.
func CreateCollisionFreeWriter(w io.Writer, header Header, opts *BoundaryOptions) (*Writer, error) {
	mediaType, _, _ := header.ContentType()
	if !strings.HasPrefix(mediaType, "multipart/") {
		return nil, errors.New("message: collision-free writer requires a multipart/* Content-Type")
	}

	// Ensure that modifications are invisible to the caller
	header = header.Copy()

	// If the message uses MIME, it has to include MIME-Version
	if !header.Has("Mime-Version") {
		header.Set("MIME-Version", "1.0")
	}
	header.Del("Content-Transfer-Encoding")

	cf := &collisionFree{
		out:      w,
		header:   &header,
		opts:     opts.withDefaults(),
		topLevel: true,
	}
	return &Writer{cf: cf}, nil
}

// collisionFree implements a multipart writer which buffers all of its parts
// and only picks a boundary (and emits anything) once it is finalized.
type collisionFree struct {
	out    io.Writer
	header *Header // boundary parameter is set at finalize time
	opts   *BoundaryOptions

	// topLevel indicates that the entity header must be written to out
	// during finalize. Nested entities write their header through their
	// parent instead.
	topLevel bool

	parts     []*collisionFreePart
	finalized bool
}

type collisionFreePart struct {
	header Header
	body   *spool
	nested *collisionFree // set if the part is itself multipart
}

func (cf *collisionFree) createPart(header Header) (*Writer, error) {
	if cf.finalized {
		return nil, errors.New("message: cannot create a part after Close")
	}

	// Ensure that modifications are invisible to the caller
	header = header.Copy()

	mediaType, mediaParams, _ := header.ContentType()

	part := &collisionFreePart{
		header: header,
		body:   newSpool(cf.opts),
	}
	cf.parts = append(cf.parts, part)

	if strings.HasPrefix(mediaType, "multipart/") {
		header.Del("Content-Transfer-Encoding")

		// The nested writer shares the part header, so that the boundary it
		// picks when finalized is visible to the parent. Its output goes to
		// the part body spool.
		nested := &collisionFree{
			out:    part.body,
			header: &part.header,
			opts:   cf.opts,
		}
		part.nested = nested
		return &Writer{cf: nested}, nil
	}

	switch strings.ToLower(mediaParams["charset"]) {
	case "", "us-ascii", "utf-8":
		// This is OK
	default:
		// Anything else is invalid
		return nil, fmt.Errorf("unhandled charset %q", mediaParams["charset"])
	}

	// The encoding writer wraps the spool: the spooled bytes are the final
	// transfer bytes.
	wc, err := encodingWriter(header.Get("Content-Transfer-Encoding"), part.body)
	if err != nil {
		return nil, err
	}
	return &Writer{w: wc, c: wc}, nil
}

func (cf *collisionFree) finalize() error {
	if cf.finalized {
		return errors.New("message: writer already closed")
	}
	cf.finalized = true
	defer func() {
		for _, part := range cf.parts {
			part.body.close()
		}
	}()

	// Finalize nested multipart parts the user forgot to close, so that
	// their header carries a boundary and their body is flushed.
	for _, part := range cf.parts {
		if part.nested != nil && !part.nested.finalized {
			if err := part.nested.finalize(); err != nil {
				return err
			}
		}
	}

	boundary, err := cf.pickBoundary()
	if err != nil {
		return err
	}

	mediaType, mediaParams, _ := cf.header.ContentType()
	if mediaParams == nil {
		mediaParams = make(map[string]string)
	}
	mediaParams["boundary"] = boundary
	cf.header.SetContentType(mediaType, mediaParams)

	if cf.topLevel {
		if err := textproto.WriteHeader(cf.out, cf.header.Header); err != nil {
			return err
		}
	}

	for i, part := range cf.parts {
		if i == 0 {
			fmt.Fprintf(cf.out, "--%s\r\n", boundary)
		} else {
			fmt.Fprintf(cf.out, "\r\n--%s\r\n", boundary)
		}
		if err := textproto.WriteHeader(cf.out, part.header.Header); err != nil {
			return err
		}
		if err := part.body.writeTo(cf.out); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(cf.out, "\r\n--%s--\r\n", boundary)
	return err
}

// pickBoundary returns the first candidate boundary which doesn't collide
// with any of the spooled part bodies.
func (cf *collisionFree) pickBoundary() (string, error) {
	for attempt := 0; attempt < maxBoundaryAttempts; attempt++ {
		boundary := cf.opts.NextBoundary()
		if err := checkBoundary(boundary); err != nil {
			return "", err
		}
		collides, err := cf.scanParts(boundary)
		if err != nil {
			return "", err
		}
		if !collides {
			return boundary, nil
		}
	}
	return "", errors.New("message: failed to generate a collision-free boundary")
}

func (cf *collisionFree) scanParts(boundary string) (bool, error) {
	for _, part := range cf.parts {
		r, err := part.body.reader()
		if err != nil {
			return false, err
		}
		collides, err := boundaryCollides(r, boundary)
		if err != nil {
			return false, err
		}
		if collides {
			return true, nil
		}
	}
	return false, nil
}

// checkBoundary verifies that a boundary complies with RFC 2046 section
// 5.1.1, mirroring textproto.MultipartWriter.SetBoundary.
func checkBoundary(boundary string) error {
	if len(boundary) < 1 || len(boundary) > 70 {
		return errors.New("message: invalid boundary length")
	}
	end := len(boundary) - 1
	for i, b := range boundary {
		if 'A' <= b && b <= 'Z' || 'a' <= b && b <= 'z' || '0' <= b && b <= '9' {
			continue
		}
		switch b {
		case '\'', '(', ')', '+', '_', ',', '-', '.', '/', ':', '=', '?':
			continue
		case ' ':
			if i != end {
				continue
			}
		}
		return errors.New("message: invalid boundary character")
	}
	return nil
}

// boundaryCollides reports whether the content read from r contains a line
// which a multipart parser would interpret as a boundary delimiter for
// boundary. The check mirrors the scanner in
// textproto.MultipartReader: a delimiter is "--boundary" at the very
// beginning of the content or preceded by a newline, followed by transport
// padding, a dash, a newline, or the end of the content.
func boundaryCollides(r io.Reader, boundary string) (bool, error) {
	nlDash := []byte("\n--" + boundary)
	dash := nlDash[1:]

	var buf []byte // unconsumed data
	tmp := make([]byte, 32*1024)
	atStart := true
	for {
		n, readErr := r.Read(tmp)
		eof := readErr != nil
		buf = append(buf, tmp[:n]...)

		if atStart && len(buf) > 0 {
			switch {
			case bytes.HasPrefix(buf, dash):
				if len(buf) > len(dash) || eof {
					if matchAfterBoundary(buf, len(dash), eof) {
						return true, nil
					}
					atStart = false
				}
				// Otherwise not enough data to decide yet.
			case eof || !bytes.HasPrefix(dash, buf):
				atStart = false
			}
		}

		for {
			i := bytes.Index(buf, nlDash)
			if i < 0 {
				// Keep a tail long enough to hold a partial match.
				if keep := len(nlDash) - 1; len(buf) > keep {
					buf = buf[len(buf)-keep:]
				}
				break
			}
			end := i + len(nlDash)
			if end == len(buf) && !eof {
				// The byte following the match hasn't been read yet.
				buf = buf[i:]
				break
			}
			if matchAfterBoundary(buf, end, eof) {
				return true, nil
			}
			buf = buf[i+1:]
		}

		if eof {
			if readErr == io.EOF {
				readErr = nil
			}
			return false, readErr
		}
	}
}

// matchAfterBoundary reports whether a boundary match ending at offset i in
// buf is a delimiter, i.e. it is followed by transport padding, a dash, a
// newline, or the end of the content.
func matchAfterBoundary(buf []byte, i int, eof bool) bool {
	if i >= len(buf) {
		return eof
	}
	switch buf[i] {
	case ' ', '\t', '\r', '\n', '-':
		return true
	}
	return false
}

// spool buffers a part body in memory up to a budget, optionally spilling
// over to a temporary file.
type spool struct {
	mem         []byte
	file        *os.File
	limit       int64 // memory budget, negative means unlimited
	tempDir     string
	tempPattern string
	err         error
}

func newSpool(opts *BoundaryOptions) *spool {
	return &spool{
		limit:       opts.MaxMemoryBytes,
		tempDir:     opts.TempDir,
		tempPattern: opts.TempFilePattern,
	}
}

func (s *spool) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.file == nil && s.limit >= 0 && int64(len(s.mem))+int64(len(p)) > s.limit {
		if s.tempPattern == "" {
			s.err = ErrBoundaryBufferExceeded
			return 0, s.err
		}
		f, err := os.CreateTemp(s.tempDir, s.tempPattern)
		if err != nil {
			s.err = err
			return 0, err
		}
		if _, err := f.Write(s.mem); err != nil {
			f.Close()
			os.Remove(f.Name())
			s.err = err
			return 0, err
		}
		s.file = f
		s.mem = nil
	}
	if s.file != nil {
		n, err := s.file.Write(p)
		if err != nil {
			s.err = err
		}
		return n, err
	}
	s.mem = append(s.mem, p...)
	return len(p), nil
}

// reader returns a reader over the spooled content, positioned at the
// beginning.
func (s *spool) reader() (io.Reader, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.file != nil {
		if _, err := s.file.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return s.file, nil
	}
	return bytes.NewReader(s.mem), nil
}

func (s *spool) writeTo(w io.Writer) error {
	r, err := s.reader()
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r)
	return err
}

// close releases the resources held by the spool, removing the temporary
// file if any.
func (s *spool) close() {
	if s.file != nil {
		s.file.Close()
		os.Remove(s.file.Name())
		s.file = nil
	}
	s.mem = nil
}
