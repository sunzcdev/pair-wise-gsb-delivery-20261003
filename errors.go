package paseto

import (
	"errors"
	"fmt"
)

// ErrorKind classifies the errors returned by Parser parse methods, so that
// callers can decide how to handle a failure (e.g. which log level to use)
// without matching against error message strings.
type ErrorKind int

const (
	// ErrorKindSyntax indicates the token envelope itself was malformed:
	// an unexpected number of message parts, invalid base64 encoding, or a
	// truncated payload.
	ErrorKindSyntax ErrorKind = iota + 1
	// ErrorKindProtocolMismatch indicates the token header did not match the
	// protocol (version and purpose) the parser was asked to use.
	ErrorKindProtocolMismatch
	// ErrorKindAuthentication indicates cryptographic authentication of the
	// token failed: a bad signature, a bad message authentication code, or a
	// message which could not be decrypted.
	ErrorKindAuthentication
	// ErrorKindClaimsDecode indicates the token was authentic, but its claims
	// payload could not be decoded.
	ErrorKindClaimsDecode
	// ErrorKindRule indicates the token was authentic and decoded, but was
	// rejected by a validation Rule.
	ErrorKindRule
)

// String returns a short, log-safe description of the ErrorKind. It never
// contains token, key, or claim data.
func (k ErrorKind) String() string {
	switch k {
	case ErrorKindSyntax:
		return "malformed token"
	case ErrorKindProtocolMismatch:
		return "protocol mismatch"
	case ErrorKindAuthentication:
		return "authentication failed"
	case ErrorKindClaimsDecode:
		return "claims could not be decoded"
	case ErrorKindRule:
		return "validation rule failed"
	default:
		return "unclassified parse failure"
	}
}

// ParseError is the classified error returned by all Parser parse methods
// (ParseV2Local, ParseV2Public, ParseV3Local, ParseV3Public, ParseV4Local,
// ParseV4Public). Its Error text is a safe summary only: it never contains
// the token, key material, or claim values. The original error chain is
// preserved and can be inspected with errors.Is and errors.As.
type ParseError struct {
	kind      ErrorKind
	ruleName  string
	ruleIndex int
	err       error
}

func newParseError(err error) error {
	var parseErr *ParseError
	if errors.As(err, &parseErr) {
		return err
	}

	var ruleErr RuleError
	if errors.As(err, &ruleErr) {
		return &ParseError{
			kind:      ErrorKindRule,
			ruleName:  ruleErr.ruleName,
			ruleIndex: ruleErr.ruleIndex,
			err:       err,
		}
	}

	var tokenErr TokenError
	if errors.As(err, &tokenErr) && tokenErr.kind != 0 {
		return &ParseError{kind: tokenErr.kind, err: err}
	}

	return &ParseError{err: err}
}

// Error returns a safe summary of the failure. It never contains the token,
// key material, or claim values.
func (e *ParseError) Error() string {
	return "paseto: " + e.kind.String()
}

// Unwrap returns the original error chain which caused this parse failure.
func (e *ParseError) Unwrap() error {
	return e.err
}

// Kind returns the classification of this parse failure.
func (e *ParseError) Kind() ErrorKind {
	return e.kind
}

// RuleName returns the function name of the Rule which rejected the token
// when Kind is ErrorKindRule, and the empty string otherwise.
func (e *ParseError) RuleName() string {
	if e.kind != ErrorKindRule {
		return ""
	}

	return e.ruleName
}

// RuleIndex returns the index (within the Parser's rule set) of the Rule
// which rejected the token when Kind is ErrorKindRule, and -1 otherwise.
func (e *ParseError) RuleIndex() int {
	if e.kind != ErrorKindRule {
		return -1
	}

	return e.ruleIndex
}

// Classify reports the ErrorKind of an error returned by a Parser parse
// method, allowing callers to e.g. select a log level without matching
// error message strings. The second return value is false if err is not a
// classified parse error.
func Classify(err error) (ErrorKind, bool) {
	var parseErr *ParseError
	if errors.As(err, &parseErr) {
		return parseErr.kind, true
	}

	return 0, false
}

// Any cryptography issue (with the token) or formatting error.
// This does not include cryptography errors with input key material, these will
// return regular errors.
type TokenError struct {
	e    error
	kind ErrorKind
}

func newTokenError(e error) error {
	return newTokenErrorKind(ErrorKindSyntax, e)
}

func newTokenErrorKind(kind ErrorKind, e error) error {
	return TokenError{e: e, kind: kind}
}

func (e TokenError) Error() string {
	return e.e.Error()
}

func (TokenError) Is(e error) bool {
	_, ok1 := e.(TokenError)
	_, ok2 := e.(*TokenError)
	return ok1 || ok2
}

func (e TokenError) Unwrap() error {
	return e.e
}

// Any error which is the result of a rule failure (distinct from a TokenError)
// Can be used to detect cryptographically valid tokens which have failed only
// due to a rule failure: which may warrant a slightly different processing
// follow up.
type RuleError struct {
	e         error
	ruleName  string
	ruleIndex int
}

func newRuleError(e error, ruleName string, ruleIndex int) RuleError {
	return RuleError{e: e, ruleName: ruleName, ruleIndex: ruleIndex}
}

func (e RuleError) Error() string {
	return e.e.Error()
}

func (RuleError) Is(e error) bool {
	_, ok1 := e.(RuleError)
	_, ok2 := e.(*RuleError)
	return ok1 || ok2
}

func (e RuleError) Unwrap() error {
	return e.e
}

func errorKeyLength(expected, given int) error {
	return fmt.Errorf("key length incorrect (%d), expected %d", given, expected)
}

var errorKeyWrongCurve = fmt.Errorf("input key was for the wrong curve")

func errorSeedLength(expected, given int) error {
	return fmt.Errorf("seed length incorrect (%d), expected %d", given, expected)
}

func errorMessageParts(given int) error {
	return newTokenError(fmt.Errorf("invalid number of message parts in token (%d)", given))
}

func errorMessageHeader(expected Protocol, givenHeader string) error {
	return newTokenErrorKind(ErrorKindProtocolMismatch, fmt.Errorf("message header `%s' is not valid, expected `%s'", givenHeader, expected.Header()))
}

func errorMessageHeaderDecrypt(expected Protocol, givenHeader string) error {
	return fmt.Errorf("cannot decrypt message: %w", errorMessageHeader(expected, givenHeader))
}

func errorMessageHeaderVerify(expected Protocol, givenHeader string) error {
	return fmt.Errorf("cannot verify message: %w", errorMessageHeader(expected, givenHeader))
}

var unsupportedPasetoVersion = fmt.Errorf("unsupported PASETO version")
var unsupportedPasetoPurpose = fmt.Errorf("unsupported PASETO purpose")
var unsupportedPayload = fmt.Errorf("unsupported payload")

var errorPayloadShort = newTokenError(fmt.Errorf("payload is not long enough to be a valid PASETO message"))
var errorBadSignature = newTokenErrorKind(ErrorKindAuthentication, fmt.Errorf("bad signature"))
var errorBadMAC = newTokenErrorKind(ErrorKindAuthentication, fmt.Errorf("bad message authentication code"))

var errorKeyInvalid = fmt.Errorf("key was not valid")

func errorDecrypt(err error) error {
	return fmt.Errorf("the message could not be decrypted: %w", newTokenErrorKind(ErrorKindAuthentication, err))
}
