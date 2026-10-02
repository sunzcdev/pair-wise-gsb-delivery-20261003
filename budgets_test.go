package paseto_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"aidanwoods.dev/go-paseto"
	"github.com/stretchr/testify/require"
)

type budgetFixture struct {
	v2Key    paseto.V2SymmetricKey
	v2Secret paseto.V2AsymmetricSecretKey
	v3Key    paseto.V3SymmetricKey
	v3Secret paseto.V3AsymmetricSecretKey
	v4Key    paseto.V4SymmetricKey
	v4Secret paseto.V4AsymmetricSecretKey

	claimsLen int

	v2Local  string
	v2Public string
	v3Local  string
	v3Public string
	v4Local  string
	v4Public string
}

func newBudgetFixture(footer []byte) budgetFixture {
	token := paseto.NewToken()
	token.SetString("data", "example")
	token.SetFooter(footer)

	fixture := budgetFixture{
		v2Key:     paseto.NewV2SymmetricKey(),
		v2Secret:  paseto.NewV2AsymmetricSecretKey(),
		v3Key:     paseto.NewV3SymmetricKey(),
		v3Secret:  paseto.NewV3AsymmetricSecretKey(),
		v4Key:     paseto.NewV4SymmetricKey(),
		v4Secret:  paseto.NewV4AsymmetricSecretKey(),
		claimsLen: len(token.ClaimsJSON()),
	}

	fixture.v2Local = token.V2Encrypt(fixture.v2Key)
	fixture.v2Public = token.V2Sign(fixture.v2Secret)
	fixture.v3Local = token.V3Encrypt(fixture.v3Key, nil)
	fixture.v3Public = token.V3Sign(fixture.v3Secret, nil)
	fixture.v4Local = token.V4Encrypt(fixture.v4Key, nil)
	fixture.v4Public = token.V4Sign(fixture.v4Secret, nil)

	return fixture
}

func (f budgetFixture) tokens() map[string]string {
	return map[string]string{
		"v2.local":  f.v2Local,
		"v2.public": f.v2Public,
		"v3.local":  f.v3Local,
		"v3.public": f.v3Public,
		"v4.local":  f.v4Local,
		"v4.public": f.v4Public,
	}
}

func (f budgetFixture) parse(parser *paseto.Parser, name string) error {
	switch name {
	case "v2.local":
		_, err := parser.ParseV2Local(f.v2Key, f.v2Local)
		return err
	case "v2.public":
		_, err := parser.ParseV2Public(f.v2Secret.Public(), f.v2Public)
		return err
	case "v3.local":
		_, err := parser.ParseV3Local(f.v3Key, f.v3Local, nil)
		return err
	case "v3.public":
		_, err := parser.ParseV3Public(f.v3Secret.Public(), f.v3Public, nil)
		return err
	case "v4.local":
		_, err := parser.ParseV4Local(f.v4Key, f.v4Local, nil)
		return err
	case "v4.public":
		_, err := parser.ParseV4Public(f.v4Secret.Public(), f.v4Public, nil)
		return err
	default:
		panic("unknown token: " + name)
	}
}

func (f budgetFixture) payloadOverheads() map[string]int {
	return map[string]int{
		"v2.local":  paseto.V2LocalPayloadOverhead,
		"v2.public": paseto.V2PublicPayloadOverhead,
		"v3.local":  paseto.V3LocalPayloadOverhead,
		"v3.public": paseto.V3PublicPayloadOverhead,
		"v4.local":  paseto.V4LocalPayloadOverhead,
		"v4.public": paseto.V4PublicPayloadOverhead,
	}
}

func countingParser(ruleCalls *int) paseto.Parser {
	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddRule(func(token paseto.Token) error {
		*ruleCalls++
		return nil
	})

	return parser
}

func requireBudgetError(t *testing.T, err error, budget paseto.Budget, token string) {
	t.Helper()

	var budgetErr paseto.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	require.Equal(t, budget, budgetErr.Budget)
	require.NotContains(t, err.Error(), token)
}

func TestBudgetsValidTokensPass(t *testing.T) {
	fixture := newBudgetFixture([]byte("footer"))

	for name := range fixture.tokens() {
		var ruleCalls int
		parser := countingParser(&ruleCalls)
		parser.SetMaxTokenLength(4096)
		parser.SetMaxPayloadLength(4096)
		parser.SetMaxFooterLength(4096)

		require.NoError(t, fixture.parse(&parser, name), name)
		require.Equal(t, 1, ruleCalls, name)
	}
}

func TestTokenBudgetExceeded(t *testing.T) {
	fixture := newBudgetFixture(nil)

	for name, token := range fixture.tokens() {
		var ruleCalls int
		parser := countingParser(&ruleCalls)
		parser.SetMaxTokenLength(len(token) - 1)

		err := fixture.parse(&parser, name)
		requireBudgetError(t, err, paseto.TokenBudget, token)
		require.Equal(t, 0, ruleCalls, name)
	}
}

