package paseto_test

import (
	"fmt"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"
	"github.com/stretchr/testify/require"
)

func signedTokenV4(t *testing.T, setters func(token *paseto.Token), footer []byte) (string, paseto.V4AsymmetricSecretKey) {
	t.Helper()

	token := paseto.NewToken()
	token.SetExpiration(time.Now().Add(time.Minute))
	setters(&token)
	token.SetFooter(footer)

	key := paseto.NewV4AsymmetricSecretKey()

	return token.V4Sign(key, nil), key
}

func TestLegacyRuleIsolation(t *testing.T) {
	signed, key := signedTokenV4(t, func(token *paseto.Token) {
		token.SetString("data", "original")
	}, []byte("authentic-footer"))

	mutatingRule := func(token paseto.Token) error {
		require.NoError(t, token.Set("injected", true))
		token.SetString("data", "tampered")
		token.SetFooter([]byte("evil-footer"))

		extracted := token.Footer()
		for i := range extracted {
			extracted[i] = 'x'
		}

		claims := token.Claims()
		claims["data"] = "tampered-again"

		return nil
	}

	parser := paseto.NewParser()
	parser.AddRule(mutatingRule)
	parser.AddRule(func(token paseto.Token) error {
		// a later rule must still see the authenticated content
		data, err := token.GetString("data")
		require.NoError(t, err)
		require.Equal(t, "original", data)
		require.Equal(t, []byte("authentic-footer"), token.Footer())

		return nil
	})

	parsed, err := parser.ParseV4Public(key.Public(), signed, nil)
	require.NoError(t, err)

	claims := parsed.Claims()
	require.Equal(t, "original", claims["data"])
	require.NotContains(t, claims, "injected")
	require.Contains(t, claims, "exp")
	require.Equal(t, []byte("authentic-footer"), parsed.Footer())

	var injected bool
	require.Error(t, parsed.Get("injected", &injected))
}

func TestReadOnlyRule(t *testing.T) {
	signed, key := signedTokenV4(t, func(token *paseto.Token) {
		token.SetString("sub", "subject")
		token.SetAudience("audience")
	}, []byte("authentic-footer"))

	parser := paseto.NewParser()
	parser.AddReadOnlyRule(func(token paseto.ReadOnlyToken) error {
		sub, err := token.GetSubject()
		if err != nil {
			return err
		}
		if sub != "subject" {
			return fmt.Errorf("wrong subject")
		}

		aud, err := token.GetAudience()
		if err != nil {
			return err
		}
		if aud != "audience" {
			return fmt.Errorf("wrong audience")
		}

		if string(token.Footer()) != "authentic-footer" {
			return fmt.Errorf("wrong footer")
		}

		return nil
	})

	parsed, err := parser.ParseV4Public(key.Public(), signed, nil)
	require.NoError(t, err)
	require.Equal(t, []byte("authentic-footer"), parsed.Footer())
}

func TestReadOnlyRuleFailureRejectsToken(t *testing.T) {
	signed, key := signedTokenV4(t, func(token *paseto.Token) {
		token.SetString("sub", "someone")
	}, nil)

	parser := paseto.NewParser()
	parser.AddReadOnlyRule(func(token paseto.ReadOnlyToken) error {
		return fmt.Errorf("nope")
	})

	_, err := parser.ParseV4Public(key.Public(), signed, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, &paseto.RuleError{})
}

func TestReadOnlyViewReturnsCopies(t *testing.T) {
	signed, key := signedTokenV4(t, func(token *paseto.Token) {
		require.NoError(t, token.Set("nested", map[string]interface{}{"admin": false}))
	}, []byte("authentic-footer"))

	parser := paseto.NewParser()
	parser.AddReadOnlyRule(func(token paseto.ReadOnlyToken) error {
		footer := token.Footer()
		for i := range footer {
			footer[i] = 'x'
		}

		claims := token.Claims()
		claims["injected"] = true
		claims["nested"].(map[string]interface{})["admin"] = true

		raw, err := token.GetRaw("nested")
		require.NoError(t, err)
		for i := range raw {
			raw[i] = 'x'
		}

		var nested map[string]interface{}
		require.NoError(t, token.Get("nested", &nested))
		nested["admin"] = true

		claimsJSON := token.ClaimsJSON()
		for i := range claimsJSON {
			claimsJSON[i] = 'x'
		}

		return nil
	})

	parsed, err := parser.ParseV4Public(key.Public(), signed, nil)
	require.NoError(t, err)

	require.Equal(t, []byte("authentic-footer"), parsed.Footer())

	var nested map[string]interface{}
	require.NoError(t, parsed.Get("nested", &nested))
	require.Equal(t, map[string]interface{}{"admin": false}, nested)

	var injected bool
	require.Error(t, parsed.Get("injected", &injected))
}

