package pglogrepl

import "time"

// MessageDecoderV3 decodes messages from the V3 protocol into a struct.
//
// Protocol version 3 is a superset of version 2: it adds the two-phase commit
// messages (BeginPrepare, Prepare, CommitPrepared, RollbackPrepared and
// StreamPrepare) enabled by the two_phase streaming option.
type MessageDecoderV3 interface {
	MessageDecoder
	DecodeV3(src []byte, inStream bool) error
}

// BeginPrepareMessageV3 is a begin prepare message.
//
//	Byte1('b')   Identifies the message as a begin prepare message.
//	Byte8        The final LSN of the transaction.
//	Byte8        Commit timestamp of the transaction (microseconds since PG epoch).
//	Byte4        Xid of the transaction.
type BeginPrepareMessageV3 struct {
	baseMessage

	FinalLSN    LSN
	PrepareTime time.Time
	Xid         uint32
}

// DecodeV3 decodes the message from V3 src.
func (m *BeginPrepareMessageV3) DecodeV3(src []byte, _ bool) error {
	if len(src) < 20 {
		return m.lengthError("BeginPrepareMessageV3", 20, len(src))
	}

	var low, used int
	m.FinalLSN, used = m.decodeLSN(src)
	low += used
	m.PrepareTime, used = m.decodeTime(src[low:])
	low += used
	m.Xid, _ = m.decodeUint32(src[low:])

	m.SetType(MessageTypeBeginPrepare)

	return nil
}

// PrepareMessageV3 is a prepare message.
//
//	Byte1('P')   Identifies the message as a prepare message.
//	Byte8        The LSN of the prepare.
//	Byte8        The end LSN of the prepared transaction.
//	Byte8        Prepare timestamp of the transaction (microseconds since PG epoch).
//	String       The GID of the prepared transaction.
type PrepareMessageV3 struct {
	baseMessage

	PrepareLSN  LSN
	EndLSN      LSN
	PrepareTime time.Time
	GID         string
}

// DecodeV3 decodes the message from V3 src.
func (m *PrepareMessageV3) DecodeV3(src []byte, _ bool) error {
	if len(src) < 24 {
		return m.lengthError("PrepareMessageV3", 24, len(src))
	}

	var low, used int
	m.PrepareLSN, used = m.decodeLSN(src)
	low += used
	m.EndLSN, used = m.decodeLSN(src[low:])
	low += used
	m.PrepareTime, used = m.decodeTime(src[low:])
	low += used
	m.GID, used = m.decodeString(src[low:])
	if used < 0 {
		return m.decodeStringError("PrepareMessageV3", "GID")
	}

	m.SetType(MessageTypePrepare)

	return nil
}

// CommitPreparedMessageV3 is a commit prepared message.
//
//	Byte1('K')   Identifies the message as a commit prepared message.
//	Byte8        The LSN of the commit.
//	Byte8        The end LSN of the prepared transaction.
//	Byte8        Commit timestamp of the transaction (microseconds since PG epoch).
//	String       The GID of the prepared transaction.
type CommitPreparedMessageV3 struct {
	baseMessage

	CommitLSN         LSN
	TransactionEndLSN LSN
	CommitTime        time.Time
	GID               string
}

// DecodeV3 decodes the message from V3 src.
func (m *CommitPreparedMessageV3) DecodeV3(src []byte, _ bool) error {
	if len(src) < 24 {
		return m.lengthError("CommitPreparedMessageV3", 24, len(src))
	}

	var low, used int
	m.CommitLSN, used = m.decodeLSN(src)
	low += used
	m.TransactionEndLSN, used = m.decodeLSN(src[low:])
	low += used
	m.CommitTime, used = m.decodeTime(src[low:])
	low += used
	m.GID, used = m.decodeString(src[low:])
	if used < 0 {
		return m.decodeStringError("CommitPreparedMessageV3", "GID")
	}

	m.SetType(MessageTypeCommitPrepared)

	return nil
}

// RollbackPreparedMessageV3 is a rollback prepared message.
//
//	Byte1('r')   Identifies the message as a rollback prepared message.
//	Byte8        The LSN of the rollback.
//	Byte8        The end LSN of the prepared transaction.
//	Byte8        Prepare timestamp of the transaction (microseconds since PG epoch).
//	Byte8        Rollback timestamp of the transaction (microseconds since PG epoch).
//	Byte4        Xid of the transaction (only present for streamed transactions).
//	String       The GID of the prepared transaction.
type RollbackPreparedMessageV3 struct {
	baseMessage

	Xid          uint32
	RollbackLSN  LSN
	EndLSN       LSN
	PrepareTime  time.Time
	RollbackTime time.Time
	GID          string
}

