package pglogrepl

import (
	"time"
)

// BeginPrepareMessage is a begin prepare message.
//
// Begin Prepare Message (B) is sent when a prepared transaction is decoded
// before its prepare record. See
// https://www.postgresql.org/docs/current/protocol-logicalrep-message-formats.html
type BeginPrepareMessage struct {
	baseMessage

	// PrepareLSN is the LSN of the prepare record.
	PrepareLSN LSN
	// EndLSN is the end LSN of the prepared transaction.
	EndLSN LSN
	// PrepareTime is the prepare timestamp of the transaction.
	PrepareTime time.Time
	// Xid of the transaction.
	Xid uint32
	// GID is the global transaction identifier of the prepared transaction.
	GID string
}

// Decode decodes a message from src.
func (m *BeginPrepareMessage) Decode(src []byte) error {
	if len(src) < 29 {
		return m.lengthError("BeginPrepareMessage", 29, len(src))
	}

	var low, used int
	m.PrepareLSN, used = m.decodeLSN(src)
	low += used
	m.EndLSN, used = m.decodeLSN(src[low:])
	low += used
	m.PrepareTime, used = m.decodeTime(src[low:])
	low += used
	m.Xid, used = m.decodeUint32(src[low:])
	low += used
	m.GID, used = m.decodeString(src[low:])
	if used < 0 {
		return m.decodeStringError("BeginPrepareMessage", "GID")
	}

	m.SetType(MessageTypeBeginPrepare)

	return nil
}

// PrepareMessage is a prepare message.
//
// Prepare Message (P) is sent when a transaction is prepared.
type PrepareMessage struct {
	baseMessage

	// Flags currently unused (must be 0).
	Flags uint8
	// PrepareLSN is the LSN of the prepare record.
	PrepareLSN LSN
	// EndLSN is the end LSN of the prepared transaction.
	EndLSN LSN
	// PrepareTime is the prepare timestamp of the transaction.
	PrepareTime time.Time
	// Xid of the transaction.
	Xid uint32
	// GID is the global transaction identifier of the prepared transaction.
	GID string
}

// Decode decodes a message from src.
func (m *PrepareMessage) Decode(src []byte) error {
	if len(src) < 30 {
		return m.lengthError("PrepareMessage", 30, len(src))
	}

	var low, used int
	m.Flags = src[low]
	low++
	m.PrepareLSN, used = m.decodeLSN(src[low:])
	low += used
	m.EndLSN, used = m.decodeLSN(src[low:])
	low += used
	m.PrepareTime, used = m.decodeTime(src[low:])
	low += used
	m.Xid, used = m.decodeUint32(src[low:])
	low += used
	m.GID, used = m.decodeString(src[low:])
	if used < 0 {
		return m.decodeStringError("PrepareMessage", "GID")
	}

	m.SetType(MessageTypePrepare)

	return nil
}

// CommitPreparedMessage is a commit prepared message.
//
// Commit Prepared Message (K) is sent when a prepared transaction is committed.
type CommitPreparedMessage struct {
	baseMessage

	// Flags currently unused (must be 0).
	Flags uint8
	// CommitLSN is the LSN of the commit prepared record.
	CommitLSN LSN
	// TransactionEndLSN is the end LSN of the commit prepared transaction.
	TransactionEndLSN LSN
	// CommitTime is the commit timestamp of the transaction.
	CommitTime time.Time
	// Xid of the transaction.
	Xid uint32
	// GID is the global transaction identifier of the prepared transaction.
	GID string
}

// Decode decodes a message from src.
func (m *CommitPreparedMessage) Decode(src []byte) error {
	if len(src) < 30 {
		return m.lengthError("CommitPreparedMessage", 30, len(src))
	}

	var low, used int
	m.Flags = src[low]
	low++
	m.CommitLSN, used = m.decodeLSN(src[low:])
	low += used
	m.TransactionEndLSN, used = m.decodeLSN(src[low:])
	low += used
	m.CommitTime, used = m.decodeTime(src[low:])
	low += used
	m.Xid, used = m.decodeUint32(src[low:])
	low += used
	m.GID, used = m.decodeString(src[low:])
	if used < 0 {
		return m.decodeStringError("CommitPreparedMessage", "GID")
	}

	m.SetType(MessageTypeCommitPrepared)

	return nil
}