func TestRuleExecutionOrder(t *testing.T) {
	signed, key := signedTokenV4(t, func(token *paseto.Token) {}, nil)

	var order []string

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddRule(func(token paseto.Token) error {
		order = append(order, "legacy-1")
		return nil
	})
	parser.AddReadOnlyRule(func(token paseto.ReadOnlyToken) error {
		order = append(order, "readonly-1")
		return nil
	})
	parser.AddRule(func(token paseto.Token) error {
		order = append(order, "legacy-2")
		return nil
	})
	parser.AddReadOnlyRule(func(token paseto.ReadOnlyToken) error {
		order = append(order, "readonly-2")
		return nil
	})

	_, err := parser.ParseV4Public(key.Public(), signed, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"legacy-1", "readonly-1", "legacy-2", "readonly-2"}, order)
}

func TestRuleExecutionStopsOnFailure(t *testing.T) {
	signed, key := signedTokenV4(t, func(token *paseto.Token) {}, nil)

	var order []string

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddRule(func(token paseto.Token) error {
		order = append(order, "legacy-1")
		return nil
	})
	parser.AddReadOnlyRule(func(token paseto.ReadOnlyToken) error {
		order = append(order, "readonly-fails")
		return fmt.Errorf("fails")
	})
	parser.AddRule(func(token paseto.Token) error {
		order = append(order, "legacy-2")
		return nil
	})

	_, err := parser.ParseV4Public(key.Public(), signed, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, &paseto.RuleError{})
	require.Equal(t, []string{"legacy-1", "readonly-fails"}, order)
}

func TestFooterInputBufferIsolated(t *testing.T) {
	footer := []byte("original-footer")

	token, err := paseto.MakeToken(map[string]interface{}{"foo": "bar"}, footer)
	require.NoError(t, err)

	for i := range footer {
		footer[i] = 'x'
	}
	require.Equal(t, []byte("original-footer"), token.Footer())

	newFooter := []byte("new-footer")
	token.SetFooter(newFooter)
	for i := range newFooter {
		newFooter[i] = 'x'
	}
	require.Equal(t, []byte("new-footer"), token.Footer())

	extracted := token.Footer()
	for i := range extracted {
		extracted[i] = 'y'
	}
	require.Equal(t, []byte("new-footer"), token.Footer())
}

func TestClaimsJSONInputBufferIsolated(t *testing.T) {
	footer := []byte("some-footer")

	token, err := paseto.NewTokenFromClaimsJSON([]byte(`{"foo":"bar"}`), footer)
	require.NoError(t, err)

	for i := range footer {
		footer[i] = 'x'
	}
	require.Equal(t, []byte("some-footer"), token.Footer())
}

func TestNestedClaimInputIsolated(t *testing.T) {
	nested := map[string]interface{}{"admin": false}

	token, err := paseto.MakeToken(map[string]interface{}{"nested": nested}, nil)
	require.NoError(t, err)

	nested["admin"] = true

	var output map[string]interface{}
	require.NoError(t, token.Get("nested", &output))
	require.Equal(t, map[string]interface{}{"admin": false}, output)
}

func TestTokenValueCopyFooterIsolated(t *testing.T) {
	token := paseto.NewToken()
	token.SetString("data", "original")
	token.SetFooter([]byte("original-footer"))

	copied := token
	copied.SetFooter([]byte("tampered-footer"))

	data, err := token.GetString("data")
	require.NoError(t, err)
	require.Equal(t, "original", data)
	require.Equal(t, []byte("original-footer"), token.Footer())
}
