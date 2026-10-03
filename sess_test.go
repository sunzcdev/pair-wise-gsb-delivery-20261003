package digest

import (
	"io"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

// Independent golden vectors for the *-sess algorithm variants.
//
// The expected values come from sessReference, a reference digest
// implementation written directly on top of the standard library hash
// functions. It does not call any of the production code under test.
// Fixed inputs:
//
//	username = Mufasa
//	realm    = http-auth@example.org
//	password = Circle of Life
//	nonce    = 7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v
//	cnonce   = f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ
//	nc       = 00000001
//	method   = GET
//	uri      = /dir/index.html
//	body     = {"hello":"world"}   (auth-int only)
const (
	sessRealm    = "http-auth@example.org"
	sessNonce    = "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v"
	sessCnonce   = "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ"
	sessOpaque   = "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"
	sessBody     = `{"hello":"world"}`
	sessPassword = "Circle of Life"

	md5SessHA1     = "3d78807defe7de2157e2b0b6573a855f"
	sha256SessHA1  = "7987c64c30e25f1b74be53f966b49b90f2808aa92faf9a00262392d7b4794232"
	sha512SessHA1  = "fb174f5c3c7802721517cae13b98e2b8dae2e0118cb705d94ee29946319204ce"
	emptySHA256HA1 = "6625f4e112f64a29ef6d77574810ce1c8c8d135a7c0df7a4c51abd28a8936495"
)

type sessCase struct {
	name     string
	algo     string // value as sent in the challenge
	password string
	a1       string // precomputed base HA1
	qop      []string
	wantQOP  string
	response string
}

func sessCases() []sessCase {
	return []sessCase{
		// no qop: cnonce must still be present and used for the session HA1
		{name: "md5-noqop", algo: "MD5-sess",
			response: "37a7f42db2090a3fcac4e26e747d4fc8"},
		{name: "sha256-noqop", algo: "SHA-256-sess",
			response: "30bc0742357e535c85094121d22cf826b794be0667e19537beb7b28f2251c274"},
		{name: "sha512256-noqop", algo: "SHA-512-256-sess",
			response: "7c70dbd9f483cbccedbd3e92ac246a9f2b712bed77914b52c3ae2254fdb2edba"},
		// qop=auth
		{name: "md5-auth", algo: "MD5-sess", qop: []string{"auth"}, wantQOP: "auth",
			response: "e783283f46242139c486a698fec7211d"},
		{name: "sha256-auth", algo: "SHA-256-sess", qop: []string{"auth"}, wantQOP: "auth",
			response: "2fd51b3a77ad75bad6afad6003e818d767133c46d9e2749e7f5232ae1ea3efd7"},
		{name: "sha512256-auth", algo: "SHA-512-256-sess",
			qop: []string{"auth", "auth-int"}, wantQOP: "auth",
			response: "3f2a34f923c38b0fb26dce2fdfc2ce326c23cecf86fbb1444f3e51fbbc2cb92e"},
		// qop=auth-int
		{name: "md5-authint", algo: "MD5-sess", qop: []string{"auth-int"}, wantQOP: "auth-int",
			response: "826df0a3004e0b5edf70489f99ef32b9"},
		{name: "sha256-authint", algo: "SHA-256-sess", qop: []string{"auth-int"}, wantQOP: "auth-int",
			response: "9cc400a1bfd3d6d92b9b73008746a84f094b5e023b69adfe744047c7575f65e7"},
		{name: "sha512256-authint", algo: "SHA-512-256-sess", qop: []string{"auth-int"}, wantQOP: "auth-int",
			response: "1ab1595cb69c24061f6d3a6eef49cfc7610543a96da76a9d14ccb8e0c9016dd6"},
		// case-insensitive algorithm name matching
		{name: "lowercase-name", algo: "md5-sess", qop: []string{"auth"}, wantQOP: "auth",
			response: "e783283f46242139c486a698fec7211d"},
		{name: "mixedcase-name", algo: "Sha-256-Sess", qop: []string{"auth"}, wantQOP: "auth",
			response: "2fd51b3a77ad75bad6afad6003e818d767133c46d9e2749e7f5232ae1ea3efd7"},
		// empty password
		{name: "md5-empty-noqop", algo: "MD5-sess", password: "",
			response: "f164dbc114d6bd7cab327ef0741b6f38"},
		{name: "sha256-empty-auth", algo: "SHA-256-sess", password: "",
			qop: []string{"auth"}, wantQOP: "auth",
			response: "65b218ef3bae3ffc6ef5595372da1876867d7af6b68b4da47d2f8600308b987c"},
		{name: "sha512256-empty-authint", algo: "SHA-512-256-sess", password: "",
			qop: []string{"auth-int"}, wantQOP: "auth-int",
			response: "0e9a7cd9b6cd662522b93cd43cabffb3e5dee187640f2e3ec655306aba45f7fc"},
		// precomputed base HA1: the per-session derivation must still happen once
		{name: "md5-a1-noqop", algo: "MD5-sess", a1: md5SessHA1,
			response: "37a7f42db2090a3fcac4e26e747d4fc8"},
		{name: "sha256-a1-auth", algo: "SHA-256-sess", a1: sha256SessHA1,
			qop: []string{"auth"}, wantQOP: "auth",
			response: "2fd51b3a77ad75bad6afad6003e818d767133c46d9e2749e7f5232ae1ea3efd7"},
		{name: "sha512256-a1-authint", algo: "SHA-512-256-sess", a1: sha512SessHA1,
			qop: []string{"auth-int"}, wantQOP: "auth-int",
			response: "1ab1595cb69c24061f6d3a6eef49cfc7610543a96da76a9d14ccb8e0c9016dd6"},
		{name: "sha256-empty-a1-auth", algo: "SHA-256-sess", a1: emptySHA256HA1,
			qop: []string{"auth"}, wantQOP: "auth",
			response: "65b218ef3bae3ffc6ef5595372da1876867d7af6b68b4da47d2f8600308b987c"},
	}
}

func TestDigestSess(t *testing.T) {
	for _, tc := range sessCases() {
		t.Run(tc.name, func(t *testing.T) {
			password := sessPassword
			if tc.password != "" {
				password = tc.password
			}
			if strings.Contains(tc.name, "-empty") {
				password = ""
			}
			chal := &Challenge{
				Realm:     sessRealm,
				Nonce:     sessNonce,
				Opaque:    sessOpaque,
				Algorithm: tc.algo,
				QOP:       tc.qop,
			}
			opt := Options{
				Method:   "GET",
				URI:      "/dir/index.html",
				Username: "Mufasa",
				Password: password,
				A1:       tc.a1,
				Cnonce:   sessCnonce,
				Count:    1,
				GetBody: func() (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader(sessBody)), nil
				},
			}
			cred, err := Digest(chal, opt)
			assert.NilError(t, err)
			want := &Credentials{
				Username:  "Mufasa",
				Realm:     sessRealm,
				Nonce:     sessNonce,
				URI:       "/dir/index.html",
				Response:  tc.response,
				Algorithm: tc.algo,
				Cnonce:    sessCnonce,
				Opaque:    sessOpaque,
				QOP:       tc.wantQOP,
				Nc:        1,
			}
			assert.DeepEqual(t, cred, want)
		})
	}
}

