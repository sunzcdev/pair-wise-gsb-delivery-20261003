package digest_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/icholy/digest"
	"gotest.tools/v3/assert"
)

var (
	testRealm    = "http-auth@example.org"
	testNonce    = "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v"
	testOpaque   = "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"
	testUsername = "Mufasa"
	testPassword = "Circle of Life"
	testHA1      = "7987c64c30e25f1b74be53f966b49b90f2808aa92faf9a00262392d7b4794232"
	testCnonce   = "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ"
)

func testChallenge() *digest.Challenge {
	return &digest.Challenge{
		Realm:     testRealm,
		Nonce:     testNonce,
		Opaque:    testOpaque,
		Algorithm: "SHA-256",
		QOP:       []string{"auth"},
	}
}

func TestVerifyInterop(t *testing.T) {
	for _, tt := range []struct {
		name   string
		secret digest.Secret
	}{
		{
			name:   "password callback",
			secret: digest.Secret{Password: testPassword},
		},
		{
			name:   "ha1 callback",
			secret: digest.Secret{HA1: testHA1},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cred, err := digest.Digest(testChallenge(), digest.Options{
				Method:   "POST",
				URI:      "/resource?next=/home",
				Username: testUsername,
				Password: testPassword,
				Cnonce:   testCnonce,
				Count:    1,
			})
			assert.NilError(t, err)

			store := digest.NewNcStore()
			auth, err := digest.Verify(digest.VerifyRequest{
				Challenge:   testChallenge(),
				Method:      "POST",
				URI:         "/resource?next=/home",
				Credentials: cred.String(),
				LookupSecret: func(username string, realm string) (digest.Secret, error) {
					assert.Equal(t, username, testUsername)
					assert.Equal(t, realm, testRealm)
					return tt.secret, nil
				},
				CommitNc: store.CommitNc,
			})
			assert.NilError(t, err)
			assert.DeepEqual(t, auth, &digest.Authentication{
				Username:  testUsername,
				Realm:     testRealm,
				Nonce:     testNonce,
				URI:       "/resource?next=/home",
				Method:    "POST",
				Algorithm: "SHA-256",
				QOP:       "auth",
				Nc:        1,
				Cnonce:    testCnonce,
			})

			_, err = digest.Verify(digest.VerifyRequest{
				Challenge:   testChallenge(),
				Method:      "POST",
				URI:         "/resource?next=/home",
				Credentials: strings.ReplaceAll(cred.String(), cred.Response, strings.ToUpper(cred.Response)),
				LookupSecret: func(string, string) (digest.Secret, error) {
					return tt.secret, nil
				},
				CommitNc: digest.NewNcStore().CommitNc,
			})
			assert.NilError(t, err)
		})
	}
}

func TestVerifyFixedVector(t *testing.T) {
	const header = `Digest username="Mufasa", realm="http-auth@example.org", ` +
		`nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v", ` +
		`uri="/dir/index.html", algorithm=SHA-256, ` +
		`cnonce="f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ", ` +
		`opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS", qop=auth, ` +
		`nc=00000001, response="753927fa0e85d155564e2e272a28d1802ca10daf4496794697cf8db5856cb6c1"`

	for _, tt := range []struct {
		name   string
		secret digest.Secret
	}{
		{name: "password", secret: digest.Secret{Password: testPassword}},
		{name: "ha1", secret: digest.Secret{HA1: testHA1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			chal := testChallenge()
			chal.QOP = []string{"auth", "auth-int"}
			auth, err := digest.Verify(digest.VerifyRequest{
				Challenge:   chal,
				Method:      "GET",
				URI:         "/dir/index.html",
				Credentials: header,
				LookupSecret: func(string, string) (digest.Secret, error) {
					return tt.secret, nil
				},
				CommitNc: digest.NewNcStore().CommitNc,
			})
			assert.NilError(t, err)
			assert.Equal(t, auth.Username, testUsername)
			assert.Equal(t, auth.Nc, 1)
		})
	}
}

func TestVerifyRejectsClaimedURI(t *testing.T) {
	cred, err := digest.Digest(testChallenge(), digest.Options{
		Method:   "GET",
		URI:      "/allowed",
		Username: testUsername,
		Password: testPassword,
		Cnonce:   testCnonce,
		Count:    1,
	})
	assert.NilError(t, err)

	var lookedUp bool
	_, err = digest.Verify(digest.VerifyRequest{
		Challenge:   testChallenge(),
		Method:      "GET",
		URI:         "/allowed/../admin",
		Credentials: cred.String(),
		LookupSecret: func(string, string) (digest.Secret, error) {
			lookedUp = true
			return digest.Secret{Password: testPassword}, nil
		},
		CommitNc: digest.NewNcStore().CommitNc,
	})
	assert.ErrorIs(t, err, digest.ErrInvalidCredentials)
	assert.Assert(t, !lookedUp)
}

