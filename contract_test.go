package paseto_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"
	"github.com/stretchr/testify/require"
)

func tokenFromJSON(t *testing.T, data string) paseto.Token {
	t.Helper()

	token, err := paseto.NewTokenFromClaimsJSON([]byte(data), nil)
	require.NoError(t, err)

	return *token
}

func claimError(t *testing.T, err error) paseto.ClaimError {
	t.Helper()

	var claimErr paseto.ClaimError
	require.ErrorAs(t, err, &claimErr)

	return claimErr
}

func TestContractAcceptsDeclaredTypes(t *testing.T) {
	contract, err := paseto.NewClaimsContract([]paseto.ObjectField{
		{Name: "sub", Spec: paseto.RequiredString()},
		{Name: "admin", Spec: paseto.Boolean()},
		{Name: "count", Spec: paseto.Number()},
		{Name: "level", Spec: paseto.Integer()},
		{Name: "roles", Spec: paseto.RequiredArray(paseto.String())},
		{
			Name: "meta",
			Spec: paseto.RequiredObject([]paseto.ObjectField{
				{Name: "org", Spec: paseto.String()},
				{Name: "tags", Spec: paseto.Array(paseto.Integer())},
			}),
		},
	})
	require.NoError(t, err)

	token := tokenFromJSON(t, `{
		"sub": "user-1",
		"admin": true,
		"count": 1.5,
		"level": 3,
		"roles": ["a", "b"],
		"meta": {"org": "acme", "tags": [1, 2], "extra": true}
	}`)

	require.NoError(t, contract.Validate(token))
}

func TestContractMissingRequired(t *testing.T) {
	contract, err := paseto.NewClaimsContract([]paseto.ObjectField{
		{Name: "sub", Spec: paseto.RequiredString()},
		{Name: "admin", Spec: paseto.Boolean()},
	})
	require.NoError(t, err)

	err = contract.Validate(tokenFromJSON(t, `{"admin": true}`))
	claimErr := claimError(t, err)
	require.Equal(t, "sub", claimErr.Path)
	require.Equal(t, paseto.ClaimMissing, claimErr.Kind)
	require.True(t, errors.Is(err, paseto.ErrClaimMissing))
}

func TestContractNullIsDistinctFromMissing(t *testing.T) {
	contract, err := paseto.NewClaimsContract([]paseto.ObjectField{
		{Name: "sub", Spec: paseto.RequiredString()},
		{Name: "note", Spec: paseto.String()},
	})
	require.NoError(t, err)

	err = contract.Validate(tokenFromJSON(t, `{"sub": null}`))
	claimErr := claimError(t, err)
	require.Equal(t, "sub", claimErr.Path)
	require.Equal(t, paseto.ClaimNull, claimErr.Kind)

	// an optional claim may be null or absent
	require.NoError(t, contract.Validate(tokenFromJSON(t, `{"sub": "x", "note": null}`)))
	require.NoError(t, contract.Validate(tokenFromJSON(t, `{"sub": "x"}`)))
}

func TestContractTypeMismatch(t *testing.T) {
	contract, err := paseto.NewClaimsContract([]paseto.ObjectField{
		{Name: "admin", Spec: paseto.Boolean()},
		{Name: "level", Spec: paseto.Integer()},
		{Name: "roles", Spec: paseto.Array(paseto.String())},
	})
	require.NoError(t, err)

	err = contract.Validate(tokenFromJSON(t, `{"admin": "yes"}`))
	claimErr := claimError(t, err)
	require.Equal(t, "admin", claimErr.Path)
	require.Equal(t, paseto.ClaimTypeMismatch, claimErr.Kind)
	require.Equal(t, paseto.ClaimBoolean, claimErr.Expected)
	require.Equal(t, paseto.ClaimString, claimErr.Actual)

	err = contract.Validate(tokenFromJSON(t, `{"level": 1.5}`))
	claimErr = claimError(t, err)
	require.Equal(t, "level", claimErr.Path)
	require.Equal(t, paseto.ClaimTypeMismatch, claimErr.Kind)

	err = contract.Validate(tokenFromJSON(t, `{"roles": ["ok", 3]}`))
	claimErr = claimError(t, err)
	require.Equal(t, "roles[1]", claimErr.Path)
	require.Equal(t, paseto.ClaimTypeMismatch, claimErr.Kind)
}

