package dhcpv4

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/insomniacslk/dhcp/iana"
	"github.com/insomniacslk/dhcp/rfc1035label"
	"github.com/u-root/uio/uio"
)

var (
	// ErrShortByteStream is an error that is thrown any time a short byte stream is
	// detected during option parsing.
	ErrShortByteStream = errors.New("short byte stream")

	// ErrZeroLengthByteStream is an error that is thrown any time a zero-length
	// byte stream is encountered.
	ErrZeroLengthByteStream = errors.New("zero-length byte stream")
)

// OptionValue is an interface that all DHCP v4 options adhere to.
type OptionValue interface {
	ToBytes() []byte
	String() string
}

// Option is a DHCPv4 option and consists of a 1-byte option code and a value
// stream of bytes.
//
// The value is to be interpreted based on the option code.
type Option struct {
	Code  OptionCode
	Value OptionValue
}

// String returns a human-readable version of this option.
func (o Option) String() string {
	v := o.Value.String()
	if strings.Contains(v, "\n") {
		return fmt.Sprintf("%s:\n%s", o.Code, v)
	}
	return fmt.Sprintf("%s: %s", o.Code, v)
}

// Options is a collection of options.
type Options map[uint8][]byte

// OptionsFromList adds all given options to an options map.
func OptionsFromList(o ...Option) Options {
	opts := make(Options)
	for _, opt := range o {
		opts.Update(opt)
	}
	return opts
}

// Get will attempt to get all options that match a DHCPv4 option
// from its OptionCode.  If the option was not found it will return an
// empty list.
//
// According to RFC 3396, options that are specified more than once are
// concatenated, and hence this should always just return one option. This
// currently returns a list to be API compatible.
func (o Options) Get(code OptionCode) []byte {
	return o[code.Code()]
}

// Has checks whether o has the given opcode.
func (o Options) Has(opcode OptionCode) bool {
	_, ok := o[opcode.Code()]
	return ok
}

// Del deletes the option matching the option code.
func (o Options) Del(opcode OptionCode) {
	delete(o, opcode.Code())
}

// Update updates the existing options with the passed option, adding it
// at the end if not present already
func (o Options) Update(option Option) {
	o[option.Code.Code()] = option.Value.ToBytes()
}

// ToBytes makes Options usable as an OptionValue as well.
//
// Used in the case of vendor-specific and relay agent options.
func (o Options) ToBytes() []byte {
	return uio.ToBigEndian(o)
}

// FromBytes parses a sequence of bytes until the end and builds a list of
// options from it.
//
// The sequence should not contain the DHCP magic cookie.
//
// Returns an error if any invalid option or length is found.
func (o Options) FromBytes(data []byte) error {
	return o.fromBytesCheckEnd(data, false)
}

// OptionField identifies one of the option-carrying fields of a DHCPv4 packet.
//
// Per RFC 2131 section 4.1 "Extending BOOTP options", the usual options area
// may be supplemented by the legacy "file" and "sname" fields when the
// "option overload" option is present.
type OptionField uint8

const (
	// FieldOptions is the regular DHCPv4 options area that starts right
	// after the magic cookie.
	FieldOptions OptionField = iota
	// FieldFile is the legacy BOOTP "file" field (128 bytes) reused to carry
	// options when option overload is in effect.
	FieldFile
	// FieldSName is the legacy BOOTP "sname" field (64 bytes) reused to
	// carry options when option overload is in effect.
	FieldSName
)

// String returns a human-readable name for the option field.
func (f OptionField) String() string {
	switch f {
	case FieldFile:
		return "file"
	case FieldSName:
		return "sname"
	default:
		return "options"
	}
}

// FragmentKind distinguishes ordinary options from the single-byte Pad and End
// markers, which carry no length or value.
type FragmentKind uint8

const (
	// FragmentOption is a regular TLV option (code, length, value).
	FragmentOption FragmentKind = iota
	// FragmentPad is a single-byte Pad marker (code 0).
	FragmentPad
	// FragmentEnd is a single-byte End marker (code 255).
	FragmentEnd
)

// String returns a human-readable name for the fragment kind.
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

