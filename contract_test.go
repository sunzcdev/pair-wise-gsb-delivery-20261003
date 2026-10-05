package paseto_test

import (
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

func TestContractAcceptsMatchingClaims(t *testing.T) {
	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"sub":     paseto.StringSpec(),
		"admin":   paseto.BooleanSpec(),
		"score":   paseto.NumberSpec(),
		"counter": paseto.IntegerSpec(),
		"roles":   paseto.ArraySpec(paseto.StringSpec()),
		"meta": paseto.ObjectSpec(map[string]paseto.FieldSpec{
			"nested": paseto.StringSpec(),
		}),
	})
	require.NoError(t, err)

	token := tokenFromJSON(t, `{
		"sub": "alice",
		"admin": true,
		"score": 1.5,
		"counter": 42,
		"roles": ["a", "b"],
		"meta": {"nested": "v"},
		"extra": "allowed by default"
	}`)

	require.NoError(t, contract.CheckClaims(token))
}

func TestContractMissingRequired(t *testing.T) {
	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"sub": paseto.StringSpec(),
	})
	require.NoError(t, err)

	err = contract.CheckClaims(tokenFromJSON(t, `{}`))
	var claimErr paseto.ClaimError
	require.ErrorAs(t, err, &claimErr)
	require.Equal(t, "sub", claimErr.Path)
	require.Equal(t, paseto.MissingClaim, claimErr.Category)
}

func TestContractMissingOptionalAllowed(t *testing.T) {
	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"sub": paseto.StringSpec().Optional(),
	})
	require.NoError(t, err)

	require.NoError(t, contract.CheckClaims(tokenFromJSON(t, `{}`)))
}

func TestContractNullDistinctFromMissing(t *testing.T) {
	required, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"sub": paseto.StringSpec(),
	})
	require.NoError(t, err)

	var claimErr paseto.ClaimError
	err = required.CheckClaims(tokenFromJSON(t, `{"sub": null}`))
	require.ErrorAs(t, err, &claimErr)
	require.Equal(t, paseto.NullClaim, claimErr.Category)
	require.Equal(t, "sub", claimErr.Path)

	optional, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"sub": paseto.StringSpec().Optional(),
	})
	require.NoError(t, err)
	err = optional.CheckClaims(tokenFromJSON(t, `{"sub": null}`))
	require.ErrorAs(t, err, &claimErr)
	require.Equal(t, paseto.NullClaim, claimErr.Category)
}

func TestContractWrongTypes(t *testing.T) {
	cases := []struct {
		name     string
		spec     paseto.FieldSpec
		json     string
		expected paseto.ClaimKind
	}{
		{"string", paseto.StringSpec(), `{"k": 1}`, paseto.String},
		{"boolean", paseto.BooleanSpec(), `{"k": "x"}`, paseto.Boolean},
		{"number from string", paseto.NumberSpec(), `{"k": "x"}`, paseto.Number},
		{"number from bool", paseto.NumberSpec(), `{"k": true}`, paseto.Number},
		{"integer from fraction", paseto.IntegerSpec(), `{"k": 1.5}`, paseto.Integer},
		{"array from object", paseto.ArraySpec(paseto.StringSpec()), `{"k": {}}`, paseto.Array},
		{"object from array", paseto.ObjectSpec(map[string]paseto.FieldSpec{"a": paseto.StringSpec()}), `{"k": []}`, paseto.Object},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{"k": tc.spec})
			require.NoError(t, err)

			err = contract.CheckClaims(tokenFromJSON(t, tc.json))
			var claimErr paseto.ClaimError
			require.ErrorAs(t, err, &claimErr)
			require.Equal(t, paseto.WrongType, claimErr.Category)
			require.Equal(t, tc.expected, claimErr.Expected)
			require.Equal(t, "k", claimErr.Path)
		})
	}
}

func TestContractIntegerBeyondFloat64PrecisionAccepted(t *testing.T) {
	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"id": paseto.IntegerSpec(),
	})
	require.NoError(t, err)

	// 9007199254740993 cannot be represented exactly as a float64.
	require.NoError(t, contract.CheckClaims(tokenFromJSON(t, `{"id": 9007199254740993}`)))
}

func TestContractFractionBeyondFloat64PrecisionRejected(t *testing.T) {
	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"id": paseto.IntegerSpec(),
	})
	require.NoError(t, err)

	// This fractional value would round to an integer if decoded via
	// float64; the contract must detect the fraction without rounding first.
	err = contract.CheckClaims(tokenFromJSON(t, `{"id": 9007199254740993.5}`))
	var claimErr paseto.ClaimError
	require.ErrorAs(t, err, &claimErr)
	require.Equal(t, paseto.WrongType, claimErr.Category)
	require.Equal(t, paseto.Integer, claimErr.Expected)
}

