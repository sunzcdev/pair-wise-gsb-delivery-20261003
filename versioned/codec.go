package versioned

import (
	"bytes"
	"fmt"
	"reflect"

	"github.com/vmihailenco/msgpack/v5"
)

// Marshal encodes v into a self-describing versioned record.
//
// The wire payload is a MessagePack map containing the schema id and version
// under reserved keys (-1 and -2) plus one entry per tagged field keyed by its
// stable integer identity. The version therefore travels inside the data and
// never has to be guessed from deployment configuration.
func (r *Registry) Marshal(v interface{}) ([]byte, error) {
	if v == nil {
		return nil, fmt.Errorf("versioned: Marshal(nil)")
	}

	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil, fmt.Errorf("versioned: Marshal(nil %T)", v)
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, fmt.Errorf("versioned: Marshal expects a struct, got %s", rv.Kind())
	}

	spec, err := r.lookupSpecByType(rv)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)

	if err := enc.EncodeMapLen(2 + len(spec.fields)); err != nil {
		return nil, err
	}

	// Reserved metadata keys first. Negative fixmap keys encode as a single
	// byte (-1/-2), keeping the envelope compact.
	if err := enc.EncodeInt64(schemaKey); err != nil {
		return nil, err
	}
	if err := enc.EncodeString(spec.schema); err != nil {
		return nil, err
	}
	if err := enc.EncodeInt64(versionKey); err != nil {
		return nil, err
	}
	if err := enc.EncodeUint64(uint64(spec.version)); err != nil {
		return nil, err
	}

	// Field keys in ascending order for deterministic output.
	ids := make([]int64, 0, len(spec.fields))
	for id := range spec.fields {
		ids = append(ids, id)
	}
	sortInt64(ids)

	for _, id := range ids {
		field := spec.fields[id]
		if err := enc.EncodeInt64(id); err != nil {
			return nil, err
		}
		if err := enc.EncodeValue(rv.Field(field.index)); err != nil {
			return nil, fmt.Errorf("versioned: encoding %s field %s (id %d): %w",
				spec.typ, field.name, id, err)
		}
	}

	return buf.Bytes(), nil
}

// Unmarshal decodes data into dst, which must be a pointer to a registered
// struct. When the stored version differs from dst's version, Unmarshal runs
// the explicitly registered migration chain to convert the record.
func (r *Registry) Unmarshal(data []byte, dst interface{}) error {
	if dst == nil {
		return fmt.Errorf("versioned: Unmarshal(nil destination)")
	}

	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("versioned: Unmarshal destination must be a non-nil pointer, got %T", dst)
	}
	target, err := r.lookupSpecByType(rv.Elem())
	if err != nil {
		return err
	}

	rec, schemaID, storedVersion, err := decodeEnvelope(data)
	if err != nil {
		return err
	}
	if schemaID != target.schema {
		return &FormatError{Reason: fmt.Sprintf(
			"schema mismatch: data is %q, destination expects %q", schemaID, target.schema)}
	}

	if storedVersion != target.version {
		rec, err = r.migrate(schemaID, storedVersion, target.version, rec)
		if err != nil {
			return err
		}
	}

	return assignRecord(rv.Elem(), target, rec)
}

// UnmarshalAs is like Unmarshal but lets the caller state which registered
// schema/version they are reading, returning a fresh value of the registered
// type. It is useful when the destination type is chosen dynamically.
func (r *Registry) UnmarshalAs(schema string, version uint32, data []byte) (interface{}, error) {
	target, err := r.lookupVersion(schema, version)
	if err != nil {
		return nil, err
	}
	dst := reflect.New(target.typ).Interface()
	if err := r.Unmarshal(data, dst); err != nil {
		return nil, err
	}
	return dst, nil
}

