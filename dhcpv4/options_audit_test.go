package dhcpv4

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildAuditPacket assembles a minimal packet whose sname, file and options
// areas are replaced with the given raw bytes. sname and file must already be
// the canonical sizes (64 and 128 respectively).
func buildAuditPacket(t *testing.T, sname, file, opts []byte) []byte {
	t.Helper()
	require.Len(t, sname, 64)
	require.Len(t, file, 128)

	pkt := make([]byte, optionsOffset+4)
	copy(pkt[snameOffset:snameOffset+64], sname)
	copy(pkt[fileOffset:fileOffset+128], file)
	copy(pkt[optionsOffset:optionsOffset+4], magicCookie[:])
	return append(pkt, opts...)
}

func TestAuditBasicFragments(t *testing.T) {
	// pad, two distinct options and end.
	data := []byte{
		0, // pad
		10, 2, 255, 254,
		11, 1, 7,
		255,
	}

	opts := make(Options)
	audit, err := opts.AuditFromBytes(data)
	require.NoError(t, err)
	require.True(t, audit.Complete)

	require.Equal(t, Options{
		10: {255, 254},
		11: {7},
	}, audit.Options)

	require.Len(t, audit.Fragments, 4)
	require.Equal(t, OptionFragment{
		Code: 0, Kind: FragmentPad, Field: FieldOptions,
		Offset: 0, PacketOffset: 0, Order: 0, Part: 0,
	}, audit.Fragments[0])
	require.Equal(t, OptionFragment{
		Code: 255, Kind: FragmentEnd, Field: FieldOptions,
		Offset: 8, PacketOffset: 8, Order: 3, Part: 0,
	}, audit.Fragments[3])

	opt10 := audit.Fragments[1]
	require.Equal(t, uint8(10), opt10.Code)
	require.Equal(t, FragmentOption, opt10.Kind)
	require.Equal(t, []byte{255, 254}, opt10.Value)
	require.Equal(t, 1, opt10.Offset)
	require.Equal(t, 0, opt10.Part)
	require.Nil(t, audit.Trailing)
}

func TestAuditWireOrderIsNotSerializedOrder(t *testing.T) {
	// High code appears before low code on the wire; Order must preserve
	// the raw sequence rather than the sorted map order.
	data := []byte{
		100, 1, 1,
		3, 1, 2,
		255,
	}
	audit, err := make(Options).AuditFromBytes(data)
	require.NoError(t, err)

	require.Equal(t, []uint8{100, 3, 255}, []uint8{
		audit.Fragments[0].Code,
		audit.Fragments[1].Code,
		audit.Fragments[2].Code,
	})
	require.Equal(t, 0, audit.Fragments[0].Order)
	require.Equal(t, 1, audit.Fragments[1].Order)
	require.Equal(t, 2, audit.Fragments[2].Order)
}

