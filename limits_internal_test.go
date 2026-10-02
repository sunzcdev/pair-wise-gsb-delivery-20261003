package paseto

import (
	"testing"

	"aidanwoods.dev/go-paseto/internal/encoding"
	"github.com/stretchr/testify/require"
)

func TestDecodedLen(t *testing.T) {
	cases := []struct {
		encodedLen int
		decodedLen int64
		ok         bool
	}{
		{0, 0, true},
		{1, 0, false}, // impossible for unpadded base64
		{2, 1, true},  // "AA" -> 1 byte
		{3, 2, true},  // "AAA" -> 2 bytes
		{4, 3, true},  // "AAAA" -> 3 bytes
		{5, 0, false},
		{6, 4, true},
		{7, 5, true},
		{8, 6, true},
		{100, 75, true},
	}

	for _, c := range cases {
		decodedLen, ok := encoding.DecodedLen(c.encodedLen)
		require.Equal(t, c.ok, ok, "encodedLen=%d", c.encodedLen)
		require.Equal(t, c.decodedLen, decodedLen, "encodedLen=%d", c.encodedLen)
	}
}

func TestOversizedPayloadScreenedBeforeBase64Decode(t *testing.T) {
	// Valid unpadded base64 length (n % 4 != 1), but the guaranteed decoded
	// length cannot fit. This must be reported as a payload budget error, not a
	// decoding error, and must never reach cryptography.
	encoded := "v2.local." + repeatRunes(1000, 'A')

	err := newMessage(V2Local, Limits{Payload: 10}, encoded).Err()
	require.Error(t, err)
	require.ErrorIs(t, err, BudgetExceededError{Budget: PayloadBudget})

	var budgetErr BudgetExceededError
	require.ErrorAs(t, err, &budgetErr)
	require.Equal(t, PayloadBudget, budgetErr.Budget)
	require.Equal(t, 10, budgetErr.Limit)
	require.Equal(t, int64(750), int64(budgetErr.Given))
}

func TestOversizedFooterScreenedBeforeBase64Decode(t *testing.T) {
	encoded := "v2.local." + repeatRunes(10, 'A') + "." + repeatRunes(400, 'A')

	err := newMessage(V2Local, Limits{Footer: 5}, encoded).Err()
	require.Error(t, err)
	require.ErrorIs(t, err, BudgetExceededError{Budget: FooterBudget})
}

func TestInvalidBase64LengthNotMistakenForBudgetError(t *testing.T) {
	// 41 % 4 == 1: an impossible unpadded base64 length. The budget precheck
	// must not fire (the length is not a valid encoded length), and the token
	// should fail later as a regular token (decoding) error instead.
	encoded := "v2.local." + repeatRunes(41, 'A')

	err := newMessage(V2Local, Limits{Payload: 1_000_000}, encoded).Err()
	require.Error(t, err)
	require.ErrorIs(t, err, TokenError{})
	require.NotErrorIs(t, err, BudgetExceededError{})
}

func TestExtraSeparatorNotBypassedByLimits(t *testing.T) {
	encoded := "v2.local." + repeatRunes(100, 'A') + ".a.b"

	err := newMessage(V2Local, Limits{Payload: 1_000_000, Footer: 1_000_000}, encoded).Err()
	require.Error(t, err)
	require.ErrorIs(t, err, TokenError{})
	require.NotErrorIs(t, err, BudgetExceededError{})
}

func TestEmptyFooterNotBypassedByLimits(t *testing.T) {
	// A trailing dot yields an empty footer section, which decodes to zero
	// bytes and satisfies every footer limit.
	encoded := "v2.local." + repeatRunes(100, 'A') + "."

	err := newMessage(V2Local, Limits{Payload: 1_000_000, Footer: 1}, encoded).Err()
	require.NoError(t, err)
}

func TestHeaderMismatchStillReportedWhenWithinBudget(t *testing.T) {
	encoded := "v4.local." + repeatRunes(10, 'A')

	err := newMessage(V2Local, Limits{Payload: 1_000_000}, encoded).Err()
	require.Error(t, err)
	require.ErrorIs(t, err, TokenError{})
	require.NotErrorIs(t, err, BudgetExceededError{})
}

func TestTokenBudgetCheckUsesFullEncodedLength(t *testing.T) {
	encoded := "v2.local." + repeatRunes(1000, 'A')

	err := newMessage(V2Local, Limits{Token: 50}, encoded).Err()
	require.Error(t, err)
	require.ErrorIs(t, err, BudgetExceededError{Budget: TokenBudget})

	var budgetErr BudgetExceededError
	require.ErrorAs(t, err, &budgetErr)
	require.Equal(t, len(encoded), budgetErr.Given)
	require.Equal(t, 50, budgetErr.Limit)
}

func TestPostDecodeCheckCatchesActualLength(t *testing.T) {
	// An encoded section exactly at the encoded precheck boundary but with
	// real bytes: the actual decoded length is verified after decoding.
	encoded := "v4.local." + repeatRunes(8, 'A') // decodes to 6 bytes

	err := newMessage(V4Local, Limits{Payload: 5}, encoded).Err()
	require.Error(t, err)
	require.ErrorIs(t, err, BudgetExceededError{Budget: PayloadBudget})

	// At the exact decoded length, the payload proceeds (and fails later on
	// payload shortness, never on budget).
	encoded = "v4.local." + repeatRunes(8, 'A')
	err = newMessage(V4Local, Limits{Payload: 6}, encoded).Err()
	require.Error(t, err)
	require.ErrorIs(t, err, TokenError{})
	require.NotErrorIs(t, err, BudgetExceededError{})
}

func repeatRunes(n int, r rune) string {
	runes := make([]rune, n)
	for i := range runes {
		runes[i] = r
	}
	return string(runes)
}
