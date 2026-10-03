package digest

import (
	"crypto/md5"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
)

// sessRefInput is the fixed input set used by the *-sess golden vectors.
type sessRefInput struct {
	username  string
	realm     string
	password  string
	a1        string // precomputed base HA1 (hex); when set, password is ignored
	nonce     string
	cnonce    string
	method    string
	uri       string
	body      string
	algorithm string
	qop       string // "", "auth" or "auth-int"
}

// sessReference is a standalone implementation of the *-sess response
// calculation written only with standard library primitives. It deliberately
// does not call Digest, parseAlgorithm, hashjoin or any other production code,
// so the golden values are not derived from the functions they test.
func sessReference(in sessRefInput) string {
	h, err := refNewHash(in.algorithm)
	if err != nil {
		panic(err)
	}
	sum := func(parts ...string) string {
		h.Reset()
		for i, part := range parts {
			if i > 0 {
				h.Write([]byte(":"))
			}
			h.Write([]byte(part))
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	// base HA1 = H(username:realm:password)
	ha1 := in.a1
	if ha1 == "" {
		ha1 = sum(in.username, in.realm, in.password)
	}
	// session HA1 = H(HA1:nonce:cnonce)
	ha1sess := sum(ha1, in.nonce, in.cnonce)
	// HA2 depends on the qop
	var ha2 string
	switch in.qop {
	case "", "auth":
		ha2 = sum(in.method, in.uri)
	case "auth-int":
		ha2 = sum(in.method, in.uri, sum(in.body))
	default:
		panic("sessReference: unsupported qop: " + in.qop)
	}
	if in.qop == "" {
		return sum(ha1sess, in.nonce, ha2)
	}
	return sum(ha1sess, in.nonce, "00000001", in.cnonce, in.qop, ha2)
}

// refNewHash maps an algorithm name to its standard library hash constructor.
// The matching rules mirror RFC 7616 but this is independent code.
func refNewHash(algorithm string) (hash.Hash, error) {
	name := strings.ToUpper(algorithm)
	if name == "" {
		name = "MD5"
	}
	if !strings.HasSuffix(name, "-SESS") {
		return nil, fmt.Errorf("reference: not a sess algorithm: %q", algorithm)
	}
	switch name[:len(name)-len("-SESS")] {
	case "MD5":
		return md5.New(), nil
	case "SHA-256":
		return sha256.New(), nil
	case "SHA-512-256":
		return sha512.New512_256(), nil
	default:
		return nil, fmt.Errorf("reference: unsupported algorithm: %q", algorithm)
	}
}

// refMD5Join is a small independent MD5 hex helper used in tests.
func refMD5Join(parts ...string) string {
	h := md5.New()
	for i, part := range parts {
		if i > 0 {
			h.Write([]byte(":"))
		}
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}
