package paseto

import (
	"fmt"
	"math"
)

// Cryptographic overhead (in bytes) included in the decoded payload of each
// protocol, on top of the application claims.
const (
	// V2LocalPayloadOverhead is a 24 byte nonce and a 16 byte MAC.
	V2LocalPayloadOverhead = 24 + 16
	// V2PublicPayloadOverhead is a 64 byte signature.
	V2PublicPayloadOverhead = 64
	// V3LocalPayloadOverhead is a 32 byte nonce and a 48 byte MAC.
	V3LocalPayloadOverhead = 32 + 48
	// V3PublicPayloadOverhead is a 96 byte signature.
	V3PublicPayloadOverhead = 96
	// V4LocalPayloadOverhead is a 32 byte nonce and a 32 byte MAC.
	V4LocalPayloadOverhead = 32 + 32
	// V4PublicPayloadOverhead is a 64 byte signature.
	V4PublicPayloadOverhead = 64
)

// Budget identifies which parser size budget a BudgetError refers to.
type Budget uint8

const (
	// TokenBudget is the budget for the length of the full token string.
	TokenBudget Budget = iota
	// PayloadBudget is the budget for the length of the decoded payload.
	PayloadBudget
	// FooterBudget is the budget for the length of the decoded footer.
	FooterBudget
)

func (b Budget) String() string {
	switch b {
	case TokenBudget:
		return "token"
	case PayloadBudget:
		return "payload"
	case FooterBudget:
		return "footer"
	default:
		return "unknown"
	}
}

// BudgetError is returned when a token exceeds a size budget configured on
// the parser. It identifies which budget was exceeded, but deliberately does
// not contain the offending token. Use errors.As to inspect it.
type BudgetError struct {
	Budget Budget
	Limit  int
}

func newBudgetError(budget Budget, limit int) BudgetError {
	return BudgetError{budget, limit}
}

func (e BudgetError) Error() string {
	return fmt.Sprintf("%s size exceeds the configured budget of %d bytes", e.Budget, e.Limit)
}

// maxEncodedLen returns the length of the raw base64url encoding of
// decodedLimit bytes. Any encoded input longer than this definitely decodes
// to more than decodedLimit bytes, so it can be rejected before decoding.
// The arithmetic is overflow safe for any non-negative decodedLimit.
func maxEncodedLen(decodedLimit int) int {
	if decodedLimit < 0 {
		return 0
	}

	quotient, remainder := decodedLimit/3, decodedLimit%3
	if quotient > (math.MaxInt-3)/4 {
		return math.MaxInt
	}

	extra := [...]int{0, 2, 3}

	return quotient*4 + extra[remainder]
}