func TestContractNestedPath(t *testing.T) {
	contract, err := paseto.NewClaimsContract([]paseto.ObjectField{
		{
			Name: "user",
			Spec: paseto.Object([]paseto.ObjectField{
				{Name: "roles", Spec: paseto.Array(paseto.Object([]paseto.ObjectField{
					{Name: "id", Spec: paseto.RequiredInteger()},
				}))},
			}),
		},
	})
	require.NoError(t, err)

	err = contract.Validate(tokenFromJSON(t, `{"user": {"roles": [{"id": 1}, {"id": "two"}]}}`))
	claimErr := claimError(t, err)
	require.Equal(t, "user.roles[1].id", claimErr.Path)
	require.Equal(t, paseto.ClaimTypeMismatch, claimErr.Kind)
}

func TestContractRejectsUnknownFields(t *testing.T) {
	contract, err := paseto.NewClaimsContract(
		[]paseto.ObjectField{
			{Name: "sub", Spec: paseto.String()},
			{
				Name: "meta",
				Spec: paseto.Object([]paseto.ObjectField{
					{Name: "org", Spec: paseto.String()},
				}),
			},
		},
		paseto.RejectUnknownFields(),
	)
	require.NoError(t, err)

	err = contract.Validate(tokenFromJSON(t, `{"sub": "x", "rogue": true}`))
	claimErr := claimError(t, err)
	require.Equal(t, "rogue", claimErr.Path)
	require.Equal(t, paseto.ClaimUnknown, claimErr.Kind)

	// nested objects inherit the root policy
	err = contract.Validate(tokenFromJSON(t, `{"meta": {"org": "acme", "nope": 1}}`))
	claimErr = claimError(t, err)
	require.Equal(t, "meta.nope", claimErr.Path)
	require.Equal(t, paseto.ClaimUnknown, claimErr.Kind)

	// opt back in to unknown fields for a nested object
	permissive, err := paseto.NewClaimsContract([]paseto.ObjectField{
		{
			Name: "meta",
			Spec: paseto.Object(
				[]paseto.ObjectField{{Name: "org", Spec: paseto.String()}},
				paseto.AllowObjectUnknownFields(),
			),
		},
	}, paseto.RejectUnknownFields())
	require.NoError(t, err)
	require.NoError(t, permissive.Validate(tokenFromJSON(t, `{"meta": {"org": "acme", "ok": 1}}`)))
}

func TestContractAllowsUnknownByDefault(t *testing.T) {
	contract, err := paseto.NewClaimsContract([]paseto.ObjectField{
		{Name: "sub", Spec: paseto.String()},
	})
	require.NoError(t, err)

	require.NoError(t, contract.Validate(tokenFromJSON(t, `{"sub": "x", "anything": [1, 2, 3]}`)))
}

func TestContractLargeIntegerPrecision(t *testing.T) {
	// 9007199254740993 cannot be represented exactly in float64; only the raw
	// JSON digits make a reliable integer check possible.
	const big = "9007199254740993"

	contract, err := paseto.NewClaimsContract([]paseto.ObjectField{
		{Name: "id", Spec: paseto.RequiredInteger()},
	})
	require.NoError(t, err)

	require.NoError(t, contract.Validate(tokenFromJSON(t, `{"id": `+big+`}`)))

	token := tokenFromJSON(t, `{"id": `+big+`}`)

	type out struct {
		ID json.Number
	}

	var output out
	require.NoError(t, contract.Decode(token, &output))
	require.Equal(t, big, string(output.ID))
}

func TestContractNumberAcceptsFractionsAndExponents(t *testing.T) {
	contract, err := paseto.NewClaimsContract([]paseto.ObjectField{
		{Name: "v", Spec: paseto.RequiredNumber()},
	})
	require.NoError(t, err)

	require.NoError(t, contract.Validate(tokenFromJSON(t, `{"v": 1.25e3}`)))

	err = contract.Validate(tokenFromJSON(t, `{"v": "1"}`))
	require.Equal(t, paseto.ClaimTypeMismatch, claimError(t, err).Kind)
}

