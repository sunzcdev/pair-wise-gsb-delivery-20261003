package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

// https://tools.ietf.org/html/rfc7616#section-3.9
func TestDigestMD5(t *testing.T) {
	tests := []struct {
		opt  Options
		chal *Challenge
		cred *Credentials
	}{
		{
			Options{
				Method:   "GET",
				URI:      "/dir/index.html",
				Username: "Mufasa",
				Password: "Circle of Life",
				Cnonce:   "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
			},
			&Challenge{
				Realm:     "http-auth@example.org",
				Algorithm: "MD5",
				Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
				Opaque:    "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
				QOP:       []string{"auth", "auth-int"},
			},
			&Credentials{
				Username:  "Mufasa",
				Realm:     "http-auth@example.org",
				Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
				URI:       "/dir/index.html",
				Response:  "8ca523f5e9506fed4657c9700eebdbec",
				Algorithm: "MD5",
				Cnonce:    "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
				Opaque:    "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
				QOP:       "auth",
				Nc:        1,
			},
		},
		{
			Options{
				Method:   "GET",
				URI:      "/dir/index.html",
				Username: "Mufasa",
				Password: "Circle of Life",
				Cnonce:   "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
			},
			&Challenge{
				Realm:  "http-auth@example.org",
				Nonce:  "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
				Opaque: "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
				QOP:    []string{"auth", "auth-int"},
			},
			&Credentials{
				Username: "Mufasa",
				Realm:    "http-auth@example.org",
				Nonce:    "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
				URI:      "/dir/index.html",
				Response: "8ca523f5e9506fed4657c9700eebdbec",
				Cnonce:   "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
				Opaque:   "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
				QOP:      "auth",
				Nc:       1,
			},
		},
		{
			Options{
				Method:   "REGISTER",
				URI:      "sip:182.82.132.122",
				Username: "the-user",
				Password: "********",
				Cnonce:   "104adc6bd71f49678798ee646edcaa9a",
			},
			&Challenge{
				Realm: "SipPeer",
				Nonce: "970a1b42-d8a7-4fce-91a1-4767e9ed561b",
				QOP:   []string{"auth"},
			},
			&Credentials{
				Username: "the-user",
				Realm:    "SipPeer",
				Nonce:    "970a1b42-d8a7-4fce-91a1-4767e9ed561b",
				URI:      "sip:182.82.132.122",
				Response: "6cf0981e056709d40c8acc40c87e73c6",
				Cnonce:   "104adc6bd71f49678798ee646edcaa9a",
				QOP:      "auth",
				Nc:       1,
			},
		},
	}
	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			cred, err := Digest(tt.chal, tt.opt)
			assert.NilError(t, err)
			assert.DeepEqual(t, cred, tt.cred)
		})
	}
}

func TestDigestSHA256(t *testing.T) {
	opt := Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: "Mufasa",
		Password: "Circle of Life",
		Cnonce:   "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
	}
	chal := &Challenge{
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		Algorithm: "SHA-256",
		Opaque:    "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
		QOP:       []string{"auth", "auth-int"},
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)
	assert.DeepEqual(t, cred, &Credentials{
		Username:  "Mufasa",
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		URI:       "/dir/index.html",
		Response:  "753927fa0e85d155564e2e272a28d1802ca10daf4496794697cf8db5856cb6c1",
		Algorithm: "SHA-256",
		Cnonce:    "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
		Opaque:    "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
		QOP:       "auth",
		Nc:        1,
	})
}

func TestDigestA1(t *testing.T) {
	h := sha256.New()
	fmt.Fprintf(h, "%s:%s:%s",
		"Mufasa",                // username
		"http-auth@example.org", // realm
		"Circle of Life",        // password
	)
	a1 := hex.EncodeToString(h.Sum(nil))
	opt := Options{
		Method: "GET",
		URI:    "/dir/index.html",
		A1:     a1,
		Cnonce: "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
	}
	chal := &Challenge{
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		Algorithm: "SHA-256",
		Opaque:    "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
		QOP:       []string{"auth", "auth-int"},
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)
	assert.DeepEqual(t, cred, &Credentials{
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		URI:       "/dir/index.html",
		Response:  "753927fa0e85d155564e2e272a28d1802ca10daf4496794697cf8db5856cb6c1",
		Algorithm: "SHA-256",
		Cnonce:    "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
		Opaque:    "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
		QOP:       "auth",
		Nc:        1,
	})
}

