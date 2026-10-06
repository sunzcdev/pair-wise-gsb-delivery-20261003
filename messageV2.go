package pglogrepl

import (
	"encoding/binary"
	"fmt"
	"time"
)

// MessageDecoderV2 decodes message from V2 protocol into struct.
type MessageDecoderV2 interface {
	MessageDecoder
	DecodeV2(src []byte, inStream bool) error
}

// StreamStartMessageV2 is a stream start message.
type StreamStartMessageV2 struct {
	baseMessage

	Xid uint32
	// A value of 1 indicates this is the first stream segment for this XID, 0 for any other stream segment
	FirstSegment uint8
}

// DecodeV2 decodes to message from V2 src.
func (m *StreamStartMessageV2) DecodeV2(src []byte, _ bool) (err error) {
	if len(src) < 5 {
		return m.lengthError("StreamStartMessageV2", 5, len(src))
	}

	var low, used int
	m.Xid, used = m.decodeUint32(src)
	low += used
	m.FirstSegment = src[low]

	m.SetType(MessageTypeStreamStart)

	return nil
}

// StreamStopMessageV2 is a stream stop message.
type StreamStopMessageV2 struct {
	baseMessage
}

// DecodeV2 decodes to message from V2 src.
func (m *StreamStopMessageV2) DecodeV2(_ []byte, _ bool) (err error) {
	// stream stop has no data.
	m.SetType(MessageTypeStreamStop)

	return nil
}

// StreamCommitMessageV2 is a stream commit message.
type StreamCommitMessageV2 struct {
	baseMessage

	Xid               uint32
	Flags             uint8 // currently unused
	CommitLSN         LSN
	TransactionEndLSN LSN
	CommitTime        time.Time
}

// DecodeV2 decodes to message from V2 src.
func (m *StreamCommitMessageV2) DecodeV2(src []byte, _ bool) (err error) {
	if len(src) < 29 {
		return m.lengthError("StreamCommitMessageV2", 29, len(src))
	}
	var low, used int
	m.Xid, used = m.decodeUint32(src)
	low += used
	m.Flags = src[low]
	low += 1
	m.CommitLSN, used = m.decodeLSN(src[low:])
	low += used
	m.TransactionEndLSN, used = m.decodeLSN(src[low:])
	low += used
	m.CommitTime, _ = m.decodeTime(src[low:])

	m.SetType(MessageTypeStreamCommit)

	return nil
}

// StreamAbortMessageV2 is a stream abort message.
type StreamAbortMessageV2 struct {
	baseMessage

	Xid uint32
	// Xid of the subtransaction (will be same as xid of the transaction for top-level transactions).
	SubXid uint32
}

// DecodeV2 decodes to message from V2 src.
func (m *StreamAbortMessageV2) DecodeV2(src []byte, _ bool) (err error) {
	if len(src) < 8 {
		return m.lengthError("StreamAbortMessageV2", 8, len(src))
	}

	var low, used int
	m.Xid, used = m.decodeUint32(src)
	low += used
	m.SubXid, _ = m.decodeUint32(src[low:])

	m.SetType(MessageTypeStreamAbort)

	return nil
}

// ParseV2 parse a logical replication message from protocol version #2
// it accepts a slice of bytes read from PG and inStream parameter
// inStream must be true when StreamStartMessageV2 has been read
// it must be false after StreamStopMessageV2 has been read
func ParseV2(data []byte, inStream bool) (m Message, err error) {
	if len(data) == 0 {
		return nil, shortDataError(1, 0)
	}
	var decoder MessageDecoder
	msgType := MessageType(data[0])

	switch msgType {
	case MessageTypeStreamStart:
		decoder = new(StreamStartMessageV2)
	case MessageTypeStreamStop:
		decoder = new(StreamStopMessageV2)
	case MessageTypeStreamCommit:
		decoder = new(StreamCommitMessageV2)
	case MessageTypeStreamAbort:
		decoder = new(StreamAbortMessageV2)
	case MessageTypeMessage:
		decoder = new(LogicalDecodingMessageV2)
	case MessageTypeRelation:
		decoder = new(RelationMessageV2)
	case MessageTypeType:
		decoder = new(TypeMessageV2)
	case MessageTypeInsert:
		decoder = new(InsertMessageV2)
	case MessageTypeUpdate:
		decoder = new(UpdateMessageV2)
	case MessageTypeDelete:
		decoder = new(DeleteMessageV2)
	case MessageTypeTruncate:
		decoder = new(TruncateMessageV2)
	default:
		decoder = getCommonDecoder(msgType)
	}

	if decoder == nil {
		return nil, errMsgNotSupported
	}

	if v2, ok := decoder.(MessageDecoderV2); ok {
		if err = v2.DecodeV2(data[1:], inStream); err != nil {
			return nil, err
		}
	} else if err = decoder.Decode(data[1:]); err != nil {
		return nil, err
	}

	return decoder.(Message), nil
}

