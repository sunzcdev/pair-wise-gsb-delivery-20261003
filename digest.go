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

	// A1 is the pre-computed base HA1 hash: H(username:realm:password).
	// For -sess algorithms, the session HA1 is derived from this value as
	// H(A1:nonce:cnonce), so it must always be the base hash and never the
	// session-derived value.
	A1     string
	Cnonce string
}

// CanDigest checks if the algorithm and qop are supported
func CanDigest(c *Challenge) bool {
	if _, _, err := parseAlgorithm(c.Algorithm); err != nil {
		return false
	}
	return len(c.QOP) == 0 || c.SupportsQOP("auth") || c.SupportsQOP("auth-int")
}

// parseAlgorithm splits an algorithm name into its base hash algorithm and
// whether it is a session (-sess) variant. The comparison is case-insensitive.
func parseAlgorithm(algorithm string) (base string, sess bool, err error) {
	base = strings.ToUpper(algorithm)
	if strings.HasSuffix(base, "-SESS") {
		sess = true
		base = strings.TrimSuffix(base, "-SESS")
	}
	switch base {
	case "MD5", "SHA-256", "SHA-512", "SHA-512-256":
	case "":
		if sess {
			return "", false, fmt.Errorf("digest: unsupported algorithm: %q", algorithm)
		}
	default:
		return "", false, fmt.Errorf("digest: unsupported algorithm: %q", algorithm)
	}
	return base, sess, nil
}

// isSess returns true if the algorithm is a session (-sess) variant
func isSess(algorithm string) bool {
	return strings.HasSuffix(strings.ToUpper(algorithm), "-SESS")
}

// newHash returns the hash.Hash for a base algorithm name
func newHash(base string) hash.Hash {
	switch base {
	case "", "MD5":
		return md5.New()
	case "SHA-256":
		return sha256.New()
	case "SHA-512":
		return sha512.New()
	case "SHA-512-256":
		return sha512.New512_256()
	default:
		panic(fmt.Sprintf("digest: unsupported algorithm: %q", base))
	}
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
	base, sess, err := parseAlgorithm(cred.Algorithm)
	if err != nil {
		return nil, err
	}
	// we re-use the same hash.Hash
	h := newHash(base)
	// hash the username if requested
	if cred.Userhash {
		cred.Username = hashjoin(h, o.Username, cred.Realm)
	}
	// generate the base a1 hash if one was not provided
	a1 := o.A1
	if a1 == "" {
		a1 = hashjoin(h, o.Username, cred.Realm, o.Password)
	}
	// for sess algorithms, derive the session a1 from the base a1, nonce and
	// cnonce. The cnonce is required here even when the challenge has no qop.
	if sess {
		if cred.Cnonce == "" {
			cred.Cnonce = cnonce()
		}
		a1 = hashjoin(h, a1, cred.Nonce, cred.Cnonce)
	}
	// generate the response
	switch {
	case len(chal.QOP) == 0:
		cred.Response = hashjoin(h,
			a1,
			cred.Nonce,
			hashjoin(h, o.Method, o.URI), // A2
		)
	case chal.SupportsQOP("auth"):
		cred.QOP = "auth"
		if cred.Cnonce == "" {
			cred.Cnonce = cnonce()
		}
		if cred.Nc == 0 {
			cred.Nc = 1
		}
		cred.Response = hashjoin(h,
			a1,
			cred.Nonce,
			fmt.Sprintf("%08x", cred.Nc),
			cred.Cnonce,
			cred.QOP,
			hashjoin(h, o.Method, o.URI), // A2
		)
	case chal.SupportsQOP("auth-int"):
		cred.QOP = "auth-int"
		if cred.Cnonce == "" {
			cred.Cnonce = cnonce()
		}
		if cred.Nc == 0 {
			cred.Nc = 1
		}
		hbody, err := hashbody(h, o.GetBody)
		if err != nil {
			return nil, fmt.Errorf("digest: failed to read body for auth-int: %w", err)
		}
		cred.Response = hashjoin(h,
			a1,
			cred.Nonce,
			fmt.Sprintf("%08x", cred.Nc),
			cred.Cnonce,
			cred.QOP,
			hashjoin(h, o.Method, o.URI, hbody), // A2
		)
	default:
		return nil, fmt.Errorf("digest: unsupported qop: %q", strings.Join(chal.QOP, ","))
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
