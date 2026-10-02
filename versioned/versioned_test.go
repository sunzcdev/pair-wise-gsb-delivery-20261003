package versioned_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
	"github.com/vmihailenco/msgpack/v5/versioned"
)

// ---------------------------------------------------------------------------
// Demo schema "person"
//
// v1 -> v2 contains, in one migration:
//   1. a field rename:        Name   (Go field) -> FullName, same stable id 1
//   2. a one-to-two split:    Name field value -> FirstName (id 2) + LastName (id 3)
//   3. a type change:         WeightKg float64 (id 4) -> WeightGrams int64 (id 5),
//      which fails for values that do not convert losslessly.
// ---------------------------------------------------------------------------

type PersonV1 struct {
	Name     string  `versioned:"1"`
	WeightKg float64 `versioned:"4"`
}

type PersonV2 struct {
	FullName    string `versioned:"1"` // renamed Go field, stable identity unchanged
	FirstName   string `versioned:"2"` // split out of v1.Name
	LastName    string `versioned:"3"` // split out of v1.Name
	WeightGrams int64  `versioned:"5"` // type changed; id 4 is intentionally retired

	InternalOnly string // no tag: never touches the wire
}

func newPersonRegistry(t *testing.T) *versioned.Registry {
	t.Helper()
	r := versioned.NewRegistry()
	r.MustRegister("person", 1, PersonV1{})
	r.MustRegister("person", 2, PersonV2{})
	r.MustRegisterMigration("person", 1, 2, personV1ToV2)
	return r
}

func personV1ToV2(in versioned.Record) (versioned.Record, error) {
	out := versioned.Record{}

	// (1) Rename: nothing to do at the wire level. The stable id 1 keeps
	// pointing at the same value even though the Go field is now FullName.
	if in.Has(1) {
		out.Set(1, in[1])
	}

	// (2) Split one field into two.
	var full string
	if err := in.Decode(1, &full); err != nil {
		return nil, err
	}
	first, last := splitName(full)
	if err := out.Encode(2, first); err != nil {
		return nil, err
	}
	if err := out.Encode(3, last); err != nil {
		return nil, err
	}

	// (3) Type change float64 kg -> int64 grams, with an explicit
	// non-lossless error.
	var kg float64
	if err := in.Decode(4, &kg); err != nil {
		return nil, err
	}
	grams, err := kgToGrams(kg)
	if err != nil {
		return nil, err
	}
	if err := out.Encode(5, grams); err != nil {
		return nil, err
	}

	return out, nil
}

func splitName(full string) (first, last string) {
	for i := 0; i < len(full); i++ {
		if full[i] == ' ' {
			return full[:i], full[i+1:]
		}
	}
	return full, ""
}

var errLossyWeight = errors.New("cannot convert fractional kg to whole grams losslessly")

func kgToGrams(kg float64) (int64, error) {
	grams := kg * 1000
	if grams != float64(int64(grams)) {
		return 0, fmt.Errorf("%w: %v kg", errLossyWeight, kg)
	}
	return int64(grams), nil
}

// ---------------------------------------------------------------------------

func TestRoundTripCurrentVersion(t *testing.T) {
	r := newPersonRegistry(t)

	p := PersonV2{
		FullName:     "Ada Lovelace",
		FirstName:    "Ada",
		LastName:     "Lovelace",
		WeightGrams:  63000,
		InternalOnly: "ignored",
	}
	data, err := r.Marshal(p)
	require.NoError(t, err)

	var got PersonV2
	require.NoError(t, r.Unmarshal(data, &got))
	require.Equal(t, "Ada Lovelace", got.FullName)
	require.Equal(t, "Ada", got.FirstName)
	require.Equal(t, "Lovelace", got.LastName)
	require.Equal(t, int64(63000), got.WeightGrams)
	require.Empty(t, got.InternalOnly, "untagged field is not part of the wire format")
}

