package dhcpv4

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildAuditPacket assembles a minimal DHCPv4 packet: the 240-byte fixed
// header (ending with the magic cookie), followed by the given options, and
// with sname/file populated from the supplied (option-encoded) contents.
func buildAuditPacket(t *testing.T, sname, file, opts []byte) []byte {
	t.Helper()
	q := make([]byte, 240)
	copy(q[236:], magicCookie[:])
	require.LessOrEqual(t, len(sname), 64)
	require.LessOrEqual(t, len(file), 128)
	copy(q[44:], sname)
	copy(q[108:], file)
	return append(q, opts...)
}

func opts60(v byte) []byte { return []byte{60, 1, v} }

func TestAuditPreservesRepeatsPadEndUnknownAndTrailing(t *testing.T) {
	opts := []byte{
		0x00,       // pad at relative offset 0
		60, 1, 'A', // vendor class fragment 1
		0x00,       // pad
		60, 1, 'B', // vendor class fragment 2
		240, 2, 0xaa, 0xbb, // unknown option
		99, 0, // zero-length option
		0xff,       // end
		0xde, 0xad, // trailing garbage after End
	}
	q := buildAuditPacket(t, nil, nil, opts)

	p, audit, err := AuditFromBytes(q)
	require.NoError(t, err)
	require.NotNil(t, p)
	require.True(t, audit.Complete)

	// Logical, merged value still sees concatenated data exactly once.
	require.Equal(t, []byte("AB"), audit.Options[60])
	require.Equal(t, []byte("AB"), p.GetOneOption(GenericOptionCode(60)))
	require.Equal(t, []byte{0xaa, 0xbb}, audit.Options[240])

	// Occurrence count is recoverable even though the map merged the value.
	require.Equal(t, 2, audit.Occurrences(60))
	require.Equal(t, 1, audit.Occurrences(240))

	// Raw fragments preserve original order and structural bytes.
	require.Len(t, audit.Fragments, 7)
	kinds := []FragmentKind{
		FragmentPad, FragmentOption, FragmentPad, FragmentOption,
		FragmentOption, FragmentOption, FragmentEnd,
	}
	for i, want := range kinds {
		require.Equal(t, want, audit.Fragments[i].Kind, "fragment %d", i)
		require.Equal(t, i, audit.Fragments[i].Order)
	}

	first := audit.Fragments[1]
	require.Equal(t, uint8(60), first.Code)
	require.Equal(t, FieldOptions, first.Field)
	require.Equal(t, 1, first.Offset)
	require.Equal(t, 241, first.PacketOffset)
	require.Equal(t, []byte("A"), first.Value)
	require.Equal(t, 0, first.IndexInGroup)

	second := audit.Fragments[3]
	require.Equal(t, []byte("B"), second.Value)
	require.Equal(t, first.Group, second.Group, "repeated option shares a group")
	require.Equal(t, 1, second.IndexInGroup)

	unknown := audit.Fragments[4]
	require.Equal(t, uint8(240), unknown.Code)
	require.Equal(t, []byte{0xaa, 0xbb}, unknown.Value)

	zero := audit.Fragments[5]
	require.Equal(t, uint8(99), zero.Code)
	require.Equal(t, 0, zero.Length)
	require.Len(t, zero.Value, 0)

	end := audit.Fragments[6]
	require.Equal(t, FieldOptions, end.Field)
	require.Equal(t, 14, end.Offset)
	require.Equal(t, 254, end.PacketOffset)

	// End position and bytes following it are evidence, not silently dropped.
	require.Len(t, audit.Fields, 1)
	ev := audit.Fields[0]
	require.Equal(t, 14, ev.EndOffset)
	require.Equal(t, 15, ev.TrailingOffset)
	require.Equal(t, []byte{0xde, 0xad}, ev.Trailing)
}

func TestAuditOverloadSplitsAcrossFields(t *testing.T) {
	mainOpts := []byte{
		53, 1, 1, // message type = discover
		52, 1, 3, // overload = file + sname
		60, 1, 'X',
		0xff,
	}
	file := append([]byte{
		60, 1, 'Y',
		240, 1, 0xcc, // unknown option only present in file
		0xff,
	}, make([]byte, 120)...)
	sname := append([]byte{
		60, 1, 'Z',
		0xff,
	}, make([]byte, 56)...)
	q := buildAuditPacket(t, sname, file, mainOpts)

	p, audit, err := AuditFromBytes(q)
	require.NoError(t, err)
	require.True(t, audit.Complete)

	// Concatenation follows RFC 2132 parse order: options, then file, sname.
	require.Equal(t, []byte("XYZ"), audit.Options[60])
	require.Equal(t, []byte("XYZ"), p.GetOneOption(GenericOptionCode(60)))
	require.Equal(t, MessageTypeDiscover, p.MessageType())

	// Overloaded fields no longer claim to be hostname/bootfile strings.
	require.Empty(t, p.BootFileName)
	require.Empty(t, p.ServerHostName)

	fr := audit.FragmentsFor(60)
	require.Len(t, fr, 3)
	require.Equal(t, []byte("X"), fr[0].Value)
	require.Equal(t, FieldOptions, fr[0].Field)
	require.Equal(t, 0, fr[0].IndexInGroup)
	require.Equal(t, []byte("Y"), fr[1].Value)
	require.Equal(t, FieldFile, fr[1].Field)
	require.Equal(t, 108, fr[1].PacketOffset) // file starts at packet byte 108
	require.Equal(t, 1, fr[1].IndexInGroup)
	require.Equal(t, []byte("Z"), fr[2].Value)
	require.Equal(t, FieldSname, fr[2].Field)
	require.Equal(t, 44, fr[2].PacketOffset) // sname starts at packet byte 44
	require.Equal(t, 2, fr[2].IndexInGroup)
	require.Equal(t, fr[0].Group, fr[1].Group)
	require.Equal(t, fr[1].Group, fr[2].Group)

	// Observation order is monotonically increasing across all fields.
	for i := 1; i < len(audit.Fragments); i++ {
		require.Greater(t, audit.Fragments[i].Order, audit.Fragments[i-1].Order)
	}

	// Unknown option in an overloaded field is kept and attributed correctly.
	var foundUnknown bool
	for _, f := range audit.Fragments {
		if f.Code == 240 {
			foundUnknown = true
			require.Equal(t, FieldFile, f.Field)
			require.Equal(t, []byte{0xcc}, f.Value)
		}
	}
	require.True(t, foundUnknown)

	// Every scanned field has its own End evidence.
	require.Len(t, audit.Fields, 3)
	require.Equal(t, FieldOptions, audit.Fields[0].Field)
	require.Equal(t, FieldFile, audit.Fields[1].Field)
	require.Equal(t, 6, audit.Fields[1].EndOffset)
	require.Equal(t, FieldSname, audit.Fields[2].Field)
	require.Equal(t, 3, audit.Fields[2].EndOffset)
}