func TestDigestSessReference(t *testing.T) {
	// Cross-check every golden vector against an independent reference
	// implementation using nothing but the standard library.
	for _, tc := range sessCases() {
		t.Run(tc.name, func(t *testing.T) {
			password := sessPassword
			if strings.Contains(tc.name, "-empty") {
				password = ""
			}
			got := sessReference(sessRefInput{
				username:  "Mufasa",
				realm:     sessRealm,
				password:  password,
				a1:        tc.a1,
				nonce:     sessNonce,
				cnonce:    sessCnonce,
				method:    "GET",
				uri:       "/dir/index.html",
				body:      sessBody,
				algorithm: tc.algo,
				qop:       tc.wantQOP,
			})
			assert.Equal(t, got, tc.response)
		})
	}
}

func TestCanDigestAlgorithms(t *testing.T) {
	supported := []string{
		"", "MD5", "md5", "SHA-256", "sha-256",
		"SHA-512", "SHA-512-256", "sha-512-256",
		"MD5-sess", "md5-sess", "Md5-Sess",
		"SHA-256-sess", "sha-256-SESS",
		"SHA-512-256-sess", "sha-512-256-sess",
	}
	unsupported := []string{
		"MD5-sess-sess", "SHA-256-ss", "SHA256-sess",
		"SHA-512-256-sessx", "bogus", "MD5SESS",
	}
	for _, algo := range supported {
		c := &Challenge{Algorithm: algo, Nonce: "n"}
		assert.Assert(t, CanDigest(c), "expected %q to be supported", algo)
	}
	for _, algo := range unsupported {
		c := &Challenge{Algorithm: algo, Nonce: "n"}
		assert.Assert(t, !CanDigest(c), "expected %q to be unsupported", algo)
	}
	// unsupported qop values are rejected even with a known algorithm
	assert.Assert(t, !CanDigest(&Challenge{Algorithm: "SHA-256-sess", QOP: []string{"auth-conf"}}))
}

