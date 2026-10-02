package versioning_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
	"github.com/vmihailenco/msgpack/v5/versioning"
)

// oldEnvelope builds historical on-disk bytes directly, simulating a record
// written by an older deployment. The schema version is the first array
// element, i.e. part of the data itself.
func oldEnvelope(t *testing.T, ver uint64, fields map[uint64]interface{}) []byte {
	t.Helper()
	raw := make(map[uint64]msgpack.RawMessage, len(fields))
	for id, v := range fields {
		b, err := msgpack.Marshal(v)
		require.NoError(t, err)
		raw[id] = b
	}
	b, err := msgpack.Marshal([]interface{}{ver, raw})
	require.NoError(t, err)
	return b
}

// ---------------------------------------------------------------------------
// Demo 1: renaming a field.
//
// v1 calls the email field EmailAddr, v2 renames it to Email. Because the wire
// identity is the stable numeric id 2 (not the Go name), no field-level
// migration is needed. The only explicit registration is the version hop.
// ---------------------------------------------------------------------------

type ContactV1 struct {
	ID        int64  `version:"1"`
	EmailAddr string `version:"2"`
}

type ContactV2 struct {
	ID    int64  `version:"1"`
	Email string `version:"2"`
}

func TestDemo_Rename(t *testing.T) {
	r := versioning.NewRegistry()
	r.MustRegister(1, ContactV1{})
	r.MustRegister(2, ContactV2{})
	// Explicit version hop; it is an identity conversion because id 2 moved
	// with the field rather than being matched by name.
	r.MustConversion(1, 2, func(in versioning.Fields) (versioning.Fields, error) {
		return in, nil
	})

	historical := oldEnvelope(t, 1, map[uint64]interface{}{
		1: int64(7),
		2: "a@example.com",
	})

	var got ContactV2
	require.NoError(t, r.Unmarshal(historical, &got))
	require.Equal(t, ContactV2{ID: 7, Email: "a@example.com"}, got)

	// Freshly encoded data is stamped with version 2 in the envelope.
	fresh, err := r.Marshal(ContactV2{ID: 8, Email: "b@example.com"})
	require.NoError(t, err)

	var round ContactV2
	require.NoError(t, r.Unmarshal(fresh, &round))
	require.Equal(t, ContactV2{ID: 8, Email: "b@example.com"}, round)
}

// ---------------------------------------------------------------------------
// Demo 2: splitting one field into two.
//
// v1 has FullName (id 1). v2 drops it and introduces FirstName (id 2) and
// LastName (id 3). Name similarity does not tell the library how to split, so
// the caller supplies the exact conversion.
// ---------------------------------------------------------------------------

type PersonV1 struct {
	FullName string `version:"1"`
}

type PersonV2 struct {
	FirstName string `version:"2"`
	LastName  string `version:"3"`
}

func TestDemo_SplitField(t *testing.T) {
	r := versioning.NewRegistry()
	r.MustRegister(1, PersonV1{})
	r.MustRegister(2, PersonV2{})

	r.MustConversion(1, 2, func(in versioning.Fields) (versioning.Fields, error) {
		out := versioning.Fields{}
		var full string
		if _, err := in.Get(1, &full); err != nil {
			return nil, err
		}
		first, last, _ := strings.Cut(full, " ")
		if err := out.Set(2, first); err != nil {
			return nil, err
		}
		if err := out.Set(3, last); err != nil {
			return nil, err
		}
		return out, nil
	})

	historical := oldEnvelope(t, 1, map[uint64]interface{}{
		1: "Grace Hopper",
	})

	var got PersonV2
	require.NoError(t, r.Unmarshal(historical, &got))
	require.Equal(t, PersonV2{FirstName: "Grace", LastName: "Hopper"}, got)
}

// Without an explicit split conversion there is no path, and the library does
// not invent one from the names.
func TestDemo_SplitField_RequiresExplicitConversion(t *testing.T) {
	r := versioning.NewRegistry()
	r.MustRegister(1, PersonV1{})
	r.MustRegister(2, PersonV2{})

	historical := oldEnvelope(t, 1, map[uint64]interface{}{1: "Grace Hopper"})

	var got PersonV2
	err := r.Unmarshal(historical, &got)
	require.Error(t, err)
	require.True(t, errors.Is(err, versioning.ErrNoConversion), "got %v", err)
}

// ---------------------------------------------------------------------------
// Demo 3: a type change that is not lossless.
//
// v1 stores a measurement as float64 (id 1); v2 stores it as int64. A whole
// number converts losslessly, a fractional value must not silently truncate,
// so the conversion returns an error wrapping ErrLossy.
// ---------------------------------------------------------------------------

type MeasurementV1 struct {
	Value float64 `version:"1"`
}

type MeasurementV2 struct {
	Value int64 `version:"1"`
}

