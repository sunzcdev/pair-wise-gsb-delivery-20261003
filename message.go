package paseto

import (
	"strings"

	"aidanwoods.dev/go-paseto/internal/encoding"
	"aidanwoods.dev/go-result/result"
)

// Message is a building block type, only use if you need to use Paseto
// cryptography without Paseto's token or validator semantics.
type message struct {
	protocol Protocol
	p        payload
	footer   []byte
}

// newMessage creates a new message from the given token, with an expected
// protocol and size limits. If the token does not match the expected protocol,
// exceeds a limit, or cannot be parsed, an error is returned instead.
func newMessage(protocol Protocol, limits Limits, token string) result.Result[message] {
	var parts deconstructedToken
	if err := limits.checkMessage(protocol, token).Ok(&parts); err != nil {
		return result.Err[message](singleTokenError(err))
	}

	decoded := encoding.Decode(parts.encodedPayload)
	checkedPayload := result.FlatMap(decoded, func(bytes []byte) result.Result[[]byte] {
		if err := limits.enforceDecoded(PayloadBudget, limits.Payload, bytes); err != nil {
			return result.Err[[]byte](err)
		}

		return result.Ok(bytes)
	})
	p := result.FlatMap(checkedPayload, protocol.newPayload)

	footer := result.FlatMap(encoding.Decode(parts.encodedFooter), func(bytes []byte) result.Result[[]byte] {
		if err := limits.enforceDecoded(FooterBudget, limits.Footer, bytes); err != nil {
			return result.Err[[]byte](err)
		}

		return result.Ok(bytes)
	})

	msg := result.Map2(p, footer, newMessageFromPayloadAndFooter)

	return msg.MapError(singleTokenError)
}

// Header returns the header string for a Paseto message.
func (m message) header() string {
	return m.protocol.Header()
}

// UnsafeFooter returns the footer of a Paseto message. Beware that this footer
// is not cryptographically verified at this stage.
func (m message) unsafeFooter() []byte {
	return m.footer
}

// Encoded returns the string representation of a Paseto message.
func (m message) encoded() string {
	main := m.header() + encoding.Encode(m.p.bytes())

	if len(m.footer) == 0 {
		return main
	}

	return main + "." + encoding.Encode(m.footer)
}

func newMessageFromPayloadAndFooter(payload payload, footer []byte) message {
	// Assume internal callers won't construct bad payloads
	protocol := protocolForPayload(payload).Expect("sanity check for payload failed")
	return message{protocol, payload, footer}
}

type deconstructedToken struct {
	header         string
	encodedPayload string
	encodedFooter  string
}

func deconstructToken(token string) result.Result[deconstructedToken] {
	parts := strings.Split(token, ".")

	partsLen := len(parts)
	if partsLen != 3 && partsLen != 4 {
		return result.Err[deconstructedToken](errorMessageParts(len(parts)))
	}

	header := parts[0] + "." + parts[1] + "."
	encodedPayload := parts[2]

	encodedFooter := ""
	if partsLen == 4 {
		encodedFooter = parts[3]
	}

	return result.Ok(deconstructedToken{
		header:         header,
		encodedPayload: encodedPayload,
		encodedFooter:  encodedFooter,
	})
}

// V2Verify will verify a v2 public paseto message. Will return a pointer to
// the verified token (but not validated with rules) if successful, or error in
// the event of failure.
func (m message) v2Verify(key V2AsymmetricPublicKey) result.Result[Token] {
	return result.FlatMap(v2PublicVerify(m, key), packet.token)
}

// V2Decrypt will decrypt a v2 local paseto message. Will return a pointer to
// the decrypted token (but not validated with rules) if successful, or error in
// the event of failure.
func (m message) v2Decrypt(key V2SymmetricKey) result.Result[Token] {
	return result.FlatMap(v2LocalDecrypt(m, key), packet.token)
}

// V3Verify will verify a v4 public paseto message. Will return a pointer to
// the verified token (but not validated with rules) if successful, or error in
// the event of failure.
func (m message) v3Verify(key V3AsymmetricPublicKey, implicit []byte) result.Result[Token] {
	return result.FlatMap(v3PublicVerify(m, key, implicit), packet.token)
}

// V3Decrypt will decrypt a v3 local paseto message. Will return a pointer to
// the decrypted token (but not validated with rules) if successful, or error in
// the event of failure.
func (m message) v3Decrypt(key V3SymmetricKey, implicit []byte) result.Result[Token] {
	return result.FlatMap(v3LocalDecrypt(m, key, implicit), packet.token)
}

// V4Verify will verify a v4 public paseto message. Will return a pointer to
// the verified token (but not validated with rules) if successful, or error in
// the event of failure.
func (m message) v4Verify(key V4AsymmetricPublicKey, implicit []byte) result.Result[Token] {
	return result.FlatMap(v4PublicVerify(m, key, implicit), packet.token)
}

// V4Decrypt will decrypt a v4 local paseto message. Will return a pointer to
// the decrypted token (but not validated with rules) if successful, or error in
// the event of failure.
func (m message) v4Decrypt(key V4SymmetricKey, implicit []byte) result.Result[Token] {
	return result.FlatMap(v4LocalDecrypt(m, key, implicit), packet.token)
}
