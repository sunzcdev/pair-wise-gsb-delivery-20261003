package paseto

import (
	"time"

	"aidanwoods.dev/go-result/result"
)

// Parser is used to verify or decrypt a token, and can be provided with
// a set of rules.
type Parser struct {
	rules []parserRule
}

// NewParser returns a parser with NotExpired rule preloaded.
func NewParser() Parser {
	return Parser{[]parserRule{adaptRule(NotExpired())}}
}

// NewParserWithoutExpiryCheck returns a parser with no currently set rules.
func NewParserWithoutExpiryCheck() Parser {
	return Parser{nil}
}

// NewParserForValidNow returns a parser that will require parsed tokens to be
// valid "now".
func NewParserForValidNow() Parser {
	return Parser{[]parserRule{adaptRule(ValidAt(time.Now()))}}
}

// MakeParser allows a parser to be constructed with a specified set of rules.
func MakeParser(rules []Rule) Parser {
	parser := Parser{}
	parser.SetRules(rules)

	return parser
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
	p.rules = nil
	p.AddRule(rules...)
}

// AddRule will add the given rule(s) to any already specified. Rules are
// executed in the order they are added.
//
// Isolation: each legacy Rule is executed against an isolated deep copy of
// the parsed token. Any mutation a rule performs (Set, SetFooter, or
// modifying previously extracted footers or claims) is discarded and cannot
// affect the token delivered by the parser, nor the token seen by other
// rules. Prefer AddReadOnlyRule for new rules, which enforces this at the
// type level.
func (p *Parser) AddRule(rule ...Rule) {
	for _, r := range rule {
		p.rules = append(p.rules, adaptRule(r))
	}
}

// AddReadOnlyRule will add the given read-only rule(s) to any already
// specified. Rules are executed in the order they are added, interleaved
// with any legacy rules in the order both kinds were added.
//
// Read-only rules receive a ReadOnlyToken view of the authenticated claims
// and footer: they can read them by type, but cannot mutate the token which
// is delivered by the parser.
func (p *Parser) AddReadOnlyRule(rule ...ReadOnlyRule) {
	for _, r := range rule {
		p.rules = append(p.rules, adaptReadOnlyRule(r))
	}
}

func (p Parser) validate(token Token) result.Result[Token] {
	for _, rule := range p.rules {
		if err := rule(token); err != nil {
			return result.Err[Token](newRuleError(err))
		}
	}

	return result.Ok(token)
}

// parserRule is the internal, execution-ready form of a rule. It receives
// the parsed token and is responsible for isolating it as needed.
type parserRule func(token Token) error

// adaptRule adapts a legacy Rule for execution. The rule is run against a
// deep copy of the token so that mutations performed by the rule (the token
// is passed by value, but its claims map and footer would otherwise be
// shared) cannot leak into the token delivered by the parser.
func adaptRule(rule Rule) parserRule {
	return func(token Token) error {
		return rule(token.deepCopy())
	}
}

// adaptReadOnlyRule adapts a ReadOnlyRule for execution. The read-only view
// only exposes getters which return copies, so the parsed token can be
// wrapped directly without copying.
func adaptReadOnlyRule(rule ReadOnlyRule) parserRule {
	return func(token Token) error {
		return rule(token.ReadOnly())
	}
}
