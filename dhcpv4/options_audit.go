package dhcpv4

import (
	"errors"
	"fmt"
	"io"
)

// OptionField identifies the packet field an option fragment was decoded
// from.
type OptionField uint8

const (
	// FieldOptions is the main DHCP options field (after the magic cookie).
	FieldOptions OptionField = iota
	// FieldFile is the 128-byte "file" field, used for options when the
	// option-overload flag claims it (RFC 2132, Section 9.3, values 1/3).
	FieldFile
	// FieldSname is the 64-byte "sname" field, used for options when the
	// option-overload flag claims it (RFC 2132, Section 9.3, values 2/3).
	FieldSname
)

// String returns a short, stable name for the field.
func (f OptionField) String() string {
	switch f {
	case FieldOptions:
		return "options"
	case FieldFile:
		return "file"
	case FieldSname:
		return "sname"
	}
	return fmt.Sprintf("unknown-field(%d)", int(f))
}

// packetOffset converts a field-relative offset into an absolute packet
// offset.
//
// The fixed BOOTP header places "sname" at byte 44, "file" at byte 108 and
// the options field (after the magic cookie) at byte 240.
func packetOffset(field OptionField, relative int) int {
	switch field {
	case FieldSname:
		return 44 + relative
	case FieldFile:
		return 108 + relative
	default:
		return 240 + relative
	}
}

// FragmentKind classifies an OptionFragment.
type FragmentKind uint8

const (
	// FragmentOption is a regular code/length/value option.
	FragmentOption FragmentKind = iota
	// FragmentPad is a single Pad (code 0) byte.
	FragmentPad
	// FragmentEnd is a single End (code 255) byte.
	FragmentEnd
)

// String returns a short name for the fragment kind.
func (k FragmentKind) String() string {
	switch k {
	case FragmentPad:
		return "pad"
	case FragmentEnd:
		return "end"
	default:
		return "option"
	}
}

// TruncatedOptionError describes an option whose declared value (or its length
// byte) runs past the end of the field it was found in.
//
// Offset is field-relative; PacketOffset is relative to the start of the
// packet. HasLength is false when even the length byte could not be read.
type TruncatedOptionError struct {
	Field          OptionField
	Code           uint8
	Offset         int
	PacketOffset   int
	HasLength      bool
	DeclaredLength int
	Available      int
}

func (e *TruncatedOptionError) Error() string {
	if !e.HasLength {
		return fmt.Sprintf("truncated option code %d in %s field at packet offset %d: length byte is missing",
			e.Code, e.Field, e.PacketOffset)
	}
	return fmt.Sprintf(
		"truncated option code %d in %s field at packet offset %d: declares %d value byte(s) but only %d remain",
		e.Code, e.Field, e.PacketOffset, e.DeclaredLength, e.Available)
}

// OptionFragment is an original, on-the-wire option record.
//
// Unlike the merged Options map, fragments preserve repeats, Pad/End bytes and
// the order in which options were actually observed.
//
// Offset is field-relative and PacketOffset is relative to the start of the
// packet. Order is the zero-based, packet-wide observation index across every
// field (options, then file, then sname), so it reflects the real wire order
// rather than the sorted/re-serialized order.
//
// Group numbers options that are concatenated per RFC 3396; all fragments with
// the same option code share a group, assigned in order of first appearance.
// IndexInGroup is the fragment's zero-based position inside that group. Pad
// and End fragments have Group and IndexInGroup set to -1.
type OptionFragment struct {
	Kind         FragmentKind
	Code         uint8
	Field        OptionField
	Offset       int
	PacketOffset int
	Length       int
	Value        []byte
	Truncated    bool
	Group        int
	IndexInGroup int
	Order        int
}

// FieldEvidence records structural facts about a single option-carrying
// field.
//
// EndOffset is the field-relative offset of the End byte, or -1 when no End
// was found. Any bytes observed after End are copied into Trailing, with their
// field-relative offset in TrailingOffset.
type FieldEvidence struct {
	Field          OptionField
	EndOffset      int
	TrailingOffset int
	Trailing       []byte
}

