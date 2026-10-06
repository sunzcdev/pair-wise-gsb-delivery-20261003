package pglogrepl

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// legalSamples returns well-formed messages built by the existing test data
// helpers. Every sample must parse without error.
func legalSamples(s *messageSuite) map[string][]byte {
	relation, _ := s.createRelationTestData()
	typeMsg, _ := s.createTypeTestData()
	insert, _ := s.createInsertTestData()
	updateK, _ := s.createUpdateTestDataTypeK()
	updateO, _ := s.createUpdateTestDataTypeO()
	updateN, _ := s.createUpdateTestDataWithoutOldTuple()
	deleteK, _ := s.createDeleteTestDataTypeK()
	deleteO, _ := s.createDeleteTestDataTypeO()
	truncate, _ := s.createTruncateTestData()

	logical := make([]byte, 1+1+8+5+4+5)
	logical[0] = 'M'
	s.putMessageTestData(logical[1:])

	begin := make([]byte, 1+8+8+4)
	begin[0] = 'B'
	bigEndian.PutUint64(begin[1:], 0x0102030405060708)
	bigEndian.PutUint64(begin[9:], 0x1122334455667788)
	bigEndian.PutUint32(begin[17:], 0xaabbccdd)

	commit := make([]byte, 1+1+8+8+8)
	commit[0] = 'C'
	bigEndian.PutUint64(commit[2:], 0x0102030405060708)
	bigEndian.PutUint64(commit[10:], 0x1122334455667788)
	bigEndian.PutUint64(commit[18:], 0x99aabbccddeeff00)

	origin := make([]byte, 1+8+len("someorigin")+1)
	origin[0] = 'O'
	bigEndian.PutUint64(origin[1:], 0x0102030405060708)
	s.putString(origin[9:], "someorigin")

	insertXid, _ := s.insertXid(insert)
	streamStart := make([]byte, 1+4+1)
	streamStart[0] = 'S'
	bigEndian.PutUint32(streamStart[1:], 0x11223344)
	streamStart[5] = 1
	streamStop := []byte{'E'}
	streamCommit := make([]byte, 1+4+1+8+8+8)
	streamCommit[0] = 'c'
	bigEndian.PutUint32(streamCommit[1:], 0x11223344)
	bigEndian.PutUint64(streamCommit[6:], 0x0102030405060708)
	bigEndian.PutUint64(streamCommit[14:], 0x1122334455667788)
	bigEndian.PutUint64(streamCommit[22:], 0x99aabbccddeeff00)
	streamAbort := make([]byte, 1+4+4)
	streamAbort[0] = 'A'
	bigEndian.PutUint32(streamAbort[1:], 0x11223344)
	bigEndian.PutUint32(streamAbort[5:], 0x55667788)

	return map[string][]byte{
		"Begin":          begin,
		"Commit":         commit,
		"Origin":         origin,
		"Relation":       relation,
		"Type":           typeMsg,
		"Insert":         insert,
		"UpdateK":        updateK,
		"UpdateO":        updateO,
		"UpdateN":        updateN,
		"DeleteK":        deleteK,
		"DeleteO":        deleteO,
		"Truncate":       truncate,
		"Logical":        logical,
		"InsertV2Stream": insertXid,
		"StreamStart":    streamStart,
		"StreamStop":     streamStop,
		"StreamCommit":   streamCommit,
		"StreamAbort":    streamAbort,
	}
}

func parseSample(name string, data []byte) (Message, error) {
	switch name {
	case "InsertV2Stream":
		return ParseV2(data, true)
	case "StreamStart", "StreamStop", "StreamCommit", "StreamAbort":
		return ParseV2(data, false)
	default:
		return Parse(data)
	}
}

// TestParseTruncatedSamples cuts every legal message byte by byte and makes
// sure each prefix returns an incomplete-input error instead of panicking.
func TestParseTruncatedSamples(t *testing.T) {
	s := new(messageSuite)
	s.SetT(t)
	samples := legalSamples(s)

	for name, msg := range samples {
		// Sanity check: the full sample is legal.
		_, err := parseSample(name, msg)
		require.NoError(t, err, "sample %q must parse", name)

		for cut := 0; cut < len(msg); cut++ {
			prefix := append([]byte(nil), msg[:cut]...)
			_, perr := parseSample(name, prefix)
			require.Errorf(t, perr, "truncated sample %q at %d bytes must error", name, cut)
			require.True(t, errors.Is(perr, errMsgLengthInvalid),
				"truncated sample %q at %d bytes must be classified as incomplete input, got %v", name, cut, perr)
		}
	}
}

// TestParseEmptyAndUnsupported checks message header handling: empty input is
// incomplete data while a present but unknown type is unsupported.
func TestParseEmptyAndUnsupported(t *testing.T) {
	for _, parse := range []func([]byte) (Message, error){
		func(data []byte) (Message, error) { return Parse(data) },
		func(data []byte) (Message, error) { return ParseV2(data, false) },
	} {
		_, err := parse(nil)
		require.Error(t, err)
		require.True(t, errors.Is(err, errMsgLengthInvalid), "nil input: %v", err)
		require.False(t, errors.Is(err, errMsgNotSupported))

		_, err = parse([]byte{})
		require.Error(t, err)
		require.True(t, errors.Is(err, errMsgLengthInvalid), "empty input: %v", err)

		_, err = parse([]byte{'X'})
		require.Error(t, err)
		require.True(t, errors.Is(err, errMsgNotSupported), "unknown type: %v", err)
		require.False(t, errors.Is(err, errMsgLengthInvalid))
	}
}

