package versioning

import (
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

// Fields is the version-neutral representation of a record passed to a
// Conversion. Each entry is one field keyed by its stable wire ID.
//
// Values are kept as raw MessagePack so that a conversion can decode an old
// field with one Go type and encode a new field with a different type. A
// conversion never sees Go field names, so it cannot accidentally rely on name
// similarity; it only sees the stable numeric identities.
type Fields map[uint64]msgpack.RawMessage

// Get decodes the field with the given stable ID into v. It reports false
// (without touching v) when the field is absent or explicitly null.
func (f Fields) Get(id uint64, v interface{}) (bool, error) {
	raw, ok := f[id]
	if !ok || raw == nil {
		return false, nil
	}
	if err := msgpack.Unmarshal(raw, v); err != nil {
		return false, fmt.Errorf("versioning: decode field id %d: %w", id, err)
	}
	return true, nil
}

// Set stores v under the given stable ID, replacing any previous value.
func (f Fields) Set(id uint64, v interface{}) error {
	raw, err := msgpack.Marshal(v)
	if err != nil {
		return fmt.Errorf("versioning: encode field id %d: %w", id, err)
	}
	f[id] = raw
	return nil
}

// Rename copies a field from oldID to newID and drops the original. It is the
// mechanical part of evolving a schema; the identity change is always stated
// explicitly by the caller. It reports false when oldID is absent.
func (f Fields) Rename(oldID, newID uint64) bool {
	raw, ok := f[oldID]
	if !ok {
		return false
	}
	f[newID] = raw
	delete(f, oldID)
	return true
}

// Delete removes a field.
func (f Fields) Delete(id uint64) {
	delete(f, id)
}

// Has reports whether a field is present.
func (f Fields) Has(id uint64) bool {
	_, ok := f[id]
	return ok
}
