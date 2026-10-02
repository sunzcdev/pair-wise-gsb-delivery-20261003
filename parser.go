package paseto

import (
	"time"

	"aidanwoods.dev/go-result/result"
)

// Parser is used to verify or decrypt a token, and can be provided with
// a set of rules.
type Parser struct {
	rules []Rule

	maxTokenLength   int
	maxPayloadLength int
	maxFooterLength  int
}

// NewParser returns a parser with NotExpired rule preloaded.
func NewParser() Parser {
	return Parser{rules: []Rule{NotExpired()}}
}

// NewParserWithoutExpiryCheck returns a parser with no currently set rules.
func NewParserWithoutExpiryCheck() Parser {
	return Parser{}
}

// NewParserForValidNow returns a parser that will require parsed tokens to be
// valid "now".
func NewParserForValidNow() Parser {
	return Parser{rules: []Rule{ValidAt(time.Now())}}
}

// MakeParser allows a parser to be constructed with a specified set of rules.
func MakeParser(rules []Rule) Parser {
	return Parser{rules: rules}
}

// ParseV2Local will parse and decrypt a v2 local paseto and validate against
// any parser rules. Error if parsing, decryption, or any rule fails.
func (p Parser) ParseV2Local(key V2SymmetricKey, tainted string) (*Token, error) {
	msg := p.parseMessage(V2Local, tainted)
	decrypted := result.FlatMap(msg, func(m message) result.Result[Token] {
		return m.v2Decrypt(key)
	})
	token := result.FlatMap(decrypted, p.validate)

	var t Token
	if err := token.Ok(&t); err != nil {
		return nil, err
	}

	return &t, nil
}

// ParseV2Public will parse and verify a v2 public paseto and validate against
// any parser rules. Error if parsing, verification, or any rule fails.
func (p Parser) ParseV2Public(key V2AsymmetricPublicKey, tainted string) (*Token, error) {
	msg := p.parseMessage(V2Public, tainted)
	decrypted := result.FlatMap(msg, func(m message) result.Result[Token] {
		return m.v2Verify(key)
	})
	token := result.FlatMap(decrypted, p.validate)

	var t Token
	if err := token.Ok(&t); err != nil {
		return nil, err
	}

	return &t, nil
}

// ParseV3Local will parse and decrypt a v3 local paseto and validate against
// any parser rules. Error if parsing, decryption, or any rule fails.
func (p Parser) ParseV3Local(key V3SymmetricKey, tainted string, implicit []byte) (*Token, error) {
	msg := p.parseMessage(V3Local, tainted)
	decrypted := result.FlatMap(msg, func(m message) result.Result[Token] {
		return m.v3Decrypt(key, implicit)
	})
	token := result.FlatMap(decrypted, p.validate)

	var t Token
	if err := token.Ok(&t); err != nil {
		return nil, err
	}

	return &t, nil
}

// ParseV3Public will parse and verify a v3 public paseto and validate against
// any parser rules. Error if parsing, verification, or any rule fails.
func (p Parser) ParseV3Public(key V3AsymmetricPublicKey, tainted string, implicit []byte) (*Token, error) {
	msg := p.parseMessage(V3Public, tainted)
	decrypted := result.FlatMap(msg, func(m message) result.Result[Token] {
		return m.v3Verify(key, implicit)
	})
	token := result.FlatMap(decrypted, p.validate)

	var t Token
	if err := token.Ok(&t); err != nil {
		return nil, err
	}

	return &t, nil
}

// ParseV4Local will parse and decrypt a v4 local paseto and validate against
// any parser rules. Error if parsing, decryption, or any rule fails.
func (p Parser) ParseV4Local(key V4SymmetricKey, tainted string, implicit []byte) (*Token, error) {
	msg := p.parseMessage(V4Local, tainted)
	decrypted := result.FlatMap(msg, func(m message) result.Result[Token] {
		return m.v4Decrypt(key, implicit)
	})
	token := result.FlatMap(decrypted, p.validate)

	var t Token
	if err := token.Ok(&t); err != nil {
		return nil, err
	}

	return &t, nil
}