// RollbackPreparedMessage is a rollback prepared message.
//
// Rollback Prepared Message (R) is sent when a prepared transaction is rolled back.
type RollbackPreparedMessage struct {
	baseMessage

	// PrepareLSN is the LSN of the prepare record.
	PrepareLSN LSN
	// PrepareEndLSN is the end LSN of the prepared transaction.
	PrepareEndLSN LSN
	// PrepareTime is the prepare timestamp of the transaction.
	PrepareTime time.Time
	// Xid of the transaction.
	Xid uint32
	// GID is the global transaction identifier of the prepared transaction.
	GID string
}

// Decode decodes a message from src.
func (m *RollbackPreparedMessage) Decode(src []byte) error {
	if len(src) < 29 {
		return m.lengthError("RollbackPreparedMessage", 29, len(src))
	}

	var low, used int
	m.PrepareLSN, used = m.decodeLSN(src)
	low += used
	m.PrepareEndLSN, used = m.decodeLSN(src[low:])
	low += used
	m.PrepareTime, used = m.decodeTime(src[low:])
	low += used
	m.Xid, used = m.decodeUint32(src[low:])
	low += used
	m.GID, used = m.decodeString(src[low:])
	if used < 0 {
		return m.decodeStringError("RollbackPreparedMessage", "GID")
	}

	m.SetType(MessageTypeRollbackPrepared)

	return nil
}

// StreamPrepareMessage is a stream prepare message.
//
// Stream Prepare Message (P) is sent when a streamed in-progress transaction
// is prepared. It is only sent when both streaming and two-phase commits are
// enabled.
type StreamPrepareMessage struct {
	baseMessage

	// PrepareLSN is the LSN of the prepare record.
	PrepareLSN LSN
	// EndLSN is the end LSN of the prepared transaction.
	EndLSN LSN
	// PrepareTime is the prepare timestamp of the transaction.
	PrepareTime time.Time
	// Xid of the transaction.
	Xid uint32
	// GID is the global transaction identifier of the prepared transaction.
	GID string
}

// Decode decodes a message from src.
func (m *StreamPrepareMessage) Decode(src []byte) error {
	if len(src) < 29 {
		return m.lengthError("StreamPrepareMessage", 29, len(src))
	}

	var low, used int
	m.PrepareLSN, used = m.decodeLSN(src)
	low += used
	m.EndLSN, used = m.decodeLSN(src[low:])
	low += used
	m.PrepareTime, used = m.decodeTime(src[low:])
	low += used
	m.Xid, used = m.decodeUint32(src[low:])
	low += used
	m.GID, used = m.decodeString(src[low:])
	if used < 0 {
		return m.decodeStringError("StreamPrepareMessage", "GID")
	}

	m.SetType(MessageTypeStreamPrepare)

	return nil
}

// ParseV3 parses a logical replication message from protocol version #3.
// It accepts a slice of bytes read from PG and the inStream parameter.
// inStream must be true when StreamStartMessageV2 has been read
// and it must be false after StreamStopMessageV2 has been read.
//
// Protocol version 3 adds the two-phase commit messages (Begin Prepare,
// Prepare, Commit Prepared, Rollback Prepared and Stream Prepare) on top of
// protocol version 2. All other messages are decoded exactly as in ParseV2.
// Parse and ParseV2 keep their original semantics and never return the
// two-phase message types.
func ParseV3(data []byte, inStream bool) (m Message, err error) {
	var decoder MessageDecoder
	msgType := MessageType(data[0])

	switch msgType {
	case MessageTypeBeginPrepare:
		decoder = new(BeginPrepareMessage)
	case MessageTypePrepare:
		decoder = new(PrepareMessage)
	case MessageTypeCommitPrepared:
		decoder = new(CommitPreparedMessage)
	case MessageTypeRollbackPrepared:
		decoder = new(RollbackPreparedMessage)
	case MessageTypeStreamPrepare:
		decoder = new(StreamPrepareMessage)
	default:
		// Every other message (Begin, Commit, Insert, ..., Stream Start/Stop/
		// Commit/Abort) is identical to protocol version 2.
		return ParseV2(data, inStream)
	}

	if err = decoder.Decode(data[1:]); err != nil {
		return nil, err
	}

	return decoder.(Message), nil
}