// OptionAudit is the result of an audit decode: it carries both the normal,
// merged logical options and the raw, ordered option fragments captured from
// the packet.
//
// Options uses the same merge semantics as the regular decoder (RFC 3396
// concatenation), including options split across overloaded fields. Fragments
// is ordered by observation across fields and never re-sorted.
//
// Complete reports whether every scanned field terminated cleanly at End with
// no truncation. A false value means Options is a usable prefix but must not
// be treated as a complete decode.
type OptionAudit struct {
	Options   Options
	Fragments []OptionFragment
	Fields    []FieldEvidence
	Complete  bool
}

// Occurrences returns how many times the given option code appeared on the
// wire (counting each fragment separately).
func (a *OptionAudit) Occurrences(code uint8) int {
	var n int
	for _, f := range a.Fragments {
		if f.Kind == FragmentOption && f.Code == code {
			n++
		}
	}
	return n
}

// FragmentsFor returns the raw fragments belonging to a single concatenation
// group, in observation order.
func (a *OptionAudit) FragmentsFor(code uint8) []OptionFragment {
	var out []OptionFragment
	for _, f := range a.Fragments {
		if f.Kind == FragmentOption && f.Code == code {
			out = append(out, f)
		}
	}
	return out
}

// optionScanner accumulates merged logical options and ordered fragments while
// walking one or more option-carrying fields.
type optionScanner struct {
	merged    Options
	fragments []OptionFragment
	groupOf   map[uint8]int
	indexOf   map[uint8]int
	nextGroup int
	order     int
}

func newOptionScanner(merged Options) *optionScanner {
	return &optionScanner{
		merged:  merged,
		groupOf: make(map[uint8]int),
		indexOf: make(map[uint8]int),
	}
}

func (s *optionScanner) addStructural(kind FragmentKind, code uint8, field OptionField, offset int) {
	s.fragments = append(s.fragments, OptionFragment{
		Kind:         kind,
		Code:         code,
		Field:        field,
		Offset:       offset,
		PacketOffset: packetOffset(field, offset),
		Group:        -1,
		IndexInGroup: -1,
		Order:        s.order,
	})
	s.order++
}

func (s *optionScanner) groupFor(code uint8) int {
	g, ok := s.groupOf[code]
	if !ok {
		g = s.nextGroup
		s.nextGroup++
		s.groupOf[code] = g
	}
	return g
}

func (s *optionScanner) indexFor(code uint8) int {
	idx := s.indexOf[code]
	s.indexOf[code] = idx + 1
	return idx
}

// scanField walks one option-carrying field, merging regular options into the
// shared logical map and appending raw fragments.
func scanField(field OptionField, data []byte, requireEnd bool, s *optionScanner) (FieldEvidence, error) {
	ev := FieldEvidence{Field: field, EndOffset: -1}

	i := 0
	for i < len(data) {
		code := data[i]

		if code == optPad {
			s.addStructural(FragmentPad, optPad, field, i)
			i++
			continue
		}
		if code == optEnd {
			s.addStructural(FragmentEnd, optEnd, field, i)
			ev.EndOffset = i
			i++
			if i < len(data) {
				ev.TrailingOffset = i
				ev.Trailing = append([]byte(nil), data[i:]...)
			}
			return ev, nil
		}

		// Regular option: code at i, length at i+1, value follows.
		if i+1 >= len(data) {
			s.fragments = append(s.fragments, OptionFragment{
				Kind:         FragmentOption,
				Code:         code,
				Field:        field,
				Offset:       i,
				PacketOffset: packetOffset(field, i),
				Truncated:    true,
				Group:        s.groupFor(code),
				IndexInGroup: s.indexFor(code),
				Order:        s.order,
			})
			s.order++
			return ev, &TruncatedOptionError{
				Field:        field,
				Code:         code,
				Offset:       i,
				PacketOffset: packetOffset(field, i),
			}
		}

		declared := int(data[i+1])
		valStart := i + 2
		available := len(data) - valStart
		end := valStart + declared
		truncated := end > len(data)
		if truncated {
			end = len(data)
		}
		// Copy so evidence stays stable even if the caller reuses the
		// input buffer.
		value := append([]byte(nil), data[valStart:end]...)

		s.merged[code] = append(s.merged[code], value...)
		s.fragments = append(s.fragments, OptionFragment{
			Kind:         FragmentOption,
			Code:         code,
			Field:        field,
			Offset:       i,
			PacketOffset: packetOffset(field, i),
			Length:       declared,
			Value:        value,
			Truncated:    truncated,
			Group:        s.groupFor(code),
			IndexInGroup: s.indexFor(code),
			Order:        s.order,
		})
		s.order++

		if truncated {
			return ev, &TruncatedOptionError{
				Field:          field,
				Code:           code,
				Offset:         i,
				PacketOffset:   packetOffset(field, i),
				HasLength:      true,
				DeclaredLength: declared,
				Available:      available,
			}
		}
		i = valStart + declared
	}

	if requireEnd {
		return ev, io.ErrUnexpectedEOF
	}
	return ev, nil
}