func (r *Registry) migrate(schema string, from, to uint32, rec Record) (Record, error) {
	// Validate the source record against the registered source version before
	// touching any migration function: unknown field ids are rejected, never
	// silently dropped or matched by Go field name.
	source, err := r.lookupVersion(schema, from)
	if err != nil {
		return nil, err
	}
	if err := validateRecord(rec, source); err != nil {
		return nil, err
	}

	path, err := r.migrationPath(schema, from, to)
	if err != nil {
		return nil, err
	}

	current := from
	for _, step := range path {
		next, fnErr := step.fn(rec)
		if fnErr != nil {
			return nil, fmt.Errorf("versioned: migration %s v%d->v%d failed: %w",
				schema, current, step.to, fnErr)
		}
		rec = next
		current = step.to
		if step.to != to {
			mid, lookupErr := r.lookupVersion(schema, current)
			if lookupErr != nil {
				return nil, lookupErr
			}
			if validateErr := validateRecord(rec, mid); validateErr != nil {
				return nil, validateErr
			}
		}
	}
	return rec, nil
}

func validateRecord(rec Record, spec *versionSpec) error {
	for id := range rec {
		if _, ok := spec.fields[id]; !ok {
			return &FormatError{Reason: fmt.Sprintf(
				"unknown field id %d in %s v%d (fields are matched by stable id, never by name)",
				id, spec.schema, spec.version)}
		}
	}
	return nil
}

// assignRecord writes each field of rec into the matching struct field.
func assignRecord(dst reflect.Value, spec *versionSpec, rec Record) error {
	if dst.Kind() == reflect.Ptr {
		dst = dst.Elem()
	}
	if err := validateRecord(rec, spec); err != nil {
		return err
	}
	for id, field := range spec.fields {
		raw, ok := rec[id]
		if !ok {
			continue // missing field keeps the zero value already present
		}
		fv := dst.Field(field.index)
		if err := decodeIntoField(raw, fv); err != nil {
			return fmt.Errorf("versioned: decoding %s field %s (id %d): %w",
				spec.typ, field.name, id, err)
		}
	}
	return nil
}

func decodeIntoField(raw msgpack.RawMessage, fv reflect.Value) error {
	if !fv.CanSet() {
		return fmt.Errorf("field cannot be set")
	}
	ptr := reflect.New(fv.Type()).Interface()
	if err := msgpack.Unmarshal(raw, ptr); err != nil {
		return err
	}
	fv.Set(reflect.ValueOf(ptr).Elem())
	return nil
}

// decodeEnvelope parses the self-describing map header and metadata.
func decodeEnvelope(data []byte) (Record, string, uint32, error) {
	dec := msgpack.NewDecoder(bytes.NewReader(data))

	n, err := dec.DecodeMapLen()
	if err != nil {
		return nil, "", 0, &FormatError{Reason: "payload is not a msgpack map: " + err.Error()}
	}
	if n < 0 {
		return nil, "", 0, &FormatError{Reason: "payload map is nil"}
	}

	rec := make(Record)
	var schemaID string
	var version uint32
	var haveSchema, haveVersion bool

	for i := 0; i < n; i++ {
		key, err := dec.DecodeInt64()
		if err != nil {
			return nil, "", 0, &FormatError{Reason: "map key is not an integer: " + err.Error()}
		}

		switch key {
		case schemaKey:
			s, err := dec.DecodeString()
			if err != nil {
				return nil, "", 0, &FormatError{Reason: "schema id is not a string: " + err.Error()}
			}
			schemaID = s
			haveSchema = true
		case versionKey:
			v, err := dec.DecodeUint64()
			if err != nil {
				return nil, "", 0, &FormatError{Reason: "version is not an unsigned integer: " + err.Error()}
			}
			if v == 0 {
				return nil, "", 0, &FormatError{Reason: "version must be >= 1"}
			}
			version = uint32(v)
			haveVersion = true
		default:
			if key < 1 {
				return nil, "", 0, &FormatError{Reason: fmt.Sprintf("reserved key %d is not supported", key)}
			}
			raw, err := dec.DecodeRaw()
			if err != nil {
				return nil, "", 0, &FormatError{Reason: "field value: " + err.Error()}
			}
			if _, dup := rec[key]; dup {
				return nil, "", 0, &FormatError{Reason: fmt.Sprintf("duplicate field id %d", key)}
			}
			rec[key] = raw
		}
	}

	if !haveSchema {
		return nil, "", 0, &FormatError{Reason: "missing schema id (key -1)"}
	}
	if !haveVersion {
		return nil, "", 0, &FormatError{Reason: "missing version (key -2)"}
	}

	return rec, schemaID, version, nil
}

func sortInt64(xs []int64) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}