func TestDigestUserhash(t *testing.T) {
	opt := Options{
		Method:   "GET",
		URI:      "/doe.json",
		Username: "J\u00E4s\u00F8n Doe",
		Password: "Secret, or not?",
		Cnonce:   "NTg6RKcb9boFIAS3KrFK9BGeh+iDa/sm6jUMp2wds69v",
	}
	chal := &Challenge{
		Realm:     "api@example.org",
		Nonce:     "5TsQWLVdgBdmrQ0XsxbDODV+57QdFR34I9HAbC/RVvkK",
		Algorithm: "SHA-512-256",
		Opaque:    "HRPCssKJSGjCrkzDg8OhwpzCiGPChXYjwrI2QmXDnsOS",
		QOP:       []string{"auth"},
		Charset:   "UTF-8",
		Userhash:  true,
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)
	assert.DeepEqual(t, cred, &Credentials{
		Username:  "793263caabb707a56211940d90411ea4a575adeccb7e360aeb624ed06ece9b0b",
		Realm:     "api@example.org",
		Nonce:     "5TsQWLVdgBdmrQ0XsxbDODV+57QdFR34I9HAbC/RVvkK",
		URI:       "/doe.json",
		Response:  "3798d4131c277846293534c3edc11bd8a5e4cdcbff78b05db9d95eeb1cec68a5",
		Algorithm: "SHA-512-256",
		Cnonce:    "NTg6RKcb9boFIAS3KrFK9BGeh+iDa/sm6jUMp2wds69v",
		Opaque:    "HRPCssKJSGjCrkzDg8OhwpzCiGPChXYjwrI2QmXDnsOS",
		QOP:       "auth",
		Nc:        1,
		Userhash:  true,
	})
}

func TestDigestAuthInt(t *testing.T) {
	opt := Options{
		Method:   "GET",
		URI:      "/digest-auth/auth-int/foo/bar",
		Username: "foo",
		Password: "bar",
		Cnonce:   "MjhjOWI2ZDRmNmVkNjlmYzRmMTdjZjAxYmU4ZTNkM2U=",
	}
	chal := &Challenge{
		Realm:     "me@kennethreitz.com",
		Nonce:     "7a5462bc2121c2e609e6f71c64d341c1",
		Opaque:    "5498295c3383fbb467b160f1143e51d4",
		Algorithm: "MD5",
		QOP:       []string{"auth-int"},
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)
	assert.DeepEqual(t, cred, &Credentials{
		Username:  "foo",
		Realm:     "me@kennethreitz.com",
		Nonce:     "7a5462bc2121c2e609e6f71c64d341c1",
		URI:       "/digest-auth/auth-int/foo/bar",
		Response:  "a86955ec413135f7902c0bae37d75469",
		Algorithm: "MD5",
		Cnonce:    "MjhjOWI2ZDRmNmVkNjlmYzRmMTdjZjAxYmU4ZTNkM2U=",
		Opaque:    "5498295c3383fbb467b160f1143e51d4",
		QOP:       "auth-int",
		Nc:        1,
	})
}

func TestDigestMD5Sess(t *testing.T) {
	opt := Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: "Mufasa",
		Password: "Circle of Life",
		Cnonce:   "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
	}
	chal := &Challenge{
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		Algorithm: "MD5-sess",
		Opaque:    "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
		QOP:       []string{"auth", "auth-int"},
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)
	assert.DeepEqual(t, cred, &Credentials{
		Username:  "Mufasa",
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		URI:       "/dir/index.html",
		Response:  "e783283f46242139c486a698fec7211d",
		Algorithm: "MD5-sess",
		Cnonce:    "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
		Opaque:    "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
		QOP:       "auth",
		Nc:        1,
	})
}

func TestDigestSessCaseInsensitive(t *testing.T) {
	opt := Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: "Mufasa",
		Password: "Circle of Life",
		Cnonce:   "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
	}
	chal := &Challenge{
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		Algorithm: "md5-SESS",
		QOP:       []string{"auth"},
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)
	// the response must match the MD5-sess vector regardless of name casing
	assert.Equal(t, cred.Response, "e783283f46242139c486a698fec7211d")
	assert.Equal(t, cred.Algorithm, "md5-SESS")
}

