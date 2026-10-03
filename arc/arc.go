// Package arc creates and verifies ARC (Authenticated Received Chain) sets,
// as specified in RFC 8617.
//
// ARC lets a mail relay preserve authentication results (SPF, DKIM, DMARC)
// collected on a message before it is modified in transit. Each relay that
// participates in the chain adds one ARC set consisting of:
//
//   - an ARC-Authentication-Results header field (AAR),
//   - an ARC-Message-Signature header field (AMS),
//   - an ARC-Seal header field (AS).
//
// Every set carries an instance number (the "i=" tag). The first relay uses
// instance 1; each subsequent relay increments the highest existing instance
// number by one.
//
// Cryptographic verification alone does not mean a message should be
// accepted: [Verification] exposes the chain status and every set's signing
// domain so the caller can apply its own trust policy.
//
// Verification semantics differ from DKIM in one important way: only the
// newest AMS is required to have a valid body hash. An older AMS may have a
// stale body hash because a later relay legitimately modified the body; what
// matters for chain validity is that each older AMS header signature still
// verifies and that every ARC-Seal is intact with a consistent "cv=" claim.
package arc

import (
	"errors"

	"github.com/emersion/go-msgauth/dkim"
)

const (
	headerFieldNameAAR = "ARC-Authentication-Results"
	headerFieldNameAMS = "ARC-Message-Signature"
	headerFieldNameAS  = "ARC-Seal"
)

// ChainResult is the overall result of validating an ARC chain.
type ChainResult string

const (
	// ChainNone means the message contains no ARC sets.
	ChainNone ChainResult = "none"
	// ChainPass means the chain is cryptographically valid: the newest AMS
	// verifies against the current message and every ARC-Seal is valid with
	// a consistent "cv=" claim.
	ChainPass ChainResult = "pass"
	// ChainFail means the chain is broken: see Verification.Err for the
	// stage where validation failed.
	ChainFail ChainResult = "fail"
)

// ErrMalformedChain is returned when the sets do not form a valid chain
// (missing instance numbers, duplicate instance numbers, a set missing one
// of its AAR/AMS/AS fields, ...).
var ErrMalformedChain = errors.New("arc: malformed ARC chain")

type permFailError string

func (err permFailError) Error() string { return "arc: " + string(err) }

// IsPermFail reports whether err is a permanent failure (e.g. malformed
// fields or revoked keys).
func IsPermFail(err error) bool {
	_, ok := err.(permFailError)
	return ok || dkim.IsPermFail(err)
}

type tempFailError string

func (err tempFailError) Error() string { return "arc: " + string(err) }

// IsTempFail reports whether err is a temporary failure (e.g. a DNS
// timeout).
func IsTempFail(err error) bool {
	_, ok := err.(tempFailError)
	return ok || dkim.IsTempFail(err)
}

type failError string

func (err failError) Error() string { return "arc: " + string(err) }

func isFail(err error) bool {
	_, ok := err.(failError)
	return ok
}

func asPermFail(err error) error {
	if err == nil {
		return nil
	}
	if IsTempFail(err) {
		return err
	}
	if IsPermFail(err) {
		return err
	}
	return permFailError(err.Error())
}