func TestContractScientificIntegerAccepted(t *testing.T) {
	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"n": paseto.IntegerSpec(),
	})
	require.NoError(t, err)

	require.NoError(t, contract.CheckClaims(tokenFromJSON(t, `{"n": 1e3}`)))

	err = contract.CheckClaims(tokenFromJSON(t, `{"n": 1.25e1}`))
	var claimErr paseto.ClaimError
	require.ErrorAs(t, err, &claimErr)
	require.Equal(t, paseto.WrongType, claimErr.Category)
}

func TestContractNumberKeepsRawValue(t *testing.T) {
	token := tokenFromJSON(t, `{"id": 9007199254740993}`)
	number, err := token.GetNumber("id")
	require.NoError(t, err)
	require.Equal(t, "9007199254740993", number.String())

	id, err := token.GetInt64("id")
	require.NoError(t, err)
	require.Equal(t, int64(9007199254740993), id)
}

func TestContractGetInt64Errors(t *testing.T) {
	token := tokenFromJSON(t, `{"frac": 1.5, "huge": 999999999999999999999999999999}`)
	_, err := token.GetInt64("frac")
	require.Error(t, err)
	_, err = token.GetInt64("huge")
	require.Error(t, err)
	_, err = token.GetInt64("missing")
	require.Error(t, err)
}

func TestContractUnknownFields(t *testing.T) {
	fields := map[string]paseto.FieldSpec{"sub": paseto.StringSpec()}

	allowed, err := paseto.NewClaimsContract(fields)
	require.NoError(t, err)
	require.NoError(t, allowed.CheckClaims(tokenFromJSON(t, `{"sub":"a","rogue":1}`)))

	rejecting, err := paseto.NewClaimsContract(fields)
	require.NoError(t, err)
	_, err = rejecting.RejectUnknownFields()
	require.NoError(t, err)

	err = rejecting.CheckClaims(tokenFromJSON(t, `{"sub":"a","rogue":1}`))
	var claimErr paseto.ClaimError
	require.ErrorAs(t, err, &claimErr)
	require.Equal(t, paseto.UnknownClaim, claimErr.Category)
	require.Equal(t, "rogue", claimErr.Path)
}

func TestContractNestedUnknownFields(t *testing.T) {
	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"meta": paseto.ObjectSpec(map[string]paseto.FieldSpec{
			"a": paseto.StringSpec(),
		}),
	})
	require.NoError(t, err)
	_, err = contract.RejectUnknownFields()
	require.NoError(t, err)

	err = contract.CheckClaims(tokenFromJSON(t, `{"meta":{"a":"x","b":"y"}}`))
	var claimErr paseto.ClaimError
	require.ErrorAs(t, err, &claimErr)
	require.Equal(t, paseto.UnknownClaim, claimErr.Category)
	require.Equal(t, "meta.b", claimErr.Path)
}

func TestContractArrayElementPath(t *testing.T) {
	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"roles": paseto.ArraySpec(paseto.StringSpec()),
	})
	require.NoError(t, err)

	err = contract.CheckClaims(tokenFromJSON(t, `{"roles":["a", 3]}`))
	var claimErr paseto.ClaimError
	require.ErrorAs(t, err, &claimErr)
	require.Equal(t, paseto.WrongType, claimErr.Category)
	require.Equal(t, "roles[1]", claimErr.Path)
}

func TestContractNullArrayElement(t *testing.T) {
	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"roles": paseto.ArraySpec(paseto.StringSpec()),
	})
	require.NoError(t, err)

	err = contract.CheckClaims(tokenFromJSON(t, `{"roles":["a", null]}`))
	var claimErr paseto.ClaimError
	require.ErrorAs(t, err, &claimErr)
	require.Equal(t, paseto.NullClaim, claimErr.Category)
	require.Equal(t, "roles[1]", claimErr.Path)
}

func TestContractNestedObjectPath(t *testing.T) {
	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"meta": paseto.ObjectSpec(map[string]paseto.FieldSpec{
			"inner": paseto.ObjectSpec(map[string]paseto.FieldSpec{
				"deep": paseto.StringSpec(),
			}),
		}),
	})
	require.NoError(t, err)

	err = contract.CheckClaims(tokenFromJSON(t, `{"meta":{"inner":{"deep": 7}}}`))
	var claimErr paseto.ClaimError
	require.ErrorAs(t, err, &claimErr)
	require.Equal(t, "meta.inner.deep", claimErr.Path)
}

func TestContractErrorsDoNotContainValues(t *testing.T) {
	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"secret": paseto.StringSpec(),
	})
	require.NoError(t, err)

	err = contract.CheckClaims(tokenFromJSON(t, `{"secret": 12345}`))
	require.Error(t, err)
	require.False(t, strings.Contains(err.Error(), "12345"), "error must not leak the claim value: %v", err)
}

func TestContractErrorsIs(t *testing.T) {
	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"sub": paseto.StringSpec(),
	})
	require.NoError(t, err)

	err = contract.CheckClaims(tokenFromJSON(t, `{}`))
	require.True(t, errors.Is(err, paseto.ClaimError{}))
}

