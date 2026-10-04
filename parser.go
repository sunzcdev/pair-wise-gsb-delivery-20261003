package paseto

import (
	"time"

	"aidanwoods.dev/go-result/result"
)

// Parser is used to verify or decrypt a token, and can be provided with
// a set of rules.
type Parser struct {
	rules []CheckedRule
}

// adaptRule exposes a legacy Rule to the read-only rule pipeline. The legacy
// rule receives a deep clone of the verified token: anything it does through
// the mutable Token API (Set, SetFooter, mutating a footer it fetched, and so
// on) is confined to that clone and can never reach the token the parser
// returns. This preserves the Rule entry point while clearly isolating it.
func adaptRule(rule Rule) CheckedRule {
	return func(view CheckedToken) error {
		return rule(view.(checkedToken).token.clone())
	}
}

// NewParser returns a parser with NotExpired rule preloaded.
func NewParser() Parser {
	return Parser{[]CheckedRule{adaptRule(NotExpired())}}
}

// NewParserWithoutExpiryCheck returns a parser with no currently set rules.
func NewParserWithoutExpiryCheck() Parser {
	return Parser{nil}
}

// NewParserForValidNow returns a parser that will require parsed tokens to be
// valid "now".
func NewParserForValidNow() Parser {
	return Parser{[]CheckedRule{adaptRule(ValidAt(time.Now()))}}
}

// MakeParser allows a parser to be constructed with a specified set of rules.
func MakeParser(rules []Rule) Parser {
	return Parser{adaptRules(rules)}
}

// MakeCheckedParser allows a parser to be constructed with a specified set of
// read-only CheckedRules.
func MakeCheckedParser(rules []CheckedRule) Parser {
	return Parser{cloneCheckedRules(rules)}
}

func adaptRules(rules []Rule) []CheckedRule {
	compiled := make([]CheckedRule, len(rules))
	for i, rule := range rules {
		compiled[i] = adaptRule(rule)
	}

	return compiled
}

func cloneCheckedRules(rules []CheckedRule) []CheckedRule {
	compiled := make([]CheckedRule, len(rules))
	copy(compiled, rules)

	return compiled
}

// ParseV2Local will parse and decrypt a v2 local paseto and validate against
// any parser rules. Error if parsing, decryption, or any rule fails.
func (p Parser) ParseV2Local(key V2SymmetricKey, tainted string) (*Token, error) {
	msg := newMessage(V2Local, tainted)
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
	msg := newMessage(V2Public, tainted)
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
	msg := newMessage(V3Local, tainted)
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
	msg := newMessage(V3Public, tainted)
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
	msg := newMessage(V4Local, tainted)
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
	msg := newMessage(V4Public, tainted)
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
	p.rules = adaptRules(rules)
}

// SetCheckedRules will overwrite any currently set rules with the specified
// read-only CheckedRules.
func (p *Parser) SetCheckedRules(rules []CheckedRule) {
	p.rules = cloneCheckedRules(rules)
}

// AddRule will add the given rule(s) to any already specified.
func (p *Parser) AddRule(rule ...Rule) {
	for _, r := range rule {
		p.rules = append(p.rules, adaptRule(r))
	}
}

// AddCheckedRule will add the given read-only CheckedRule(s) to any already
// specified. Legacy rules and checked rules can be interleaved; they always
// run in the order they were added.
func (p *Parser) AddCheckedRule(rule ...CheckedRule) {
	p.rules = append(p.rules, rule...)
}

func (p Parser) validate(token Token) result.Result[Token] {
	view := checkedTokenFrom(token)

	for _, rule := range p.rules {
		if err := rule(view); err != nil {
			return result.Err[Token](newRuleError(err))
		}
	}

	return result.Ok(token)
}
