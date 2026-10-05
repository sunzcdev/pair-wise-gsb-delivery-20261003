package centrifuge

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sort"

	"github.com/centrifugal/protocol"
)

// ErrIdempotencyConflict is returned by strict idempotent publishing when a
// cached result exists for the key but the retry content digest differs.
var ErrIdempotencyConflict = errors.New("idempotency key used with different content")

const idempotencyConflictMarker = "__centrifuge_idempotency_conflict__"

func publicationContentDigest(data []byte, opts PublishOptions) string {
	var infoBytes []byte
	if opts.ClientInfo != nil {
		protoInfo := &protocol.ClientInfo{
			Client:   opts.ClientInfo.ClientID,
			User:     opts.ClientInfo.UserID,
			ConnInfo: opts.ClientInfo.ConnInfo,
			ChanInfo: opts.ClientInfo.ChanInfo,
		}
		infoBytes, _ = protoInfo.MarshalVT()
	}

	buf := make([]byte, 0, 128)
	buf = appendBytesField(buf, 1, data)

	buf = appendBytesField(buf, 2, infoBytes)

	tagKeys := make([]string, 0, len(opts.Tags))
	for key := range opts.Tags {
		tagKeys = append(tagKeys, key)
	}
	sort.Strings(tagKeys)
	for _, key := range tagKeys {
		buf = appendBytesField(buf, 3, []byte(key))
		buf = appendBytesField(buf, 3, []byte(opts.Tags[key]))
	}

	flags := uint64(0)
	if opts.UseDelta {
		flags |= 1 << 0
	}
	if opts.Removed {
		flags |= 1 << 1
	}
	var flagBytes [9]byte
	flagBytes[0] = 5
	binary.BigEndian.PutUint64(flagBytes[1:], flags)
	buf = append(buf, flagBytes[:]...)

	buf = appendUint64Field(buf, 6, opts.Version)
	buf = appendStringField(buf, 7, opts.VersionEpoch)
	buf = appendStringField(buf, 8, opts.Key)
	buf = appendInt64Field(buf, 9, opts.score)
	buf = appendUint64Field(buf, 10, opts.Offset)
	buf = appendStringField(buf, 11, opts.Epoch)
	buf = appendBytesField(buf, 12, opts.PrevData)

	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

func appendBytesField(buf []byte, tag byte, value []byte) []byte {
	buf = append(buf, tag)
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	buf = append(buf, length[:]...)
	return append(buf, value...)
}

func appendStringField(buf []byte, tag byte, value string) []byte {
	return appendBytesField(buf, tag, []byte(value))
}

func appendUint64Field(buf []byte, tag byte, value uint64) []byte {
	var field [9]byte
	field[0] = tag
	binary.BigEndian.PutUint64(field[1:], value)
	return append(buf, field[:]...)
}

func appendInt64Field(buf []byte, tag byte, value int64) []byte {
	var field [9]byte
	field[0] = tag
	binary.BigEndian.PutUint64(field[1:], uint64(value))
	return append(buf, field[:]...)
}
