package message

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-message/textproto"
)

// Writer writes message entities.
//
// If the message is not multipart, it should be used as a WriteCloser. Don't
// forget to call Close.
//
// If the message is multipart, users can either use CreatePart to write child
// parts or Write to directly pipe a multipart message. In any case, Close must
// be called at the end.
type Writer struct {
	w  io.Writer
	c  io.Closer
	mw *textproto.MultipartWriter
}

// createWriter creates a new Writer writing to w with the provided header.
// Nothing is written to w when it is called. header is modified in-place.
func createWriter(w io.Writer, header *Header, opts []Option) (*Writer, error) {
	ww := &Writer{w: w}

	mediaType, mediaParams, _ := header.ContentType()
	if strings.HasPrefix(mediaType, "multipart/") {
		ww.mw = textproto.NewMultipartWriter(ww.w)

		// Do not set ww's io.Closer for now: if this is a multipart entity but
		// CreatePart is not used (only Write is used), then the final boundary
		// is expected to be written by the user too. In this case, ww.Close
		// shouldn't write the final boundary.

		if mediaParams["boundary"] != "" {
			ww.mw.SetBoundary(mediaParams["boundary"])
		} else {
			mediaParams["boundary"] = ww.mw.Boundary()
			header.SetContentType(mediaType, mediaParams, opts...)
		}

		header.Del("Content-Transfer-Encoding")
	} else {
		wc, err := encodingWriter(header.Get("Content-Transfer-Encoding"), ww.w)
		if err != nil {
			return nil, err
		}
		ww.w = wc
		ww.c = wc
	}

	switch strings.ToLower(mediaParams["charset"]) {
	case "", "us-ascii", "utf-8":
		// This is OK
	default:
		// Anything else is invalid
		return nil, fmt.Errorf("unhandled charset %q", mediaParams["charset"])
	}

	return ww, nil
}

// CreateWriter creates a new message writer to w. If header contains an
// encoding, data written to the Writer will automatically be encoded with it.
// The charset needs to be utf-8 or us-ascii.
func CreateWriter(w io.Writer, header Header, opts ...Option) (*Writer, error) {
	// Ensure that modifications are invisible to the caller
	header = header.Copy()

	// If the message uses MIME, it has to include MIME-Version
	if !header.Has("Mime-Version") {
		header.Set("MIME-Version", "1.0")
	}

	applyParameterOptions(&header, opts)

	ww, err := createWriter(w, &header, opts)
	if err != nil {
		return nil, err
	}
	if err := textproto.WriteHeader(w, header.Header); err != nil {
		return nil, err
	}
	return ww, nil
}

// applyParameterOptions reformats Content-Type and Content-Disposition fields
// using the provided formatting options, so that extended parameters are
// split into RFC 2231 continuations even when a header was built via Set.
func applyParameterOptions(header *Header, opts []Option) {
	if len(opts) == 0 {
		return
	}
	o := resolveOptions(opts)
	for _, field := range []struct {
		key string
		get func() (string, map[string]string, error)
		set func(string, map[string]string, ...Option)
	}{
		{"Content-Type", header.ContentType, header.SetContentType},
		{"Content-Disposition", header.ContentDisposition, header.SetContentDisposition},
	} {
		if !header.Has(field.key) {
			continue
		}
		value, params, err := field.get()
		if err != nil {
			continue
		}
		field.set(value, params, WithParameterLineLen(o.paramLineLen))
	}
}

// Write implements io.Writer.
func (w *Writer) Write(b []byte) (int, error) {
	return w.w.Write(b)
}

// Close implements io.Closer.
func (w *Writer) Close() error {
	if w.c != nil {
		return w.c.Close()
	}
	return nil
}

// CreatePart returns a Writer to a new part in this multipart entity. If this
// entity is not multipart, it fails. The body of the part should be written to
// the returned io.WriteCloser.
func (w *Writer) CreatePart(header Header, opts ...Option) (*Writer, error) {
	if w.mw == nil {
		return nil, errors.New("cannot create a part in a non-multipart message")
	}

	if w.c == nil {
		// We know that the user calls CreatePart so Close should write the final
		// boundary
		w.c = w.mw
	}

	// cw -> ww -> pw -> w.mw -> w.w

	ww := &struct{ io.Writer }{nil}

	// ensure that modifications are invisible to the caller
	header = header.Copy()
	applyParameterOptions(&header, opts)
	cw, err := createWriter(ww, &header, opts)
	if err != nil {
		return nil, err
	}
	pw, err := w.mw.CreatePart(header.Header)
	if err != nil {
		return nil, err
	}

	ww.Writer = pw
	return cw, nil
}
