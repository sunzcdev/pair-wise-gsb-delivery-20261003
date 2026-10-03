package digest

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

// https://www.rfc-editor.org/rfc/rfc7616#section-3.9.2
func TestDigestUsernameExtended(t *testing.T) {
	username := `¯\_(ツ)_/¯`
	opt := Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: username,
		Password: "Secret, or not?",
		Cnonce:   "NTg6RKcb9boFIAS3KrFK9BGeh+iDa/sm6jUMp2wds69v",
	}
	chal := &Challenge{
		Realm:     "http-auth@example.org",
		Nonce:     "5TsQWLVdgBdmrQ0XsxbDODV+57QdFR34I9HAbC/RVvkK",
		Algorithm: "SHA-256",
		Opaque:    "HRPCssKJSGjCrkzDg8OhwpzCiGPChXYjwrI2QmXDnsOS",
		QOP:       []string{"auth"},
		Charset:   "UTF-8",
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)
	assert.Equal(t, cred.Username, username)
	assert.Assert(t, cred.ExtendedUsername)
	assert.Equal(t, cred.Response, "80943177d08941640e42cfdbdcdcecbcc3bf44b3e1f6646a6d6282a03273b86a")

	s := cred.String()
	assert.Assert(t, strings.Contains(s, `username*=UTF-8''%C2%AF%5C_%28%E3%83%84%29_%2F%C2%AF`), s)
	// the conflicting legacy parameter must not be emitted
	assert.Assert(t, !strings.Contains(s, "username="), s)
}

// The digest must be computed from the original username, not from the
// percent-encoded text. This is the required "different digest" counterexample.
func TestDigestExtendedHashesOriginalUsername(t *testing.T) {
	username := `张 三"100%`
	opt := Options{
		Method:   "GET",
		URI:      "/",
		Username: username,
		Password: "secret",
		Cnonce:   "0a1b2c3d4e5f6071",
	}
	chal := &Challenge{
		Realm:     "example",
		Nonce:     "nonce",
		Algorithm: "MD5",
		QOP:       []string{"auth"},
		Charset:   "UTF-8",
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)

	want := md5Hex(
		md5Hex(username, chal.Realm, opt.Password),
		chal.Nonce, "00000001", opt.Cnonce, "auth",
		md5Hex("GET", "/"),
	)
	assert.Equal(t, cred.Response, want)

	// hashing the encoded ext-value text must yield a different response
	encoded := strings.SplitN(strings.TrimPrefix(cred.String(), Prefix), ",", 2)[0]
	encoded = strings.TrimPrefix(encoded, "username*=UTF-8''")
	assert.Assert(t, encoded != username)
	wrong := md5Hex(
		md5Hex(encoded, chal.Realm, opt.Password),
		chal.Nonce, "00000001", opt.Cnonce, "auth",
		md5Hex("GET", "/"),
	)
	assert.Assert(t, wrong != cred.Response)
}

// Spaces, quotes, percent signs and CJK characters round-trip through the
// username* header and parse back to the original username.
func TestExtendedRoundTrip(t *testing.T) {
	username := `张 三"100%`
	opt := Options{
		Method:   "GET",
		URI:      "/%E5%BC%A0",
		Username: username,
		Password: "secret",
		Cnonce:   "0a1b2c3d4e5f6071",
	}
	chal := &Challenge{
		Realm:   "example",
		Nonce:   "nonce",
		QOP:     []string{"auth"},
		Charset: "UTF-8",
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)

	s := cred.String()
	assert.Assert(t, strings.Contains(s, `username*=UTF-8''%E5%BC%A0%20%E4%B8%89%22100%25`), s)

	parsed, err := ParseCredentials(s)
	assert.NilError(t, err)
	assert.Equal(t, parsed.Username, username)
	assert.Assert(t, parsed.ExtendedUsername)
	assert.Equal(t, parsed.Response, cred.Response)

	// a server reconstructing credentials from the restored username matches
	server, err := Digest(chal, Options{
		Method:   opt.Method,
		URI:      opt.URI,
		Username: parsed.Username,
		Password: opt.Password,
		Cnonce:   parsed.Cnonce,
		Count:    parsed.Nc,
	})
	assert.NilError(t, err)
	assert.Equal(t, server.Response, parsed.Response)
}

func TestASCIIUsesLegacyHeader(t *testing.T) {
	for _, charset := range []string{"", "UTF-8"} {
		chal := &Challenge{Realm: "example", Nonce: "nonce", QOP: []string{"auth"}, Charset: charset}
		for _, username := range []string{"foo", `a b%c"d`} {
			cred, err := Digest(chal, Options{
				Method:   "GET",
				URI:      "/",
				Username: username,
				Password: "secret",
				Cnonce:   "0a1b2c3d4e5f6071",
			})
			assert.NilError(t, err)
			assert.Assert(t, !cred.ExtendedUsername)
			assert.Assert(t, strings.HasPrefix(cred.String(), Prefix+"username="), cred.String())
			assert.Assert(t, !strings.Contains(cred.String(), "username*="), cred.String())
		}
	}
}

