package versioned

import (
	"errors"
	"sort"
	"strconv"

	"github.com/vmihailenco/msgpack/v5"
)

// ErrFieldNotFound is returned by Record.Decode when the record does not
// contain a value for the requested field identity.
var ErrFieldNotFound = errors.New("versioned: field not found in record")

// Record is the version-neutral representation of a versioned message.
//
// Each entry maps a stable field identity (a positive integer declared by the
// caller) to the field's MessagePack-encoded value. A Record never carries Go
// field names: renaming a Go field therefore never affects the wire format and
// never triggers an implicit migration.
type Record map[int64]msgpack.RawMessage

// Has reports whether the record contains a value for field id.
func (r Record) Has(id int64) bool {
	_, ok := r[id]
	return ok
}

// IDs returns the field identities present in the record in ascending order.
func (r Record) IDs() []int64 {
	ids := make([]int64, 0, len(r))
	for id := range r {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Set stores raw as the value of field id, replacing any previous value.
func (r Record) Set(id int64, raw msgpack.RawMessage) {
	r[id] = raw
}

// Delete removes field id from the record.
func (r Record) Delete(id int64) {
	delete(r, id)
}

// Encode stores the MessagePack encoding of value under field id.
func (r Record) Encode(id int64, value interface{}) error {
	raw, err := msgpack.Marshal(value)
	if err != nil {
		return err
	}
	r[id] = raw
	return nil
}

// Decode decodes the value of field id into dst, which must be a pointer.
// It returns an error wrapping ErrFieldNotFound when the field is absent.
func (r Record) Decode(id int64, dst interface{}) error {
	raw, ok := r[id]
	if !ok {
		return &fieldNotFoundError{id: id}
	}
	return msgpack.Unmarshal(raw, dst)
}

type fieldNotFoundError struct {
	id int64
}

func (e *fieldNotFoundError) Error() string {
	return "versioned: field not found in record: " + strconv.FormatInt(e.id, 10)
}

func (e *fieldNotFoundError) Is(target error) bool {
	return target == ErrFieldNotFound
}