func convertMeasurement(in versioning.Fields) (versioning.Fields, error) {
	var f float64
	if _, err := in.Get(1, &f); err != nil {
		return nil, err
	}
	if f != float64(int64(f)) {
		return nil, versioning.LossyError(1, "fractional value cannot be represented as int")
	}
	out := versioning.Fields{}
	if err := out.Set(1, int64(f)); err != nil {
		return nil, err
	}
	return out, nil
}

func TestDemo_TypeChange_Lossless(t *testing.T) {
	r := versioning.NewRegistry()
	r.MustRegister(1, MeasurementV1{})
	r.MustRegister(2, MeasurementV2{})
	r.MustConversion(1, 2, convertMeasurement)

	historical := oldEnvelope(t, 1, map[uint64]interface{}{1: 42.0})

	var got MeasurementV2
	require.NoError(t, r.Unmarshal(historical, &got))
	require.Equal(t, MeasurementV2{Value: 42}, got)
}

func TestDemo_TypeChange_LossyError(t *testing.T) {
	r := versioning.NewRegistry()
	r.MustRegister(1, MeasurementV1{})
	r.MustRegister(2, MeasurementV2{})
	r.MustConversion(1, 2, convertMeasurement)

	historical := oldEnvelope(t, 1, map[uint64]interface{}{1: 3.5})

	var got MeasurementV2
	err := r.Unmarshal(historical, &got)
	require.Error(t, err)
	require.True(t, errors.Is(err, versioning.ErrLossy), "got %v", err)
	require.Contains(t, err.Error(), "fractional value")
}

// ---------------------------------------------------------------------------
// The version lives in the data: plain structs that never opt in keep the exact
// bytes of msgpack.Marshal, and versioned envelopes carry their own version.
// ---------------------------------------------------------------------------

type Plain struct {
	Name string
	Age  int
}

func TestPlainMarshalUnchanged(t *testing.T) {
	r := versioning.NewRegistry()

	in := Plain{Name: "Ada", Age: 36}
	got, err := r.Marshal(in)
	require.NoError(t, err)

	want, err := msgpack.Marshal(in)
	require.NoError(t, err)
	require.Equal(t, want, got)

	var decoded Plain
	require.NoError(t, msgpack.Unmarshal(got, &decoded))
	require.Equal(t, in, decoded)
}

func TestVersionIsEmbeddedInData(t *testing.T) {
	r := versioning.NewRegistry()
	r.MustRegister(2, ContactV2{})

	b, err := r.Marshal(ContactV2{ID: 1, Email: "x@example.com"})
	require.NoError(t, err)

	// The envelope is [version, fields]: read the leading array header and
	// version straight off the bytes.
	dec := msgpack.NewDecoder(bytes.NewReader(b))
	n, err := dec.DecodeArrayLen()
	require.NoError(t, err)
	require.Equal(t, 2, n)
	var ver uint64
	require.NoError(t, dec.Decode(&ver))
	require.EqualValues(t, 2, ver)

	// Plain (non-envelope) bytes are not accepted as a versioned record.
	plain, err := msgpack.Marshal(map[string]interface{}{"ID": 1, "Email": "x"})
	require.NoError(t, err)
	var c ContactV2
	err = r.Unmarshal(plain, &c)
	require.Error(t, err)
	require.True(t, errors.Is(err, versioning.ErrBadEnvelope), "got %v", err)
}

func TestUnregisteredUnmarshalRejected(t *testing.T) {
	r := versioning.NewRegistry()
	var p Plain
	err := r.Unmarshal([]byte{0xc0}, &p)
	require.Error(t, err)
	require.True(t, errors.Is(err, versioning.ErrNotRegistered), "got %v", err)
}

// ---------------------------------------------------------------------------
// Multi-hop migrations run each registered conversion in order.
// ---------------------------------------------------------------------------

type ChainV1 struct {
	A string `version:"1"`
}
type ChainV2 struct {
	A string `version:"1"`
	B string `version:"2"`
}
type ChainV3 struct {
	A  string `version:"1"`
	B  string `version:"2"`
	ID int64  `version:"3"`
}

func TestMultiHop(t *testing.T) {
	r := versioning.NewRegistry()
	r.MustRegister(1, ChainV1{})
	r.MustRegister(2, ChainV2{})
	r.MustRegister(3, ChainV3{})

	r.MustConversion(1, 2, func(in versioning.Fields) (versioning.Fields, error) {
		in.Set(2, "from-v1")
		return in, nil
	})
	r.MustConversion(2, 3, func(in versioning.Fields) (versioning.Fields, error) {
		in.Set(3, int64(99))
		return in, nil
	})

	historical := oldEnvelope(t, 1, map[uint64]interface{}{1: "hello"})

	var got ChainV3
	require.NoError(t, r.Unmarshal(historical, &got))
	require.Equal(t, ChainV3{A: "hello", B: "from-v1", ID: 99}, got)
}
