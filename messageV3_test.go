package pglogrepl

// The wire byte samples below are fixed big-endian records built from the
// PostgreSQL "Logical Replication Message Formats" chapter for protocol
// version 3:
// https://www.postgresql.org/docs/current/protocol-logicalrep-message-formats.html
//
// All timestamps are Int64 microseconds since the PostgreSQL epoch
// (2000-01-01 00:00:00 UTC); see pgTimeToTime/timeToPgTime in pglogrepl.go.
//
//	prepare micros  780105600123456 = 2024-09-20 00:00:00.123456 UTC
//	commit  micros  782179200654321 = 2024-10-14 00:00:00.654321 UTC
//	rollback micros 783302400987654 = 2024-10-27 00:00:00.987654 UTC
//	xid  = 0x000012bc (4796), GID = "test_gid_123"

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

const (
	v3Xid        = uint32(0x000012bc)
	v3FinalLSN   = LSN(0x016f000012345678)
	v3EndLSN     = LSN(0x016f0000123456d0)
	v3CommitLSN  = LSN(0x016f0000123456c0)
	v3AbortLSN   = LSN(0x016f0000123456e0)
	v3GID        = "test_gid_123"
	v3PrepareUS  = int64(780105600123456)
	v3CommitUS   = int64(782179200654321)
	v3RollbackUS = int64(783302400987654)
)

