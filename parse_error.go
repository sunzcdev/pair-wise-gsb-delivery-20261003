package paseto

import (
	"errors"
	"reflect"
	"runtime"
)

// ErrorKind classifies the failures which can occur while parsing a PASETO
// token. It allows callers to make stable decisions (e.g. log level, retry
// behaviour) without matching on error message strings.
type ErrorKind int

const (
	errorKindUnset ErrorKind = iota
	// ErrorKindSyntax indicates the token envelope itself was malformed:
	// wrong number of message parts, invalid base64, or a payload too short
	// to be a valid PASETO message.
	ErrorKindSyntax
	// ErrorKindProtocol indicates the token header did not match the
	// protocol (version and purpose) the parser expected.
	ErrorKindProtocol
	// ErrorKindAuthentication indicates cryptographic authentication of the
	// token failed: bad signature, bad message authentication code, or a
	// decryption failure.
	ErrorKindAuthentication
	// ErrorKindClaims indicates the token was authenticated but its claims
	// payload could not be decoded as JSON.
	ErrorKindClaims
	// ErrorKindRule indicates the token was authenticated but rejected by a
	// validation Rule.
	ErrorKindRule
)

// String returns a short stable label for the kind, suitable for structured
// logging.
func (k ErrorKind) String() string {
	switch k {
	case ErrorKindSyntax:
		return "syntax"
	case ErrorKindProtocol:
		return "protocol"
	case ErrorKindAuthentication:
		return "authentication"
	case ErrorKindClaims:
		return "claims"
	case ErrorKindRule:
		return "rule"
	default:
		return "unknown"
	}
}

// Safe summaries for public error text. These MUST remain static: they must
// never interpolate token contents, key material, or claim values.
const (
	summarySyntax         = "paseto: token is malformed (invalid message envelope)"
	summaryProtocol       = "paseto: token header does not match the expected protocol"
	summaryAuthentication = "paseto: token failed cryptographic authentication"
	summaryClaims         = "paseto: token claims could not be decoded"
	summaryRule           = "paseto: token rejected by a validation rule"
)

func summaryForKind(kind ErrorKind) string {
	switch kind {
	case ErrorKindSyntax:
		return summarySyntax
	case ErrorKindProtocol:
		return summaryProtocol
	case ErrorKindAuthentication:
		return summaryAuthentication
	case ErrorKindClaims:
		return summaryClaims
	case ErrorKindRule:
		return summaryRule
	default:
		return "paseto: token could not be parsed"
	}
}

// ParseError is the classified error returned by the Parser parse functions.
// Its Error text is a safe static summary; the underlying (more detailed)
// error chain is preserved and can be inspected with errors.Is/errors.As.
type ParseError struct {
	kind      ErrorKind
	summary   string
	cause     error
	ruleIndex int
	ruleName  string
}

func newParseError(kind ErrorKind, cause error) *ParseError {
	return &ParseError{
		kind:      kind,
		summary:   summaryForKind(kind),
		cause:     cause,
		ruleIndex: -1,
	}
}

func newRuleParseError(index int, rule Rule, cause error) *ParseError {
	name := "unknown"
	if fn := runtime.FuncForPC(reflect.ValueOf(rule).Pointer()); fn != nil {
		name = fn.Name()
	}

	return &ParseError{
		kind:      ErrorKindRule,
		summary:   summaryForKind(ErrorKindRule),
		cause:     newRuleError(cause),
		ruleIndex: index,
		ruleName:  name,
	}
}

// Error returns a safe static summary of the failure. It never contains the
// token, key material, or claim values.
func (e *ParseError) Error() string {
	return e.summary
}

// Unwrap returns the underlying error chain, preserving the original
// (detailed) cause for errors.Is/errors.As.
func (e *ParseError) Unwrap() error {
	return e.cause
}

// Kind returns the classification of this error.
func (e *ParseError) Kind() ErrorKind {
	return e.kind
}

// RuleIndex returns the index of the failing rule in the parser's rule set,
// or -1 if this error was not caused by a rule failure.
func (e *ParseError) RuleIndex() int {
	return e.ruleIndex
}

// RuleName returns the name of the failing rule function, or an empty string
// if this error was not caused by a rule failure.
func (e *ParseError) RuleName() string {
	return e.ruleName
}

// Classify extracts the ErrorKind from any error returned by this package's
// parse functions. The second return value is false if the error does not
// carry a classification.
func Classify(err error) (ErrorKind, bool) {
	var parseErr *ParseError
	if errors.As(err, &parseErr) {
		return parseErr.Kind(), true
	}

	return errorKindUnset, false
}