func TestAuditDuplicatePartsAcrossOverload(t *testing.T) {
	// Options area: overload=3, first half of option 10, pad.
	optsArea := []byte{
		52, 1, 3, // option overload = file + sname
		10, 2, 1, 2,
		0, // pad
		255,
	}

	// File area: second half of option 10, an unknown option, then end
	// followed by two trailing bytes.
	file := make([]byte, 128)
	fileOpts := []byte{
		10, 2, 3, 4,
		200, 1, 9, // unknown option
		255,
		0xAA, 0xBB, // trailing bytes after End
	}
	copy(file, fileOpts)

	// Sname area (parsed last per RFC 2131): third half of option 10, end.
	sname := make([]byte, 64)
	snameOpts := []byte{
		10, 1, 5,
		255,
	}
	copy(sname, snameOpts)

	pkt := buildAuditPacket(t, sname, file, optsArea)

	p, audit, err := FromBytesWithAudit(pkt)
	require.NoError(t, err)
	require.True(t, audit.Complete)

	// Merged value concatenates all three parts in wire order.
	require.Equal(t, []byte{1, 2, 3, 4, 5}, p.Options.Get(GenericOptionCode(10)))

	// Overloaded legacy fields no longer hold strings.
	require.Empty(t, p.ServerHostName)
	require.Empty(t, p.BootFileName)

	var parts []OptionFragment
	var unknowns []OptionFragment
	var pads []OptionFragment
	var ends []OptionFragment
	for _, f := range audit.Fragments {
		switch {
		case f.Code == 0:
			pads = append(pads, f)
		case f.Code == 255:
			ends = append(ends, f)
		case f.Code == 10:
			parts = append(parts, f)
		case f.Code == 200:
			unknowns = append(unknowns, f)
		}
	}

	require.Len(t, parts, 3)
	require.Equal(t, []byte{1, 2}, parts[0].Value)
	require.Equal(t, []byte{3, 4}, parts[1].Value)
	require.Equal(t, []byte{5}, parts[2].Value)
	for i, f := range parts {
		require.Equal(t, i, f.Part, "part index of fragment %d", i)
	}
	require.Equal(t, FieldOptions, parts[0].Field)
	require.Equal(t, FieldFile, parts[1].Field)
	require.Equal(t, FieldSName, parts[2].Field)

	// Offsets are relative to the field and absolute within the packet.
	require.Equal(t, 3, parts[0].Offset)
	require.Equal(t, optionsOffset+3, parts[0].PacketOffset)
	require.Equal(t, 0, parts[1].Offset)
	require.Equal(t, fileOffset, parts[1].PacketOffset)
	require.Equal(t, 0, parts[2].Offset)
	require.Equal(t, snameOffset, parts[2].PacketOffset)

	require.Len(t, unknowns, 1)
	require.Equal(t, FieldFile, unknowns[0].Field)
	require.Equal(t, []byte{9}, unknowns[0].Value)

	require.Len(t, pads, 1)
	require.Equal(t, FieldOptions, pads[0].Field)
	require.Nil(t, pads[0].Value)

	// Exactly three End markers (options, file, sname).
	require.Len(t, ends, 3)

	// The file-field End fragment is present at offset 7.
	var fileEnd *OptionFragment
	for i := range audit.Fragments {
		if audit.Fragments[i].Code == 255 && audit.Fragments[i].Field == FieldFile {
			fileEnd = &audit.Fragments[i]
		}
	}
	require.NotNil(t, fileEnd)
	require.Equal(t, 7, fileEnd.Offset)

	// Trailing bytes after the file End are preserved verbatim,
	// field-attributed, including the pad zeros that fill the fixed field.
	fileTrailing := audit.Trailing[FieldFile]
	require.Len(t, fileTrailing, 128-8)
	require.Equal(t, []byte{0xAA, 0xBB}, fileTrailing[:2])
	_, hasOptionsTrailing := audit.Trailing[FieldOptions]
	require.False(t, hasOptionsTrailing)
}

func TestAuditZeroLengthOption(t *testing.T) {
	data := []byte{
		80, 0,
		255,
	}
	audit, err := make(Options).AuditFromBytes(data)
	require.NoError(t, err)
	require.True(t, audit.Complete)

	require.Contains(t, audit.Options, uint8(80))
	require.Empty(t, audit.Options[80])

	f := audit.Fragments[0]
	require.Equal(t, uint8(80), f.Code)
	require.NotNil(t, f.Value)
	require.Len(t, f.Value, 0)
}

func TestAuditEndTrailingStandalone(t *testing.T) {
	data := []byte{
		10, 1, 2,
		255,
		7, 8, 9,
	}
	audit, err := make(Options).AuditFromBytes(data)
	require.NoError(t, err)
	require.True(t, audit.Complete)

	require.Equal(t, []byte{7, 8, 9}, audit.Trailing[FieldOptions])
	// Last real fragment is End; nothing after it is decoded as an option.
	require.Equal(t, uint8(255), audit.Fragments[len(audit.Fragments)-1].Code)
}

func TestAuditTruncatedValue(t *testing.T) {
	// Option 10 declares 4 bytes but supplies only 2.
	data := []byte{
		10, 4, 1, 2,
	}
	audit, err := make(Options).AuditFromBytes(data)
	require.Error(t, err)
	require.Contains(t, err.Error(), "option code 10")
	require.Contains(t, err.Error(), "length 4")
	require.Contains(t, err.Error(), "only 2 value bytes remain")
	require.False(t, audit.Complete)

	// Prefix diagnosis: the fragment is recorded but its truncated value
	// is not fabricated, and the logical options must not claim 10.
	require.Len(t, audit.Fragments, 1)
	require.Equal(t, uint8(10), audit.Fragments[0].Code)
	require.Equal(t, []byte{1, 2}, audit.Fragments[0].Value)
	require.NotContains(t, audit.Options, uint8(10))
}

func TestAuditTruncatedLengthByte(t *testing.T) {
	// Option 11 begins but its length byte never arrives.
	data := []byte{10, 1, 9, 11}
	audit, err := make(Options).AuditFromBytes(data)
	require.Error(t, err)
	require.Contains(t, err.Error(), "length byte is missing")
	require.False(t, audit.Complete)

	// Prefix fragment 10 is fully available, and the headerless option 11
	// is retained as evidence with an empty value (never merged).
	require.Len(t, audit.Fragments, 2)
	require.Equal(t, uint8(10), audit.Fragments[0].Code)
	require.Equal(t, []byte{9}, audit.Fragments[0].Value)
	require.Equal(t, uint8(11), audit.Fragments[1].Code)
	require.NotNil(t, audit.Fragments[1].Value)
	require.Empty(t, audit.Fragments[1].Value)
	require.NotContains(t, audit.Options, uint8(11))
}