func TestContractArrayNullElements(t *testing.T) {
	contract, err := paseto.NewClaimsContract([]paseto.ObjectField{
		{Name: "roles", Spec: paseto.Array(paseto.String())},
	})
	require.NoError(t, err)

	err = contract.Validate(tokenFromJSON(t, `{"roles": ["a", null]}`))
	claimErr := claimError(t, err)
	require.Equal(t, "roles[1]", claimErr.Path)
	require.Equal(t, paseto.ClaimNull, claimErr.Kind)
}

func TestContractBuildErrors(t *testing.T) {
	_, err := paseto.NewClaimsContract([]paseto.ObjectField{
		{Name: "sub", Spec: paseto.String()},
		{Name: "sub", Spec: paseto.Number()},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "declared more than once")

	_, err = paseto.NewClaimsContract([]paseto.ObjectField{
		{Name: "", Spec: paseto.String()},
	})
	require.Error(t, err)

	_, err = paseto.NewClaimsContract([]paseto.ObjectField{
		{Name: "roles", Spec: paseto.Array(nil)},
	})
	require.Error(t, err)

	_, err = paseto.NewClaimsContract([]paseto.ObjectField{
		{Name: "meta", Spec: paseto.Object([]paseto.ObjectField{
			{Name: "x", Spec: nil},
		})},
	})
	require.Error(t, err)

	require.Panics(t, func() {
		paseto.MustClaimsContract([]paseto.ObjectField{
			{Name: "sub", Spec: paseto.String()},
			{Name: "sub", Spec: paseto.String()},
		})
	})
}

func TestContractErrorOmitsValuesByDefault(t *testing.T) {
	contract, err := paseto.NewClaimsContract([]paseto.ObjectField{
		{Name: "secret", Spec: paseto.RequiredString()},
	})
	require.NoError(t, err)

	err = contract.Validate(tokenFromJSON(t, `{"secret": 42}`))
	require.NotContains(t, err.Error(), "42")

	verbose, err := paseto.NewClaimsContract([]paseto.ObjectField{
		{Name: "secret", Spec: paseto.RequiredString()},
	}, paseto.IncludeClaimValues())
	require.NoError(t, err)

	err = verbose.Validate(tokenFromJSON(t, `{"secret": 42}`))
	require.Contains(t, err.Error(), "42")
}

func TestContractRuleRunsAfterCrypto(t *testing.T) {
	token := paseto.NewToken()
	token.SetExpiration(time.Now().Add(time.Minute))
	token.SetString("sub", "user-1")

	contract := paseto.MustClaimsContract([]paseto.ObjectField{
		{Name: "sub", Spec: paseto.RequiredString()},
		{Name: "scope", Spec: paseto.RequiredString()},
	})

	key := paseto.NewV4SymmetricKey()
	signed := token.V4Encrypt(key, nil)

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddRule(contract.Rule())

	_, err := parser.ParseV4Local(key, signed, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, &paseto.RuleError{})

	var claimErr paseto.ClaimError
	require.ErrorAs(t, err, &claimErr)
	require.Equal(t, "scope", claimErr.Path)

	// a valid token passes the same rule
	token.SetString("scope", "read")
	signed = token.V4Encrypt(key, nil)

	parser2 := paseto.NewParserWithoutExpiryCheck()
	parser2.AddRule(contract.Rule())

	_, err = parser2.ParseV4Local(key, signed, nil)
	require.NoError(t, err)
}

func TestContractDepthLimit(t *testing.T) {
	var nested paseto.ClaimSpec = paseto.RequiredInteger()
	for i := 0; i < paseto.MaxContractDepth+5; i++ {
		nested = paseto.Array(nested)
	}

	contract := paseto.MustClaimsContract([]paseto.ObjectField{
		{Name: "nested", Spec: nested},
	})

	var builder strings.Builder
	builder.WriteString(`{"nested": `)
	for i := 0; i < paseto.MaxContractDepth+5; i++ {
		builder.WriteString(`[`)
	}
	builder.WriteString(`1`)
	for i := 0; i < paseto.MaxContractDepth+5; i++ {
		builder.WriteString(`]`)
	}
	builder.WriteString(`}`)

	err := contract.Validate(tokenFromJSON(t, builder.String()))
	claimErr := claimError(t, err)
	require.Equal(t, paseto.ClaimTooDeep, claimErr.Kind)
}
