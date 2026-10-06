package pglogrepl

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// validMessageSamples builds one valid wire sample (including the leading
// message-type byte) for every message supported by Parse.
func validMessageSamples() map[string][]byte {
	s := &messageSuite{}

	samples := map[string][]byte{}

	begin := s.beginSample()
	samples["begin"] = begin
	samples["commit"] = s.commitSample()
	samples["origin"] = s.originSample()

	relation, _ := s.createRelationTestData()
	samples["relation"] = relation
	typeMsg, _ := s.createTypeTestData()
	samples["type"] = typeMsg
	insert, _ := s.createInsertTestData()
	samples["insert"] = insert
	update, _ := s.createUpdateTestDataTypeK()
	samples["update"] = update
	updateO, _ := s.createUpdateTestDataTypeO()
	samples["updateO"] = updateO
	updateNone, _ := s.createUpdateTestDataWithoutOldTuple()
	samples["updateNone"] = updateNone
	del, _ := s.createDeleteTestDataTypeK()
	samples["delete"] = del
	delO, _ := s.createDeleteTestDataTypeO()
	samples["deleteO"] = delO
	truncate, _ := s.createTruncateTestData()
	samples["truncate"] = truncate
	samples["message"] = s.logicalMessageSample(false)

	return samples
}

type v2Sample struct {
	data     []byte
	inStream bool
}

// validMessageV2Samples builds valid protocol v2 wire samples.
func validMessageV2Samples() map[string]v2Sample {
	s := &messageSuite{}

	samples := map[string]v2Sample{}
	put := func(name string, data []byte, inStream bool) {
		samples[name] = v2Sample{data: data, inStream: inStream}
	}

	put("streamStart", s.streamStartSample(), false)
	put("streamStop", []byte{'E'}, false)
	put("streamCommit", s.streamCommitSample(), false)
	put("streamAbort", s.streamAbortSample(), false)
	put("messageNoStream", s.logicalMessageSample(false), false)
	put("messageInStream", s.logicalMessageSample(true), true)

	rel, _ := s.createRelationTestData()
	put("relationNoStream", rel, false)
	relV2, _ := s.insertXid(rel)
	put("relationInStream", relV2, true)

	ins, _ := s.createInsertTestData()
	insV2, _ := s.insertXid(ins)
	put("insertInStream", insV2, true)

	upd, _ := s.createUpdateTestDataTypeK()
	updV2, _ := s.insertXid(upd)
	put("updateInStream", updV2, true)

	del, _ := s.createDeleteTestDataTypeK()
	delV2, _ := s.insertXid(del)
	put("deleteInStream", delV2, true)

	tr, _ := s.createTruncateTestData()
	trV2, _ := s.insertXid(tr)
	put("truncateInStream", trV2, true)

	return samples
}

func putBE32(dst []byte, v uint32) { binary.BigEndian.PutUint32(dst, v) }
func putBE64(dst []byte, v uint64) { binary.BigEndian.PutUint64(dst, v) }

// beginSample builds 'B' + finalLSN(8) + commitTime(8) + xid(4).
func (s *messageSuite) beginSample() []byte {
	msg := make([]byte, 1+8+8+4)
	msg[0] = 'B'
	putBE64(msg[1:], 0x0102030405060708)
	putBE64(msg[9:], uint64(timeToPgTime(fixedTime())))
	putBE32(msg[17:], 42)
	return msg
}

func (s *messageSuite) commitSample() []byte {
	msg := make([]byte, 1+1+8+8+8)
	msg[0] = 'C'
	msg[1] = 0
	putBE64(msg[2:], 100)
	putBE64(msg[10:], 200)
	putBE64(msg[18:], uint64(timeToPgTime(fixedTime())))
	return msg
}

func (s *messageSuite) originSample() []byte {
	name := "someorigin"
	msg := make([]byte, 1+8+len(name)+1)
	msg[0] = 'O'
	putBE64(msg[1:], 300)
	s.putString(msg[9:], name)
	return msg
}

func (s *messageSuite) streamStartSample() []byte {
	msg := make([]byte, 1+4+1)
	msg[0] = 'S'
	putBE32(msg[1:], 7)
	msg[5] = 1
	return msg
}