func TestDigestSHA256SessNoQOP(t *testing.T) {
	opt := Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: "Mufasa",
		Password: "Circle of Life",
		Cnonce:   "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
	}
	chal := &Challenge{
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		Algorithm: "SHA-256-sess",
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)
	assert.DeepEqual(t, cred, &Credentials{
		Username:  "Mufasa",
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		URI:       "/dir/index.html",
		Response:  "30bc0742357e535c85094121d22cf826b794be0667e19537beb7b28f2251c274",
		Algorithm: "SHA-256-sess",
		Cnonce:    "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
	})
	// the cnonce must be sent even when there is no qop
	header := cred.String()
	assert.Assert(t, strings.Contains(header, `cnonce="f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ"`))
	assert.Assert(t, strings.Contains(header, "algorithm=SHA-256-sess"))
	// the header must round-trip
	parsed, err := ParseCredentials(header)
	assert.NilError(t, err)
	assert.DeepEqual(t, parsed, cred)
}

func TestDigestSHA512_256SessAuthInt(t *testing.T) {
	opt := Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: "Mufasa",
		Password: "Circle of Life",
		Cnonce:   "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
		GetBody: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("hello world")), nil
		},
	}
	chal := &Challenge{
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		Algorithm: "SHA-512-256-sess",
		QOP:       []string{"auth-int"},
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)
	assert.DeepEqual(t, cred, &Credentials{
		Username:  "Mufasa",
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		URI:       "/dir/index.html",
		Response:  "eb0d9777fd19f555a459f25d61c357df89eae74645c6816d40026e54f406bb1a",
		Algorithm: "SHA-512-256-sess",
		Cnonce:    "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
		QOP:       "auth-int",
		Nc:        1,
	})
}

func TestDigestSessEmptyPassword(t *testing.T) {
	opt := Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: "Mufasa",
		Password: "",
		Cnonce:   "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
	}
	chal := &Challenge{
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		Algorithm: "MD5-sess",
		QOP:       []string{"auth"},
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)
	assert.Equal(t, cred.Response, "7115e3c881838853a051057415a47733")
}

func TestDigestSessA1(t *testing.T) {
	// A1 is the base HA1 = H(username:realm:password), not the session value.
	// The session derivation is applied on top of it by Digest.
	opt := Options{
		Method: "GET",
		URI:    "/dir/index.html",
		A1:     "3d78807defe7de2157e2b0b6573a855f", // md5("Mufasa:http-auth@example.org:Circle of Life")
		Cnonce: "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
	}
	chal := &Challenge{
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		Algorithm: "MD5-sess",
		QOP:       []string{"auth"},
	}
	cred, err := Digest(chal, opt)
	assert.NilError(t, err)
	// must match the password-based MD5-sess vector
	assert.Equal(t, cred.Response, "e783283f46242139c486a698fec7211d")
}

func TestCanDigestSess(t *testing.T) {
	supported := []string{
		"MD5-sess",
		"SHA-256-sess",
		"SHA-512-256-sess",
		"sha-256-SESS",
	}
	for _, algorithm := range supported {
		chal := &Challenge{
			Realm:     "test",
			Nonce:     "test",
			Algorithm: algorithm,
			QOP:       []string{"auth"},
		}
		assert.Assert(t, CanDigest(chal), algorithm)
	}
	unsupported := []string{
		"SHA-1-sess",
		"MD5-sess-sess",
		"-sess",
		"SHA-256-sessx",
		"SHA-256x-sess",
	}
	for _, algorithm := range unsupported {
		chal := &Challenge{
			Realm:     "test",
			Nonce:     "test",
			Algorithm: algorithm,
			QOP:       []string{"auth"},
		}
		assert.Assert(t, !CanDigest(chal), algorithm)
		_, err := Digest(chal, Options{Method: "GET", URI: "/"})
		assert.ErrorContains(t, err, "unsupported algorithm")
	}
}

var digestResult *Credentials

func BenchmarkDigest(b *testing.B) {
	opt := Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: "Mufasa",
		Password: "Circle of Life",
		Cnonce:   "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
	}
	chal := &Challenge{
		Realm:     "http-auth@example.org",
		Algorithm: "MD5",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		Opaque:    "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
		QOP:       []string{"auth", "auth-int"},
	}
	var cred *Credentials
	b.ResetTimer()
	for range b.N {
		var err error
		cred, err = Digest(chal, opt)
		if err != nil {
			b.Fatal(err)
		}
	}
	digestResult = cred // prevent optimizations
}
