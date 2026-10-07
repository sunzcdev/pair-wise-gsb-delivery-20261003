package pglogrepl

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Fixed wire-protocol samples for the protocol version 3 (two-phase) messages.
// Byte layout follows the official message definitions in
// https://www.postgresql.org/docs/current/protocol-logicalrep-message-formats.html
//
// Common field values used by the samples:
//
//	prepareLSN   = 0x0000000002A00010
//	endLSN       = 0x0000000002A00080
//	commitLSN    = 0x0000000002A00100
//	commitEndLSN = 0x0000000002A00140
//	prepareTime  = 750000000000000 microseconds since 2000-01-01
//	commitTime   = 760000000000000 microseconds since 2000-01-01
//	xid          = 500
//	gid          = "txn-42"
const (
	v3PrepareLSN   = LSN(0x0000000002A00010)
	v3EndLSN       = LSN(0x0000000002A00080)
	v3CommitLSN    = LSN(0x0000000002A00100)
	v3CommitEndLSN = LSN(0x0000000002A00140)

	v3PrepareTimeMicros = int64(750_000_000_000_000)
	v3CommitTimeMicros  = int64(760_000_000_000_000)

	v3Xid = uint32(500)
	v3GID = "txn-42"
)

// Prepare Message (Byte1('P')):
//
//	Byte1       Flags; currently unused (0).
//	Int64       The LSN of the prepare record.
//	Int64       End LSN of the prepared transaction.
//	Int64       Prepare timestamp of the transaction (microseconds since 2000-01-01).
//	Int32       Xid of the transaction.
//	String      GID of the prepared transaction (null-terminated).
var prepareMsgSample = []byte{
	'P',
	0x00,                                           // flags
	0x00, 0x00, 0x00, 0x00, 0x02, 0xA0, 0x00, 0x10, // prepare LSN
	0x00, 0x00, 0x00, 0x00, 0x02, 0xA0, 0x00, 0x80, // end LSN
	0x00, 0x02, 0xAA, 0x1E, 0xFB, 0x94, 0xE0, 0x00, // prepare timestamp
	0x00, 0x00, 0x01, 0xF4, // xid = 500
	't', 'x', 'n', '-', '4', '2', 0x00, // GID
}

// Commit Prepared Message (Byte1('K')):
//
//	Byte1       Flags; currently unused (0).
//	Int64       The LSN of the commit prepared record.
//	Int64       End LSN of the commit prepared transaction.
//	Int64       Commit timestamp of the transaction (microseconds since 2000-01-01).
//	Int32       Xid of the transaction.
//	String      GID of the prepared transaction (null-terminated).
var commitPreparedMsgSample = []byte{
	'K',
	0x00,                                           // flags
	0x00, 0x00, 0x00, 0x00, 0x02, 0xA0, 0x01, 0x00, // commit LSN
	0x00, 0x00, 0x00, 0x00, 0x02, 0xA0, 0x01, 0x40, // transaction end LSN
	0x00, 0x02, 0xB3, 0x37, 0x4A, 0x07, 0x80, 0x00, // commit timestamp
	0x00, 0x00, 0x01, 0xF4, // xid = 500
	't', 'x', 'n', '-', '4', '2', 0x00, // GID
}

// Rollback Prepared Message (Byte1('r')):
//
//	Int64       The LSN of the prepare record.
//	Int64       End LSN of the prepared transaction.
//	Int64       Prepare timestamp of the transaction (microseconds since 2000-01-01).
//	Int32       Xid of the transaction.
//	String      GID of the prepared transaction (null-terminated).
var rollbackPreparedMsgSample = []byte{
	'r',
	0x00, 0x00, 0x00, 0x00, 0x02, 0xA0, 0x00, 0x10, // prepare LSN
	0x00, 0x00, 0x00, 0x00, 0x02, 0xA0, 0x00, 0x80, // prepare end LSN
	0x00, 0x02, 0xAA, 0x1E, 0xFB, 0x94, 0xE0, 0x00, // prepare timestamp
	0x00, 0x00, 0x01, 0xF4, // xid = 500
	't', 'x', 'n', '-', '4', '2', 0x00, // GID
}

// Begin Prepare Message (Byte1('b')):
//
//	Int64       The LSN of the prepare record.
//	Int64       End LSN of the prepared transaction.
//	Int64       Prepare timestamp of the transaction (microseconds since 2000-01-01).
//	Int32       Xid of the transaction.
//	String      GID of the prepared transaction (null-terminated).
var beginPrepareMsgSample = []byte{
	'b',
	0x00, 0x00, 0x00, 0x00, 0x02, 0xA0, 0x00, 0x10, // prepare LSN
	0x00, 0x00, 0x00, 0x00, 0x02, 0xA0, 0x00, 0x80, // end LSN
	0x00, 0x02, 0xAA, 0x1E, 0xFB, 0x94, 0xE0, 0x00, // prepare timestamp
	0x00, 0x00, 0x01, 0xF4, // xid = 500
	't', 'x', 'n', '-', '4', '2', 0x00, // GID
}