func (s *messageSuite) streamCommitSample() []byte {
	msg := make([]byte, 1+4+1+8+8+8)
	msg[0] = 'c'
	putBE32(msg[1:], 7)
	msg[5] = 0
	putBE64(msg[6:], 100)
	putBE64(msg[14:], 200)
	putBE64(msg[22:], uint64(timeToPgTime(fixedTime())))
	return msg
}

func (s *messageSuite) streamAbortSample() []byte {
	msg := make([]byte, 1+4+4)
	msg[0] = 'A'
	putBE32(msg[1:], 7)
	putBE32(msg[5:], 9)
	return msg
}

// logicalMessageSample builds an 'M' message, optionally prefixed with the
// 4-byte XID used inside a v2 stream.
func (s *messageSuite) logicalMessageSample(inStream bool) []byte {
	prefix := "test"
	content := "hello"
	msg := make([]byte, 1+1+8+len(prefix)+1+4+len(content))
	msg[0] = 'M'
	off := 1
	msg[off] = 1
	off++
	putBE64(msg[off:], 500)
	off += 8
	off += s.putString(msg[off:], prefix)
	putBE32(msg[off:], uint32(len(content)))
	off += 4
	copy(msg[off:], content)

	if inStream {
		out := make([]byte, 1+4+len(msg)-1)
		out[0] = 'M'
		putBE32(out[1:], 7)
		copy(out[5:], msg[1:])
		return out
	}
	return msg
}

func fixedTime() time.Time {
	return time.Date(2023, time.January, 2, 3, 4, 5, 0, time.UTC)
}

// assertNoPanic runs fn and fails the test if it panics. Parsing untrusted
// network data must surface errors, not crash the process.
func assertNoPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	fn()
}

// TestParseByteTruncations feeds every prefix length of each valid v1 sample.
// No prefix may panic, and every truncated message must be reported as an
// incomplete message rather than an unsupported type.
func TestParseByteTruncations(t *testing.T) {
	for name, sample := range validMessageSamples() {
		sample := sample
		t.Run(name, func(t *testing.T) {
			for n := 0; n < len(sample); n++ {
				truncated := sample[:n]
				msg, err := Parse(truncated)
				if err == nil {
					t.Fatalf("Parse(%d/%d bytes) succeeded unexpectedly: %#v", n, len(sample), msg)
				}
				if !errors.Is(err, errMsgLength) {
					t.Fatalf("Parse(%d/%d bytes) error = %v, want errMsgLength", n, len(sample), err)
				}
				if msg != nil {
					t.Fatalf("Parse(%d/%d bytes) returned a message with an error: %#v", n, len(sample), msg)
				}
			}
		})
	}
}

// TestParseV2ByteTruncations does the same for protocol v2 samples, including
// the extra XID prefix in inStream mode.
func TestParseV2ByteTruncations(t *testing.T) {
	for name, sample := range validMessageV2Samples() {
		sample := sample
		t.Run(name, func(t *testing.T) {
			for n := 0; n < len(sample.data); n++ {
				truncated := sample.data[:n]
				msg, err := ParseV2(truncated, sample.inStream)
				if err == nil {
					t.Fatalf("ParseV2(%d/%d bytes, inStream=%v) succeeded unexpectedly: %#v",
						n, len(sample.data), sample.inStream, msg)
				}
				if !errors.Is(err, errMsgLength) {
					t.Fatalf("ParseV2(%d/%d bytes, inStream=%v) error = %v, want errMsgLength",
						n, len(sample.data), sample.inStream, err)
				}
				if msg != nil {
					t.Fatalf("ParseV2(%d/%d bytes) returned a message with an error: %#v",
						n, len(sample.data), msg)
				}
			}
		})
	}
}

// TestParseEmptyAndUnknown checks the message header handling: empty input is
// incomplete, a complete but unknown type is unsupported.
func TestParseEmptyAndUnknown(t *testing.T) {
	if _, err := Parse(nil); !errors.Is(err, errMsgLength) {
		t.Fatalf("Parse(nil) error = %v, want errMsgLength", err)
	}
	if _, err := Parse([]byte{}); !errors.Is(err, errMsgLength) {
		t.Fatalf("Parse(empty) error = %v, want errMsgLength", err)
	}

	for _, data := range [][]byte{{'Z'}, {'X'}, {0}} {
		if _, err := Parse(data); !errors.Is(err, errMsgNotSupported) {
			t.Fatalf("Parse(%v) error = %v, want errMsgNotSupported", data, err)
		}
		if _, err := ParseV2(data, false); !errors.Is(err, errMsgNotSupported) {
			t.Fatalf("ParseV2(%v) error = %v, want errMsgNotSupported", data, err)
		}
	}

	if _, err := ParseV2(nil, false); !errors.Is(err, errMsgLength) {
		t.Fatalf("ParseV2(nil) error = %v, want errMsgLength", err)
	}
	if _, err := ParseV2(nil, true); !errors.Is(err, errMsgLength) {
		t.Fatalf("ParseV2(nil, true) error = %v, want errMsgLength", err)
	}
}