func TestUserhashTakesPrecedence(t *testing.T) {
	username := "Jäsøn Doe"
	opt := Options{
		Method:   "GET",
		URI:      "/doe.json",
		Username: username,
		Password: "Secret, or not?",
		Cnonce:   "NTg6RKcb9boFIAS3KrFK9BGeh+iDa/sm6jUMp2wds69v",
	}
	chal := &Challenge{
		Realm:    "api@example.org",
		Nonce:    "5TsQWLVdgBdmrQ0XsxbDODV+57QdFR34I9HAbC/RVvkK",
		QOP:      []string{"auth"},
		Charset:  "UTF-8",
		Userhash: true,
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)
	assert.Assert(t, cred.Userhash)
	assert.Assert(t, !cred.ExtendedUsername)
	assert.Equal(t, len(cred.Username), 32) // md5 hex digest
	assert.Assert(t, cred.Username != username)

	s := cred.String()
	assert.Assert(t, strings.Contains(s, `username="`+cred.Username+`"`), s)
	assert.Assert(t, strings.Contains(s, "userhash=true"), s)
	assert.Assert(t, !strings.Contains(s, "username*="), s)

	parsed, err := ParseCredentials(s)
	assert.NilError(t, err)
	assert.Equal(t, parsed.Username, cred.Username)
	assert.Assert(t, parsed.Userhash)
	assert.Assert(t, !parsed.ExtendedUsername)
}

func TestDigestRejectsBadCharsetAndUTF8(t *testing.T) {
	_, err := Digest(&Challenge{
		Realm:   "example",
		Nonce:   "nonce",
		Charset: "ISO-8859-1",
	}, Options{Username: "foo", Password: "bar"})
	assert.ErrorContains(t, err, `unsupported charset`)

	_, err = Digest(&Challenge{
		Realm:   "example",
		Nonce:   "nonce",
		Charset: "UTF-8",
	}, Options{Username: string([]byte{0xff, 0xfe}), Password: "bar"})
	assert.ErrorContains(t, err, `not valid UTF-8`)

	// invalid UTF-8 is rejected even when no charset is advertised
	_, err = Digest(&Challenge{Realm: "example", Nonce: "nonce"},
		Options{Username: string([]byte{0xff}), Password: "bar"})
	assert.ErrorContains(t, err, `not valid UTF-8`)
}

func TestParseCredentialsExtended(t *testing.T) {
	input := `Digest username*=UTF-8''%E7%94%A8%E6%88%B725%25, realm="test", ` +
		`nonce="n", uri="/", qop=auth, nc=00000001, cnonce="c", response="r"`
	cred, err := ParseCredentials(input)
	assert.NilError(t, err)
	assert.Equal(t, cred.Username, `用户25%`)
	assert.Assert(t, cred.ExtendedUsername)

	// formatting round-trips back to the same extended parameter
	cred.Response = "r"
	assert.Equal(t, cred.String(), `Digest username*=UTF-8''%E7%94%A8%E6%88%B725%25, `+
		`realm="test", nonce="n", uri="/", cnonce="c", qop=auth, nc=00000001, response="r"`)
}

func TestParseCredentialsConflicts(t *testing.T) {
	tests := map[string]string{
		"duplicate username":  `Digest username="a", username="b", realm="r", nonce="n", uri="/", response="x"`,
		"duplicate username*": `Digest username*=UTF-8''a, username*=UTF-8''b, realm="r", nonce="n", uri="/", response="x"`,
		"username conflict":   `Digest username="a", username*=UTF-8''b, realm="r", nonce="n", uri="/", response="x"`,
		"userhash conflict":   `Digest username*=UTF-8''a, userhash=true, realm="r", nonce="n", uri="/", response="x"`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseCredentials(input)
			assert.Assert(t, err != nil, input)
		})
	}
}

func TestParseCredentialsBadEscapes(t *testing.T) {
	tests := map[string]string{
		"missing apostrophes": `Digest username*=UTF-8%E5, realm="r", nonce="n", uri="/", response="x"`,
		"bad hex digits":      `Digest username*=UTF-8''%zz, realm="r", nonce="n", uri="/", response="x"`,
		"truncated escape":    `Digest username*=UTF-8''%E5%BC, realm="r", nonce="n", uri="/", response="x"`,
		"invalid utf-8":       `Digest username*=UTF-8''%ff%fe, realm="r", nonce="n", uri="/", response="x"`,
		"unsupported charset": `Digest username*=ISO-8859-1''%E5, realm="r", nonce="n", uri="/", response="x"`,
		"uppercase bad hex":   `Digest username*=UTF-8''100%2G, realm="r", nonce="n", uri="/", response="x"`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseCredentials(input)
			assert.Assert(t, err != nil, input)
		})
	}
}

func md5Hex(parts ...string) string {
	h := md5.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte(":"))
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}
