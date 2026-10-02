package versioning

import (
	"bytes"
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

// envelopeMarker is reserved and not currently emitted. The envelope is the
// two-element MessagePack array [version, fields] described below.
//
// Wire layout (MessagePack):
//
//	fixarray/array16 with 2 elements
//	  [0] schema version as a positive integer
//	  [1] map<uint64 fieldID, raw field value>
//
// Putting the version as the first element means the format is self
// describing: the version belongs to the bytes, not to a deploy flag.

func encodeEnvelope(ver uint64, fields Fields) ([]byte, error) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	if err := enc.EncodeArrayLen(2); err != nil {
		return nil, err
	}
	if err := enc.EncodeUint64(ver); err != nil {
		return nil, err
	}
	if err := enc.Encode(map[uint64]msgpack.RawMessage(fields)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeEnvelope(data []byte) (uint64, Fields, error) {
	dec := msgpack.NewDecoder(bytes.NewReader(data))

	n, err := dec.DecodeArrayLen()
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrBadEnvelope, err)
	}
	if n != 2 {
		return 0, nil, fmt.Errorf("%w: expected 2 array elements, got %d",
			ErrBadEnvelope, n)
	}

	var ver uint64
	if err := dec.Decode(&ver); err != nil {
		return 0, nil, fmt.Errorf("%w: version: %v", ErrBadEnvelope, err)
	}

	mLen, err := dec.DecodeMapLen()
	if err != nil {
		return 0, nil, fmt.Errorf("%w: fields: %v", ErrBadEnvelope, err)
	}
	if mLen == -1 {
		mLen = 0
	}

	fields := make(Fields, mLen)
	for i := 0; i < mLen; i++ {
		var id uint64
		if err := dec.Decode(&id); err != nil {
			return 0, nil, fmt.Errorf("%w: field id: %v", ErrBadEnvelope, err)
		}
		raw, err := dec.DecodeRaw()
		if err != nil {
			return 0, nil, fmt.Errorf("%w: field %d: %v", ErrBadEnvelope, id, err)
		}
		fields[id] = raw
	}
	return ver, fields, nil
}

// msgpackUnmarshal is a thin indirection used by the reflected struct decoder.
func msgpackUnmarshal(data []byte, v interface{}) error {
	return msgpack.Unmarshal(data, v)
}