func v3Time(micros int64) time.Time {
	return pgTimeToTime(micros)
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

// Begin Prepare ('b'): Byte8(final lsn) Byte8(prepare time) Byte4(xid)
var beginPrepareV3Sample = "62" +
	"016f000012345678" + // final LSN
	"0002c5807d2ec240" + // prepare time
	"000012bc" // xid

// Prepare ('P'): Byte8(prepare lsn) Byte8(end lsn) Byte8(prepare time) String(gid)
var prepareV3Sample = "50" +
	"016f000012345678" + // prepare LSN
	"016f0000123456d0" + // end LSN
	"0002c5807d2ec240" + // prepare time
	"746573745f6769645f31323300" // "test_gid_123\0"

// Commit Prepared ('K'): Byte8(commit lsn) Byte8(end lsn) Byte8(commit time) String(gid)
var commitPreparedV3Sample = "4b" +
	"016f0000123456c0" + // commit LSN
	"016f0000123456d0" + // end LSN
	"0002c7634967dbf1" + // commit time
	"746573745f6769645f31323300" // "test_gid_123\0"

// Rollback Prepared ('r') outside a stream:
// Byte8(rollback lsn) Byte8(end lsn) Byte8(prepare time) Byte8(rollback time) String(gid)
var rollbackPreparedV3Sample = "72" +
	"016f0000123456e0" + // rollback LSN
	"016f0000123456d0" + // end LSN
	"0002c5807d2ec240" + // prepare time
	"0002c868cd5cd206" + // rollback time
	"746573745f6769645f31323300" // "test_gid_123\0"

// Stream Rollback Prepared ('r') inside a stream adds Byte4(xid) before the GID.
var streamRollbackPreparedV3Sample = "72" +
	"016f0000123456e0" + // rollback LSN
	"016f0000123456d0" + // end LSN
	"0002c5807d2ec240" + // prepare time
	"0002c868cd5cd206" + // rollback time
	"000012bc" + // xid (only present for streamed transactions)
	"746573745f6769645f31323300" // "test_gid_123\0"

// Stream Prepare ('P') inside a stream:
// Byte4(xid) Byte8(prepare lsn) Byte8(end lsn) Byte8(prepare time) String(gid)
var streamPrepareV3Sample = "50" +
	"000012bc" + // xid
	"016f000012345678" + // prepare LSN
	"016f0000123456d0" + // end LSN
	"0002c5807d2ec240" + // prepare time
	"746573745f6769645f31323300" // "test_gid_123\0"

// Truncated GID samples: the String field is cut before the terminating NUL.
var (
	prepareTruncGIDSample             = "50016f000012345678016f0000123456d00002c5807d2ec2407472756e635f676964"
	commitPreparedTruncGIDSample      = "4b016f0000123456c0016f0000123456d00002c7634967dbf17472756e635f676964"
	rollbackPreparedTruncGIDSample    = "72016f0000123456e0016f0000123456d00002c5807d2ec2400002c868cd5cd2067472756e635f676964"
	streamRollbackPreparedTruncSample = "72016f0000123456e0016f0000123456d00002c5807d2ec2400002c868cd5cd206000012bc7472756e635f676964"
	streamPrepareTruncGIDSample       = "50000012bc016f000012345678016f0000123456d00002c5807d2ec2407472756e635f676964"
)

func TestParseV3PrepareThenCommit(t *testing.T) {
	// A two-phase transaction that is eventually committed arrives as
	// BeginPrepare, Prepare and finally CommitPrepared.
	stream := append(mustHex(t, beginPrepareV3Sample), mustHex(t, prepareV3Sample)...)
	stream = append(stream, mustHex(t, commitPreparedV3Sample)...)

	var msg Message

	msg, err := ParseV3(stream[:len(beginPrepareV3Sample)/2], false)
	require.NoError(t, err)
	begin, ok := msg.(*BeginPrepareMessageV3)
	require.True(t, ok)
	require.Equal(t, MessageTypeBeginPrepare, begin.Type())
	require.Equal(t, v3FinalLSN, begin.FinalLSN)
	require.Equal(t, v3Xid, begin.Xid)
	require.True(t, begin.PrepareTime.Equal(v3Time(v3PrepareUS)))

	prepareBytes := stream[len(beginPrepareV3Sample)/2 : len(beginPrepareV3Sample)/2+len(prepareV3Sample)/2]
	msg, err = ParseV3(prepareBytes, false)
	require.NoError(t, err)
	prepare, ok := msg.(*PrepareMessageV3)
	require.True(t, ok)
	require.Equal(t, MessageTypePrepare, prepare.Type())
	require.Equal(t, v3FinalLSN, prepare.PrepareLSN)
	require.Equal(t, v3EndLSN, prepare.EndLSN)
	require.True(t, prepare.PrepareTime.Equal(v3Time(v3PrepareUS)))
	require.Equal(t, v3GID, prepare.GID)

	commitBytes := stream[len(beginPrepareV3Sample)/2+len(prepareV3Sample)/2:]
	msg, err = ParseV3(commitBytes, false)
	require.NoError(t, err)
	commit, ok := msg.(*CommitPreparedMessageV3)
	require.True(t, ok)
	require.Equal(t, MessageTypeCommitPrepared, commit.Type())
	require.Equal(t, v3CommitLSN, commit.CommitLSN)
	require.Equal(t, v3EndLSN, commit.TransactionEndLSN)
	require.True(t, commit.CommitTime.Equal(v3Time(v3CommitUS)))
	require.Equal(t, v3GID, commit.GID)
}

func TestParseV3PrepareThenRollback(t *testing.T) {
	// A rolled-back prepared transaction: BeginPrepare, Prepare, RollbackPrepared.
	begin := mustHex(t, beginPrepareV3Sample)
	prepare := mustHex(t, prepareV3Sample)
	rollback := mustHex(t, rollbackPreparedV3Sample)

	msg, err := ParseV3(begin, false)
	require.NoError(t, err)
	require.IsType(t, &BeginPrepareMessageV3{}, msg)

	msg, err = ParseV3(prepare, false)
	require.NoError(t, err)
	require.IsType(t, &PrepareMessageV3{}, msg)

	msg, err = ParseV3(rollback, false)
	require.NoError(t, err)
	rb, ok := msg.(*RollbackPreparedMessageV3)
	require.True(t, ok)
	require.Equal(t, MessageTypeRollbackPrepared, rb.Type())
	require.Equal(t, v3AbortLSN, rb.RollbackLSN)
	require.Equal(t, v3EndLSN, rb.EndLSN)
	require.True(t, rb.PrepareTime.Equal(v3Time(v3PrepareUS)))
	require.True(t, rb.RollbackTime.Equal(v3Time(v3RollbackUS)))
	require.Equal(t, uint32(0), rb.Xid) // Xid is absent outside a stream
	require.Equal(t, v3GID, rb.GID)

	// The streamed variant carries the Xid right before the GID.
	msg, err = ParseV3(mustHex(t, streamRollbackPreparedV3Sample), true)
	require.NoError(t, err)
	streamRB, ok := msg.(*RollbackPreparedMessageV3)
	require.True(t, ok)
	require.Equal(t, v3Xid, streamRB.Xid)
	require.Equal(t, v3GID, streamRB.GID)
}

func TestParseV3StreamPrepare(t *testing.T) {
	// Inside an in-progress streaming transaction the prepare phase is
	// announced with StreamPrepare; StreamStop ends the segment.
	msg, err := ParseV3(mustHex(t, streamPrepareV3Sample), true)
	require.NoError(t, err)
	sp, ok := msg.(*StreamPrepareMessageV3)
	require.True(t, ok)
	require.Equal(t, MessageTypeStreamPrepare, sp.Type())
	require.Equal(t, v3Xid, sp.Xid)
	require.Equal(t, v3FinalLSN, sp.PrepareLSN)
	require.Equal(t, v3EndLSN, sp.EndLSN)
	require.True(t, sp.PrepareTime.Equal(v3Time(v3PrepareUS)))
	require.Equal(t, v3GID, sp.GID)

	// The two shapes are distinguished solely by the inStream flag, since the
	// on-wire identifier is the same: outside a stream it is a plain Prepare,
	// inside a stream the first four bytes belong to the Xid prefix.
	msg, err = ParseV3(mustHex(t, streamPrepareV3Sample), false)
	require.NoError(t, err)
	_, ok = msg.(*PrepareMessageV3)
	require.True(t, ok)
}

func TestParseV3TruncatedGID(t *testing.T) {
	cases := []struct {
		name     string
		sample   string
		inStream bool
	}{
		{"Prepare", prepareTruncGIDSample, false},
		{"CommitPrepared", commitPreparedTruncGIDSample, false},
		{"RollbackPrepared", rollbackPreparedTruncGIDSample, false},
		{"StreamRollbackPrepared", streamRollbackPreparedTruncSample, true},
		{"StreamPrepare", streamPrepareTruncGIDSample, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseV3(mustHex(t, tc.sample), tc.inStream)
			require.Error(t, err)
		})
	}
}

