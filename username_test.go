package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

// username with spaces, quotes, percent signs and CJK characters
const starUsername = "中 文\"100%\""

func starChallenge() *Challenge {
	return &Challenge{
		Realm:     "api@example.org",
		Nonce:     "5TsQWLVdgBdmrQ0XsxbDODV+57QdFR34I9HAbC/RVvkK",
		Algorithm: "SHA-256",
		QOP:       []string{"auth"},
		Charset:   "UTF-8",
	}
}

func starOptions() Options {
	return Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: starUsername,
		Password: "Secret, or not?",
		Cnonce:   "NTg6RKcb9boFIAS3KrFK9BGeh+iDa/sm6jUMp2wds69v",
	}
}

func sha256sum(parts ...string) string {
	h := sha256.New()
	fmt.Fprint(h, strings.Join(parts, ":"))
	return hex.EncodeToString(h.Sum(nil))
}

func TestDigestUsernameStar(t *testing.T) {
	cred, err := Digest(starChallenge(), starOptions())
	assert.NilError(t, err)
	// the raw username is kept and marked for extended notation
	assert.Equal(t, cred.Username, starUsername)
	assert.Assert(t, cred.UsernameStar)
	// the response is computed over the raw username, not the encoded text
	a1 := sha256sum(starUsername, "api@example.org", "Secret, or not?")
	a2 := sha256sum("GET", "/dir/index.html")
	expected := sha256sum(a1, cred.Nonce, "00000001", cred.Cnonce, "auth", a2)
	assert.Equal(t, cred.Response, expected)
	// counter-example: hashing the percent-encoded text produces a different digest
	encoded := encodeUsernameStar(starUsername)
	a1bad := sha256sum(encoded, "api@example.org", "Secret, or not?")
	bad := sha256sum(a1bad, cred.Nonce, "00000001", cred.Cnonce, "auth", a2)
	assert.Assert(t, bad != expected)
	// the header carries username* and no conflicting username parameter
	header := cred.String()
	assert.Assert(t, strings.Contains(header, `username*=UTF-8''%E4%B8%AD%20%E6%96%87%22100%25%22`))
	assert.Assert(t, !strings.Contains(header, `username="`))
}

func TestDigestUsernameStarASCIICompat(t *testing.T) {
	opt := starOptions()
	opt.Username = "Mufasa"
	cred, err := Digest(starChallenge(), opt)
	assert.NilError(t, err)
	assert.Assert(t, !cred.UsernameStar)
	header := cred.String()
	assert.Assert(t, strings.Contains(header, `username="Mufasa"`))
	assert.Assert(t, !strings.Contains(header, "username*"))
}

func TestDigestUsernameStarUserhash(t *testing.T) {
	chal := starChallenge()
	chal.Userhash = true
	cred, err := Digest(chal, starOptions())
	assert.NilError(t, err)
	// userhash takes precedence: hashed username, no username*, no original name
	assert.Assert(t, !cred.UsernameStar)
	assert.Equal(t, cred.Username, sha256sum(starUsername, "api@example.org"))
	header := cred.String()
	assert.Assert(t, strings.Contains(header, `username="`+cred.Username+`"`))
	assert.Assert(t, strings.Contains(header, "userhash=true"))
	assert.Assert(t, !strings.Contains(header, "username*"))
}

func TestDigestUnsupportedCharset(t *testing.T) {
	chal := starChallenge()
	chal.Charset = "ISO-8859-1"
	assert.Assert(t, !CanDigest(chal))
	_, err := Digest(chal, starOptions())
	assert.ErrorContains(t, err, "unsupported charset")
}

func TestDigestInvalidUTF8Username(t *testing.T) {
	opt := starOptions()
	opt.Username = "valid\xffinvalid"
	_, err := Digest(starChallenge(), opt)
	assert.ErrorContains(t, err, "not valid UTF-8")
}

func TestParseCredentialsUsernameStar(t *testing.T) {
	input := `Digest username*=UTF-8''%E4%B8%AD%20%E6%96%87%22100%25%22, realm="api@example.org", nonce="abc", uri="/dir/index.html", response="123"`
	cred, err := ParseCredentials(input)
	assert.NilError(t, err)
	assert.Equal(t, cred.Username, starUsername)
	assert.Assert(t, cred.UsernameStar)
	// round-trips back to the extended notation
	assert.Equal(t, cred.String(), input)
}

func TestParseCredentialsUsernameStarErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"conflict", `Digest username="foo", username*=UTF-8''foo, realm="r"`},
		{"conflict reversed", `Digest username*=UTF-8''foo, username="foo", realm="r"`},
		{"duplicate username", `Digest username="foo", username="bar", realm="r"`},
		{"duplicate username*", `Digest username*=UTF-8''foo, username*=UTF-8''bar, realm="r"`},
		{"unsupported charset", `Digest username*=ISO-8859-1''foo, realm="r"`},
		{"bad escape", `Digest username*=UTF-8''%ZZ, realm="r"`},
		{"truncated escape", `Digest username*=UTF-8''abc%, realm="r"`},
		{"invalid utf-8", `Digest username*=UTF-8''%E4%B8, realm="r"`},
		{"missing language field", `Digest username*=UTF-8, realm="r"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCredentials(tt.input)
			assert.Assert(t, err != nil, "expected error for %s", tt.input)
		})
	}
}
