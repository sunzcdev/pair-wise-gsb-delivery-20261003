package paseto_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"aidanwoods.dev/go-paseto"
	"github.com/stretchr/testify/require"
)

type nestedView struct {
	Tags []string          `json:"tags"`
	Meta map[string]string `json:"meta"`
}

func signedTokenForChecks(t *testing.T) (paseto.V4AsymmetricSecretKey, paseto.V4AsymmetricPublicKey, string) {
	t.Helper()

	token := paseto.NewToken()
	require.NoError(t, token.Set("sub", "alice"))
	require.NoError(t, token.Set("n", nestedView{
		Tags: []string{"a", "b"},
		Meta: map[string]string{"k": "v"},
	}))
	token.SetFooter([]byte(`{"kid":"key-1"}`))

	secretKey := paseto.NewV4AsymmetricSecretKey()
	signed := token.V4Sign(secretKey, nil)

	return secretKey, secretKey.Public(), signed
}

func TestCheckedRuleReadsAuthenticatedClaimsAndFooter(t *testing.T) {
	secretKey, publicKey, signed := signedTokenForChecks(t)

	var seen struct {
		sub    string
		nested nestedView
		footer []byte
	}

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddCheckedRule(func(token paseto.CheckedToken) error {
		var err error
		seen.sub, err = token.GetSubject()
		if err != nil {
			return err
		}

		if err := token.Get("n", &seen.nested); err != nil {
			return err
		}

		seen.footer = token.Footer()

		return nil
	})

	parsed, err := parser.ParseV4Public(publicKey, signed, nil)
	require.NoError(t, err)

	require.Equal(t, "alice", seen.sub)
	require.Equal(t, []string{"a", "b"}, seen.nested.Tags)
	require.Equal(t, map[string]string{"k": "v"}, seen.nested.Meta)
	require.JSONEq(t, `{"kid":"key-1"}`, string(seen.footer))

	// The delivered token is identical to what was authenticated.
	roundTripped := parsed.V4Sign(secretKey, nil)
	_, err = paseto.NewParserWithoutExpiryCheck().ParseV4Public(publicKey, roundTripped, nil)
	require.NoError(t, err)
	require.JSONEq(t, `{"kid":"key-1"}`, string(parsed.Footer()))
}

func TestLegacyRuleMutationIsIsolated(t *testing.T) {
	_, publicKey, signed := signedTokenForChecks(t)

	// A legacy rule tries every mutable entry point available on Token.
	tamperingRule := func(token paseto.Token) error {
		require.NoError(t, token.Set("sub", "mallory"))
		require.NoError(t, token.Set("injected", true))

		footer := token.Footer()
		copy(footer, []byte("XXXXXXXXXXXXXXXXX"))

		token.SetFooter([]byte("tampered-footer"))

		return nil
	}

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddRule(tamperingRule)

	parsed, err := parser.ParseV4Public(publicKey, signed, nil)
	require.NoError(t, err)

	sub, err := parsed.GetSubject()
	require.NoError(t, err)
	require.Equal(t, "alice", sub)

	_, err = parsed.GetString("injected")
	require.Error(t, err)

	require.JSONEq(t, `{"kid":"key-1"}`, string(parsed.Footer()))
	require.JSONEq(t,
		`{"sub":"alice","n":{"tags":["a","b"],"meta":{"k":"v"}}}`,
		string(parsed.ClaimsJSON()),
	)
}

func TestCheckedViewHasNoMutableBackDoors(t *testing.T) {
	_, publicKey, signed := signedTokenForChecks(t)

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddCheckedRule(func(token paseto.CheckedToken) error {
		// json.RawMessage fetched as a single claim, and every value in the
		// raw claims map: mutate them in place.
		raw, ok := token.ClaimJSON("sub")
		require.True(t, ok)
		copy(raw, []byte("\"z\""))

		rawClaims := token.RawClaims()
		subRaw := rawClaims["sub"]
		copy(subRaw, []byte("\"z\""))
		rawClaims["injected"] = json.RawMessage("true")

		// ClaimsJSON and Footer slices: mutate them in place.
		claimsJSON := token.ClaimsJSON()
		copy(claimsJSON, []byte("{}"))

		footer := token.Footer()
		copy(footer, []byte("XXXXXXXXXXXXXXXXX"))

		// Decoded generic map, including nested maps and slices.
		claims := token.Claims()
		claims["sub"] = "mallory"
		nested, _ := claims["n"].(map[string]interface{})
		nested["tags"].([]interface{})[0] = "z"
		nested["meta"].(map[string]interface{})["k"] = "z"

		// Typed decode into a value that carries nested aliases: mutate it.
		var n nestedView
		require.NoError(t, token.Get("n", &n))
		n.Tags[0] = "z"
		n.Meta["k"] = "z"

		return nil
	})

	parsed, err := parser.ParseV4Public(publicKey, signed, nil)
	require.NoError(t, err)

	require.JSONEq(t,
		`{"sub":"alice","n":{"tags":["a","b"],"meta":{"k":"v"}}}`,
		string(parsed.ClaimsJSON()),
	)
	require.JSONEq(t, `{"kid":"key-1"}`, string(parsed.Footer()))
}

