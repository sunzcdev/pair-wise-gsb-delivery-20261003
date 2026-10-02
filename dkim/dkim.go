// Package dkim creates and verifies DKIM signatures, as specified in RFC 6376.
//
// # FAQ
//
// Why can't I verify a [net/mail.Message] directly? A [net/mail.Message]
// header is already parsed, and whitespace characters (especially continuation
// lines) are removed. Thus, the signature computed from the parsed header is
// not the same as the one computed from the raw header.
//
// How can I publish my public key? You have to add a TXT record to your DNS
// zone. See [RFC 6376 appendix C]. You can use the dkim-keygen tool included
// in go-msgauth to generate the key and the TXT record.
//
// Does a passing signature guarantee that From or Subject is protected? No:
// cryptographic verification only proves the signed data is unmodified. An
// application that needs to know exactly which header instances are signed
// (important when a header occurs more than once) can inspect
// [Verification.HeaderCoverage], or supply a [Policy] via [VerifyOptions] and
// read [Verification.PolicyResult]. The two conclusions are kept separate: a
// signature can verify cryptographically yet fail the policy.
//
// [RFC 6376 appendix C]: https://tools.ietf.org/html/rfc6376#appendix-C
package dkim

import (
	"time"
)

var now = time.Now

const headerFieldName = "DKIM-Signature"