func TestVerifyDuplicateNc(t *testing.T) {
	cred, err := digest.Digest(testChallenge(), digest.Options{
		Method:   "GET",
		URI:      "/resource",
		Username: testUsername,
		Password: testPassword,
		Cnonce:   testCnonce,
		Count:    7,
	})
	assert.NilError(t, err)

	store := digest.NewNcStore()
	request := digest.VerifyRequest{
		Challenge:   testChallenge(),
		Method:      "GET",
		URI:         "/resource",
		Credentials: cred.String(),
		LookupSecret: func(string, string) (digest.Secret, error) {
			return digest.Secret{Password: testPassword}, nil
		},
		CommitNc: store.CommitNc,
	}

	auth, err := digest.Verify(request)
	assert.NilError(t, err)
	assert.Equal(t, auth.Nc, 7)

	_, err = digest.Verify(request)
	assert.ErrorIs(t, err, digest.ErrNcReplayed)
}

func TestVerifyConcurrentSameNc(t *testing.T) {
	cred, err := digest.Digest(testChallenge(), digest.Options{
		Method:   "GET",
		URI:      "/resource",
		Username: testUsername,
		Password: testPassword,
		Cnonce:   testCnonce,
		Count:    1,
	})
	assert.NilError(t, err)

	store := digest.NewNcStore()
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := digest.Verify(digest.VerifyRequest{
				Challenge:   testChallenge(),
				Method:      "GET",
				URI:         "/resource",
				Credentials: cred.String(),
				LookupSecret: func(string, string) (digest.Secret, error) {
					return digest.Secret{Password: testPassword}, nil
				},
				CommitNc: store.CommitNc,
			})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var success, replay int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, digest.ErrNcReplayed):
			replay++
		default:
			t.Fatalf("unexpected verification error: %v", err)
		}
	}
	assert.Equal(t, success, 1)
	assert.Equal(t, replay, 1)
}

func TestVerifyErrorClassificationAndSecretRedaction(t *testing.T) {
	valid := func(t *testing.T) digest.VerifyRequest {
		cred, err := digest.Digest(testChallenge(), digest.Options{
			Method:   "GET",
			URI:      "/resource",
			Username: testUsername,
			Password: testPassword,
			Cnonce:   testCnonce,
			Count:    1,
		})
		assert.NilError(t, err)
		return digest.VerifyRequest{
			Challenge:   testChallenge(),
			Method:      "GET",
			URI:         "/resource",
			Credentials: cred.String(),
			LookupSecret: func(string, string) (digest.Secret, error) {
				return digest.Secret{Password: testPassword}, nil
			},
			CommitNc: digest.NewNcStore().CommitNc,
		}
	}

	t.Run("unknown user", func(t *testing.T) {
		req := valid(t)
		req.LookupSecret = func(string, string) (digest.Secret, error) {
			return digest.Secret{}, digest.ErrUnknownUser
		}
		_, err := digest.Verify(req)
		assert.ErrorIs(t, err, digest.ErrUnknownUser)
		assert.Assert(t, !strings.Contains(err.Error(), testPassword))
	})

	t.Run("wrong password", func(t *testing.T) {
		req := valid(t)
		req.LookupSecret = func(string, string) (digest.Secret, error) {
			return digest.Secret{Password: "wrong-password"}, nil
		}
		_, err := digest.Verify(req)
		assert.ErrorIs(t, err, digest.ErrInvalidDigest)
		assert.Assert(t, !strings.Contains(err.Error(), testPassword))
		assert.Assert(t, !strings.Contains(err.Error(), "wrong-password"))
	})

	t.Run("malformed ha1", func(t *testing.T) {
		req := valid(t)
		req.LookupSecret = func(string, string) (digest.Secret, error) {
			return digest.Secret{HA1: "not-a-valid-ha1"}, nil
		}
		_, err := digest.Verify(req)
		assert.ErrorIs(t, err, digest.ErrLookupFailed)
		assert.Assert(t, !strings.Contains(err.Error(), "not-a-valid-ha1"))
	})

	t.Run("replay", func(t *testing.T) {
		req := valid(t)
		_, err := digest.Verify(req)
		assert.NilError(t, err)
		_, err = digest.Verify(req)
		assert.ErrorIs(t, err, digest.ErrNcReplayed)
	})
}