func TestCheckedViewGetIntoRawMessageIsIsolated(t *testing.T) {
	_, publicKey, signed := signedTokenForChecks(t)

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddCheckedRule(func(token paseto.CheckedToken) error {
		var raw json.RawMessage
		if err := token.Get("sub", &raw); err != nil {
			return err
		}
		copy(raw, []byte("\"z\""))

		return nil
	})

	parsed, err := parser.ParseV4Public(publicKey, signed, nil)
	require.NoError(t, err)

	sub, err := parsed.GetSubject()
	require.NoError(t, err)
	require.Equal(t, "alice", sub)
}

func TestRulesExecuteInRegistrationOrder(t *testing.T) {
	_, publicKey, signed := signedTokenForChecks(t)

	var order []string

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddCheckedRule(func(paseto.CheckedToken) error {
		order = append(order, "checked-1")
		return nil
	})
	parser.AddRule(func(paseto.Token) error {
		order = append(order, "legacy")
		return nil
	})
	parser.AddCheckedRule(func(paseto.CheckedToken) error {
		order = append(order, "checked-2")
		return nil
	})

	_, err := parser.ParseV4Public(publicKey, signed, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"checked-1", "legacy", "checked-2"}, order)

	// A failing rule stops later rules and surfaces as a RuleError.
	failing := errors.New("boom")

	parser = paseto.MakeCheckedParser([]paseto.CheckedRule{
		func(paseto.CheckedToken) error { return nil },
		func(paseto.CheckedToken) error { return failing },
		func(paseto.CheckedToken) error {
			t.Fatal("later rule must not run after an earlier rule fails")
			return nil
		},
	})

	_, err = parser.ParseV4Public(publicKey, signed, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, &paseto.RuleError{})
	require.ErrorIs(t, err, failing)
}

func TestMutatingInputBuffersAfterConstructionDoesNotChangeToken(t *testing.T) {
	claimsData := []byte(`{"sub":"alice"}`)
	footer := []byte(`{"kid":"key-1"}`)

	token, err := paseto.NewTokenFromClaimsJSON(claimsData, footer)
	require.NoError(t, err)

	token.SetFooter(footer)

	// Mutate the caller-owned buffers after handing them to the token.
	for i := range claimsData {
		claimsData[i] = 'X'
	}
	for i := range footer {
		footer[i] = 'Y'
	}

	require.JSONEq(t, `{"sub":"alice"}`, string(token.ClaimsJSON()))
	require.JSONEq(t, `{"kid":"key-1"}`, string(token.Footer()))

	secretKey := paseto.NewV4AsymmetricSecretKey()
	signed := token.V4Sign(secretKey, nil)

	parsed, err := paseto.NewParserWithoutExpiryCheck().ParseV4Public(secretKey.Public(), signed, nil)
	require.NoError(t, err)

	require.JSONEq(t, `{"sub":"alice"}`, string(parsed.ClaimsJSON()))
	require.JSONEq(t, `{"kid":"key-1"}`, string(parsed.Footer()))
}

func TestSetCheckedRulesAndMakeCheckedParser(t *testing.T) {
	_, publicKey, signed := signedTokenForChecks(t)

	parser := paseto.MakeCheckedParser(nil)
	parser.SetCheckedRules([]paseto.CheckedRule{
		func(token paseto.CheckedToken) error {
			sub, err := token.GetSubject()
			if err != nil {
				return err
			}
			if sub != "alice" {
				return fmt.Errorf("unexpected subject %q", sub)
			}

			return nil
		},
	})

	_, err := parser.ParseV4Public(publicKey, signed, nil)
	require.NoError(t, err)
}

func TestFooterReturnedToUserIsCopy(t *testing.T) {
	footer := []byte("some-footer")

	token, err := paseto.MakeToken(map[string]interface{}{"sub": "alice"}, footer)
	require.NoError(t, err)

	got := token.Footer()
	copy(got, []byte("XXXXXXXXXXX"))
	require.Equal(t, "some-footer", string(token.Footer()))

	footer[0] = 'X'
	require.Equal(t, "some-footer", string(token.Footer()))
}

func TestCheckedViewIsInterfaceOnly(t *testing.T) {
	// CheckedRule must be able to consume every read-only accessor, and the
	// concrete view must not be needed by callers.
	var _ paseto.CheckedRule = func(paseto.CheckedToken) error { return nil }

	_, publicKey, signed := signedTokenForChecks(t)

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddCheckedRule(func(token paseto.CheckedToken) error {
		var generic interface{}
		if err := token.Get("sub", &generic); err != nil {
			return err
		}

		if _, err := token.GetString("sub"); err != nil {
			return err
		}

		_ = token.Claims()
		_, _ = token.ClaimJSON("sub")
		_ = token.RawClaims()
		_ = token.ClaimsJSON()
		_ = token.Footer()

		return nil
	})

	_, err := parser.ParseV4Public(publicKey, signed, nil)
	require.NoError(t, err)
}
