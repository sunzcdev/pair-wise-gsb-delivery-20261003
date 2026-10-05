package message

import (
	"bufio"
	"bytes"
	"fmt"

	"github.com/emersion/go-message/textproto"
)

// readRawHeader locates and parses the header block of the entity starting at
// start. It returns the parsed header and the offset immediately after the
// terminating blank line.
//
// CRLF line endings are mandatory inside the header block: folded lines rely
// on the exact wire bytes for byte-exact edits and DKIM/S/MIME fidelity.
func readRawHeader(raw []byte, start, end int) (Header, int, error) {
	block := raw[start:end]

	sep := bytes.Index(block, []byte("\r\n\r\n"))
	if sep < 0 {
		return Header{}, 0, &LocateError{Reason: "entity header is not terminated by a CRLFCRLF blank line"}
	}

	headerBytes := block[:sep+4]
	for i := 0; i < sep; i++ {
		if headerBytes[i] == '\n' && (i == 0 || headerBytes[i-1] != '\r') {
			return Header{}, 0, &LocateError{Reason: "entity header uses bare LF line endings; CRLF is required for byte-exact edits"}
		}
	}

	br := bufio.NewReader(bytes.NewReader(headerBytes))
	h, err := textproto.ReadHeader(br)
	if err != nil {
		return Header{}, 0, &LocateError{Reason: "cannot parse entity header: " + err.Error()}
	}

	return Header{h}, start + sep + 4, nil
}

// rawPartRange describes a single part found inside a multipart body.
type rawPartRange struct {
	// headerStart is the offset of the first byte of the part's header.
	headerStart int
	// bodyEnd is the offset just past the part body, i.e. the first '-' of the
	// following boundary delimiter line. The CRLF preceding it is part of the
	// part body.
	bodyEnd int
}

// rawDelim is a located multipart boundary delimiter line.
type rawDelim struct {
	// dashAt is the offset of the first '-' of "--boundary".
	dashAt int
	// headerStart is the offset after the delimiter's CRLF, where part headers
	// begin. It equals end when the delimiter closes the multipart and is not
	// followed by a CRLF.
	headerStart int
	close       bool
}

// matchBoundary checks whether a boundary delimiter line starts at dashAt.
// It returns the offset after the terminating CRLF (where part headers begin),
// whether the delimiter closes the multipart, and whether it matches.
func matchBoundary(raw []byte, dashAt, end int, boundary []byte) (int, bool, bool) {
	p := dashAt + 2 + len(boundary)
	if p > end || !bytes.Equal(raw[dashAt:dashAt+2], []byte("--")) ||
		!bytes.Equal(raw[dashAt+2:dashAt+2+len(boundary)], boundary) {
		return 0, false, false
	}

	close := false
	if p+2 <= end && raw[p] == '-' && raw[p+1] == '-' {
		close = true
		p += 2
	}

	for p < end && (raw[p] == ' ' || raw[p] == '\t') {
		p++
	}

	// RFC 2046 requires a CRLF. A closing delimiter at the very end of the
	// buffer without a trailing CRLF is accepted for interoperability.
	if p < end && raw[p] == '\r' && p+1 < end && raw[p+1] == '\n' {
		return p + 2, close, true
	}
	if close && p == end {
		return end, true, true
	}
	return 0, false, false
}

// splitMultipart locates every part in a multipart body using raw boundary
// scanning. It never looks inside part bodies, so their transfer encodings
// and bytes are irrelevant.
func splitMultipart(raw []byte, bodyStart, bodyEnd int, boundary string) ([]rawPartRange, error) {
	if boundary == "" {
		return nil, &LocateError{Reason: "multipart entity has an empty boundary parameter"}
	}
	b := []byte(boundary)

	// Locate the opening delimiter. Canonical messages start with it directly.
	// RFC 2046 also allows a non-empty "preamble" before the first boundary, in
	// which case the delimiter is preceded by CRLF and that CRLF belongs to the
	// preamble (so a missing part terminator is not silently accepted).
	var first rawDelim
	if next, close, ok := matchBoundary(raw, bodyStart, bodyEnd, b); ok {
		first = rawDelim{dashAt: bodyStart, headerStart: next, close: close}
	} else {
		needle := append([]byte("\r\n--"), b...)
		rel := bytes.Index(raw[bodyStart:bodyEnd], needle)
		if rel <= 0 {
			return nil, &LocateError{Reason: "multipart body is missing its opening boundary delimiter"}
		}
		dashAt := bodyStart + rel + 2
		next, close, matched := matchBoundary(raw, dashAt, bodyEnd, b)
		if !matched {
			return nil, &LocateError{Reason: "multipart body is missing its opening boundary delimiter"}
		}
		first = rawDelim{dashAt: dashAt, headerStart: next, close: close}
	}

	delims := []rawDelim{first}
	if !first.close {
		cursor := first.headerStart
		needle := append([]byte("\r\n--"), b...)
		for {
			rel := bytes.Index(raw[cursor:bodyEnd], needle)
			if rel < 0 {
				return nil, &LocateError{Reason: "multipart body is missing a closing boundary delimiter"}
			}
			dashAt := cursor + rel + 2

			next, close, matched := matchBoundary(raw, dashAt, bodyEnd, b)
			if !matched {
				// A body happened to contain something that merely shares the
				// boundary prefix; keep scanning.
				cursor = dashAt + 2
				continue
			}

			delims = append(delims, rawDelim{dashAt: dashAt, headerStart: next, close: close})
			if close {
				break
			}
			cursor = next
		}
	}

	parts := make([]rawPartRange, 0, len(delims)-1)
	for i := 0; i+1 < len(delims); i++ {
		if delims[i+1].close && i+1 != len(delims)-1 {
			break // unreachable: scanning stops at the first closing delimiter
		}
		// The part body includes the CRLF that terminates it, i.e. it ends at
		// the first '-' of the next delimiter line.
		parts = append(parts, rawPartRange{
			headerStart: delims[i].headerStart,
			bodyEnd:     delims[i+1].dashAt,
		})
	}
	return parts, nil
}

