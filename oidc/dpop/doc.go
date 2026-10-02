// Package dpop implements the resource server (RS) side of OAuth 2.0
// Demonstrating Proof of Possession (DPoP) as defined by RFC 9449.
//
// A DPoP proof is a signed JWT that binds an access token request, or an
// access token presented to a resource server, to a public key held by the
// client. A [Verifier] checks the proof header and payload according to
// RFC 9449 section 4.3:
//
//   - the JWS "typ" header is "dpop+jwt" and the "jwk" header carries a
//     usable asymmetric public key;
//   - the signature is valid under the embedded public key and uses an
//     acceptable asymmetric algorithm;
//   - "htm" matches the HTTP method and "htu" the target URI of the request;
//   - "iat" is within the configured clock-skew window;
//   - "jti" has not been seen before (replay detection);
//   - "ath" is the SHA-256 hash of the presented access token; and
//   - the public key's JWK thumbprint matches the key the access token was
//     verified to be bound to.
//
// The caller is responsible for validating the access token itself (by
// minting it, introspecting it, or verifying its signature) and for passing
// in the binding that resulted from that validation via [TokenBinding]. An
// untrusted "cnf" claim read directly from an unverified token string must
// never be passed in: the proof is only as trustworthy as the token binding
// it is checked against. [BindingFromClaims] extracts the binding from the
// already-verified claims of a JWT access token.
//
// Replay protection is delegated to the [ReplayStore] interface. Instances
// running behind a shared store (for example a Redis-backed implementation
// using a SET NX operation) enforce atomic check-and-set across every
// service instance.
//
// [NonceIssuer] implements the DPoP nonce challenge of RFC 9449 section 8,
// letting a server demand a fresh proof for a previously unseen key and
// rotate nonces.
package dpop
