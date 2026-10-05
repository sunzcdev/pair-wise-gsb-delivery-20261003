package centrifuge

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"
)

// ErrIdempotencyConflict is returned in strict idempotency mode when a publish
// carries an IdempotencyKey already used within the same channel but with
// different content (data, tags or any option that affects how the publication
// is interpreted). The request is not applied: nothing is added to history and
// nothing is delivered to subscribers. It is safe to inspect with errors.Is.
var ErrIdempotencyConflict = idempotencyConflictError{}

type idempotencyConflictError struct{}

func (idempotencyConflictError) Error() string {
	return "idempotency key reused with different content"
}

// idempotencyRedisConflictMarker is emitted by the Redis Lua scripts on a
// strict-mode content conflict. The Go side maps it to ErrIdempotencyConflict.
const idempotencyRedisConflictMarker = "IDEMPOTENCY_CONFLICT"

// mapIdempotencyConflictError translates the Lua script's conflict marker into
// the typed ErrIdempotencyConflict; every other error passes through unchanged.
func mapIdempotencyConflictError(err error) error {
	if err != nil && strings.Contains(err.Error(), idempotencyRedisConflictMarker) {
		return ErrIdempotencyConflict
	}
	return err
}

// publishContentDigestHex is a convenience used by brokers that speak text protocols.
func publishContentDigestHex(data []byte, opts PublishOptions) string {
	return hex.EncodeToString(publishContentDigest(data, opts))
}

// publishContentDigest computes a deterministic digest of everything that
// defines the content and interpretation of a publication:
//
//   - data (the message bytes),
//   - tags (order-independent),
//   - ClientInfo, UseDelta, Version, VersionEpoch, Key, Removed, score, Offset,
//     Epoch and PrevData, i.e. every PublishOptions field that reaches
//     subscribers or changes how the message is explained.
//
// Fields unrelated to content — history size/TTLs and the idempotency options
// themselves — are deliberately excluded: changing them on a retry must not be
// treated as a conflict.
//
// Encoding is canonical binary with explicit field tags, so the same logical
// request always yields the same digest regardless of map iteration order.
func publishContentDigest(data []byte, opts PublishOptions) []byte {
	h := sha256.New()

	writeBytes := func(tag byte, b []byte) {
		h.Write([]byte{tag})
		var lenBuf [8]byte
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(b)))
		h.Write(lenBuf[:])
		h.Write(b)
	}
	writeString := func(tag byte, s string) {
		writeBytes(tag, []byte(s))
	}
	writeBool := func(tag byte, v bool) {
		h.Write([]byte{tag})
		if v {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	}
	writeUint64 := func(tag byte, v uint64) {
		h.Write([]byte{tag})
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], v)
		h.Write(buf[:])
	}
	writeInt64 := func(tag byte, v int64) {
		h.Write([]byte{tag})
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(v))
		h.Write(buf[:])
	}

	// Field tags 1..: message data and content-affecting options.
	writeBytes(1, data)

	// Tags are sorted by key so map order can never change the digest.
	if len(opts.Tags) > 0 {
		keys := make([]string, 0, len(opts.Tags))
		for k := range opts.Tags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			writeString(2, k)
			writeString(3, opts.Tags[k])
		}
	}

	if opts.ClientInfo != nil {
		writeString(4, opts.ClientInfo.ClientID)
		writeString(5, opts.ClientInfo.UserID)
		writeBytes(6, opts.ClientInfo.ConnInfo)
		writeBytes(7, opts.ClientInfo.ChanInfo)
	}

	writeBool(8, opts.UseDelta)
	writeUint64(9, opts.Version)
	writeString(10, opts.VersionEpoch)
	writeString(11, opts.Key)
	writeBool(12, opts.Removed)
	writeInt64(13, opts.score)
	writeUint64(14, opts.Offset)
	writeString(15, opts.Epoch)
	writeBytes(16, opts.PrevData)

	return h.Sum(nil)
}