// OptionFragment is a single raw option as found on the wire, before any
// merging is applied.
//
// Unlike the merged Options map it preserves Pad and End markers, unknown
// option codes, every duplicate occurrence and the order in which fragments
// actually appeared.
//
// Every byte slice referenced by an OptionFragment is copied, so the evidence
// stays valid even if the input buffer is reused after decoding.
type OptionFragment struct {
	// Code is the raw option code. It is 0 for Pad and 255 for End.
	Code uint8
	// Kind distinguishes regular TLV options from Pad and End markers.
	Kind FragmentKind
	// Value is the raw, uninterpreted value of the option. It is nil for
	// Pad and End and empty (but non-nil) for a zero-length option.
	Value []byte
	// Field is the packet field this fragment was decoded from.
	Field OptionField
	// Offset is the byte offset of the option code relative to the start
	// of Field.
	Offset int
	// PacketOffset is the byte offset of the option code relative to the
	// start of the whole packet for the packet-level audit entry point; it
	// equals Offset for Options.AuditFromBytes.
	PacketOffset int
	// Order is the zero-based position of this fragment in the raw stream
	// across all parsed fields. It reflects wire order, not the sorted key
	// order used when re-serializing.
	Order int
	// Part is the zero-based occurrence index of this fragment among
	// fragments sharing the same Code, indicating its place in the merged
	// value per RFC 3396. It is 0 for the first occurrence.
	Part int
}

// OptionAudit is the evidence returned by an audit decode: the merged logical
// options together with every raw fragment they came from.
type OptionAudit struct {
	// Options holds the merged logical option values, exactly as the
	// regular FromBytes decoder produces them.
	Options Options
	// Fragments lists every raw fragment in wire order, including Pad,
	// End, unknown and duplicated options.
	Fragments []OptionFragment
	// Trailing holds the raw bytes found after the End marker in a field,
	// keyed by the field the bytes belong to. Nil for fields without a
	// trailing payload.
	Trailing map[OptionField][]byte
	// Complete reports whether the decoded content is a complete,
	// non-truncated option stream. When false, Options must not be assumed
	// to describe the full message.
	Complete bool

	// parts counts occurrences of each option code for Part assignment.
	parts map[uint8]int
}

func (a *OptionAudit) addFragment(f OptionFragment) {
	if a.parts == nil {
		a.parts = make(map[uint8]int)
	}
	f.Order = len(a.Fragments)
	f.Part = a.parts[f.Code]
	a.parts[f.Code]++
	a.Fragments = append(a.Fragments, f)
}

// decodeField parses a single option-carrying field. It appends the raw
// fragments to audit.Fragments and merges ordinary option values into
// audit.Options.
//
// base is the packet-level offset of field's first byte, used to compute
// PacketOffset. requireEnd controls whether a missing End marker is an error.
//
// On a truncated fragment a precise error is returned together with the
// prefix fragments already decoded.
func (a *OptionAudit) decodeField(field OptionField, base int, fieldData []byte, requireEnd bool) (sawEnd bool, err error) {
	pos := 0
	for pos < len(fieldData) {
		offset := pos
		code := fieldData[pos]
		pos++

		frag := OptionFragment{
			Code:         code,
			Field:        field,
			Offset:       offset,
			PacketOffset: base + offset,
		}

		if code == optPad {
			frag.Kind = FragmentPad
			a.addFragment(frag)
			continue
		}
		if code == optEnd {
			frag.Kind = FragmentEnd
			a.addFragment(frag)
			trailing := fieldData[pos:]
			if len(trailing) > 0 {
				if a.Trailing == nil {
					a.Trailing = make(map[OptionField][]byte)
				}
				a.Trailing[field] = append([]byte(nil), trailing...)
			}
			return true, nil
		}

		frag.Kind = FragmentOption
		if pos >= len(fieldData) {
			frag.Value = []byte{}
			a.addFragment(frag)
			return false, fmt.Errorf("option code %d at %s offset %d: length byte is missing (truncated)", code, field, offset)
		}
		length := int(fieldData[pos])
		pos++

		start := pos
		if pos+length > len(fieldData) {
			// Keep the truncated fragment as evidence, carrying only the
			// bytes actually present. It is deliberately not merged into
			// Options so the logical value never claims completeness.
			available := make([]byte, len(fieldData)-start)
			copy(available, fieldData[start:])
			frag.Value = available
			a.addFragment(frag)
			return false, fmt.Errorf("option code %d at %s offset %d declares length %d but only %d value bytes remain (truncated)",
				code, field, offset, length, len(fieldData)-start)
		}

		value := make([]byte, length)
		copy(value, fieldData[start:start+length])
		frag.Value = value
		pos += length
		a.addFragment(frag)

		// RFC 2131 section 4.1 / RFC 3396: repeated instances of the
		// same option are concatenated into one value, in wire order and
		// across overloaded fields.
		a.Options[code] = append(a.Options[code], value...)
	}

	if requireEnd {
		return false, io.ErrUnexpectedEOF
	}
	return false, nil
}

