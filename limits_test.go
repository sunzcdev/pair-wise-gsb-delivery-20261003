package paseto_test

import (
	"errors"
	"strings"
	"testing"

	"aidanwoods.dev/go-paseto"
	"github.com/stretchr/testify/require"
)

func countingRule(calls *int) paseto.Rule {
	return func(paseto.Token) error {
		*calls++
		return nil
	}
}

func oversizedV4LocalToken(footerLen int) (paseto.V4SymmetricKey, string) {
	key := paseto.NewV4SymmetricKey()

	token := paseto.NewToken()
	token.SetString("data", strings.Repeat("A", 2000))
	if footerLen > 0 {
		token.SetFooter([]byte(strings.Repeat("F", footerLen)))
	}

	return key, token.V4Encrypt(key, nil)
}

func oversizedV4PublicToken(footerLen int) (paseto.V4AsymmetricPublicKey, string) {
	secret := paseto.NewV4AsymmetricSecretKey()

	token := paseto.NewToken()
	token.SetString("data", strings.Repeat("A", 2000))
	if footerLen > 0 {
		token.SetFooter([]byte(strings.Repeat("F", footerLen)))
	}

	return secret.Public(), token.V4Sign(secret, nil)
}

func TestTokenBudgetRejectsBeforeRulesV4(t *testing.T) {
	key, encoded := oversizedV4LocalToken(0)

	calls := 0
	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddRule(countingRule(&calls))
	parser.SetLimits(paseto.Limits{Token: 100})

	_, err := parser.ParseV4Local(key, encoded, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, paseto.TokenError{})
	require.ErrorIs(t, err, paseto.BudgetExceededError{Budget: paseto.TokenBudget})
	require.NotErrorIs(t, err, paseto.BudgetExceededError{Budget: paseto.PayloadBudget})
	require.NotErrorIs(t, err, paseto.RuleError{})
	require.Equal(t, 0, calls, "custom rule must never be called for an oversized token")
	require.NotContains(t, err.Error(), encoded)
}

func TestPayloadBudgetRejectsBeforeRulesAndCryptoV4Local(t *testing.T) {
	key, encoded := oversizedV4LocalToken(0)

	calls := 0
	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddRule(countingRule(&calls))
	parser.SetLimits(paseto.Limits{Payload: 100})

	_, err := parser.ParseV4Local(key, encoded, nil)
	require.Error(t, err)

	var budgetErr paseto.BudgetExceededError
	require.ErrorAs(t, err, &budgetErr)
	require.Equal(t, paseto.PayloadBudget, budgetErr.Budget)
	require.Equal(t, 100, budgetErr.Limit)
	require.Greater(t, budgetErr.Given, 100)

	require.ErrorIs(t, err, paseto.TokenError{})
	require.NotErrorIs(t, err, paseto.RuleError{})
	require.Equal(t, 0, calls, "custom rule must never be called for an oversized token")
	require.NotContains(t, err.Error(), encoded)
}

func TestPayloadBudgetRejectsBeforeRulesV4Public(t *testing.T) {
	publicKey, encoded := oversizedV4PublicToken(0)

	calls := 0
	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddRule(countingRule(&calls))
	parser.SetLimits(paseto.Limits{Payload: 100})

	_, err := parser.ParseV4Public(publicKey, encoded, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, paseto.BudgetExceededError{Budget: paseto.PayloadBudget})
	require.ErrorIs(t, err, paseto.TokenError{})
	require.NotErrorIs(t, err, paseto.RuleError{})
	require.Equal(t, 0, calls)
}

func TestFooterBudgetRejectsBeforeRules(t *testing.T) {
	key, encoded := oversizedV4LocalToken(100)

	calls := 0
	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddRule(countingRule(&calls))
	parser.SetLimits(paseto.Limits{Footer: 10})

	_, err := parser.ParseV4Local(key, encoded, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, paseto.BudgetExceededError{Budget: paseto.FooterBudget})
	require.ErrorIs(t, err, paseto.TokenError{})
	require.NotErrorIs(t, err, paseto.RuleError{})
	require.Equal(t, 0, calls)
}