func TestVersionIsEmbeddedInData(t *testing.T) {
	r := newPersonRegistry(t)

	data, err := r.Marshal(PersonV1{Name: "Ada Lovelace", WeightKg: 63})
	require.NoError(t, err)

	// The payload is a plain msgpack map keyed by integers; metadata lives in
	// the bytes themselves under reserved keys -1 and -2.
	var generic map[int64]interface{}
	require.NoError(t, msgpack.Unmarshal(data, &generic))
	require.Equal(t, "person", generic[-1])
	require.EqualValues(t, 1, generic[-2])
	require.Contains(t, generic, int64(1))
	require.Contains(t, generic, int64(4))
}

func TestReadHistoricalV1IntoV2_RenameAndSplit(t *testing.T) {
	r := newPersonRegistry(t)

	// Historical bytes written when v1 was current.
	oldData, err := r.Marshal(PersonV1{Name: "Ada Lovelace", WeightKg: 63})
	require.NoError(t, err)

	var got PersonV2
	require.NoError(t, r.Unmarshal(oldData, &got))

	// Rename via stable id.
	require.Equal(t, "Ada Lovelace", got.FullName)

	// One field split into two.
	require.Equal(t, "Ada", got.FirstName)
	require.Equal(t, "Lovelace", got.LastName)

	// Type change that is exactly representable.
	require.Equal(t, int64(63000), got.WeightGrams)
}

func TestTypeChangeNonLosslessReturnsError(t *testing.T) {
	r := newPersonRegistry(t)

	// 63.2505 kg -> 63250.5 grams, which has no whole-gram representation.
	oldData, err := r.Marshal(PersonV1{Name: "Ada Lovelace", WeightKg: 63.2505})
	require.NoError(t, err)

	var got PersonV2
	err = r.Unmarshal(oldData, &got)
	require.Error(t, err)
	require.True(t, errors.Is(err, errLossyWeight), "expected lossy error, got %v", err)
}

func TestMissingMigrationIsNotGuessed(t *testing.T) {
	r := versioned.NewRegistry()
	r.MustRegister("person", 1, PersonV1{})
	r.MustRegister("person", 2, PersonV2{})
	// Deliberately no 1->2 migration registered.

	oldData, err := r.Marshal(PersonV1{Name: "Ada", WeightKg: 60})
	require.NoError(t, err)

	var got PersonV2
	err = r.Unmarshal(oldData, &got)

	var notFound *versioned.MigrationNotFoundError
	require.True(t, errors.As(err, &notFound), "expected MigrationNotFoundError, got %v", err)
	require.Equal(t, "person", notFound.Schema)
	require.EqualValues(t, 1, notFound.From)
	require.EqualValues(t, 2, notFound.To)
}

// Fields are matched by stable integer identity only. A Go field that happens
// to share a name with an old field but carries a different id must not be
// treated as a rename.
type SameNameV1 struct {
	Score int `versioned:"10"`
}

type SameNameV2 struct {
	Score int `versioned:"20"` // same Go name, different stable identity
}

func TestNoNameBasedGuessing(t *testing.T) {
	r := versioned.NewRegistry()
	r.MustRegister("scores", 1, SameNameV1{})
	r.MustRegister("scores", 2, SameNameV2{})
	// No migration: identical Go field names must not imply compatibility.

	data, err := r.Marshal(SameNameV1{Score: 7})
	require.NoError(t, err)

	var got SameNameV2
	err = r.Unmarshal(data, &got)

	var notFound *versioned.MigrationNotFoundError
	require.True(t, errors.As(err, &notFound), "expected MigrationNotFoundError, got %v", err)

	// And even a migration output cannot smuggle through an id the target
	// version never declared; assignment is strict.
	r.MustRegisterMigration("scores", 1, 2, func(in versioned.Record) (versioned.Record, error) {
		// Buggy migration: forgets to remap id 10 -> 20.
		return versioned.Record{10: in[10]}, nil
	})
	err = r.Unmarshal(data, &got)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown field id 10")
}

