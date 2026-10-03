package digest

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
)

// Prefix for digest authentication headers
const Prefix = "Digest "

// IsDigest returns true if the header value is a digest auth header
func IsDigest(header string) bool {
	if len(header) < len(Prefix) {
		return false
	}
	return strings.EqualFold(header[:len(Prefix)], Prefix)
}

// CutPrefix removes the digest prefix from the header value
func CutPrefix(header string) (string, bool) {
	if !IsDigest(header) {
		return header, false
	}
	return header[len(Prefix):], true
}

// Options for creating a credentials
type Options struct {
	Method   string
	URI      string
	GetBody  func() (io.ReadCloser, error)
	Count    int
	Username string
	Password string

	// The following are provided for advanced use cases where the client needs
	// to override the default digest calculation behavior. Most users should
	// leave these fields unset.

	// A1 is the precomputed base HA1 value: H(username:realm:password),
	// encoded in lowercase hexadecimal. It never includes the nonce or
	// cnonce, even for "*-sess" algorithms. For "*-sess" algorithms, the
	// session value H(A1:nonce:cnonce) is still derived from this base
	// value, so it is not hashed twice.
	A1 string

	// Cnonce is the client nonce. For "*-sess" algorithms it is required to
	// derive the session HA1 and will be generated when empty. One is also
	// sent in the credentials even when the challenge has no qop value.
	Cnonce string
}

// CanDigest checks if the algorithm and qop are supported
func CanDigest(c *Challenge) bool {
	if _, _, ok := parseAlgorithm(c.Algorithm); !ok {
		return false
	}
	return len(c.QOP) == 0 || c.SupportsQOP("auth") || c.SupportsQOP("auth-int")
}

// parseAlgorithm parses a digest algorithm value. It returns a constructor for
// the corresponding hash and reports whether the algorithm is a "*-sess"
// variant. The matching is case-insensitive.
func parseAlgorithm(algorithm string) (func() hash.Hash, bool, bool) {
	upper := strings.ToUpper(algorithm)
	sess := false
	if upper == "" {
		return md5.New, false, true
	}
	if isSessAlgorithm(upper) {
		sess = true
		upper = strings.TrimSuffix(upper, "-SESS")
	}
	var newhash func() hash.Hash
	switch upper {
	case "MD5":
		newhash = md5.New
	case "SHA-256":
		newhash = sha256.New
	case "SHA-512":
		newhash = sha512.New
	case "SHA-512-256":
		newhash = sha512.New512_256
	default:
		return nil, false, false
	}
	return newhash, sess, true
}

// isSessAlgorithm reports whether the algorithm value names a "*-sess" variant.
func isSessAlgorithm(algorithm string) bool {
	return strings.HasSuffix(strings.ToUpper(algorithm), "-SESS")
}

// Digest creates credentials from a challenge and request options.
// Note: if you want to re-use a challenge, you must increment the Count.
func Digest(chal *Challenge, o Options) (*Credentials, error) {
	cred := &Credentials{
		Username:  o.Username,
		URI:       o.URI,
		Cnonce:    o.Cnonce,
		Nc:        o.Count,
		Realm:     chal.Realm,
		Nonce:     chal.Nonce,
		Algorithm: chal.Algorithm,
		Opaque:    chal.Opaque,
		Userhash:  chal.Userhash,
	}
	newhash, sess, ok := parseAlgorithm(cred.Algorithm)
	if !ok {
		return nil, fmt.Errorf("digest: unsupported algorithm: %q", cred.Algorithm)
	}
	// we re-use the same hash.Hash
	h := newhash()
	// hash the username if requested
	if cred.Userhash {
		cred.Username = hashjoin(h, o.Username, cred.Realm)
	}
	// HA1 is the base value H(username:realm:password). A precomputed value
	// supplied in o.A1 always means this base value, even for *-sess.
	a1 := o.A1
	if a1 == "" {
		a1 = hashjoin(h, o.Username, cred.Realm, o.Password)
	}
	// *-sess algorithms derive a per-session HA1 from the base value.
	if sess {
		if cred.Cnonce == "" {
			cred.Cnonce = cnonce()
		}
		a1 = hashjoin(h, a1, cred.Nonce, cred.Cnonce)
	}
	// select the quality of protection and derive A2
	var a2 string
	switch {
	case len(chal.QOP) == 0:
		a2 = hashjoin(h, o.Method, o.URI)
	case chal.SupportsQOP("auth"):
		cred.QOP = "auth"
		a2 = hashjoin(h, o.Method, o.URI)
	case chal.SupportsQOP("auth-int"):
		cred.QOP = "auth-int"
		hbody, err := hashbody(h, o.GetBody)
		if err != nil {
			return nil, fmt.Errorf("digest: failed to read body for auth-int: %w", err)
		}
		a2 = hashjoin(h, o.Method, o.URI, hbody)
	default:
		return nil, fmt.Errorf("digest: unsupported qop: %q", strings.Join(chal.QOP, ","))
	}
	// a cnonce is required by *-sess and whenever qop is used
	if sess || cred.QOP != "" {
		if cred.Cnonce == "" {
			cred.Cnonce = cnonce()
		}
	}
	if cred.QOP != "" {
		if cred.Nc == 0 {
			cred.Nc = 1
		}
	}
	// generate the response
	if cred.QOP == "" {
		cred.Response = hashjoin(h, a1, cred.Nonce, a2)
	} else {
		cred.Response = hashjoin(h,
			a1,
			cred.Nonce,
			fmt.Sprintf("%08x", cred.Nc),
			cred.Cnonce,
			cred.QOP,
			a2,
		)
	}
	return cred, nil
}

func hashjoin(h hash.Hash, parts ...string) string {
	h.Reset()
	for i, part := range parts {
		if i > 0 {
			h.Write([]byte(":"))
		}
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashbody(h hash.Hash, getbody func() (io.ReadCloser, error)) (string, error) {
	h.Reset()
	if getbody != nil {
		r, err := getbody()
		if err != nil {
			return "", err
		}
		defer r.Close()
		if r != http.NoBody {
			if _, err := io.Copy(h, r); err != nil {
				return "", err
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func cnonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("digest: failed to generate cnonce: %v", err))
	}
	return hex.EncodeToString(b[:])
}