func TestFooterBudgetAppliesToUnsafeParseFooter(t *testing.T) {
	_, encoded := oversizedV4LocalToken(100)

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.SetLimits(paseto.Limits{Footer: 10})

	_, err := parser.UnsafeParseFooter(paseto.V4Local, encoded)
	require.Error(t, err)
	require.ErrorIs(t, err, paseto.BudgetExceededError{Budget: paseto.FooterBudget})
}

func TestLimitsEnforcedForAllEntries(t *testing.T) {
	calls := 0
	rule := countingRule(&calls)

	t.Run("v2 local", func(t *testing.T) {
		key := paseto.NewV2SymmetricKey()
		token := paseto.NewToken()
		token.SetString("data", strings.Repeat("A", 500))
		encoded := token.V2Encrypt(key)

		parser := paseto.NewParserWithoutExpiryCheck()
		parser.AddRule(rule)
		parser.SetLimits(paseto.Limits{Payload: 10})

		_, err := parser.ParseV2Local(key, encoded)
		require.Error(t, err)
		require.ErrorIs(t, err, paseto.BudgetExceededError{Budget: paseto.PayloadBudget})
		require.Equal(t, 0, calls)
	})

	t.Run("v2 public", func(t *testing.T) {
		secret := paseto.NewV2AsymmetricSecretKey()
		token := paseto.NewToken()
		token.SetString("data", strings.Repeat("A", 500))
		encoded := token.V2Sign(secret)

		parser := paseto.NewParserWithoutExpiryCheck()
		parser.AddRule(rule)
		parser.SetLimits(paseto.Limits{Payload: 10})

		_, err := parser.ParseV2Public(secret.Public(), encoded)
		require.Error(t, err)
		require.ErrorIs(t, err, paseto.BudgetExceededError{Budget: paseto.PayloadBudget})
		require.Equal(t, 0, calls)
	})

	t.Run("v3 local", func(t *testing.T) {
		key := paseto.NewV3SymmetricKey()
		token := paseto.NewToken()
		token.SetString("data", strings.Repeat("A", 500))
		encoded := token.V3Encrypt(key, nil)

		parser := paseto.NewParserWithoutExpiryCheck()
		parser.AddRule(rule)
		parser.SetLimits(paseto.Limits{Payload: 10})

		_, err := parser.ParseV3Local(key, encoded, nil)
		require.Error(t, err)
		require.ErrorIs(t, err, paseto.BudgetExceededError{Budget: paseto.PayloadBudget})
		require.Equal(t, 0, calls)
	})

	t.Run("v3 public", func(t *testing.T) {
		secret := paseto.NewV3AsymmetricSecretKey()
		token := paseto.NewToken()
		token.SetString("data", strings.Repeat("A", 500))
		encoded := token.V3Sign(secret, nil)

		parser := paseto.NewParserWithoutExpiryCheck()
		parser.AddRule(rule)
		parser.SetLimits(paseto.Limits{Payload: 10})

		_, err := parser.ParseV3Public(secret.Public(), encoded, nil)
		require.Error(t, err)
		require.ErrorIs(t, err, paseto.BudgetExceededError{Budget: paseto.PayloadBudget})
		require.Equal(t, 0, calls)
	})

	t.Run("v4 local", func(t *testing.T) {
		key := paseto.NewV4SymmetricKey()
		token := paseto.NewToken()
		token.SetString("data", strings.Repeat("A", 500))
		encoded := token.V4Encrypt(key, nil)

		parser := paseto.NewParserWithoutExpiryCheck()
		parser.AddRule(rule)
		parser.SetLimits(paseto.Limits{Payload: 10})

		_, err := parser.ParseV4Local(key, encoded, nil)
		require.Error(t, err)
		require.ErrorIs(t, err, paseto.BudgetExceededError{Budget: paseto.PayloadBudget})
		require.Equal(t, 0, calls)
	})

	t.Run("v4 public", func(t *testing.T) {
		secret := paseto.NewV4AsymmetricSecretKey()
		token := paseto.NewToken()
		token.SetString("data", strings.Repeat("A", 500))
		encoded := token.V4Sign(secret, nil)

		parser := paseto.NewParserWithoutExpiryCheck()
		parser.AddRule(rule)
		parser.SetLimits(paseto.Limits{Payload: 10})

		_, err := parser.ParseV4Public(secret.Public(), encoded, nil)
		require.Error(t, err)
		require.ErrorIs(t, err, paseto.BudgetExceededError{Budget: paseto.PayloadBudget})
		require.Equal(t, 0, calls)
	})
}