func TestAuditTruncatedValueReturnsPrefix(t *testing.T) {
	opts := []byte{
		0x00,            // pad before the broken option
		60, 5, 'A', 'B', // declares 5 bytes, only 2 available
	}
	q := buildAuditPacket(t, nil, nil, opts)

	p, audit, err := AuditFromBytes(q)
	require.Error(t, err)
	require.NotNil(t, p)
	require.False(t, audit.Complete)

	var terr *TruncatedOptionError
	require.True(t, errors.As(err, &terr))
	require.Equal(t, uint8(60), terr.Code)
	require.Equal(t, FieldOptions, terr.Field)
	require.True(t, terr.HasLength)
	require.Equal(t, 5, terr.DeclaredLength)
	require.Equal(t, 2, terr.Available)
	require.Equal(t, 1, terr.Offset)
	require.Equal(t, 241, terr.PacketOffset)

	// Usable prefix diagnosis: pad plus the truncated fragment survive.
	require.Equal(t, []byte("AB"), audit.Options[60])
	require.Len(t, audit.Fragments, 2)
	require.Equal(t, FragmentPad, audit.Fragments[0].Kind)
	frag := audit.Fragments[1]
	require.Equal(t, FragmentOption, frag.Kind)
	require.True(t, frag.Truncated)
	require.Equal(t, []byte("AB"), frag.Value)
	require.Equal(t, 5, frag.Length)

	require.Equal(t, -1, audit.Fields[0].EndOffset)
}

func TestAuditTruncatedMissingLengthByte(t *testing.T) {
	q := buildAuditPacket(t, nil, nil, []byte{60})

	_, audit, err := AuditFromBytes(q)
	require.Error(t, err)
	require.False(t, audit.Complete)

	var terr *TruncatedOptionError
	require.True(t, errors.As(err, &terr))
	require.Equal(t, uint8(60), terr.Code)
	require.False(t, terr.HasLength)
	require.Equal(t, 0, terr.Offset)

	require.Len(t, audit.Fragments, 1)
	require.Equal(t, uint8(60), audit.Fragments[0].Code)
	require.True(t, audit.Fragments[0].Truncated)
}

func TestAuditMissingEndIsNotComplete(t *testing.T) {
	q := buildAuditPacket(t, nil, nil, []byte{60, 1, 'A'})

	_, audit, err := AuditFromBytes(q)
	require.Error(t, err)
	require.True(t, errors.Is(err, io.ErrUnexpectedEOF))
	require.False(t, audit.Complete)
	require.Equal(t, []byte("A"), audit.Options[60])
	require.Equal(t, -1, audit.Fields[0].EndOffset)
}

func TestAuditEvidenceSurvivesBufferReuse(t *testing.T) {
	opts := []byte{
		60, 1, 'A',
		0xff,
		0x11, 0x22, // trailing after End
	}
	q := buildAuditPacket(t, nil, nil, opts)

	_, audit, err := AuditFromBytes(q)
	require.NoError(t, err)

	gotValue := append([]byte(nil), audit.FragmentsFor(60)[0].Value...)
	gotTrailing := append([]byte(nil), audit.Fields[0].Trailing...)
	gotMerged := append([]byte(nil), audit.Options[60]...)

	// Simulate the network buffer being recycled with unrelated data.
	for i := range q {
		q[i] = 0xff
	}

	require.Equal(t, []byte("A"), gotValue)
	require.Equal(t, []byte{0x11, 0x22}, gotTrailing)
	require.Equal(t, []byte("A"), gotMerged)
	require.Equal(t, []byte("A"), audit.FragmentsFor(60)[0].Value)
	require.Equal(t, []byte{0x11, 0x22}, audit.Fields[0].Trailing)
	require.Equal(t, []byte("A"), audit.Options[60])
}

func TestAuditHeaderFailure(t *testing.T) {
	p, audit, err := AuditFromBytes(make([]byte, 10))
	require.Error(t, err)
	require.Nil(t, p)
	require.Nil(t, audit)
}

func TestFieldAndKindStrings(t *testing.T) {
	require.Equal(t, "options", FieldOptions.String())
	require.Equal(t, "file", FieldFile.String())
	require.Equal(t, "sname", FieldSname.String())
	require.Contains(t, OptionField(9).String(), "unknown-field")

	require.Equal(t, "pad", FragmentPad.String())
	require.Equal(t, "end", FragmentEnd.String())
	require.Equal(t, "option", FragmentOption.String())
}