// AuditFromBytes parses a sequence of option bytes and returns both the merged
// logical options and the raw fragment evidence used to build them.
//
// The sequence should not contain the DHCP magic cookie. This entry point is
// field-agnostic: every fragment is attributed to FieldOptions and its
// PacketOffset equals its Offset. Use FromBytesWithAudit to decode a full
// packet including option-overload fields.
//
// Pad, End, unknown and duplicated options are all preserved as fragments.
// A truncated fragment yields a precise error plus the fragments and merged
// prefix that could still be decoded, and Complete is left false.
//
// The returned byte slices do not alias data, so they remain stable after the
// input buffer is reused.
func (o Options) AuditFromBytes(data []byte) (*OptionAudit, error) {
	audit := &OptionAudit{Options: o}
	if audit.Options == nil {
		audit.Options = make(Options)
	}
	if len(data) == 0 {
		return audit, nil
	}
	sawEnd, err := audit.decodeField(FieldOptions, 0, data, false)
	audit.Complete = err == nil && sawEnd
	return audit, err
}

const (
	optPad       = 0
	optAgentInfo = 82
	optEnd       = 255
)

// FromBytesCheckEnd parses Options from byte sequences using the
// parsing function that is passed in as a paremeter
func (o Options) fromBytesCheckEnd(data []byte, checkEndOption bool) error {
	if len(data) == 0 {
		return nil
	}
	buf := uio.NewBigEndianBuffer(data)

	var end bool
	for buf.Len() >= 1 {
		// 1 byte: option code
		// 1 byte: option length n
		// n bytes: data
		code := buf.Read8()

		if code == optPad {
			continue
		} else if code == optEnd {
			end = true
			break
		}
		length := int(buf.Read8())

		// N bytes: option data
		data := buf.Consume(length)
		if data == nil {
			return fmt.Errorf("error collecting options: %v", buf.Error())
		}
		data = data[:length:length]

		// RFC 2131, Section 4.1 "Options may appear only once, [...].
		// The client concatenates the values of multiple instances of
		// the same option into a single parameter list for
		// configuration."
		//
		// See also RFC 3396 for concatenation order and options longer
		// than 255 bytes.
		o[code] = append(o[code], data...)
	}

	// If we never read the End option, the sender of this packet screwed
	// up.
	if !end && checkEndOption {
		return io.ErrUnexpectedEOF
	}

	return nil
}

// sortedKeys returns an ordered slice of option keys from the Options map, for
// use in serializing options to binary.
func (o Options) sortedKeys() []int {
	// Send all values for a given key
	var codes []int
	var hasOptAgentInfo, hasOptEnd bool
	for k := range o {
		// RFC 3046 section 2.1 states that option 82 SHALL come last (ignoring End).
		if k == optAgentInfo {
			hasOptAgentInfo = true
			continue
		}
		if k == optEnd {
			hasOptEnd = true
			continue
		}
		codes = append(codes, int(k))
	}

	sort.Ints(codes)

	if hasOptAgentInfo {
		codes = append(codes, optAgentInfo)
	}
	if hasOptEnd {
		codes = append(codes, optEnd)
	}
	return codes
}

// Marshal writes options binary representations to b.
func (o Options) Marshal(b *uio.Lexer) {
	for _, c := range o.sortedKeys() {
		code := uint8(c)
		// Even if the End option is in there, don't marshal it until
		// the end.
		// Don't write padding either, since the options are sorted
		// it would always be written first which isn't useful
		if code == optEnd || code == optPad {
			continue
		}

		data := o[code]

		// Ensure even 0-length options are written out
		if len(data) == 0 {
			b.Write8(code)
			b.Write8(0)
			continue
		}
		// RFC 3396: If more than 256 bytes of data are given, the
		// option is simply listed multiple times.
		for len(data) > 0 {
			// 1 byte: option code
			b.Write8(code)

			n := len(data)
			if n > math.MaxUint8 {
				n = math.MaxUint8
			}

			// 1 byte: option length
			b.Write8(uint8(n))

			// N bytes: option data
			b.WriteBytes(data[:n])
			data = data[n:]
		}
	}
}

// String prints options using DHCP-specified option codes.
func (o Options) String() string {
	return o.ToString(dhcpHumanizer)
}

