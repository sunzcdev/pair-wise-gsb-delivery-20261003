package digest

import (
	"net/http"
	"testing"

	"gotest.tools/v3/assert"
)

func TestParseAuthenticationInfo(t *testing.T) {
	input := `nextnonce="nonce-2", qop=auth, cnonce="abc", nc=0000000a, rspauth="def"`
	info, err := ParseAuthenticationInfo(input)
	assert.NilError(t, err)
	assert.DeepEqual(t, info, &AuthenticationInfo{
		NextNonce: "nonce-2",
		QOP:       "auth",
		Cnonce:    "abc",
		Nc:        10,
		Rspauth:   "def",
	})
	assert.Equal(t, info.String(), input)
}

func TestFindAuthenticationInfo(t *testing.T) {
	info, err := FindAuthenticationInfo(http.Header{})
	assert.NilError(t, err)
	assert.Assert(t, info == nil)

	h := http.Header{}
	h.Set("Authentication-Info", `nextnonce="nonce-2"`)
	info, err = FindAuthenticationInfo(h)
	assert.NilError(t, err)
	assert.Equal(t, info.NextNonce, "nonce-2")
}
