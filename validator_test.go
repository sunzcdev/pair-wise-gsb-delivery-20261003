package digest

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"gotest.tools/v3/assert"
)

const (
	testRealm  = "http-auth@example.org"
	testNonce  = "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v"
	testCnonce = "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ"
	testUser   = "Mufasa"
	testPass   = "Circle of Life"
	testURI    = "/dir/index.html"
)

func testChallenge() *Challenge {
	return &Challenge{
		Realm:     testRealm,
		Nonce:     testNonce,
		Opaque:    "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
		Algorithm: "SHA-256",
		QOP:       []string{"auth"},
	}
}

func testValidator() *ServerValidator {
	return &ServerValidator{
		Lookup: func(username, realm string) (Secret, error) {
			if username != testUser || realm != testRealm {
				return Secret{}, ErrUnknownUser
			}
			return Secret{Password: testPass}, nil
		},
		Counts: NewMemoryCounterStore(),
	}
}

// End-to-end interop: this package's client Digest() output is accepted by
// the server-side validator, and incremented nonce counts keep working.
func TestServerValidatorInterop(t *testing.T) {
	v := testValidator()
	chal := testChallenge()
	for nc := 1; nc <= 3; nc++ {
		cred, err := Digest(chal, Options{
			Method:   "GET",
			URI:      testURI,
			Username: testUser,
			Password: testPass,
			Cnonce:   testCnonce,
			Count:    nc,
		})
		assert.NilError(t, err)
		res, err := v.Validate(chal, "GET", testURI, cred.String())
		assert.NilError(t, err)
		assert.DeepEqual(t, res, &Result{
			Username: testUser,
			Realm:    testRealm,
			Nonce:    testNonce,
			URI:      testURI,
			Nc:       nc,
			Cnonce:   testCnonce,
		})
	}
}

// Interop works when the caller stores a precomputed HA1 instead of the
// cleartext password.
func TestServerValidatorInteropHA1(t *testing.T) {
	chal := testChallenge()
	cred, err := Digest(chal, Options{
		Method:   "POST",
		URI:      "/api/thing?x=1",
		Username: testUser,
		Password: testPass,
		Cnonce:   testCnonce,
		Count:    1,
	})
	assert.NilError(t, err)
	ha1 := "7987c64c30e25f1b74be53f966b49b90f2808aa92faf9a00262392d7b4794232"
	v := &ServerValidator{
		Lookup: func(username, realm string) (Secret, error) {
			return Secret{HA1: ha1}, nil
		},
		Counts: NewMemoryCounterStore(),
	}
	res, err := v.Validate(chal, "POST", "/api/thing?x=1", cred.String())
	assert.NilError(t, err)
	assert.Equal(t, res.Username, testUser)
	assert.Equal(t, res.Nc, 1)
}

// Independent fixed vector (RFC 7616 section 3.9 values, SHA-256/qop=auth).
// The Authorization header is written out literally instead of being produced
// by the client code path.
func TestServerValidatorFixedVector(t *testing.T) {
	chal := &Challenge{
		Realm:     testRealm,
		Nonce:     testNonce,
		Algorithm: "SHA-256",
		QOP:       []string{"auth"},
	}
	header := `Digest username="Mufasa", realm="http-auth@example.org", ` +
		`nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v", ` +
		`uri="/dir/index.html", algorithm=SHA-256, ` +
		`cnonce="f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ", ` +
		`qop=auth, nc=00000001, ` +
		`response="753927fa0e85d155564e2e272a28d1802ca10daf4496794697cf8db5856cb6c1"`
	v := testValidator()
	res, err := v.Validate(chal, "GET", testURI, header)
	assert.NilError(t, err)
	assert.Equal(t, res.Username, testUser)
	assert.Equal(t, res.Nc, 1)
	assert.Equal(t, res.Cnonce, testCnonce)
}

// The uri claimed by the client must equal the actual request target; the
// claimed value must never be substituted for it.
func TestServerValidatorTamperedURI(t *testing.T) {
	v := testValidator()
	chal := testChallenge()
	cred, err := Digest(chal, Options{
		Method:   "GET",
		URI:      "/dir/index.html",
		Username: testUser,
		Password: testPass,
		Cnonce:   testCnonce,
		Count:    1,
	})
	assert.NilError(t, err)
	// Client computed a valid response for /dir/index.html, but presents it on
	// a request to a different target.
	_, err = v.Validate(chal, "GET", "/admin/secret", cred.String())
	assert.Assert(t, errors.Is(err, ErrMalformed))
	var ae *AuthError
	assert.Assert(t, errors.As(err, &ae))
	assert.Equal(t, ae.Reason, FailureMalformed)

	// A digest computed for a different method over the same uri also fails.
	_, err = v.Validate(chal, "POST", testURI, cred.String())
	assert.Assert(t, errors.Is(err, ErrBadDigest))
}