// Stream Prepare Message (Byte1('p')):
//
//	Int64       The LSN of the prepare record.
//	Int64       End LSN of the prepared transaction.
//	Int64       Prepare timestamp of the transaction (microseconds since 2000-01-01).
//	Int32       Xid of the transaction.
//	String      GID of the prepared transaction (null-terminated).
var streamPrepareMsgSample = []byte{
	'p',
	0x00, 0x00, 0x00, 0x00, 0x02, 0xA0, 0x00, 0x10, // prepare LSN
	0x00, 0x00, 0x00, 0x00, 0x02, 0xA0, 0x00, 0x80, // end LSN
	0x00, 0x02, 0xAA, 0x1E, 0xFB, 0x94, 0xE0, 0x00, // prepare timestamp
	0x00, 0x00, 0x01, 0xF4, // xid = 500
	't', 'x', 'n', '-', '4', '2', 0x00, // GID
}

func TestParseV3CommitPreparedAfterPrepare(t *testing.T) {
	m, err := ParseV3(prepareMsgSample, false)
	require.NoError(t, err)
	prepare, ok := m.(*PrepareMessage)
	require.True(t, ok)
	require.Equal(t, MessageTypePrepare, prepare.Type())
	require.Equal(t, uint8(0), prepare.Flags)
	require.Equal(t, v3PrepareLSN, prepare.PrepareLSN)
	require.Equal(t, v3EndLSN, prepare.EndLSN)
	require.Equal(t, pgTimeToTime(v3PrepareTimeMicros), prepare.PrepareTime)
	require.Equal(t, v3Xid, prepare.Xid)
	require.Equal(t, v3GID, prepare.GID)

	m, err = ParseV3(commitPreparedMsgSample, false)
	require.NoError(t, err)
	commit, ok := m.(*CommitPreparedMessage)
	require.True(t, ok)
	require.Equal(t, MessageTypeCommitPrepared, commit.Type())
	require.Equal(t, uint8(0), commit.Flags)
	require.Equal(t, v3CommitLSN, commit.CommitLSN)
	require.Equal(t, v3CommitEndLSN, commit.TransactionEndLSN)
	require.Equal(t, pgTimeToTime(v3CommitTimeMicros), commit.CommitTime)
	require.Equal(t, v3Xid, commit.Xid)
	require.Equal(t, v3GID, commit.GID)

	// The GID links the prepare and the commit prepared records.
	require.Equal(t, prepare.GID, commit.GID)
}

func TestParseV3RollbackPreparedAfterPrepare(t *testing.T) {
	m, err := ParseV3(prepareMsgSample, false)
	require.NoError(t, err)
	prepare, ok := m.(*PrepareMessage)
	require.True(t, ok)

	m, err = ParseV3(rollbackPreparedMsgSample, false)
	require.NoError(t, err)
	rollback, ok := m.(*RollbackPreparedMessage)
	require.True(t, ok)
	require.Equal(t, MessageTypeRollbackPrepared, rollback.Type())
	require.Equal(t, v3PrepareLSN, rollback.PrepareLSN)
	require.Equal(t, v3EndLSN, rollback.PrepareEndLSN)
	require.Equal(t, pgTimeToTime(v3PrepareTimeMicros), rollback.PrepareTime)
	require.Equal(t, v3Xid, rollback.Xid)
	require.Equal(t, v3GID, rollback.GID)

	require.Equal(t, prepare.GID, rollback.GID)
}

func TestParseV3BeginPrepare(t *testing.T) {
	m, err := ParseV3(beginPrepareMsgSample, false)
	require.NoError(t, err)
	beginPrepare, ok := m.(*BeginPrepareMessage)
	require.True(t, ok)
	require.Equal(t, MessageTypeBeginPrepare, beginPrepare.Type())
	require.Equal(t, v3PrepareLSN, beginPrepare.PrepareLSN)
	require.Equal(t, v3EndLSN, beginPrepare.EndLSN)
	require.Equal(t, pgTimeToTime(v3PrepareTimeMicros), beginPrepare.PrepareTime)
	require.Equal(t, v3Xid, beginPrepare.Xid)
	require.Equal(t, v3GID, beginPrepare.GID)
}