func TestUnknownSchemaAndVersion(t *testing.T) {
	r := newPersonRegistry(t)

	data, err := r.Marshal(PersonV2{FullName: "X"})
	require.NoError(t, err)

	type Other struct {
		Foo string `versioned:"1"`
	}
	r2 := versioned.NewRegistry()
	r2.MustRegister("other", 2, Other{})

	var other Other
	err = r2.Unmarshal(data, &other)
	require.Error(t, err)
	require.Contains(t, err.Error(), "schema mismatch")
}

// The opt-in package must not alter classic msgpack behavior: structs that do
// not use the versioned registry keep their original, name-keyed encoding.
type plainLegacy struct {
	Name string
	Age  int
}

func TestLegacyMarshalUnmarshalUnchanged(t *testing.T) {
	before, err := msgpack.Marshal(plainLegacy{Name: "Grace", Age: 36})
	require.NoError(t, err)

	// Name-keyed map of two string keys, no integer metadata envelope.
	var asMap map[string]interface{}
	require.NoError(t, msgpack.Unmarshal(before, &asMap))
	require.Equal(t, "Grace", asMap["Name"])
	require.EqualValues(t, 36, asMap["Age"])
	_, hasVersionKey := asMap["-2"]
	require.False(t, hasVersionKey)

	var got plainLegacy
	require.NoError(t, msgpack.Unmarshal(before, &got))
	require.Equal(t, plainLegacy{Name: "Grace", Age: 36}, got)

	// Deterministic encoding across calls.
	again, err := msgpack.Marshal(plainLegacy{Name: "Grace", Age: 36})
	require.NoError(t, err)
	require.Equal(t, before, again)
}

func TestUnmarshalAsDynamicVersion(t *testing.T) {
	r := newPersonRegistry(t)

	data, err := r.Marshal(PersonV1{Name: "Ada Lovelace", WeightKg: 63})
	require.NoError(t, err)

	v, err := r.UnmarshalAs("person", 2, data)
	require.NoError(t, err)
	got, ok := v.(*PersonV2)
	require.True(t, ok)
	require.Equal(t, "Ada Lovelace", got.FullName)
	require.Equal(t, int64(63000), got.WeightGrams)
}

func TestVersionedBytesAreStableAndDeterministic(t *testing.T) {
	r := newPersonRegistry(t)

	a, err := r.Marshal(PersonV2{FullName: "Ada", FirstName: "Ada", WeightGrams: 1})
	require.NoError(t, err)
	b, err := r.Marshal(PersonV2{FullName: "Ada", FirstName: "Ada", WeightGrams: 1})
	require.NoError(t, err)
	require.Equal(t, a, b)
}

func TestMultiStepMigrationChain(t *testing.T) {
	r := versioned.NewRegistry()
	r.MustRegister("chain", 1, PersonV1{})
	r.MustRegister("chain", 2, PersonV2{})

	type PersonV3 struct {
		DisplayName string `versioned:"1"`
		WeightGrams int64  `versioned:"5"`
	}
	r.MustRegister("chain", 3, PersonV3{})

	// Register only 2->3; reading v1 still works by composing 1->2->3.
	r.MustRegisterMigration("chain", 1, 2, personV1ToV2)
	r.MustRegisterMigration("chain", 2, 3, func(in versioned.Record) (versioned.Record, error) {
		return versioned.Record{1: in[1], 5: in[5]}, nil
	})

	data, err := r.Marshal(PersonV1{Name: "Ada Lovelace", WeightKg: 63})
	require.NoError(t, err)

	var got PersonV3
	require.NoError(t, r.Unmarshal(data, &got))
	require.Equal(t, "Ada Lovelace", got.DisplayName)
	require.Equal(t, int64(63000), got.WeightGrams)
}