// parseRawEntity parses one entity occupying raw[start:end]. The interval
// contains the entity's header and body but not the surrounding multipart
// boundary delimiter lines.
func parseRawEntity(raw []byte, start, end int, path []int) (*RawEntity, error) {
	header, headerEnd, err := readRawHeader(raw, start, end)
	if err != nil {
		return nil, err
	}

	mediaType, mediaParams, err := header.ContentType()
	if err != nil {
		return nil, &LocateError{Reason: fmt.Sprintf("cannot parse Content-Type at path %v: %v", path, err)}
	}

	e := &RawEntity{
		Path:        append([]int(nil), path...),
		Header:      header,
		MediaType:   mediaType,
		MediaParams: mediaParams,
		HeaderRange: ByteRange{Start: start, End: headerEnd},
		BodyRange:   ByteRange{Start: headerEnd, End: end},
		WholeRange:  ByteRange{Start: start, End: end},
		raw:         raw,
	}

	if boundary := mediaParams["boundary"]; boundary != "" {
		parts, err := splitMultipart(raw, headerEnd, end, boundary)
		if err != nil {
			return nil, err
		}
		for i, part := range parts {
			childPath := append(append([]int(nil), path...), i)
			child, err := parseRawEntity(raw, part.headerStart, part.bodyEnd, childPath)
			if err != nil {
				return nil, err
			}
			e.Children = append(e.Children, child)
		}
	}

	return e, nil
}

// isProtectionContainer reports whether mediaType is a security multipart whose
// subtree is cryptographically protected. Only multipart/signed is detected:
// multipart/encrypted parts are opaque and contain no plaintext subtree to
// edit.
func isProtectionContainer(mediaType string) bool {
	return mediaType == "multipart/signed"
}

// ParseRawMessage parses a complete MIME message into a RawMessage.
//
// The input must satisfy the following locatability conditions:
//
//   - It is a complete, fully buffered message available as a single byte
//     slice; a streaming reader cannot be spliced into.
//   - Header blocks and multipart delimiter lines use CRLF line endings.
//     Lone-LF messages are rejected because their boundaries cannot be located
//     byte-exactly.
//   - Every multipart Content-Type carries a non-empty boundary parameter, and
//     each multipart body can be parsed up to and including its closing
//     boundary delimiter ("--boundary--").
//   - Each entity's header block is terminated by a blank line.
//
// Parsing validates only the MIME structure; it never decodes or re-encodes
// bodies, so transfer encodings and arbitrary body bytes are left untouched.
func ParseRawMessage(raw []byte) (*RawMessage, error) {
	if len(raw) == 0 {
		return nil, &LocateError{Reason: "empty message"}
	}

	root, err := parseRawEntity(raw, 0, len(raw), nil)
	if err != nil {
		return nil, err
	}

	m := &RawMessage{raw: raw, Root: root}

	var walk func(e *RawEntity, ancestors []*Protection)
	walk = func(e *RawEntity, ancestors []*Protection) {
		e.protectedBy = append([]*Protection(nil), ancestors...)
		m.Entities = append(m.Entities, e)

		if isProtectionContainer(e.MediaType) {
			p := &Protection{
				MediaType: e.MediaType,
				Path:      append([]int(nil), e.Path...),
				Range:     e.WholeRange,
			}
			m.Protections = append(m.Protections, *p)
			ancestors = append(append([]*Protection(nil), ancestors...), p)
		}
		for _, child := range e.Children {
			walk(child, ancestors)
		}
	}
	walk(root, nil)

	return m, nil
}

// Raw returns the original message bytes.
//
// The returned slice must not be modified.
func (m *RawMessage) Raw() []byte {
	return m.raw
}

// Entity returns the entity at the given multipart path, using the same
// indexing scheme as WalkFunc. A nil or empty path returns the root entity.
func (m *RawMessage) Entity(path []int) (*RawEntity, error) {
	e := m.Root
	for _, idx := range path {
		if !e.Multipart() {
			return nil, &LocateError{Reason: fmt.Sprintf("path %v crosses a non-multipart entity", path)}
		}
		if idx < 0 || idx >= len(e.Children) {
			return nil, &LocateError{Reason: fmt.Sprintf("path %v references missing part %d", path, idx)}
		}
		e = e.Children[idx]
	}
	return e, nil
}