func TestLimitsAllowTokensAtAndBelowBudget(t *testing.T) {
	key := paseto.NewV4SymmetricKey()
	token := paseto.NewToken()
	token.SetString("data", "hello")
	encoded := token.V4Encrypt(key, nil)

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.SetLimits(paseto.Limits{
		Token:   len(encoded),
		Payload: 10_000,
		Footer:  10_000,
	})

	parsed, err := parser.ParseV4Local(key, encoded, nil)
	require.NoError(t, err)
	value, err := parsed.GetString("data")
	require.NoError(t, err)
	require.Equal(t, "hello", value)
}

func TestZeroLimitsMeanUnlimited(t *testing.T) {
	key, encoded := oversizedV4LocalToken(0)

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.SetLimits(paseto.Limits{})

	_, err := parser.ParseV4Local(key, encoded, nil)
	require.NoError(t, err)
}

func TestOversizedTokenRejectedBeforeCryptoWithWrongKey(t *testing.T) {
	// The token is encrypted under a different key. Without limits this fails
	// as a cryptographic error; with a payload limit it must fail on the
	// budget instead, proving decryption never runs.
	_, encoded := oversizedV4LocalToken(0)
	wrongKey := paseto.NewV4SymmetricKey()

	parser := paseto.NewParserWithoutExpiryCheck()

	_, cryptoErr := parser.ParseV4Local(wrongKey, encoded, nil)
	require.Error(t, cryptoErr)
	require.NotErrorIs(t, cryptoErr, paseto.BudgetExceededError{})

	parser.SetLimits(paseto.Limits{Payload: 10})
	_, err := parser.ParseV4Local(wrongKey, encoded, nil)
	require.ErrorIs(t, err, paseto.BudgetExceededError{Budget: paseto.PayloadBudget})
}

func TestEmptyFooterTrailingDotSatisfiesFooterBudget(t *testing.T) {
	// A trailing dot denotes an empty footer; it must decode to zero bytes and
	// pass any footer limit rather than bypassing checks.
	key := paseto.NewV4SymmetricKey()
	token := paseto.NewToken()
	token.SetString("data", "hello")
	encoded := token.V4Encrypt(key, nil) + "."

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.SetLimits(paseto.Limits{Footer: 1})

	_, err := parser.ParseV4Local(key, encoded, nil)
	require.NoError(t, err)

	footer, err := parser.UnsafeParseFooter(paseto.V4Local, encoded)
	require.NoError(t, err)
	require.Empty(t, footer)
}

func TestExtraSeparatorRejected(t *testing.T) {
	key, encoded := oversizedV4LocalToken(0)

	extraDot := encoded + ".something.else"

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.SetLimits(paseto.Limits{Token: 1_000_000})

	_, err := parser.ParseV4Local(key, extraDot, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, paseto.TokenError{})

	// the same malformed input with a small token budget is rejected on budget
	parser.SetLimits(paseto.Limits{Token: 5})
	_, err = parser.ParseV4Local(key, extraDot, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, paseto.BudgetExceededError{Budget: paseto.TokenBudget})
}

func TestBudgetMismatchDoesNotMatchOtherBudgets(t *testing.T) {
	key, encoded := oversizedV4LocalToken(0)

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.SetLimits(paseto.Limits{Token: 10})

	_, err := parser.ParseV4Local(key, encoded, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, paseto.BudgetExceededError{})
	require.False(t, errors.Is(err, paseto.BudgetExceededError{Budget: paseto.PayloadBudget}),
		"token budget error must not be mistaken for a payload budget error")
}

func TestBudgetErrorWrapsExactlyOneTokenError(t *testing.T) {
	key, encoded := oversizedV4LocalToken(0)

	parser := paseto.NewParserWithoutExpiryCheck()
	parser.SetLimits(paseto.Limits{Token: 10})

	_, err := parser.ParseV4Local(key, encoded, nil)
	require.Error(t, err)

	depth := 0
	for err != nil {
		var tokenErr paseto.TokenError
		if !errors.As(err, &tokenErr) {
			break
		}
		depth++
		err = tokenErr.Unwrap()
	}

	require.Equal(t, 1, depth, "budget errors should be wrapped in exactly one TokenError")
}