// TestParseResyncAfterBadMessages makes sure parsing a series of bad messages
// does not affect later parsing of a legal message.
func TestParseResyncAfterBadMessages(t *testing.T) {
	s := new(messageSuite)
	s.SetT(t)
	insert, expectedInsert := s.createInsertTestData()

	bad := [][]byte{
		nil,
		{},
		{'X'},
		{'I'},
		insert[:1+4+1], // header plus tuple type but no tuple data
		append([]byte(nil), insert[:len(insert)-1]...), // last byte cut off
		{'B', 0, 1, 2},                        // truncated begin body
		{'T', 0, 0, 0, 1, 0},                  // declares one relation, no IDs
		{'M', 1, 0, 0, 0, 0, 0, 0, 0, 0, 'p'}, // prefix without NUL
	}

	for _, data := range bad {
		_, err := Parse(data)
		require.Error(t, err)
	}

	m, err := Parse(insert)
	require.NoError(t, err)
	insertMsg, ok := m.(*InsertMessage)
	require.True(t, ok)
	require.Equal(t, expectedInsert, insertMsg)
}

// malformedSeeds returns targeted malformed inputs covering tampered column
// counts, untrusted lengths and unterminated strings.
func malformedSeeds() [][]byte {
	return [][]byte{
		nil,
		{},
		{'X'},
		{'B'},
		// Insert: tuple claims 1 column, but none follows.
		{'I', 0, 0, 0, 1, 'N', 0, 1},
		// Insert: tuple claims one text column whose 4-byte length is missing.
		{'I', 0, 0, 0, 1, 'N', 0, 1, 't'},
		// Insert: text column claims 100 bytes of data, only 2 present.
		{'I', 0, 0, 0, 1, 'N', 0, 1, 't', 0, 0, 0, 100, 'a', 'b'},
		// Insert: huge untrusted text length must not trigger allocation.
		{'I', 0, 0, 0, 1, 'N', 0, 1, 't', 0xff, 0xff, 0xff, 0xff},
		// Insert: unknown tuple column data type.
		{'I', 0, 0, 0, 1, 'N', 0, 1, 'z'},
		// Insert: huge column count.
		{'I', 0, 0, 0, 1, 'N', 0xff, 0xff},
		// Truncate: claims 0x40000000 relation IDs, none present.
		{'T', 0x40, 0, 0, 0, 0},
		// Relation: header only, no namespace terminator.
		{'R', 0, 0, 0, 1, 'p', 'u', 'b', 'l', 'i', 'c'},
		// Logical message: prefix without NUL.
		{'M', 1, 0, 0, 0, 0, 0, 0, 0, 0, 'p'},
		// Logical message: content length larger than the remaining bytes.
		{'M', 0, 0, 0, 0, 0, 0, 0, 0, 0, 'p', 0, 0, 0, 0, 10, 0},
		// Update K tuple followed by no N/new tuple.
		{'U', 0, 0, 0, 1, 'K', 0, 0},
		// Delete with unsupported tuple type.
		{'D', 0, 0, 0, 1, 'X'},
	}
}

func FuzzParse(f *testing.F) {
	s := new(messageSuite)
	for _, msg := range legalSamples(s) {
		f.Add(msg)
	}
	for _, seed := range malformedSeeds() {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := Parse(data)
		if err != nil {
			require.Nil(t, m)
			if len(data) == 0 {
				require.True(t, errors.Is(err, errMsgLengthInvalid))
			}
			if len(data) > 0 {
				switch MessageType(data[0]) {
				case MessageTypeBegin, MessageTypeCommit, MessageTypeOrigin, MessageTypeRelation,
					MessageTypeType, MessageTypeInsert, MessageTypeUpdate, MessageTypeDelete,
					MessageTypeTruncate, MessageTypeMessage:
					require.False(t, errors.Is(err, errMsgNotSupported),
						"known message type must not be reported unsupported: %v", err)
				default:
					require.True(t, errors.Is(err, errMsgNotSupported))
				}
			}
			return
		}
		require.NotNil(t, m)
	})
}

func FuzzParseV2(f *testing.F) {
	s := new(messageSuite)
	for name, msg := range legalSamples(s) {
		if name == "InsertV2Stream" {
			f.Add(msg, true)
		} else {
			f.Add(msg, false)
		}
	}
	for _, seed := range malformedSeeds() {
		f.Add(seed, false)
	}

	f.Fuzz(func(t *testing.T, data []byte, inStream bool) {
		m, err := ParseV2(data, inStream)
		if err != nil {
			require.Nil(t, m)
			if len(data) == 0 {
				require.True(t, errors.Is(err, errMsgLengthInvalid))
			}
			return
		}
		require.NotNil(t, m)
	})
}