func be16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func be32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }

func cat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// malformedParseCases holds fuzz seeds with bogus column/relation counts and
// bogus field/tuple lengths. Each must be rejected without panic.
func malformedParseCases() [][]byte {
	cases := [][]byte{
		// Insert: 5 columns claimed, none supplied.
		cat([]byte{'I'}, be32(1), []byte{'N'}, be16(5)),
		// Insert: text column whose length exceeds the remaining bytes.
		cat([]byte{'I'}, be32(1), []byte{'N'}, be16(1), []byte{'t'}, be32(1000)),
		// Insert: huge length that would exhaust memory if trusted.
		cat([]byte{'I'}, be32(1), []byte{'N'}, be16(1), []byte{'t'}, be32(0xffffffff)),
		// Insert: only the length header, no payload.
		cat([]byte{'I'}, be32(1), []byte{'N'}, be16(1), []byte{'t'}, be32(4)),
		// Insert: unknown column data type marker.
		cat([]byte{'I'}, be32(1), []byte{'N'}, be16(1), []byte{'z'}),
		// Insert: column count but missing data type byte.
		cat([]byte{'I'}, be32(1), []byte{'N'}, be16(1)),
		// Update 'K' tuple with bogus length, missing following 'N'.
		cat([]byte{'U'}, be32(1), []byte{'K'}, be16(1), []byte{'t'}, be32(1000)),
		// Update 'N' tuple claiming columns without data.
		cat([]byte{'U'}, be32(1), []byte{'N'}, be16(99)),
		// Delete tuple claiming columns without data.
		cat([]byte{'D'}, be32(1), []byte{'K'}, be16(99)),
		// Truncate claiming many relation IDs, supplying none.
		cat([]byte{'T'}, be32(1000000), []byte{0}),
		// Truncate header truncated right after relation count.
		cat([]byte{'T'}, be32(2)),
		// Relation claiming columns but missing the first column flags byte.
		cat([]byte{'R'}, be32(1), []byte{'p', 0, 't', 0, 1}, be16(1)),
		// Relation column name without terminating NUL.
		cat([]byte{'R'}, be32(1), []byte{'p', 0, 't', 0, 1}, be16(1),
			[]byte{0}, []byte("nonterminated")),
		// Type message namespace without terminating NUL.
		cat([]byte{'Y'}, be32(20), []byte("public")),
		// Origin name without terminating NUL.
		cat([]byte{'O'}, be32(0), []byte("nonterminated")),
		// Logical message prefix without terminating NUL.
		cat([]byte{'M'}, []byte{1}, make([]byte, 8), []byte("prefix")),
		// Logical message content length larger than supplied content.
		cat([]byte{'M'}, []byte{1}, make([]byte, 8), []byte{'p', 0}, be32(1000)),
		// Logical message huge content length.
		cat([]byte{'M'}, []byte{1}, make([]byte, 8), []byte{'p', 0}, be32(0xffffffff)),
	}
	return cases
}

func malformedParseV2Cases() [][]byte {
	cases := [][]byte{
		// inStream message with XID prefix but truncated body.
		cat([]byte{'M'}, be32(7), []byte{1}, make([]byte, 8), []byte("p")),
		// inStream insert whose XID prefix is truncated.
		cat([]byte{'I'}, []byte{1, 2, 3}),
		// inStream relation with XID but truncated body.
		cat([]byte{'R'}, be32(7), be32(1)),
		// inStream truncate claiming too many relation IDs.
		cat([]byte{'T'}, be32(7), be32(0x7fffffff), []byte{0}),
		// stream start with truncated XID/first-segment byte.
		[]byte{'S', 1, 2},
		// stream commit truncated inside the XID.
		[]byte{'c', 1, 2},
		// stream abort with only one XID present.
		cat([]byte{'A'}, be32(7)),
	}
	return cases
}

