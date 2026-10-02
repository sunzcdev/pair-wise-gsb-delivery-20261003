package versioning_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5/versioning"
)

type dupID struct {
	A string `version:"1"`
	B string `version:"1"`
}

type missingTag struct {
	A string `version:"1"`
	B string
}

type badTag struct {
	A string `version:"x"`
}

func TestRegisterRejectsInvalidTags(t *testing.T) {
	r := versioning.NewRegistry()
	require.Error(t, r.Register(1, dupID{}))
	require.Error(t, r.Register(1, missingTag{}))
	require.Error(t, r.Register(1, badTag{}))
}

func TestConversionValidation(t *testing.T) {
	r := versioning.NewRegistry()
	r.MustRegister(1, PersonV1{})
	r.MustRegister(2, PersonV2{})

	require.Error(t, r.Conversion(1, 1, func(versioning.Fields) (versioning.Fields, error) {
		return nil, nil
	}))
	require.Error(t, r.Conversion(1, 9, func(versioning.Fields) (versioning.Fields, error) {
		return nil, nil
	}))

	fn := func(in versioning.Fields) (versioning.Fields, error) { return in, nil }
	require.NoError(t, r.Conversion(1, 2, fn))
	require.Error(t, r.Conversion(1, 2, fn)) // duplicate edge
}

func TestUnknownVersionInData(t *testing.T) {
	r := versioning.NewRegistry()
	r.MustRegister(1, PersonV1{})
	r.MustRegister(2, PersonV2{})

	data := oldEnvelope(t, 7, map[uint64]interface{}{1: "x"})
	var got PersonV2
	err := r.Unmarshal(data, &got)
	require.Error(t, err)
	require.True(t, errors.Is(err, versioning.ErrUnknownVersion), "got %v", err)
}