func TestVerifyRequiredFields(t *testing.T) {
	cred, err := digest.Digest(testChallenge(), digest.Options{
		Method:   "GET",
		URI:      "/resource",
		Username: testUsername,
		Password: testPassword,
		Cnonce:   testCnonce,
		Count:    1,
	})
	assert.NilError(t, err)

	tests := []struct {
		name      string
		modify    func(*digest.Credentials)
		wantError error
	}{
		{name: "username", modify: func(c *digest.Credentials) { c.Username = "" }, wantError: digest.ErrInvalidCredentials},
		{name: "realm", modify: func(c *digest.Credentials) { c.Realm = "other" }, wantError: digest.ErrInvalidCredentials},
		{name: "nonce", modify: func(c *digest.Credentials) { c.Nonce = "other" }, wantError: digest.ErrInvalidCredentials},
		{name: "algorithm", modify: func(c *digest.Credentials) { c.Algorithm = "MD5" }, wantError: digest.ErrInvalidCredentials},
		{name: "qop", modify: func(c *digest.Credentials) { c.QOP = "auth-int" }, wantError: digest.ErrInvalidCredentials},
		{name: "uri", modify: func(c *digest.Credentials) { c.URI = "" }, wantError: digest.ErrInvalidCredentials},
		{name: "cnonce", modify: func(c *digest.Credentials) { c.Cnonce = "" }, wantError: digest.ErrInvalidCredentials},
		{name: "nc", modify: func(c *digest.Credentials) { c.Nc = 0 }, wantError: digest.ErrInvalidCredentials},
		{name: "response", modify: func(c *digest.Credentials) { c.Response = strings.Repeat("0", 64) }, wantError: digest.ErrInvalidDigest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutated := *cred
			tt.modify(&mutated)
			store := digest.NewNcStore()
			_, err := digest.Verify(digest.VerifyRequest{
				Challenge:   testChallenge(),
				Method:      "GET",
				URI:         "/resource",
				Credentials: mutated.String(),
				LookupSecret: func(string, string) (digest.Secret, error) {
					return digest.Secret{Password: testPassword}, nil
				},
				CommitNc: store.CommitNc,
			})
			assert.ErrorIs(t, err, tt.wantError)
		})
	}
}

func TestVerifyInvalidChallengeAndRequest(t *testing.T) {
	cred, err := digest.Digest(testChallenge(), digest.Options{
		Method:   "GET",
		URI:      "/resource",
		Username: testUsername,
		Password: testPassword,
		Cnonce:   testCnonce,
		Count:    1,
	})
	assert.NilError(t, err)

	validChallenge := testChallenge()
	tests := []struct {
		name      string
		challenge *digest.Challenge
		method    string
		uri       string
		header    string
		lookup    func(string, string) (digest.Secret, error)
		commit    func(digest.Authentication) error
		want      error
	}{
		{name: "nil challenge", challenge: nil, want: digest.ErrInvalidChallenge},
		{name: "missing realm", challenge: &digest.Challenge{Nonce: testNonce, Algorithm: "SHA-256", QOP: []string{"auth"}}, want: digest.ErrInvalidChallenge},
		{name: "missing nonce", challenge: &digest.Challenge{Realm: testRealm, Algorithm: "SHA-256", QOP: []string{"auth"}}, want: digest.ErrInvalidChallenge},
		{name: "unsupported algorithm", challenge: &digest.Challenge{Realm: testRealm, Nonce: testNonce, Algorithm: "MD5", QOP: []string{"auth"}}, want: digest.ErrInvalidChallenge},
		{name: "unsupported qop", challenge: &digest.Challenge{Realm: testRealm, Nonce: testNonce, Algorithm: "SHA-256", QOP: []string{"auth-int"}}, want: digest.ErrInvalidChallenge},
		{name: "missing method", challenge: validChallenge, method: "", uri: "/resource", want: digest.ErrInvalidRequest},
		{name: "missing target", challenge: validChallenge, method: "GET", uri: "", want: digest.ErrInvalidRequest},
		{name: "missing lookup", challenge: validChallenge, method: "GET", uri: "/resource", want: digest.ErrInvalidRequest},
		{name: "missing commit", challenge: validChallenge, method: "GET", uri: "/resource", want: digest.ErrInvalidRequest},
		{name: "malformed header", challenge: validChallenge, method: "GET", uri: "/resource", header: "Basic abc", want: digest.ErrInvalidCredentials},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name != "missing lookup" {
				tt.lookup = func(string, string) (digest.Secret, error) {
					return digest.Secret{Password: testPassword}, nil
				}
			}
			if tt.name != "missing commit" {
				tt.commit = digest.NewNcStore().CommitNc
			}
			header := tt.header
			if header == "" {
				header = cred.String()
			}
			method := tt.method
			if method == "" && tt.name != "missing method" {
				method = "GET"
			}
			uri := tt.uri
			if uri == "" && tt.name != "missing target" {
				uri = "/resource"
			}
			_, err := digest.Verify(digest.VerifyRequest{
				Challenge:    tt.challenge,
				Method:       method,
				URI:          uri,
				Credentials:  header,
				LookupSecret: tt.lookup,
				CommitNc:     tt.commit,
			})
			assert.ErrorIs(t, err, tt.want)
		})
	}
}