// InStreamMessageV2WithXid is a V2 protocol message
type InStreamMessageV2WithXid struct {
	// Xid of the transaction (only present for streamed transactions).
	Xid uint32
}

// LogicalDecodingMessageV2 is a logical decoding message.
type LogicalDecodingMessageV2 struct {
	LogicalDecodingMessage
	InStreamMessageV2WithXid
}

// DecodeV2 decodes to message from V2 src.
func (m *LogicalDecodingMessageV2) DecodeV2(src []byte, inStream bool) (err error) {
	if !inStream {
		return m.LogicalDecodingMessage.Decode(src)
	}

	src, err = readXidAndAdvance(src, &m.InStreamMessageV2WithXid, inStream)
	if err != nil {
		return err
	}

	return m.LogicalDecodingMessage.Decode(src)
}

// RelationMessageV2 is a relation message.
type RelationMessageV2 struct {
	RelationMessage
	InStreamMessageV2WithXid
}

// DecodeV2 decodes to message from V2 src.
func (m *RelationMessageV2) DecodeV2(src []byte, inStream bool) (err error) {
	if !inStream {
		return m.RelationMessage.Decode(src)
	}

	src, err = readXidAndAdvance(src, &m.InStreamMessageV2WithXid, inStream)
	if err != nil {
		return err
	}

	return m.RelationMessage.Decode(src)
}

// TypeMessageV2 is a type message.
type TypeMessageV2 struct {
	TypeMessage
	InStreamMessageV2WithXid
}

// DecodeV2 decodes to message from V2 src.
func (m *TypeMessageV2) DecodeV2(src []byte, inStream bool) (err error) {
	if !inStream {
		return m.TypeMessage.Decode(src)
	}

	src, err = readXidAndAdvance(src, &m.InStreamMessageV2WithXid, inStream)
	if err != nil {
		return err
	}

	return m.TypeMessage.Decode(src)
}

// InsertMessageV2 is an insert message.
type InsertMessageV2 struct {
	InsertMessage
	InStreamMessageV2WithXid
}

// DecodeV2 decodes to message from V2 src.
func (m *InsertMessageV2) DecodeV2(src []byte, inStream bool) (err error) {
	if !inStream {
		return m.InsertMessage.Decode(src)
	}

	src, err = readXidAndAdvance(src, &m.InStreamMessageV2WithXid, inStream)
	if err != nil {
		return err
	}

	return m.InsertMessage.Decode(src)
}

// UpdateMessageV2 is an update message.
type UpdateMessageV2 struct {
	UpdateMessage
	InStreamMessageV2WithXid
}

// DecodeV2 decodes to message from V2 src.
func (m *UpdateMessageV2) DecodeV2(src []byte, inStream bool) (err error) {
	if !inStream {
		return m.UpdateMessage.Decode(src)
	}

	src, err = readXidAndAdvance(src, &m.InStreamMessageV2WithXid, inStream)
	if err != nil {
		return err
	}

	return m.UpdateMessage.Decode(src)
}

// DeleteMessageV2 is a delete message.
type DeleteMessageV2 struct {
	DeleteMessage
	InStreamMessageV2WithXid
}

// DecodeV2 decodes to message from V2 src.
func (m *DeleteMessageV2) DecodeV2(src []byte, inStream bool) (err error) {
	if !inStream {
		return m.DeleteMessage.Decode(src)
	}

	src, err = readXidAndAdvance(src, &m.InStreamMessageV2WithXid, inStream)
	if err != nil {
		return err
	}

	return m.DeleteMessage.Decode(src)
}

// TruncateMessageV2 is a truncate message.
type TruncateMessageV2 struct {
	TruncateMessage
	InStreamMessageV2WithXid
}

// DecodeV2 decodes to message from V2 src.
func (m *TruncateMessageV2) DecodeV2(src []byte, inStream bool) (err error) {
	if !inStream {
		return m.TruncateMessage.Decode(src)
	}

	src, err = readXidAndAdvance(src, &m.InStreamMessageV2WithXid, inStream)
	if err != nil {
		return err
	}

	return m.TruncateMessage.Decode(src)
}

func readXidAndAdvance(src []byte, mXid *InStreamMessageV2WithXid, inStream bool) ([]byte, error) {
	if !inStream {
		return src, nil
	}

	if err := checkDataLength(src, 4); err != nil {
		return nil, fmt.Errorf("Xid: %w", err)
	}
	mXid.Xid = binary.BigEndian.Uint32(src)
	return src[4:], nil
}