// Summary prints options in human-readable values.
//
// Summary uses vendorParser to interpret the OptionVendorSpecificInformation option.
func (o Options) Summary(vendorDecoder OptionDecoder) string {
	return o.ToString(OptionHumanizer{
		ValueHumanizer: parserFor(vendorDecoder),
		CodeHumanizer: func(c uint8) OptionCode {
			return optionCode(c)
		},
	})
}

// OptionParser gives a human-legible interpretation of data for the given option code.
type OptionParser func(code OptionCode, data []byte) fmt.Stringer

// OptionHumanizer is used to interpret a set of Options for their option code
// name and values.
//
// There should be separate OptionHumanizers for each Option "space": DHCP,
// BSDP, Relay Agent Info, and others.
type OptionHumanizer struct {
	ValueHumanizer OptionParser
	CodeHumanizer  func(code uint8) OptionCode
}

// Stringify returns a human-readable interpretation of the option code and its
// associated data.
func (oh OptionHumanizer) Stringify(code uint8, data []byte) string {
	c := oh.CodeHumanizer(code)
	val := oh.ValueHumanizer(c, data)
	return fmt.Sprintf("%s: %s", c, val)
}

// dhcpHumanizer humanizes the set of DHCP option codes.
var dhcpHumanizer = OptionHumanizer{
	ValueHumanizer: parseOption,
	CodeHumanizer: func(c uint8) OptionCode {
		return optionCode(c)
	},
}

// ToString uses parse to parse options into human-readable values.
func (o Options) ToString(humanizer OptionHumanizer) string {
	var ret string
	for _, c := range o.sortedKeys() {
		code := uint8(c)
		v := o[code]
		optString := humanizer.Stringify(code, v)
		// If this option has sub structures, offset them accordingly.
		if strings.Contains(optString, "\n") {
			optString = strings.Replace(optString, "\n  ", "\n      ", -1)
		}
		ret += fmt.Sprintf("    %v\n", optString)
	}
	return ret
}

func parseOption(code OptionCode, data []byte) fmt.Stringer {
	return parserFor(nil)(code, data)
}

func parserFor(vendorParser OptionDecoder) OptionParser {
	return func(code OptionCode, data []byte) fmt.Stringer {
		return getOption(code, data, vendorParser)
	}
}

// OptionDecoder can decode a byte stream into a human-readable option.
type OptionDecoder interface {
	fmt.Stringer
	FromBytes([]byte) error
}

func getOption(code OptionCode, data []byte, vendorDecoder OptionDecoder) fmt.Stringer {
	var d OptionDecoder
	switch code {
	case OptionRouter, OptionDomainNameServer, OptionNTPServers, OptionServerIdentifier:
		d = &IPs{}

	case OptionBroadcastAddress, OptionRequestedIPAddress:
		d = &IP{}

	case OptionClientSystemArchitectureType:
		d = &iana.Archs{}

	case OptionSubnetMask:
		d = &IPMask{}

	case OptionDHCPMessageType:
		var mt MessageType
		d = &mt

	case OptionParameterRequestList:
		d = &OptionCodeList{}

	case OptionHostName, OptionDomainName, OptionRootPath,
		OptionClassIdentifier, OptionTFTPServerName, OptionBootfileName,
		OptionMessage, OptionReferenceToTZDatabase:
		var s String
		d = &s

	case OptionRelayAgentInformation:
		d = &RelayOptions{}

	case OptionDNSDomainSearchList:
		d = &rfc1035label.Labels{}

	case OptionIPAddressLeaseTime, OptionRenewTimeValue,
		OptionRebindingTimeValue, OptionIPv6OnlyPreferred, OptionArpCacheTimeout,
		OptionTimeOffset:
		var dur Duration
		d = &dur

	case OptionMaximumDHCPMessageSize:
		var u Uint16
		d = &u

	case OptionUserClassInformation:
		var s Strings
		d = &s
		if s.FromBytes(data) != nil {
			var s String
			d = &s
		}

	case OptionAutoConfigure:
		var a AutoConfiguration
		d = &a

	case OptionVendorIdentifyingVendorClass:
		d = &VIVCIdentifiers{}

	case OptionVendorSpecificInformation:
		d = vendorDecoder

	case OptionClasslessStaticRoute:
		d = &Routes{}
	}
	if d != nil && d.FromBytes(data) == nil {
		return d
	}
	return OptionGeneric{data}
}