func TestPayloadBudgetExceeded(t *testing.T) {
	fixture := newBudgetFixture(nil)

	for name, token := range fixture.tokens() {
		payloadLen := fixture.claimsLen + fixture.payloadOverheads()[name]

		var ruleCalls int
		parser := countingParser(&ruleCalls)
		parser.SetMaxPayloadLength(payloadLen - 1)

		err := fixture.parse(&parser, name)
		requireBudgetError(t, err, paseto.PayloadBudget, token)
		require.Equal(t, 0, ruleCalls, name)

		parser.SetMaxPayloadLength(payloadLen)
		require.NoError(t, fixture.parse(&parser, name), name)
		require.Equal(t, 1, ruleCalls, name)
	}
}

func TestFooterBudgetExceeded(t *testing.T) {
	footer := []byte("secret-footer")
	fixture := newBudgetFixture(footer)

	for name, token := range fixture.tokens() {
		var ruleCalls int
		parser := countingParser(&ruleCalls)
		parser.SetMaxFooterLength(len(footer) - 1)

		err := fixture.parse(&parser, name)
		requireBudgetError(t, err, paseto.FooterBudget, token)
		require.Equal(t, 0, ruleCalls, name)

		parser.SetMaxFooterLength(len(footer))
		require.NoError(t, fixture.parse(&parser, name), name)
		require.Equal(t, 1, ruleCalls, name)
	}
}

func TestPayloadBudgetRejectsBeforeDecoding(t *testing.T) {
	fixture := newBudgetFixture(nil)

	// The payload section is not valid base64: if the parser attempted to
	// decode it, a TokenError (and not a BudgetError) would be produced.
	token := "v4.local." + strings.Repeat("*", 100)

	var ruleCalls int
	parser := countingParser(&ruleCalls)
	parser.SetMaxPayloadLength(10)

	_, err := parser.ParseV4Local(fixture.v4Key, token, nil)
	requireBudgetError(t, err, paseto.PayloadBudget, token)
	require.Equal(t, 0, ruleCalls)
}

func TestFooterBudgetRejectsBeforeDecoding(t *testing.T) {
	fixture := newBudgetFixture(nil)

	// The footer section is not valid base64: if the parser attempted to
	// decode it, a TokenError (and not a BudgetError) would be produced.
	token := fixture.v4Local + "." + strings.Repeat("*", 100)

	var ruleCalls int
	parser := countingParser(&ruleCalls)
	parser.SetMaxFooterLength(10)

	_, err := parser.ParseV4Local(fixture.v4Key, token, nil)
	requireBudgetError(t, err, paseto.FooterBudget, token)
	require.Equal(t, 0, ruleCalls)
}

func TestBudgetsOverflowSafe(t *testing.T) {
	fixture := newBudgetFixture([]byte("footer"))

	for name := range fixture.tokens() {
		var ruleCalls int
		parser := countingParser(&ruleCalls)
		parser.SetMaxTokenLength(math.MaxInt)
		parser.SetMaxPayloadLength(math.MaxInt)
		parser.SetMaxFooterLength(math.MaxInt)

		require.NoError(t, fixture.parse(&parser, name), name)
		require.Equal(t, 1, ruleCalls, name)
	}
}

func TestBudgetsExtraSeparator(t *testing.T) {
	fixture := newBudgetFixture(nil)

	token := "v4.local.aaa.bbb.ccc"

	var ruleCalls int
	parser := countingParser(&ruleCalls)
	parser.SetMaxTokenLength(4096)
	parser.SetMaxPayloadLength(4096)
	parser.SetMaxFooterLength(4096)

	_, err := parser.ParseV4Local(fixture.v4Key, token, nil)
	require.Error(t, err)

	var budgetErr paseto.BudgetError
	require.False(t, errors.As(err, &budgetErr))
	require.Equal(t, 0, ruleCalls)
}

func TestBudgetsEmptyFooter(t *testing.T) {
	fixture := newBudgetFixture(nil)

	// A trailing separator with an empty footer section must not bypass any
	// budget checks, nor cause a spurious failure.
	token := fixture.v4Local + "."

	var ruleCalls int
	parser := countingParser(&ruleCalls)
	parser.SetMaxTokenLength(len(token))
	parser.SetMaxPayloadLength(4096)
	parser.SetMaxFooterLength(1)

	parsed, err := parser.ParseV4Local(fixture.v4Key, token, nil)
	require.NoError(t, err)
	require.Empty(t, parsed.Footer())
	require.Equal(t, 1, ruleCalls)

	parser.SetMaxTokenLength(len(token) - 1)
	_, err = parser.ParseV4Local(fixture.v4Key, token, nil)
	requireBudgetError(t, err, paseto.TokenBudget, token)
	require.Equal(t, 1, ruleCalls)
}