func TestParseV1V2RejectTwoPhase(t *testing.T) {
	// Old entry points must not switch semantics: the version 3 identifiers
	// stay unsupported instead of being misinterpreted.
	samples := []string{
		beginPrepareV3Sample,
		prepareV3Sample,
		commitPreparedV3Sample,
		rollbackPreparedV3Sample,
		streamPrepareV3Sample,
	}
	for _, s := range samples {
		raw := mustHex(t, s)
		_, errV1 := Parse(raw)
		require.True(t, errors.Is(errV1, errMsgNotSupported), "v1 sample %x", raw[0])
		_, errV2 := ParseV2(raw, s == streamPrepareV3Sample)
		require.True(t, errors.Is(errV2, errMsgNotSupported), "v2 sample %x", raw[0])
	}
}

func TestParseV3OrdinaryTransaction(t *testing.T) {
	// Version 3 still carries the version 1 Begin/Commit pair.
	begin := mustHex(t, "42016f0000123456780002c5807d2ec240000012bc")
	commit := mustHex(t, "4300016f0000123456c0016f0000123456d00002c7634967dbf1")

	msg, err := ParseV3(begin, false)
	require.NoError(t, err)
	beginMsg, ok := msg.(*BeginMessage)
	require.True(t, ok)
	require.Equal(t, v3FinalLSN, beginMsg.FinalLSN)
	require.Equal(t, v3Xid, beginMsg.Xid)
	require.True(t, beginMsg.CommitTime.Equal(v3Time(v3PrepareUS)))

	msg, err = ParseV3(commit, false)
	require.NoError(t, err)
	commitMsg, ok := msg.(*CommitMessage)
	require.True(t, ok)
	require.Equal(t, v3CommitLSN, commitMsg.CommitLSN)
	require.Equal(t, v3EndLSN, commitMsg.TransactionEndLSN)
	require.True(t, commitMsg.CommitTime.Equal(v3Time(v3CommitUS)))
}

func TestParseV3StreamedTransaction(t *testing.T) {
	// StreamStart flips the parser into in-stream mode; StreamStop resets it.
	streamStart := mustHex(t, "53000012bc01")
	streamStop := []byte{'E'}

	msg, err := ParseV3(streamStart, false)
	require.NoError(t, err)
	startMsg, ok := msg.(*StreamStartMessageV2)
	require.True(t, ok)
	require.Equal(t, v3Xid, startMsg.Xid)
	require.Equal(t, uint8(1), startMsg.FirstSegment)

	msg, err = ParseV3(streamStop, true)
	require.NoError(t, err)
	require.IsType(t, &StreamStopMessageV2{}, msg)

	// StreamCommit is a streamed (V2) message decoded through ParseV3's
	// shared dispatch table.
	streamCommit := mustHex(t,
		"63"+"000012bc"+"00"+
			"016f0000123456c0"+"016f0000123456d0"+"0002c7634967dbf1")
	msg, err = ParseV3(streamCommit, true)
	require.NoError(t, err)
	streamCommitMsg, ok := msg.(*StreamCommitMessageV2)
	require.True(t, ok)
	require.Equal(t, v3Xid, streamCommitMsg.Xid)
	require.Equal(t, v3CommitLSN, streamCommitMsg.CommitLSN)
	require.True(t, streamCommitMsg.CommitTime.Equal(v3Time(v3CommitUS)))
}

func TestParseV3StreamedDMLSuite(t *testing.T) {
	// Streamed DML inside a V3 connection keeps the V2 Xid-prefixed shape.
	run := new(parseV3StreamSuite)
	suite.Run(t, run)
}

type parseV3StreamSuite struct {
	messageSuite
}

func (s *parseV3StreamSuite) TestStreamedRelation() {
	rel, expected := s.createRelationTestData()
	relV3, xid := s.insertXid(rel)

	m, err := ParseV3(relV3, true)
	s.NoError(err)
	relMsg, ok := m.(*RelationMessageV2)
	s.True(ok)
	s.Equal(xid, relMsg.Xid)
	s.Equal(expected, &relMsg.RelationMessage)
}

func TestProtocolNegotiationArgs(t *testing.T) {
	require.Equal(t, "proto_version '3'", ProtocolVersionArg(ProtocolVersionV3))
	require.Equal(t, "proto_version '2'", ProtocolVersionArg(ProtocolVersionV2))
	require.Equal(t, []string{"two_phase 'true'"}, TwoPhaseArgs(TwoPhaseEnabled))
	require.Equal(t, []string{"two_phase 'false'"}, TwoPhaseArgs(TwoPhaseDisabled))
}
