package message

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"strings"

	"github.com/emersion/go-message/textproto"
)

// ByteRange identifies a contiguous interval in the original message bytes.
// Start is inclusive and End is exclusive.
type ByteRange struct {
	Start int64
	End   int64
}

// PartRange describes the original byte intervals occupied by a MIME part.
// Path is empty for the root entity. HeaderRange and Body exclude each other;
// Part covers both and is equal to their union. Child part ranges exclude the
// CRLF that begins the next enclosing boundary delimiter and the delimiter line
// itself. HeaderRange includes the header/body separator blank line.
type PartRange struct {
	Path        []int
	Header      Header
	MediaType   string
	HeaderRange ByteRange
	Body        ByteRange
	Part        ByteRange
	Protected   bool
}

// RawMessage is a message indexed against its original byte representation.
// Ranges returned by ParseRawMessage remain valid only for the exact input
// bytes used to parse the message.
type RawMessage struct {
	raw         []byte
	root        PartRange
	parts       []PartRange
	headerEnd   int64
	mediaType   string
	mediaParams map[string]string
}

// SignatureInvalidationError indicates that an edit intersects a
// multipart/signed subtree without explicitly acknowledging that its existing
// cryptographic signature will no longer be trusted as valid.
type SignatureInvalidationError struct {
	Range ByteRange
}

func (err *SignatureInvalidationError) Error() string {
	return fmt.Sprintf("message: edit at bytes %d-%d intersects a multipart/signed subtree; signature invalidation must be explicitly allowed", err.Range.Start, err.Range.End)
}

// RawEdit replaces one original byte interval with Replacement.
type RawEdit struct {
	Range       ByteRange
	Replacement []byte

	// AllowSignatureInvalidation must be set when Range intersects a
	// multipart/signed subtree. Setting it never refreshes or validates any
	// signature.
	AllowSignatureInvalidation bool
}

type countReader struct {
	r io.Reader
	n int64
}