// ParseV4Public will parse and verify a v4 public paseto and validate against
// any parser rules. Error if parsing, verification, or any rule fails.
func (p Parser) ParseV4Public(key V4AsymmetricPublicKey, tainted string, implicit []byte) (*Token, error) {
	msg := p.parseMessage(V4Public, tainted)
	decrypted := result.FlatMap(msg, func(m message) result.Result[Token] {
		return m.v4Verify(key, implicit)
	})
	token := result.FlatMap(decrypted, p.validate)

	var t Token
	if err := token.Ok(&t); err != nil {
		return nil, err
	}

	return &t, nil
}

// UnsafeParseFooter returns the footer of a Paseto message. Beware that this
// footer is not cryptographically verified at this stage, nor are any claims
// validated.
func (p Parser) UnsafeParseFooter(protocol Protocol, tainted string) ([]byte, error) {
	msg := newMessage(protocol, tainted)
	footer := result.Map(msg, message.unsafeFooter)

	var f []byte
	if err := footer.Ok(&f); err != nil {
		return nil, err
	}

	return f, nil
}

// SetRules will overwrite any currently set rules with those specified.
func (p *Parser) SetRules(rules []Rule) {
	p.rules = rules
}

// AddRule will add the given rule(s) to any already specified.
func (p *Parser) AddRule(rule ...Rule) {
	p.rules = append(p.rules, rule...)
}

// SetMaxTokenLength sets an optional budget (in bytes) for the length of the
// full token string. Tokens longer than this are rejected before any parsing
// or cryptographic work. A non-positive value disables the check.
func (p *Parser) SetMaxTokenLength(max int) {
	p.maxTokenLength = max
}

// SetMaxPayloadLength sets an optional budget (in bytes) for the decoded
// payload of parsed tokens. A non-positive value disables the check.
//
// Note the two different size perspectives this budget sits between: the
// budget applies to the raw decoded payload, which carries cryptographic
// overhead (nonce, MAC/tag, or signature) in addition to the application
// claims. If you instead want to budget the application claims, add the
// relevant overhead constant (e.g. V4LocalPayloadOverhead) to your claims
// budget before calling this function: a claims budget of n bytes
// corresponds to a payload budget of n + overhead bytes.
func (p *Parser) SetMaxPayloadLength(max int) {
	p.maxPayloadLength = max
}

// SetMaxFooterLength sets an optional budget (in bytes) for the decoded
// footer of parsed tokens. A non-positive value disables the check.
func (p *Parser) SetMaxFooterLength(max int) {
	p.maxFooterLength = max
}

// parseMessage enforces any configured size budgets and then parses the
// token into a message. Encoded sections which are clearly too large are
// rejected before base64 decoding (based on their encoded length), and the
// exact decoded lengths are verified afterwards.
func (p Parser) parseMessage(protocol Protocol, tainted string) result.Result[message] {
	if p.maxTokenLength > 0 && len(tainted) > p.maxTokenLength {
		return result.Err[message](newBudgetError(TokenBudget, p.maxTokenLength))
	}

	if p.maxPayloadLength > 0 || p.maxFooterLength > 0 {
		var parts deconstructedToken
		if err := deconstructToken(tainted).Ok(&parts); err != nil {
			return result.Err[message](err)
		}

		if p.maxPayloadLength > 0 &&
			len(parts.encodedPayload) > maxEncodedLen(p.maxPayloadLength) {
			return result.Err[message](newBudgetError(PayloadBudget, p.maxPayloadLength))
		}

		if p.maxFooterLength > 0 &&
			len(parts.encodedFooter) > maxEncodedLen(p.maxFooterLength) {
			return result.Err[message](newBudgetError(FooterBudget, p.maxFooterLength))
		}
	}

	msg := newMessage(protocol, tainted)

	return result.FlatMap(msg, func(m message) result.Result[message] {
		if p.maxPayloadLength > 0 && len(m.p.bytes()) > p.maxPayloadLength {
			return result.Err[message](newBudgetError(PayloadBudget, p.maxPayloadLength))
		}

		if p.maxFooterLength > 0 && len(m.footer) > p.maxFooterLength {
			return result.Err[message](newBudgetError(FooterBudget, p.maxFooterLength))
		}

		return result.Ok(m)
	})
}

func (p Parser) validate(token Token) result.Result[Token] {
	for _, rule := range p.rules {
		if err := rule(token); err != nil {
			return result.Err[Token](newRuleError(err))
		}
	}

	return result.Ok(token)
}