// AuditFromBytes decodes a DHCPv4 packet like FromBytes, but additionally
// returns raw option evidence for device-compatibility troubleshooting.
//
// The returned packet carries the normal, merged logical options (including
// options carried in overloaded "file"/"sname" fields per RFC 2132, Section
// 9.3). The OptionAudit additionally preserves every on-the-wire fragment:
// repeats, Pad/End bytes, unknown options, field membership, byte offsets,
// observation order and RFC 3396 concatenation grouping.
//
// On a malformed/truncated packet a non-nil packet and audit are returned with
// whatever prefix could be decoded, together with a non-nil error; Complete is
// false and the logical Options must not be treated as complete.
// Header-level failures return nil values.
func AuditFromBytes(q []byte) (*DHCPv4, *OptionAudit, error) {
	p, snameRaw, fileRaw, opts, err := parseDHCPv4Header(q)
	if err != nil {
		return nil, nil, err
	}

	audit := &OptionAudit{Options: make(Options)}
	s := newOptionScanner(audit.Options)

	var errs []error

	ev, err := scanField(FieldOptions, opts, true, s)
	audit.Fields = append(audit.Fields, ev)
	if err != nil {
		errs = append(errs, err)
	}

	// Determine which overloaded fields to scan. The overload option is only
	// honored in the main options field and must be a single byte in 1..3.
	var extra []OptionField
	if raw, ok := audit.Options[OptionOptionOverload.Code()]; ok {
		switch {
		case len(raw) != 1:
			errs = append(errs, fmt.Errorf("invalid option-overload option: expected 1 byte, got %d", len(raw)))
		case raw[0] < 1 || raw[0] > 3:
			errs = append(errs, fmt.Errorf("invalid option-overload value %d: must be 1, 2 or 3", raw[0]))
		default:
			// RFC 2132: file is parsed before sname when both are used.
			if raw[0] == 1 || raw[0] == 3 {
				extra = append(extra, FieldFile)
				p.BootFileName = ""
			}
			if raw[0] == 2 || raw[0] == 3 {
				extra = append(extra, FieldSname)
				p.ServerHostName = ""
			}
		}
	}

	for _, f := range extra {
		var data []byte
		switch f {
		case FieldFile:
			data = fileRaw
		case FieldSname:
			data = snameRaw
		}
		ev, err := scanField(f, data, true, s)
		audit.Fields = append(audit.Fields, ev)
		if err != nil {
			errs = append(errs, err)
		}
	}

	audit.Fragments = s.fragments
	p.Options = audit.Options
	audit.Complete = len(errs) == 0

	if len(errs) == 0 {
		return p, audit, nil
	}
	if len(errs) == 1 {
		return p, audit, errs[0]
	}
	return p, audit, errors.Join(errs...)
}
