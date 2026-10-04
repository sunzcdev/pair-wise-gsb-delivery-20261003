package paseto

import (
	"encoding/json"
	"time"
)

// CheckedToken is a read-only view of a token whose contents have already been
// cryptographically verified (or decrypted). It is the only value handed to a
// CheckedRule while parser rules run.
//
// A CheckedToken exposes no way to alter claims or the footer. Every mutable
// value an accessor returns (slices, maps, and json.RawMessage values) is an
// independent copy, so mutating it cannot become a back door into the verified
// token the parser ultimately returns.
type CheckedToken interface {
	// Get decodes the claim stored under key into output (which must be a
	// pointer), returning an error if the claim is absent or cannot be
	// decoded. Nested maps and slices are decoded into fresh allocations and
	// do not alias the verified token's data.
	Get(key string, output interface{}) error

	// GetString returns the claim stored under key as a string.
	GetString(key string) (string, error)

	// GetTime returns the claim stored under key parsed as an RFC3339 time.
	GetTime(key string) (time.Time, error)

	// GetAudience returns the "aud" field.
	GetAudience() (string, error)
	// GetExpiration returns the "exp" field parsed as a time.
	GetExpiration() (time.Time, error)
	// GetIssuedAt returns the "iat" field parsed as a time.
	GetIssuedAt() (time.Time, error)
	// GetIssuer returns the "iss" field.
	GetIssuer() (string, error)
	// GetJti returns the "jti" field.
	GetJti() (string, error)
	// GetNotBefore returns the "nbf" field parsed as a time.
	GetNotBefore() (time.Time, error)
	// GetSubject returns the "sub" field.
	GetSubject() (string, error)

	// Claims returns the claims decoded into generic Go values. The returned
	// map, and every map and slice nested within it, is an independent copy.
	Claims() map[string]interface{}

	// RawClaims returns the raw JSON encoding of every claim. The returned
	// map and each json.RawMessage value is an independent copy.
	RawClaims() map[string]json.RawMessage

	// ClaimJSON returns a copy of the raw JSON value stored under key and
	// true when the claim exists, otherwise nil and false.
	ClaimJSON(key string) (json.RawMessage, bool)

	// ClaimsJSON returns an independent copy of the claims encoded as a JSON
	// object.
	ClaimsJSON() []byte

	// Footer returns an independent copy of the authenticated footer.
	Footer() []byte
}

// checkedToken is the concrete read-only view given to rules. It embeds the
// verified token by value; its fields are unexported, so rule code can only
// reach the data through the CheckedToken interface.
type checkedToken struct {
	token Token
}

// checkedTokenFrom returns a read-only view of token. The view shares no
// mutable state through its accessors: every slice, map, or json.RawMessage
// returned is copied.
func checkedTokenFrom(token Token) CheckedToken {
	return checkedToken{token: token}
}

func (c checkedToken) Get(key string, output interface{}) error {
	return c.token.Get(key, output)
}

func (c checkedToken) GetString(key string) (string, error) {
	return c.token.GetString(key)
}

func (c checkedToken) GetTime(key string) (time.Time, error) {
	return c.token.GetTime(key)
}

func (c checkedToken) GetAudience() (string, error) {
	return c.token.GetAudience()
}

func (c checkedToken) GetExpiration() (time.Time, error) {
	return c.token.GetExpiration()
}

func (c checkedToken) GetIssuedAt() (time.Time, error) {
	return c.token.GetIssuedAt()
}

func (c checkedToken) GetIssuer() (string, error) {
	return c.token.GetIssuer()
}

func (c checkedToken) GetJti() (string, error) {
	return c.token.GetJti()
}

func (c checkedToken) GetNotBefore() (time.Time, error) {
	return c.token.GetNotBefore()
}

func (c checkedToken) GetSubject() (string, error) {
	return c.token.GetSubject()
}

// Claims decodes into a fresh map of generic values, so nothing in the
// returned value graph aliases the verified token.
func (c checkedToken) Claims() map[string]interface{} {
	return c.token.Claims()
}

func (c checkedToken) RawClaims() map[string]json.RawMessage {
	claims := make(map[string]json.RawMessage, len(c.token.claims))
	for key, value := range c.token.claims {
		claims[key] = cloneRawMessage(value)
	}

	return claims
}

func (c checkedToken) ClaimJSON(key string) (json.RawMessage, bool) {
	value, ok := c.token.claims[key]
	if !ok {
		return nil, false
	}

	return cloneRawMessage(value), true
}

func (c checkedToken) ClaimsJSON() []byte {
	return cloneBytes(c.token.ClaimsJSON())
}

func (c checkedToken) Footer() []byte {
	return cloneBytes(c.token.footer)
}

// CheckedRule validates a cryptographically verified token through a
// read-only CheckedToken view. Because the view has no setters and only
// returns independent copies, a CheckedRule cannot modify the token the
// parser returns. If validation fails a CheckedRule MUST return an error,
// otherwise the error MUST be nil.
type CheckedRule func(token CheckedToken) error