func TestDigestUnsupportedVariant(t *testing.T) {
	// stripping the "-sess" suffix must not make an unknown algorithm work
	chal := &Challenge{
		Realm:     sessRealm,
		Nonce:     sessNonce,
		Algorithm: "SHA-512-256-sesss",
		QOP:       []string{"auth"},
	}
	_, err := Digest(chal, Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: "Mufasa",
		Password: sessPassword,
	})
	assert.ErrorContains(t, err, "unsupported algorithm")
}

func TestDigestSessNoQopHeader(t *testing.T) {
	// with no qop, the session cnonce is still sent, but qop and nc are not
	chal := &Challenge{
		Realm:     sessRealm,
		Nonce:     sessNonce,
		Opaque:    sessOpaque,
		Algorithm: "SHA-256-sess",
	}
	opt := Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: "Mufasa",
		Password: sessPassword,
		Cnonce:   sessCnonce,
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)
	assert.Equal(t, cred.QOP, "")
	assert.Equal(t, cred.Nc, 0)
	assert.Equal(t, cred.Cnonce, sessCnonce)

	header := cred.String()
	assert.Assert(t, strings.Contains(header, `cnonce="`+sessCnonce+`"`), "cnonce missing: %s", header)
	assert.Assert(t, !strings.Contains(header, "qop="), "qop must be absent: %s", header)
	assert.Assert(t, !strings.Contains(header, "nc="), "nc must be absent: %s", header)

	// the header must parse back into the same credentials
	parsed, err := ParseCredentials(header)
	assert.NilError(t, err)
	assert.DeepEqual(t, parsed, cred)
}

func TestDigestSessGeneratesCnonceWithoutQop(t *testing.T) {
	chal := &Challenge{
		Realm:     sessRealm,
		Nonce:     sessNonce,
		Algorithm: "md5-sess",
	}
	cred, err := Digest(chal, Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: "Mufasa",
		Password: sessPassword,
	})
	assert.NilError(t, err)
	assert.Assert(t, cred.Cnonce != "", "a cnonce must be generated for *-sess")
	// the generated cnonce must participate in the session HA1 derivation
	expected := sessReference(sessRefInput{
		username:  "Mufasa",
		realm:     sessRealm,
		password:  sessPassword,
		nonce:     sessNonce,
		cnonce:    cred.Cnonce,
		method:    "GET",
		uri:       "/dir/index.html",
		algorithm: "md5-sess",
	})
	assert.Equal(t, cred.Response, expected)
}

func TestSessDiffersFromBaseAlgorithm(t *testing.T) {
	// *-sess must not reuse the plain algorithm result
	chal := &Challenge{
		Realm:     sessRealm,
		Nonce:     sessNonce,
		Opaque:    sessOpaque,
		Algorithm: "SHA-256",
		QOP:       []string{"auth"},
	}
	base, err := Digest(chal, Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: "Mufasa",
		Password: sessPassword,
		Cnonce:   sessCnonce,
	})
	assert.NilError(t, err)
	chal.Algorithm = "SHA-256-sess"
	sess, err := Digest(chal, Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: "Mufasa",
		Password: sessPassword,
		Cnonce:   sessCnonce,
	})
	assert.NilError(t, err)
	assert.Assert(t, base.Response != sess.Response)
	assert.Equal(t, sess.Response, "2fd51b3a77ad75bad6afad6003e818d767133c46d9e2749e7f5232ae1ea3efd7")
}

func TestDigestSessA1NotDoubleHashed(t *testing.T) {
	// a base HA1 supplied as A1 must be treated as the pre-session value:
	// feeding the already-derived session value instead must produce a
	// different response
	chal := &Challenge{
		Realm:     sessRealm,
		Nonce:     sessNonce,
		Algorithm: "MD5-sess",
		QOP:       []string{"auth"},
	}
	good, err := Digest(chal, Options{
		Method: "GET", URI: "/dir/index.html",
		Username: "Mufasa", A1: md5SessHA1, Cnonce: sessCnonce,
	})
	assert.NilError(t, err)
	assert.Equal(t, good.Response, "e783283f46242139c486a698fec7211d")

	// session HA1 for these inputs (computed independently with md5 below)
	sessionHA1 := refMD5Join(md5SessHA1, sessNonce, sessCnonce)
	bad, err := Digest(chal, Options{
		Method: "GET", URI: "/dir/index.html",
		Username: "Mufasa", A1: sessionHA1, Cnonce: sessCnonce,
	})
	assert.NilError(t, err)
	assert.Assert(t, bad.Response != good.Response)
}
