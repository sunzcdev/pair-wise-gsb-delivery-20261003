package paseto

import (
	"encoding/json"
	"fmt"
	"time"
)

// ReadOnlyToken is a read-only view of a token's authenticated claims and
// footer, as passed to ReadOnlyRule functions during parsing. It exposes
// typed getters only: there is no way to mutate the underlying token through
// this view.
//
// Every returned value is a copy (or is decoded fresh from the stored
// claims), so mutating returned slices, maps, or json.RawMessage values does
// not affect the token which is ultimately delivered by the parser.
type ReadOnlyToken interface {
	// Get gets the given key and writes the value into output (which should
	// be a pointer), if present, by parsing the JSON using encoding/json.
	Get(key string, output interface{}) error

	// GetString returns the value for a given key as a string, or error if
	// this is not possible.
	GetString(key string) (string, error)

	// GetTime returns the value for a given key as a time, or error if this
	// is not possible.
	GetTime(key string) (time.Time, error)

	// GetRaw returns a copy of the raw JSON stored for the given key, or
	// error if the key is not present.
	GetRaw(key string) (json.RawMessage, error)

	// Claims returns the token's claims, decoded into a fresh map.
	Claims() map[string]interface{}

	// ClaimsJSON returns the token's claims as JSON.
	ClaimsJSON() []byte

	// Footer returns a copy of the token's footer.
	Footer() []byte

	GetAudience() (string, error)
	GetExpiration() (time.Time, error)
	GetIssuedAt() (time.Time, error)
	GetIssuer() (string, error)
	GetJti() (string, error)
	GetNotBefore() (time.Time, error)
	GetSubject() (string, error)
}

// ReadOnlyRule validates a given token for certain required preconditions
// (defined by the rule itself), using only a read-only view of the
// authenticated claims and footer. If validation fails a ReadOnlyRule MUST
// return an error, otherwise error MUST be nil.
//
// Unlike the legacy Rule type, a ReadOnlyRule cannot mutate the token which
// is delivered by the parser: the view has no setters, and all mutable values
// it returns are copies.
type ReadOnlyRule func(token ReadOnlyToken) error

// readOnlyToken is the ReadOnlyToken implementation. It wraps the parsed
// token; since Token's getters already return freshly decoded or copied
// data, no additional copying is needed here.
type readOnlyToken struct {
	token Token
}

// ReadOnly returns a read-only view of this token. The view shares no
// mutable state with the token through any value it returns.
func (t Token) ReadOnly() ReadOnlyToken {
	return readOnlyToken{t}
}

func (t readOnlyToken) Get(key string, output interface{}) error {
	return t.token.Get(key, output)
}

func (t readOnlyToken) GetString(key string) (string, error) {
	return t.token.GetString(key)
}

func (t readOnlyToken) GetTime(key string) (time.Time, error) {
	return t.token.GetTime(key)
}

func (t readOnlyToken) GetRaw(key string) (json.RawMessage, error) {
	value, ok := t.token.claims[key]
	if !ok {
		return nil, fmt.Errorf("value for key `%s' not present in claims", key)
	}

	return copyRawMessage(value), nil
}

func (t readOnlyToken) Claims() map[string]interface{} {
	return t.token.Claims()
}

func (t readOnlyToken) ClaimsJSON() []byte {
	return t.token.ClaimsJSON()
}

func (t readOnlyToken) Footer() []byte {
	return t.token.Footer()
}

func (t readOnlyToken) GetAudience() (string, error) {
	return t.token.GetAudience()
}

func (t readOnlyToken) GetExpiration() (time.Time, error) {
	return t.token.GetExpiration()
}

func (t readOnlyToken) GetIssuedAt() (time.Time, error) {
	return t.token.GetIssuedAt()
}

func (t readOnlyToken) GetIssuer() (string, error) {
	return t.token.GetIssuer()
}

func (t readOnlyToken) GetJti() (string, error) {
	return t.token.GetJti()
}

func (t readOnlyToken) GetNotBefore() (time.Time, error) {
	return t.token.GetNotBefore()
}

func (t readOnlyToken) GetSubject() (string, error) {
	return t.token.GetSubject()
}