func TestParseV3StreamPrepare(t *testing.T) {
	m, err := ParseV3(streamPrepareMsgSample, true)
	require.NoError(t, err)
	streamPrepare, ok := m.(*StreamPrepareMessage)
	require.True(t, ok)
	require.Equal(t, MessageTypeStreamPrepare, streamPrepare.Type())
	require.Equal(t, v3PrepareLSN, streamPrepare.PrepareLSN)
	require.Equal(t, v3EndLSN, streamPrepare.EndLSN)
	require.Equal(t, pgTimeToTime(v3PrepareTimeMicros), streamPrepare.PrepareTime)
	require.Equal(t, v3Xid, streamPrepare.Xid)
	require.Equal(t, v3GID, streamPrepare.GID)
}

func TestParseV3TruncatedGID(t *testing.T) {
	// GID is a null-terminated String; dropping the terminator must be an error.
	truncated := prepareMsgSample[:len(prepareMsgSample)-1]
	_, err := ParseV3(truncated, false)
	require.Error(t, err)

	// A GID cut off in the middle of the string must be an error as well.
	truncated = prepareMsgSample[:len(prepareMsgSample)-4]
	_, err = ParseV3(truncated, false)
	require.Error(t, err)
}

func TestParseV3NormalAndStreamedTransactions(t *testing.T) {
	// Begin Message (Byte1('B')): Int64 final LSN, Int64 commit timestamp, Int32 xid.
	beginSample := []byte{
		'B',
		0x00, 0x00, 0x00, 0x00, 0x02, 0xA0, 0x00, 0x80, // final LSN
		0x00, 0x02, 0xB3, 0x37, 0x4A, 0x07, 0x80, 0x00, // commit timestamp
		0x00, 0x00, 0x01, 0xF4, // xid = 500
	}
	m, err := ParseV3(beginSample, false)
	require.NoError(t, err)
	begin, ok := m.(*BeginMessage)
	require.True(t, ok)
	require.Equal(t, v3EndLSN, begin.FinalLSN)
	require.Equal(t, pgTimeToTime(v3CommitTimeMicros), begin.CommitTime)
	require.Equal(t, v3Xid, begin.Xid)

	// Commit Message (Byte1('C')): Byte1 flags, Int64 commit LSN,
	// Int64 transaction end LSN, Int64 commit timestamp.
	commitSample := []byte{
		'C',
		0x00,                                           // flags
		0x00, 0x00, 0x00, 0x00, 0x02, 0xA0, 0x01, 0x00, // commit LSN
		0x00, 0x00, 0x00, 0x00, 0x02, 0xA0, 0x01, 0x40, // transaction end LSN
		0x00, 0x02, 0xB3, 0x37, 0x4A, 0x07, 0x80, 0x00, // commit timestamp
	}
	m, err = ParseV3(commitSample, false)
	require.NoError(t, err)
	commit, ok := m.(*CommitMessage)
	require.True(t, ok)
	require.Equal(t, v3CommitLSN, commit.CommitLSN)
	require.Equal(t, v3CommitEndLSN, commit.TransactionEndLSN)
	require.Equal(t, pgTimeToTime(v3CommitTimeMicros), commit.CommitTime)

	// Stream Start Message (Byte1('S')): Int32 xid, Byte1 first-segment flag.
	streamStartSample := []byte{
		'S',
		0x00, 0x00, 0x01, 0xF4, // xid = 500
		0x01, // first segment
	}
	m, err = ParseV3(streamStartSample, false)
	require.NoError(t, err)
	streamStart, ok := m.(*StreamStartMessageV2)
	require.True(t, ok)
	require.Equal(t, v3Xid, streamStart.Xid)
	require.Equal(t, uint8(1), streamStart.FirstSegment)

	// Stream Stop Message (Byte1('E')): no data.
	m, err = ParseV3([]byte{'E'}, true)
	require.NoError(t, err)
	_, ok = m.(*StreamStopMessageV2)
	require.True(t, ok)
}

func TestV3MessagesNotSupportedByOldParsers(t *testing.T) {
	samples := [][]byte{
		beginPrepareMsgSample,
		prepareMsgSample,
		commitPreparedMsgSample,
		rollbackPreparedMsgSample,
		streamPrepareMsgSample,
	}
	for _, sample := range samples {
		_, err := Parse(sample)
		require.True(t, errors.Is(err, errMsgNotSupported), "Parse(%c)", sample[0])

		_, err = ParseV2(sample, false)
		require.True(t, errors.Is(err, errMsgNotSupported), "ParseV2(%c)", sample[0])
	}
}

func TestTwoPhasePluginArgs(t *testing.T) {
	args := TwoPhasePluginArgs()
	require.Equal(t, []string{
		"proto_version '3'",
		"streaming 'on'",
		"two_phase 'on'",
	}, args)
}
