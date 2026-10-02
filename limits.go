package paseto

import (
	"fmt"
	"math"

	"aidanwoods.dev/go-paseto/internal/encoding"
	"aidanwoods.dev/go-result/result"
)

// SizeBudget identifies which size limit a token has exceeded.
type SizeBudget string

const (
	// TokenBudget limits the length, in bytes, of the complete encoded token
	// string (header, payload, and any footer). It is checked first, before the
	// token is split or any base64 data is decoded.
	TokenBudget SizeBudget = "token"

	// PayloadBudget limits the length, in bytes, of the decoded payload section
	// of the token. This is the complete cryptographically wrapped payload, not
	// just the application data. See the Limits documentation for the fixed
	// cryptographic overhead included per protocol.
	PayloadBudget SizeBudget = "payload"

	// FooterBudget limits the length, in bytes, of the decoded footer section
	// of the token. The footer is not cryptographically verified until after
	// the payload, so this budget is enforced independently.
	FooterBudget SizeBudget = "footer"
)

// BudgetExceededError reports that a token exceeded a configured size budget.
// The budget field identifies which limit was exceeded, allowing callers to
// react differently to an oversized token, footer, or payload.
//
// The error never contains the original token.
type BudgetExceededError struct {
	// Budget identifies which size limit was exceeded.
	Budget SizeBudget
	// Limit is the configured maximum, in bytes.
	Limit int
	// Given is the offending size, in bytes. For the token budget this is the
	// encoded token length; for the payload and footer budgets it is the
	// decoded length. When an input is rejected before base64 decoding, it is
	// the exact decoded length implied by the encoded length.
	Given int
}

func (e BudgetExceededError) Error() string {
	return fmt.Sprintf(
		"%s exceeds the configured size limit: %d bytes > %d bytes",
		e.Budget,
		e.Given,
		e.Limit,
	)
}

func (e BudgetExceededError) Is(target error) bool {
	other, ok := target.(BudgetExceededError)
	if !ok {
		_, ok = target.(*BudgetExceededError)
		return ok
	}

	return other.Budget == "" || other.Budget == e.Budget
}

func errorBudgetExceeded(budget SizeBudget, limit, given int) error {
	return newTokenError(BudgetExceededError{
		Budget: budget,
		Limit:  limit,
		Given:  given,
	})
}

// Limits are optional size limits applied to tokens as they enter the parser.
// Each field is a maximum length, in bytes. A value of zero (or negative)
// means no limit for that budget; the zero value of Limits therefore accepts
// tokens of any size.
//
// Limits are enforced before any cryptographic operation runs, so an oversized
// token can never trigger signature verification or decryption work, and parser
// Rules are never invoked for it.
//
// The Token budget covers the full encoded token string supplied to the parse
// method, including the version and purpose header.
//
// The Payload budget covers the decoded payload section. The decoded payload
// contains fixed cryptographic overhead in addition to the application's
// claims JSON, so the space available for claims is smaller than this budget
// by the following amounts:
//
//   - v2.local: 40 bytes (24 byte nonce and 16 byte XChaCha20-Poly1305 tag)
//   - v2.public: 64 bytes (Ed25519 signature)
//   - v3.local: 80 bytes (32 byte nonce and 48 byte HMAC-SHA-384 tag)
//   - v3.public: 96 bytes (ECDSA signature over P-384)
//   - v4.local: 64 bytes (32 byte nonce and 32 byte HMAC-SHA-512/256 tag)
//   - v4.public: 64 bytes (Ed25519 signature)
//
// The Footer budget covers only the decoded footer bytes and is independent
// of the cryptographic overhead above. An absent or empty footer always
// satisfies any non-negative footer limit.
type Limits struct {
	Token   int
	Payload int
	Footer  int
}

func (l Limits) enforceTokenLength(token string) error {
	if l.Token > 0 && len(token) > l.Token {
		return errorBudgetExceeded(TokenBudget, l.Token, len(token))
	}

	return nil
}

// enforceEncoded rejects an encoded base64 section before decoding if its
// guaranteed decoded length cannot fit the budget. The decoded length of valid
// unpadded base64 is a function of the encoded length alone, so oversized
// inputs can be discarded without allocating their decoded bytes.
func (l Limits) enforceEncoded(budget SizeBudget, limit int, encoded string) error {
	if limit <= 0 {
		return nil
	}

	decodedLen, ok := encoding.DecodedLen(len(encoded))
	if !ok {
		// Leave structurally invalid base64 to the decoder, which produces a
		// regular token error.
		return nil
	}

	// Compare in int64 so a length beyond the platform's int range cannot
	// wrap negative and slip past the check on a 32 bit platform.
	if decodedLen > int64(limit) {
		given := limit + 1
		if decodedLen <= math.MaxInt {
			given = int(decodedLen)
		}

		return errorBudgetExceeded(budget, limit, given)
	}

	return nil
}

func (l Limits) enforceDecoded(budget SizeBudget, limit int, decoded []byte) error {
	if limit > 0 && len(decoded) > limit {
		return errorBudgetExceeded(budget, limit, len(decoded))
	}

	return nil
}

func (l Limits) checkMessage(protocol Protocol, token string) result.Result[deconstructedToken] {
	if err := l.enforceTokenLength(token); err != nil {
		return result.Err[deconstructedToken](err)
	}

	var parts deconstructedToken
	if err := deconstructToken(token).Ok(&parts); err != nil {
		return result.Err[deconstructedToken](err)
	}

	if parts.header != protocol.Header() {
		return result.Err[deconstructedToken](plainHeaderError(protocol, parts.header))
	}

	if err := l.enforceEncoded(PayloadBudget, l.Payload, parts.encodedPayload); err != nil {
		return result.Err[deconstructedToken](err)
	}

	if err := l.enforceEncoded(FooterBudget, l.Footer, parts.encodedFooter); err != nil {
		return result.Err[deconstructedToken](err)
	}

	return result.Ok(parts)
}