// DecodeV3 decodes the message from V3 src.
func (m *RollbackPreparedMessageV3) DecodeV3(src []byte, inStream bool) error {
	// The four fixed Int64 fields take 32 bytes; the Xid is only present
	// inside a streaming transaction and precedes the GID.
	fixedLen := 32
	if inStream {
		fixedLen += 4
	}
	if len(src) < fixedLen {
		return m.lengthError("RollbackPreparedMessageV3", fixedLen, len(src))
	}

	var low, used int
	m.RollbackLSN, used = m.decodeLSN(src[low:])
	low += used
	m.EndLSN, used = m.decodeLSN(src[low:])
	low += used
	m.PrepareTime, used = m.decodeTime(src[low:])
	low += used
	m.RollbackTime, used = m.decodeTime(src[low:])
	low += used
	if inStream {
		m.Xid, used = m.decodeUint32(src[low:])
		low += used
	} else {
		m.Xid = 0
	}
	m.GID, used = m.decodeString(src[low:])
	if used < 0 {
		return m.decodeStringError("RollbackPreparedMessageV3", "GID")
	}

	m.SetType(MessageTypeRollbackPrepared)

	return nil
}

// StreamPrepareMessageV3 is a stream prepare message sent inside a streaming
// transaction.
//
//	Byte1('P')   Identifies the message as a stream prepare message.
//	Byte4        Xid of the transaction.
//	Byte8        The LSN of the prepare.
//	Byte8        The end LSN of the prepared transaction.
//	Byte8        Prepare timestamp of the transaction (microseconds since PG epoch).
//	String       The GID of the prepared transaction.
type StreamPrepareMessageV3 struct {
	PrepareMessageV3
	InStreamMessageV2WithXid
}

// DecodeV3 decodes the message from V3 src.
func (m *StreamPrepareMessageV3) DecodeV3(src []byte, inStream bool) error {
	if !inStream {
		return m.PrepareMessageV3.DecodeV3(src, false)
	}

	if len(src) < 28 {
		return m.lengthError("StreamPrepareMessageV3", 28, len(src))
	}

	src = readXidAndAdvance(src, &m.InStreamMessageV2WithXid, inStream)

	return m.PrepareMessageV3.DecodeV3(src, true)
}

// ParseV3 parses a logical replication message from protocol version 3.
//
// It accepts the raw bytes of one message read from PostgreSQL and the inStream
// parameter: inStream must be true after a StreamStart message has been read and
// false after StreamStop. Protocol version 3 carries every version 2 message,
// including ordinary and streamed transactions, plus the two-phase commit
// messages gated by the two_phase streaming option.
//
// Parse and ParseV2 never interpret the new two-phase message identifiers, so an
// application that did not negotiate protocol version 3 keeps the old semantics
// and receives an error instead of a silently misparsed message.
func ParseV3(data []byte, inStream bool) (m Message, err error) {
	msgType := MessageType(data[0])

	switch msgType {
	case MessageTypeBeginPrepare:
		return decodeV3(new(BeginPrepareMessageV3), data[1:], inStream)
	case MessageTypeCommitPrepared:
		return decodeV3(new(CommitPreparedMessageV3), data[1:], inStream)
	case MessageTypeRollbackPrepared:
		return decodeV3(new(RollbackPreparedMessageV3), data[1:], inStream)
	case MessageTypePrepare:
		// A 'P' message is StreamPrepare when inside a streaming transaction
		// and a plain Prepare otherwise; the on-wire identifier is identical.
		if inStream {
			return decodeV3(new(StreamPrepareMessageV3), data[1:], inStream)
		}
		return decodeV3(new(PrepareMessageV3), data[1:], inStream)
	}

	return parseV2(data, inStream)
}

func decodeV3(decoder MessageDecoderV3, src []byte, inStream bool) (Message, error) {
	if err := decoder.DecodeV3(src, inStream); err != nil {
		return nil, err
	}

	return decoder.(Message), nil
}