// After a successful verification the same nc cannot be committed again, while
// a strictly larger nc is accepted.
func TestServerValidatorReplay(t *testing.T) {
	v := testValidator()
	chal := testChallenge()
	cred, err := Digest(chal, Options{
		Method:   "GET",
		URI:      testURI,
		Username: testUser,
		Password: testPass,
		Cnonce:   testCnonce,
		Count:    1,
	})
	assert.NilError(t, err)
	authz := cred.String()

	_, err = v.Validate(chal, "GET", testURI, authz)
	assert.NilError(t, err)
	// Identical resubmission is a replay.
	_, err = v.Validate(chal, "GET", testURI, authz)
	assert.Assert(t, errors.Is(err, ErrReplay))
	// A failed digest must not consume the count: retry with nc=1 succeeds.
	_, err = v.Validate(chal, "GET", testURI, strings.Replace(authz, cred.Response, "00cafe", 1))
	assert.Assert(t, errors.Is(err, ErrBadDigest))

	cred2, err := Digest(chal, Options{
		Method:   "GET",
		URI:      testURI,
		Username: testUser,
		Password: testPass,
		Cnonce:   testCnonce,
		Count:    2,
	})
	assert.NilError(t, err)
	_, err = v.Validate(chal, "GET", testURI, cred2.String())
	assert.NilError(t, err)
	// Going backwards (nc=1 after nc=2) is also a replay.
	_, err = v.Validate(chal, "GET", testURI, authz)
	assert.Assert(t, errors.Is(err, ErrReplay))
}

// Two concurrent attempts using the same nonce and nc: at most one succeeds.
func TestServerValidatorConcurrentNC(t *testing.T) {
	v := testValidator()
	chal := testChallenge()
	cred, err := Digest(chal, Options{
		Method:   "GET",
		URI:      testURI,
		Username: testUser,
		Password: testPass,
		Cnonce:   testCnonce,
		Count:    1,
	})
	assert.NilError(t, err)
	authz := cred.String()

	const n = 32
	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, replay, other int
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait()
			_, err := v.Validate(chal, "GET", testURI, authz)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrReplay):
				replay++
			default:
				other++
			}
		}()
	}
	start.Done()
	wg.Wait()
	assert.Equal(t, ok, 1)
	assert.Equal(t, replay, n-1)
	assert.Equal(t, other, 0)
}

func TestServerValidatorErrorClasses(t *testing.T) {
	chal := testChallenge()

	t.Run("unknown user", func(t *testing.T) {
		v := testValidator()
		cred, err := Digest(chal, Options{
			Method: "GET", URI: testURI,
			Username: "ghost", Password: "x",
			Cnonce: testCnonce, Count: 1,
		})
		assert.NilError(t, err)
		_, err = v.Validate(chal, "GET", testURI, cred.String())
		assert.Assert(t, errors.Is(err, ErrUnknownUser))
	})

	t.Run("bad digest", func(t *testing.T) {
		secretPassword := "do-not-leak-me-4242"
		v := &ServerValidator{
			Lookup: func(username, realm string) (Secret, error) {
				return Secret{Password: secretPassword}, nil
			},
			Counts: NewMemoryCounterStore(),
		}
		cred, err := Digest(chal, Options{
			Method: "GET", URI: testURI,
			Username: testUser, Password: "wrong-password",
			Cnonce: testCnonce, Count: 1,
		})
		assert.NilError(t, err)
		_, err = v.Validate(chal, "GET", testURI, cred.String())
		assert.Assert(t, errors.Is(err, ErrBadDigest))
		assert.Assert(t, !strings.Contains(err.Error(), secretPassword))
		assert.Assert(t, !strings.Contains(err.Error(), "wrong-password"))
	})

	t.Run("rejected nonce", func(t *testing.T) {
		v := testValidator()
		v.CheckNonce = func(nonce string) error {
			return ErrInvalidNonce
		}
		cred, err := Digest(chal, Options{
			Method: "GET", URI: testURI,
			Username: testUser, Password: testPass,
			Cnonce: testCnonce, Count: 1,
		})
		assert.NilError(t, err)
		_, err = v.Validate(chal, "GET", testURI, cred.String())
		assert.Assert(t, errors.Is(err, ErrInvalidNonce))
	})

	t.Run("nonce mismatch", func(t *testing.T) {
		v := testValidator()
		other := *chal
		other.Nonce = "some-other-nonce"
		cred, err := Digest(&other, Options{
			Method: "GET", URI: testURI,
			Username: testUser, Password: testPass,
			Cnonce: testCnonce, Count: 1,
		})
		assert.NilError(t, err)
		_, err = v.Validate(chal, "GET", testURI, cred.String())
		assert.Assert(t, errors.Is(err, ErrInvalidNonce))
	})
}

func TestServerValidatorRejectsUnsupported(t *testing.T) {
	v := testValidator()
	cred, err := Digest(testChallenge(), Options{
		Method: "GET", URI: testURI,
		Username: testUser, Password: testPass,
		Cnonce: testCnonce, Count: 1,
	})
	assert.NilError(t, err)

	md5chal := testChallenge()
	md5chal.Algorithm = "MD5"
	_, err = v.Validate(md5chal, "GET", testURI, cred.String())
	assert.Assert(t, errors.Is(err, ErrUnsupported))

	noqopchal := testChallenge()
	noqopchal.QOP = nil
	_, err = v.Validate(noqopchal, "GET", testURI, cred.String())
	assert.Assert(t, errors.Is(err, ErrUnsupported))
}

func TestServerValidatorRequiredFields(t *testing.T) {
	v := testValidator()
	chal := testChallenge()
	// Not a Digest header at all.
	_, err := v.Validate(chal, "GET", testURI, "Basic dXNlcjpwYXNz")
	assert.Assert(t, errors.Is(err, ErrMalformed))

	cred, err := Digest(chal, Options{
		Method: "GET", URI: testURI,
		Username: testUser, Password: testPass,
		Cnonce: testCnonce, Count: 1,
	})
	assert.NilError(t, err)
	// Realm mismatch is rejected structurally.
	other := *chal
	other.Realm = "different-realm"
	_, err = v.Validate(&other, "GET", testURI, cred.String())
	assert.Assert(t, errors.Is(err, ErrMalformed))
}