func TestParseMalformedSeeds(t *testing.T) {
	for i, data := range malformedParseCases() {
		data := data
		t.Run("v1", func(t *testing.T) {
			assertNoPanic(t, func() {
				msg, err := Parse(data)
				if err == nil {
					t.Fatalf("case %d: Parse(% x) succeeded: %#v", i, data, msg)
				}
			})
		})
		t.Run("v2", func(t *testing.T) {
			assertNoPanic(t, func() {
				msg, err := ParseV2(data, false)
				if err == nil {
					t.Fatalf("case %d: ParseV2(% x) succeeded: %#v", i, data, msg)
				}
			})
		})
	}

	for i, data := range malformedParseV2Cases() {
		data := data
		t.Run("v2stream", func(t *testing.T) {
			assertNoPanic(t, func() {
				msg, err := ParseV2(data, true)
				if err == nil {
					t.Fatalf("case %d: ParseV2(% x, true) succeeded: %#v", i, data, msg)
				}
			})
		})
	}
}

// TestParseRecoversAfterBadMessages makes sure repeated failures leave no
// shared state behind and a following valid message still decodes correctly.
func TestParseRecoversAfterBadMessages(t *testing.T) {
	for i := 0; i < 3; i++ {
		if _, err := Parse(nil); !errors.Is(err, errMsgLength) {
			t.Fatalf("iteration %d: Parse(nil) error = %v", i, err)
		}
		if _, err := Parse([]byte{'I'}); !errors.Is(err, errMsgLength) {
			t.Fatalf("iteration %d: truncated insert error = %v", i, err)
		}
	}

	s := &messageSuite{}
	msg, expected := s.createInsertTestData()
	parsed, err := Parse(msg)
	if err != nil {
		t.Fatalf("Parse after bad messages: %v", err)
	}
	insert, ok := parsed.(*InsertMessage)
	if !ok {
		t.Fatalf("expected *InsertMessage, got %T", parsed)
	}
	if insert.RelationID != expected.RelationID {
		t.Fatalf("RelationID = %d, want %d", insert.RelationID, expected.RelationID)
	}
	if insert.Tuple == nil || insert.Tuple.ColumnNum != expected.Tuple.ColumnNum {
		t.Fatalf("tuple mismatch: %#v", insert.Tuple)
	}

	for i := 0; i < 3; i++ {
		if _, err := ParseV2(nil, true); !errors.Is(err, errMsgLength) {
			t.Fatalf("iteration %d: ParseV2(nil, true) error = %v", i, err)
		}
	}

	rel, relExpected := s.createRelationTestData()
	relV2, xid := s.insertXid(rel)
	parsedV2, err := ParseV2(relV2, true)
	if err != nil {
		t.Fatalf("ParseV2 after bad messages: %v", err)
	}
	relMsg, ok := parsedV2.(*RelationMessageV2)
	if !ok {
		t.Fatalf("expected *RelationMessageV2, got %T", parsedV2)
	}
	if relMsg.Xid != xid {
		t.Fatalf("Xid = %d, want %d", relMsg.Xid, xid)
	}
	if relMsg.RelationID != relExpected.RelationID || relMsg.ColumnNum != relExpected.ColumnNum {
		t.Fatalf("relation mismatch: %#v", relMsg)
	}
}

func FuzzParse(f *testing.F) {
	for _, sample := range validMessageSamples() {
		f.Add(sample)
		f.Add(sample[:len(sample)/2])
	}
	for _, data := range malformedParseCases() {
		f.Add(data)
	}
	f.Add([]byte{})
	f.Add([]byte{'I'})

	f.Fuzz(func(t *testing.T, data []byte) {
		// Both v1 and v2 decoding must never panic on attacker-controlled
		// input; internal errors are returned, not recovered away elsewhere.
		m1, err1 := Parse(data)
		if (err1 == nil) != (m1 != nil) {
			t.Fatalf("Parse: message=%#v err=%v", m1, err1)
		}

		m2, err2 := ParseV2(data, true)
		if (err2 == nil) != (m2 != nil) {
			t.Fatalf("ParseV2(inStream): message=%#v err=%v", m2, err2)
		}
		m3, err3 := ParseV2(data, false)
		if (err3 == nil) != (m3 != nil) {
			t.Fatalf("ParseV2: message=%#v err=%v", m3, err3)
		}
	})
}