func TestAuditTruncatedPacket(t *testing.T) {
	// Full packet whose last option is short; audit must return prefix
	// evidence and a precise error rather than claiming completeness.
	optsArea := []byte{
		10, 2, 1, 2,
		11, 3, 9, // declares 3, provides 1
	}
	pkt := buildAuditPacket(t, make([]byte, 64), make([]byte, 128), optsArea)

	p, audit, err := FromBytesWithAudit(pkt)
	require.Error(t, err)
	require.False(t, audit.Complete)
	require.NotNil(t, p)
	require.Equal(t, []byte{1, 2}, p.Options.Get(GenericOptionCode(10)))
	require.NotContains(t, p.Options, uint8(11))

	// The short option 11 is kept as evidence with the bytes present, but
	// it is not merged into the logical options.
	require.Len(t, audit.Fragments, 2)
	require.Equal(t, uint8(10), audit.Fragments[0].Code)
	require.Equal(t, uint8(11), audit.Fragments[1].Code)
	require.Equal(t, []byte{9}, audit.Fragments[1].Value)
}

func TestAuditMissingEnd(t *testing.T) {
	// No End and no overload: packet path reports UnexpectedEOF while the
	// standalone field audit is lenient but reports Complete=false.
	optsArea := []byte{10, 1, 2}
	pkt := buildAuditPacket(t, make([]byte, 64), make([]byte, 128), optsArea)

	_, _, err := FromBytesWithAudit(pkt)
	require.Error(t, err)
	require.True(t, errors.Is(err, io.ErrUnexpectedEOF))

	audit, ferr := make(Options).AuditFromBytes(optsArea)
	require.NoError(t, ferr)
	require.False(t, audit.Complete)
}

func TestAuditEmptyOptions(t *testing.T) {
	pkt := buildAuditPacket(t, make([]byte, 64), make([]byte, 128), nil)
	p, audit, err := FromBytesWithAudit(pkt)
	require.NoError(t, err)
	require.False(t, audit.Complete) // no End marker present
	require.Empty(t, p.Options)
	require.Empty(t, audit.Fragments)
}

func TestAuditEvidenceStableAfterBufferReuse(t *testing.T) {
	data := []byte{
		10, 3, 1, 2, 3,
		255,
		0xDD, // trailing
	}
	audit, err := make(Options).AuditFromBytes(data)
	require.NoError(t, err)

	// Reuse / overwrite the input buffer.
	for i := range data {
		data[i] = 0xFF
	}

	require.Equal(t, []byte{1, 2, 3}, audit.Fragments[0].Value)
	require.Equal(t, []byte{0xDD}, audit.Trailing[FieldOptions])

	// The retained value must not share capacity with the reused input.
	tampered := append([]byte(nil), audit.Fragments[0].Value...)
	_ = append(data[:0], 9, 9, 9, 9, 9, 9, 9, 9)
	require.Equal(t, tampered, audit.Fragments[0].Value)
}

func TestAuditFieldNames(t *testing.T) {
	require.Equal(t, "options", FieldOptions.String())
	require.Equal(t, "file", FieldFile.String())
	require.Equal(t, "sname", FieldSName.String())
	require.Equal(t, "option", FragmentOption.String())
	require.Equal(t, "pad", FragmentPad.String())
	require.Equal(t, "end", FragmentEnd.String())
}

func TestAuditMergedOptionsMatchFromBytes(t *testing.T) {
	// A normal packet (no overload) with duplicates of option 10, a pad,
	// an unknown code and an End. The audit path must merge identically to
	// the standard decoder.
	optsArea := []byte{
		10, 2, 1, 2,
		0,
		10, 1, 3,
		123, 1, 99, // unknown code
		53, 1, 3,
		255,
	}
	pkt := buildAuditPacket(t, make([]byte, 64), make([]byte, 128), optsArea)

	standard, err := FromBytes(pkt)
	require.NoError(t, err)

	audited, audit, err := FromBytesWithAudit(pkt)
	require.NoError(t, err)
	require.True(t, audit.Complete)

	require.Equal(t, standard.Options, audited.Options)
	require.Equal(t, []byte{1, 2, 3}, audited.Options.Get(GenericOptionCode(10)))
	require.Equal(t, []byte{3}, audited.Options.Get(OptionDHCPMessageType))
}
