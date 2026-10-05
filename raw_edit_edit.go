package message

import (
	"bytes"
	"fmt"
	"io"
	"sort"

	"github.com/emersion/go-message/textproto"
)

// Edit is a single byte-exact replacement: the bytes [Range.Start, Range.End)
// of the original message are replaced by Replacement.
type Edit struct {
	// Range is the byte interval to replace, relative to the original message.
	Range ByteRange
	// Replacement is the new content. It may be shorter or longer than Range.
	Replacement []byte
}

// EditReport summarizes the side effects of applying edits.
type EditReport struct {
	// InvalidatedProtections lists the cryptographically protected subtrees
	// whose bytes were modified and whose existing signatures are therefore no
	// longer valid.
	InvalidatedProtections []Protection
}

type editConfig struct {
	invalidateSignatures bool
}

// EditOption configures an Edit call.
type EditOption func(*editConfig)

// WithInvalidateSignatures acknowledges that edits inside a cryptographically
// protected subtree (e.g. multipart/signed) invalidate the existing signature.
//
// This only allows the edit to proceed and records the invalidated protection
// in the EditReport. It does not remove or re-generate any signature.
func WithInvalidateSignatures() EditOption {
	return func(cfg *editConfig) {
		cfg.invalidateSignatures = true
	}
}

// isLocatableRange reports whether an edit stays inside a single well-defined
// structural region and never spans a multipart boundary delimiter.
func (m *RawMessage) isLocatableRange(ed Edit) bool {
	var check func(e *RawEntity) bool
	check = func(e *RawEntity) bool {
		if ed.Range == e.HeaderRange || ed.Range == e.BodyRange || ed.Range == e.WholeRange {
			return true
		}
		if e.IsLeaf() && e.BodyRange.Contains(ed.Range.Start, ed.Range.End) {
			return true
		}
		for _, child := range e.Children {
			if check(child) {
				return true
			}
		}
		return false
	}
	return check(m.Root)
}

// Edit applies one or more byte-exact edits to the message and returns the
// resulting bytes. Every byte outside the edited ranges is copied from the
// original message unchanged.
//
// An edit range must be locatable: it must either exactly match an entity's
// HeaderRange, BodyRange or WholeRange, or be fully contained within the
// BodyRange of a single leaf (non-multipart) entity. Ranges that span a
// multipart boundary or fall outside the parseable structure are rejected,
// because the surrounding MIME framing could not be guaranteed intact.
//
// Edits must not overlap. After applying them, the result is re-parsed to make
// sure the MIME structure (including all multipart boundaries) is still valid;
// an edit that corrupts the framing is rejected even if its input range was
// valid.
//
// If an edit intersects a protected subtree (such as a multipart/signed part),
// Edit returns a ProtectedSubtreeError unless WithInvalidateSignatures is
// passed. In that case the edit proceeds and the invalidated protection is
// reported. The caller is responsible for removing or re-generating the
// signature.
//
// Edit does not assess DKIM: any change to content covered by a DKIM
// signature typically invalidates that DKIM signature, regardless of whether
// the edited region is inside an application-level signed subtree. The only
// guarantee provided here is byte fidelity of the unedited regions.
func (m *RawMessage) Edit(edits []Edit, opts ...EditOption) ([]byte, *EditReport, error) {
	cfg := &editConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	if len(edits) == 0 {
		return append([]byte(nil), m.raw...), &EditReport{}, nil
	}

	sorted := make([]Edit, len(edits))
	copy(sorted, edits)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Range.Start < sorted[j].Range.Start
	})

	for i, ed := range sorted {
		r := ed.Range
		if r.Start < 0 || r.End < r.Start || r.End > len(m.raw) {
			return nil, nil, &LocateError{Reason: fmt.Sprintf("edit range [%d,%d) is out of bounds", r.Start, r.End)}
		}
		if !m.isLocatableRange(ed) {
			return nil, nil, &LocateError{Reason: fmt.Sprintf(
				"edit range [%d,%d) does not match a single entity header, body or whole-part region, or spans a multipart boundary",
				r.Start, r.End)}
		}
		if i > 0 && r.Start < sorted[i-1].Range.End {
			return nil, nil, &LocateError{Reason: fmt.Sprintf("edits at [%d,%d) and [%d,%d) overlap",
				sorted[i-1].Range.Start, sorted[i-1].Range.End, r.Start, r.End)}
		}
	}

	var invalidated []*Protection
	for _, ed := range sorted {
		for i := range m.Protections {
			p := &m.Protections[i]
			if !p.Range.Intersects(ed.Range.Start, ed.Range.End) {
				continue
			}
			if !cfg.invalidateSignatures {
				return nil, nil, &ProtectedSubtreeError{Protection: *p}
			}
			already := false
			for _, seen := range invalidated {
				if seen == p {
					already = true
					break
				}
			}
			if !already {
				invalidated = append(invalidated, p)
			}
		}
	}

	out := append([]byte(nil), m.raw...)
	for i := len(sorted) - 1; i >= 0; i-- {
		ed := sorted[i]
		replacement := append([]byte(nil), ed.Replacement...)
		out = append(out[:ed.Range.Start], append(replacement, out[ed.Range.End:]...)...)
	}

	if _, err := ParseRawMessage(out); err != nil {
		return nil, nil, &LocateError{Reason: "resulting message is not a valid MIME structure after edit: " + err.Error()}
	}

	report := &EditReport{}
	for _, p := range invalidated {
		report.InvalidatedProtections = append(report.InvalidatedProtections, *p)
	}
	return out, report, nil
}

// ReplacePart replaces a whole MIME part (header plus transfer-encoded body)
// at the given multipart path. It is a convenience wrapper around Edit using
// the part's WholeRange.
//
// rawPart must contain the new part's header block (terminated by a blank
// CRLF line), the body already encoded with the part's
// Content-Transfer-Encoding, and the CRLF that terminates the body before the
// enclosing multipart boundary delimiter. EncodePart produces exactly this
// format for leaf parts. All other bytes of the message are preserved.
//
// The same protection and DKIM caveats as for Edit apply.
func (m *RawMessage) ReplacePart(path []int, rawPart []byte, opts ...EditOption) ([]byte, *EditReport, error) {
	part, err := m.Entity(path)
	if err != nil {
		return nil, nil, err
	}
	return m.Edit([]Edit{{Range: part.WholeRange, Replacement: rawPart}}, opts...)
}

// EncodePart serializes a leaf (non-multipart) MIME part for use with
// ReplacePart.
//
// The header is written verbatim, so folding, field ordering and
// capitalization present in header are retained. The body is encoded with the
// transfer encoding advertised by the header's Content-Transfer-Encoding
// field (e.g. base64), and the final CRLF before the multipart boundary
// delimiter is appended.
//
// Multipart parts are not supported: they must be constructed from already
// encoded bytes (or edited part by part).
func EncodePart(header Header, body io.Reader) ([]byte, error) {
	h := header.Copy()

	var buf bytes.Buffer
	w, err := createWriter(&buf, &h)
	if err != nil {
		return nil, err
	}
	if w.mw != nil {
		return nil, &LocateError{Reason: "EncodePart does not support multipart parts"}
	}

	if err := textproto.WriteHeader(&buf, h.Header); err != nil {
		return nil, err
	}
	if _, err := io.Copy(w, body); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	buf.WriteString("\r\n")
	return buf.Bytes(), nil
}
