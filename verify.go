package digest

import (
	"crypto/subtle"
	"fmt"
	"hash"
)

// verifyAuthenticationInfo is the default AuthenticationInfoVerifier.
//
// It recomputes rspauth from the credentials the Transport actually sent and
// checks it in constant time. Only qop=auth is verified here; qop=auth-int
// requires hashing the response body, which is deliberately out of scope.
func (t *Transport) verifyAuthenticationInfo(cred *Credentials, chal *Challenge, ai *AuthenticationInfo) error {
	// the server must echo the qop, cnonce and nc it is acknowledging
	if ai.QOP != cred.QOP {
		return fmt.Errorf("digest: authentication-info qop %q does not match credentials %q", ai.QOP, cred.QOP)
	}
	if ai.QOP == "auth-int" {
		return fmt.Errorf("digest: authentication-info rspauth verification is not supported for qop=auth-int")
	}
	if ai.Cnonce != "" && ai.Cnonce != cred.Cnonce {
		return fmt.Errorf("digest: authentication-info cnonce does not match credentials")
	}
	if ai.Nc != 0 && ai.Nc != cred.Nc {
		return fmt.Errorf("digest: authentication-info nc %08x does not match credentials %08x", ai.Nc, cred.Nc)
	}
	if ai.RspAuth == "" {
		return fmt.Errorf("digest: authentication-info is missing rspauth")
	}
	h, err := newHasher(chal.Algorithm)
	if err != nil {
		return err
	}
	// A1 is H(username, realm, password). Userhash only changes the username
	// carried in the credentials, not the A1 inputs.
	a1 := hashjoin(h, t.Username, chal.Realm, t.Password)
	rspauth := rspauth(h, a1, cred, chal)
	if subtle.ConstantTimeCompare([]byte(rspauth), []byte(ai.RspAuth)) != 1 {
		return fmt.Errorf("digest: authentication-info rspauth mismatch")
	}
	return nil
}

// rspauth computes the expected rspauth value (RFC 2617 §3.2.3).
// With qop=auth, A2 is method:uri and the response hash replaces response.
func rspauth(h hash.Hash, a1 string, cred *Credentials, chal *Challenge) string {
	// A2 is ":uri" for rspauth; the request method is deliberately omitted
	// (RFC 2617 §3.2.3).
	a2 := hashjoin(h, "", cred.URI)
	switch {
	case cred.QOP == "auth":
		return hashjoin(h,
			a1,
			cred.Nonce,
			fmt.Sprintf("%08x", cred.Nc),
			cred.Cnonce,
			cred.QOP,
			a2,
		)
	default:
		// legacy (no qop): H(A1, nonce, A2)
		return hashjoin(h,
			a1,
			cred.Nonce,
			a2,
		)
	}
}