func TestContractConstructionErrors(t *testing.T) {
	t.Run("zero value spec", func(t *testing.T) {
		_, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
			"a": {},
		})
		require.Error(t, err)
	})

	t.Run("empty top level field name", func(t *testing.T) {
		_, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
			"": paseto.StringSpec(),
		})
		require.Error(t, err)
	})

	t.Run("empty nested field name", func(t *testing.T) {
		_, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
			"o": paseto.ObjectSpec(map[string]paseto.FieldSpec{
				"": paseto.StringSpec(),
			}),
		})
		require.Error(t, err)
	})
}

func TestContractSharedSpecReuseIsAllowed(t *testing.T) {
	leaf := paseto.StringSpec()
	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"a": paseto.ObjectSpec(map[string]paseto.FieldSpec{"x": leaf}),
		"b": paseto.ObjectSpec(map[string]paseto.FieldSpec{"y": leaf}),
	})
	require.NoError(t, err)

	require.NoError(t, contract.CheckClaims(tokenFromJSON(t, `{"a":{"x":"p"},"b":{"y":"q"}}`)))
}

func chainFields(nestedCount int) map[string]paseto.FieldSpec {
	fields := map[string]paseto.FieldSpec{"leaf": paseto.StringSpec()}
	for i := 0; i < nestedCount; i++ {
		fields = map[string]paseto.FieldSpec{"nested": paseto.ObjectSpec(fields)}
	}
	return fields
}

func chainJSON(nestedCount int) string {
	return strings.Repeat(`{"nested":`, nestedCount) + `{"leaf":"x"}` + strings.Repeat(`}`, nestedCount)
}

func TestContractDepthLimit(t *testing.T) {
	// The innermost object sits at depth nestedCount+1, so nestedCount 31
	// reaches depth MaxContractDepth and is accepted.
	atLimit, err := paseto.NewClaimsContract(chainFields(paseto.MaxContractDepth - 1))
	require.NoError(t, err)
	require.NoError(t, atLimit.CheckClaims(tokenFromJSON(t, chainJSON(paseto.MaxContractDepth-1))))

	// One more level of nesting trips the runtime recursion limit.
	tooDeep, err := paseto.NewClaimsContract(chainFields(paseto.MaxContractDepth))
	require.NoError(t, err)
	err = tooDeep.CheckClaims(tokenFromJSON(t, chainJSON(paseto.MaxContractDepth)))
	var claimErr paseto.ClaimError
	require.ErrorAs(t, err, &claimErr)
	require.Equal(t, paseto.ClaimTooDeep, claimErr.Category)
}

func TestContractRuleAppliedByParser(t *testing.T) {
	token := paseto.NewToken()
	token.SetString("sub", "alice")

	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"sub": paseto.StringSpec(),
		"n":   paseto.IntegerSpec(),
	})
	require.NoError(t, err)

	key := paseto.NewV4SymmetricKey()
	encrypted := token.V4Encrypt(key, nil)

	// The contract runs only after successful decryption, but surfaces as a
	// rule failure rather than a token error.
	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddClaimsContract(contract)
	_, err = parser.ParseV4Local(key, encrypted, nil)
	var claimErr paseto.ClaimError
	require.ErrorAs(t, err, &claimErr)
	require.Equal(t, paseto.MissingClaim, claimErr.Category)
	require.Equal(t, "n", claimErr.Path)

	var ruleErr paseto.RuleError
	require.ErrorAs(t, err, &ruleErr)
}

func TestContractComposesWithExistingRules(t *testing.T) {
	token := paseto.NewToken()
	token.SetIssuer("iss")
	token.SetExpiration(time.Now().Add(time.Minute))
	token.SetString("org_id", "not-an-int")

	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"iss":    paseto.StringSpec(),
		"org_id": paseto.IntegerSpec(),
	})
	require.NoError(t, err)

	key := paseto.NewV4SymmetricKey()
	encrypted := token.V4Encrypt(key, nil)

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddRule(paseto.IssuedBy("iss"))
	parser.AddClaimsContract(contract)

	_, err = parser.ParseV4Local(key, encrypted, nil)
	var claimErr paseto.ClaimError
	require.ErrorAs(t, err, &claimErr)
	require.Equal(t, "org_id", claimErr.Path)
	require.Equal(t, paseto.WrongType, claimErr.Category)
}

func TestContractPassesWithExistingRules(t *testing.T) {
	token := paseto.NewToken()
	token.SetIssuer("iss")
	token.SetExpiration(time.Now().Add(time.Minute))
	require.NoError(t, token.Set("org_id", 7))

	contract, err := paseto.NewClaimsContract(map[string]paseto.FieldSpec{
		"iss":    paseto.StringSpec(),
		"org_id": paseto.IntegerSpec(),
	})
	require.NoError(t, err)

	key := paseto.NewV4SymmetricKey()
	encrypted := token.V4Encrypt(key, nil)

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddRule(paseto.IssuedBy("iss"))
	parser.AddClaimsContract(contract)

	parsed, err := parser.ParseV4Local(key, encrypted, nil)
	require.NoError(t, err)

	id, err := parsed.GetInt64("org_id")
	require.NoError(t, err)
	require.Equal(t, int64(7), id)
}