func (r *countReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

// ParseRawMessage parses raw and returns byte intervals that can be passed to
// RawMessage.Edit.
//
// The input must be a complete, parseable MIME message whose multipart
// boundaries can be located according to their Content-Type parameters. The
// caller must retain raw unchanged and use ranges only with that exact byte
// sequence. Ranges are byte offsets, not character offsets; they must be
// aligned with a returned complete header, complete part, or leaf body
// interval. A subrange is only permitted inside one leaf body. The replacement
// must remain valid under the part's existing Content-Transfer-Encoding and
// MIME structure; this API does not encode or normalize it.
func ParseRawMessage(raw []byte) (*RawMessage, error) {
	cr := &countReader{r: bytes.NewReader(raw)}
	br := bufio.NewReader(cr)
	header, err := textproto.ReadHeader(br)
	if err != nil {
		return nil, err
	}

	headerEnd := cr.n - int64(br.Buffered())
	rootHeader := Header{header}
	mediaType, mediaParams, err := rootHeader.ContentType()
	if err != nil {
		return nil, err
	}

	root := PartRange{
		Header:      rootHeader,
		MediaType:   mediaType,
		HeaderRange: ByteRange{Start: 0, End: headerEnd},
		Body:        ByteRange{Start: headerEnd, End: int64(len(raw))},
		Part:        ByteRange{Start: 0, End: int64(len(raw))},
		Protected:   mediaType == "multipart/signed",
	}
	m := &RawMessage{
		raw:         raw,
		root:        root,
		headerEnd:   headerEnd,
		mediaType:   mediaType,
		mediaParams: mediaParams,
	}

	if isMultipart(mediaType) {
		if mediaParams["boundary"] == "" {
			return nil, errors.New("message: multipart Content-Type has no boundary parameter")
		}
		if err := m.parseParts(
			io.NewSectionReader(bytes.NewReader(raw), headerEnd, int64(len(raw))-headerEnd),
			mediaParams["boundary"],
			headerEnd,
			nil,
			false,
			false,
		); err != nil {
			return nil, err
		}
		if err := validateFinalMultipartDelimiter(m.raw, m.root.Body, mediaParams["boundary"]); err != nil {
			return nil, err
		}
	}

	return m, nil
}

// Root returns the byte range for the whole root entity.
func (m *RawMessage) Root() PartRange {
	return m.root
}

// Parts returns all located MIME parts in depth-first order. The root entity is
// not included; use Root for its range.
func (m *RawMessage) Parts() []PartRange {
	out := make([]PartRange, len(m.parts))
	copy(out, m.parts)
	return out
}

func (m *RawMessage) parseParts(r io.Reader, boundary string, base int64, path []int, protected, signedContainer bool) error {
	mr := textproto.NewMultipartReader(r, boundary)

	for index := 0; ; index++ {
		part, err := mr.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		partPath := append(copyPath(path), index)
		partHeader := Header{part.Header}
		mediaType, mediaParams, mediaErr := partHeader.ContentType()
		if mediaErr != nil {
			return mediaErr
		}
		if isMultipart(mediaType) && mediaParams["boundary"] == "" {
			return errors.New("message: multipart Content-Type has no boundary parameter")
		}

		headerStart := base + part.StartOffset()
		bodyStart := base + part.BodyOffset()
		childProtected := protected || signedContainer

		partRange := PartRange{
			Path:        partPath,
			Header:      partHeader,
			MediaType:   mediaType,
			HeaderRange: ByteRange{Start: headerStart, End: bodyStart},
			Body:        ByteRange{Start: bodyStart, End: bodyStart},
			Part:        ByteRange{Start: headerStart, End: bodyStart},
			Protected:   childProtected || mediaType == "multipart/signed",
		}
		m.parts = append(m.parts, partRange)
		partIndex := len(m.parts) - 1

		if isMultipart(mediaType) {
			if err := m.parseParts(part, mediaParams["boundary"], bodyStart, partPath, childProtected, mediaType == "multipart/signed"); err != nil {
				return err
			}
			if _, err := io.Copy(ioutil.Discard, part); err != nil {
				return err
			}
		} else {
			if _, err := io.Copy(ioutil.Discard, part); err != nil {
				return err
			}
		}

		bodyEnd := base + part.BodyEndOffset()
		m.parts[partIndex].Body.End = bodyEnd
		m.parts[partIndex].Part.End = bodyEnd
		if isMultipart(mediaType) && !bytes.HasSuffix(m.raw[bodyStart:bodyEnd], []byte("\r\n")) {
			return fmt.Errorf("message: multipart part ending at byte %d lacks a complete CRLF delimiter line", bodyEnd)
		}
		if err := validatePartDelimiter(m.raw, bodyEnd, boundary); err != nil {
			return err
		}
	}
}

func validatePartDelimiter(raw []byte, offset int64, boundary string) error {
	prefix := []byte("\r\n--" + boundary)
	if !bytes.HasPrefix(raw[offset:], prefix) {
		return fmt.Errorf("message: part ending at byte %d is not followed by its CRLF-prefixed MIME boundary", offset)
	}
	rest := raw[offset+int64(len(prefix)):]
	if len(rest) == 0 {
		return nil
	}
	switch rest[0] {
	case '-', '\r', '\n', ' ', '\t':
		return nil
	default:
		return fmt.Errorf("message: part ending at byte %d is not followed by a valid MIME boundary", offset)
	}
}

func validateFinalMultipartDelimiter(raw []byte, body ByteRange, boundary string) error {
	expected := []byte("\r\n--" + boundary + "--\r\n")
	if !bytes.HasSuffix(raw[body.Start:body.End], expected) {
		return fmt.Errorf("message: multipart body does not end with a complete CRLF-delimited final boundary")
	}
	return nil
}

// Edit applies one or more byte-range replacements and returns the resulting
// message. Bytes outside edited ranges are copied without parsing, decoding,
// re-encoding, refolding headers, or changing multipart boundaries.
//
// Byte preservation only applies to unedited regions. It does not imply that a
// whole-message DKIM signature covering the edited body remains valid.
//
// Edits must not overlap, must be ordered by start offset, and must target
// bytes in raw. A range may cover a whole part, a complete part header, or a
// leaf part body/subbody. Ranges cannot cross structural MIME boundaries, such
// as a header/body separator or an adjacent part delimiter. Replacements for a
// full child header or body must not include the CRLF-prefixed enclosing
// boundary delimiter; that delimiter remains outside the range. A complete
// header replacement must still include the header/body separator blank line.
func (m *RawMessage) Edit(edits []RawEdit) ([]byte, error) {
	for i, edit := range edits {
		if err := m.validateRange(edit.Range); err != nil {
			return nil, err
		}
		if !edit.AllowSignatureInvalidation && m.rangeIsProtected(edit.Range) {
			return nil, &SignatureInvalidationError{Range: edit.Range}
		}
		if i > 0 && edit.Range.Start < edits[i-1].Range.End {
			return nil, errors.New("message: raw edits overlap or are not ordered")
		}
	}

	var out bytes.Buffer
	last := int64(0)
	for _, edit := range edits {
		out.Write(m.raw[last:edit.Range.Start])
		out.Write(edit.Replacement)
		last = edit.Range.End
	}
	out.Write(m.raw[last:])
	return out.Bytes(), nil
}

func (m *RawMessage) validateRange(r ByteRange) error {
	if r.Start < 0 || r.End < r.Start || r.End > int64(len(m.raw)) {
		return fmt.Errorf("message: invalid raw byte range %d-%d", r.Start, r.End)
	}
	if r == m.root.Part || r == m.root.HeaderRange || r == m.root.Body {
		return nil
	}
	if !isMultipart(m.mediaType) && r.Start >= m.root.Body.Start && r.End <= m.root.Body.End {
		return nil
	}

	for _, part := range m.parts {
		if r == part.HeaderRange || r == part.Body || r == part.Part {
			return nil
		}
		if r.Start >= part.Body.Start && r.End <= part.Body.End && !isMultipart(part.MediaType) {
			return nil
		}
	}
	return fmt.Errorf("message: raw byte range %d-%d is not aligned with a MIME header or leaf body", r.Start, r.End)
}

func (m *RawMessage) rangeIsProtected(r ByteRange) bool {
	if m.root.Protected && rangesOverlap(r, m.root.Part) {
		return true
	}
	for _, part := range m.parts {
		if !part.Protected {
			continue
		}
		if r.Start == r.End {
			if r.Start >= part.Part.Start && r.Start <= part.Part.End {
				return true
			}
			continue
		}
		if r.Start < part.Part.End && r.End > part.Part.Start {
			return true
		}
	}
	return false
}

func rangesOverlap(a, b ByteRange) bool {
	if a.Start == a.End {
		return a.Start >= b.Start && a.Start <= b.End
	}
	return a.Start < b.End && a.End > b.Start
}

func isMultipart(mediaType string) bool {
	return strings.HasPrefix(mediaType, "multipart/")
}

func copyPath(path []int) []int {
	if len(path) == 0 {
		return nil
	}
	out := make([]int, len(path))
	copy(out, path)
	return out
}
