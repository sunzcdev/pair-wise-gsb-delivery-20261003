package sasl_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/emersion/go-sasl"
)

func TestNewOAuthBearerClientNoHostOrPort(t *testing.T) {
	c := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
		Username: "user@example.com",
		Token:    "vF9dft4qmTc2Nvb3RlckBhbHRhdmlzdGEuY29tCg==",
	})
	mech, ir, err := c.Start()
	if err != nil {
		t.Fatal("Error while starting client:", err)
	}
	if mech != "OAUTHBEARER" {
		t.Error("Invalid mechanism name:", mech)
	}
	expected := []byte{110, 44, 97, 61, 117, 115, 101, 114,
		64, 101, 120, 97, 109, 112, 108, 101, 46, 99, 111,
		109, 44, 1,
		97, 117, 116, 104, 61, 66, 101, 97, 114, 101,
		114, 32, 118, 70, 57, 100, 102, 116, 52, 113, 109,
		84, 99, 50, 78, 118, 98, 51, 82, 108, 99, 107, 66,
		104, 98, 72, 82, 104, 100, 109, 108, 122, 100, 71,
		69, 117, 89, 50, 57, 116, 67, 103, 61, 61, 1, 1}
	if bytes.Compare(ir, expected) != 0 {
		t.Error("Invalid initial response:", ir)
	}
}

func TestNewOAuthBearerClient(t *testing.T) {
	c := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
		Username: "user@example.com",
		Token:    "vF9dft4qmTc2Nvb3RlckBhbHRhdmlzdGEuY29tCg==",
		Host:     "server.example.com",
		Port:     143,
	})

	mech, ir, err := c.Start()
	if err != nil {
		t.Fatal("Error while starting client:", err)
	}
	if mech != "OAUTHBEARER" {
		t.Error("Invalid mechanism name:", mech)
	}

	expected := []byte{110, 44, 97, 61, 117, 115, 101, 114,
		64, 101, 120, 97, 109, 112, 108, 101, 46, 99, 111,
		109, 44, 1, 104, 111, 115, 116, 61, 115, 101, 114,
		118, 101, 114, 46, 101, 120, 97, 109, 112, 108, 101,
		46, 99, 111, 109, 1, 112, 111, 114, 116, 61, 49, 52,
		51, 1, 97, 117, 116, 104, 61, 66, 101, 97, 114, 101,
		114, 32, 118, 70, 57, 100, 102, 116, 52, 113, 109,
		84, 99, 50, 78, 118, 98, 51, 82, 108, 99, 107, 66,
		104, 98, 72, 82, 104, 100, 109, 108, 122, 100, 71,
		69, 117, 89, 50, 57, 116, 67, 103, 61, 61, 1, 1}
	if bytes.Compare(ir, expected) != 0 {
		t.Error("Invalid initial response:", ir)
	}

	challenge := []byte("eyJzdGF0dXMiOiJpbnZhbGlkX3Rva2VuIiwic2NvcGUiOiJleGFt" +
		"cGxlX3Njb3BlIiwib3BlbmlkLWNvbmZpZ3VyYXRpb24iOiJodHRwczovL2V4YW1wbGUu" +
		"Y29tLy53ZWxsLWtub3duL29wZW5pZC1jb25maWd1cmF0aW9uIn0=")

	if _, err := c.Next(challenge); err == nil {
		t.Fatal("Expected error from handling challenge")
	}

	if _, err := c.Next([]byte("")); err == nil {
		t.Fatal("Expected error from handling challenge")
	}

}

func TestOAuthBearerServerAndClient(t *testing.T) {
	oauthErr := sasl.OAuthBearerError{
		Status:  "invalid_token",
		Scope:   "email",
		Schemes: "bearer",
	}
	authenticator := func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		if opts.Username == "fxcp" && opts.Token == "VkIvciKi9ijpiKNWrQmYCJrzgd9QYCMB" {
			return nil
		}
		return &oauthErr
	}

	t.Run("valid token", func(t *testing.T) {
		s := sasl.NewOAuthBearerServer(authenticator)
		c := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
			Username: "fxcp",
			Token:    "VkIvciKi9ijpiKNWrQmYCJrzgd9QYCMB",
		})
		_, ir, err := c.Start()
		if err != nil {
			t.Fatal(err)
		}
		_, done, err := s.Next(ir)
		if err != nil {
			t.Fatal("Unexpected error")
		}
		if !done {
			t.Fatal("Exchange is not complete")
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		s := sasl.NewOAuthBearerServer(authenticator)
		c := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
			Username: "fxcp",
			Token:    "adiffrentone",
		})
		_, ir, err := c.Start()
		if err != nil {
			t.Fatal(err)
		}
		val, done, err := s.Next(ir)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			t.Fatal("Exchange is marked complete")
		}

		_, err = c.Next(val)
		if err == nil {
			t.Fatal("Expected an error")
		}
		authzErr, ok := err.(*sasl.OAuthBearerError)
		if !ok {
			t.Fatal("Not OAuthBearerError")
		}
		if authzErr.Status != "invalid_token" {
			t.Fatal("Wrong status:", authzErr.Status)
		}
		if authzErr.Scope != "email" {
			t.Fatal("Wrong scope:", authzErr.Scope)
		}
	})

	authenticator = func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		if opts.Username == "" && opts.Token == "VkIvciKi9ijpiKNWrQmYCJrzgd9QYCMB" {
			return nil
		}
		return &oauthErr
	}
	t.Run("valid token, no username", func(t *testing.T) {
		s := sasl.NewOAuthBearerServer(authenticator)
		c := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
			Token: "VkIvciKi9ijpiKNWrQmYCJrzgd9QYCMB",
		})
		_, ir, err := c.Start()
		if err != nil {
			t.Fatal(err)
		}
		_, done, err := s.Next(ir)
		if err != nil {
			t.Fatal("Unexpected error")
		}
		if !done {
			t.Fatal("Exchange is not complete")
		}
	})
}

// A username containing the structural characters of the GS2 header and
// the key/value fields must not break the wire format: the client escapes
// them and the strict server recovers the exact username.
func TestOAuthBearerClientEscapesAuthzid(t *testing.T) {
	c := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
		Username: `a,b=c`,
		Token:    "vF9dft4qmTc2Nvb3RlckBhbHRhdmlzdGEuY29tCg==",
	})
	_, ir, err := c.Start()
	if err != nil {
		t.Fatal("Error while starting client:", err)
	}
	expected := []byte("n,a=a=2Cb=3Dc,\x01auth=Bearer vF9dft4qmTc2Nvb3RlckBhbHRhdmlzdGEuY29tCg==\x01\x01")
	if !bytes.Equal(ir, expected) {
		t.Fatalf("Invalid initial response:\n got: %q\nwant: %q", ir, expected)
	}
}

func TestOAuthBearerStrictRoundTrip(t *testing.T) {
	const (
		username = `a,b=c, 中文`
		token    = "VkIvciKi9ijpiKNWrQmYCJrzgd9QYCMB"
		host     = "server.example.com"
		port     = 143
	)
	var called int
	s := sasl.NewOAuthBearerServerStrict(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		called++
		if opts.Username != username {
			t.Errorf("Username = %q, want %q", opts.Username, username)
		}
		if opts.Token != token {
			t.Errorf("Token = %q, want %q", opts.Token, token)
		}
		if opts.Host != host {
			t.Errorf("Host = %q, want %q", opts.Host, host)
		}
		if opts.Port != port {
			t.Errorf("Port = %d, want %d", opts.Port, port)
		}
		return nil
	})
	c := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
		Username: username,
		Token:    token,
		Host:     host,
		Port:     port,
	})

	// Normal exchange from RFC 7628 section 3.1: the client sends its
	// initial response, the empty server challenge is never needed, and
	// the exchange completes entirely in memory.
	mech, ir, err := c.Start()
	if err != nil {
		t.Fatal(err)
	}
	if mech != sasl.OAuthBearer {
		t.Fatalf("mechanism = %q", mech)
	}
	challenge, done, err := s.Next(ir)
	if err != nil {
		t.Fatal("strict server rejected well-formed message:", err)
	}
	if !done {
		t.Fatal("exchange not complete after initial response")
	}
	if challenge != nil {
		t.Fatalf("unexpected challenge: %q", challenge)
	}
	if called != 1 {
		t.Fatalf("authenticator called %d times, want 1", called)
	}
}

// Failure exchange from RFC 7628 section 3.2: after a failed
// authentication the server sends a JSON error challenge, the client
// reports the error, and confirms the abort with a single 0x01 byte.
func TestOAuthBearerStrictFailureExchange(t *testing.T) {
	oauthErr := &sasl.OAuthBearerError{
		Status:  "invalid_token",
		Schemes: "bearer",
		Scope:   "email",
	}
	var calls int
	s := sasl.NewOAuthBearerServerStrict(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		calls++
		return oauthErr
	})
	c := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
		Username: "fxcp",
		Token:    "adiffrentone",
	})

	_, ir, err := c.Start()
	if err != nil {
		t.Fatal(err)
	}
	challenge, done, err := s.Next(ir)
	if err != nil {
		t.Fatal("server must not return a Go error on auth failure:", err)
	}
	if done {
		t.Fatal("exchange marked complete on auth failure")
	}
	if calls != 1 {
		t.Fatalf("authenticator called %d times, want 1", calls)
	}
	var gotErr sasl.OAuthBearerError
	if err := json.Unmarshal(challenge, &gotErr); err != nil {
		t.Fatal("challenge is not JSON:", err)
	}
	if gotErr.Status != oauthErr.Status || gotErr.Scope != oauthErr.Scope {
		t.Fatalf("challenge = %+v, want %+v", gotErr, oauthErr)
	}

	// The client turns the JSON challenge into an OAuthBearerError.
	_, clientErr := c.Next(challenge)
	var clientOAuthErr *sasl.OAuthBearerError
	if !errors.As(clientErr, &clientOAuthErr) {
		t.Fatalf("client error = %T, want *OAuthBearerError", clientErr)
	}

	// The protocol-mandated confirmation is exactly one 0x01 byte.
	_, done, err = s.Next([]byte{0x01})
	if !done {
		t.Fatal("exchange not complete after confirmation")
	}
	if !errors.Is(err, oauthErr) && err.Error() != clientOAuthErr.Error() {
		t.Fatalf("server error = %v, want the authentication error", err)
	}
	if calls != 1 {
		t.Fatalf("authenticator called again (%d total)", calls)
	}
}

// After the failure challenge, only the single 0x01 confirmation is
// accepted. Empty slices and arbitrary longer inputs must not panic, must
// terminate the exchange, and must never invoke the authenticator again.
func TestOAuthBearerServerRejectsInvalidConfirmation(t *testing.T) {
	badConfirmations := [][]byte{
		nil,
		{},
		{0x00},
		{0x02},
		{0x01, 0x01},
		{'*'},
		[]byte("not a confirmation"),
		bytes.Repeat([]byte{0x01}, 256),
	}
	for _, confirmation := range badConfirmations {
		t.Run("", func(t *testing.T) {
			var calls int
			s := sasl.NewOAuthBearerServer(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
				calls++
				return &sasl.OAuthBearerError{Status: "invalid_token"}
			})
			c := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{Token: "adiffrentone"})
			_, ir, err := c.Start()
			if err != nil {
				t.Fatal(err)
			}
			challenge, done, err := s.Next(ir)
			if err != nil || done {
				t.Fatalf("Next(ir) = (%q, %v, %v)", challenge, done, err)
			}

			challenge, done, err = s.Next(confirmation)
			if !done {
				t.Errorf("done = false for confirmation %v", confirmation)
			}
			if !errors.Is(err, sasl.ErrInvalidClientResponse) {
				t.Errorf("err = %v for confirmation %v, want ErrInvalidClientResponse", err, confirmation)
			}
			if challenge != nil {
				t.Errorf("challenge = %q, want nil", challenge)
			}
			if calls != 1 {
				t.Errorf("authenticator called %d times, want 1", calls)
			}
		})
	}
}

// invalidRequest is a strict server that has rejected a malformed initial
// response: it returns an invalid_request JSON challenge without calling
// the authenticator.
func assertInvalidRequest(t *testing.T, s sasl.Server, response []byte, calls *int) {
	t.Helper()
	challenge, done, err := s.Next(response)
	if err != nil {
		t.Fatalf("malformed response must produce a challenge, got error: %v", err)
	}
	if done {
		t.Fatalf("malformed response unexpectedly completed exchange: %q", response)
	}
	var oauthErr sasl.OAuthBearerError
	if jerr := json.Unmarshal(challenge, &oauthErr); jerr != nil {
		t.Fatalf("challenge %q is not JSON: %v", challenge, jerr)
	}
	if oauthErr.Status != "invalid_request" {
		t.Fatalf("status = %q, want invalid_request (response %q)", oauthErr.Status, response)
	}
	if *calls != 0 {
		t.Fatalf("authenticator called %d times for malformed response %q", *calls, response)
	}
	// Only the single 0x01 confirmation ends the rejected exchange.
	if _, done, err := s.Next([]byte{0x01}); !done || err == nil {
		t.Fatalf("confirmation after malformed response: done=%v err=%v", done, err)
	}
}

func TestOAuthBearerStrictRejectsMalformed(t *testing.T) {
	malformed := []string{
		"",
		"y,a=user,\x01auth=Bearer token\x01\x01",
		"n,user,\x01auth=Bearer token\x01\x01",
		"n,a=us=er,\x01auth=Bearer token\x01\x01",
		"n,a=us=xer,\x01auth=Bearer token\x01\x01",
		"n,a=us=2cer,\x01auth=Bearer token\x01\x01",
		"n,a=us=3der,\x01auth=Bearer token\x01\x01",
		"n,a=us,er,\x01auth=Bearer token\x01\x01",
		"n,a=user\x01auth=Bearer token\x01\x01",
		"n,a=user,auth=Bearer token\x01\x01",
		"n,a=user,\x01auth=Bearer token\x01",
		"n,a=user,\x01auth=Bearer token\x01\x01\x01",
		"n,a=user,\x01\x01auth=Bearer token\x01\x01",
		"n,a=user,\x01host\x01auth=Bearer token\x01\x01",
		"n,a=user,\x01=value\x01auth=Bearer token\x01\x01",
		"n,a=user,\x01host=a.example.com\x01host=b.example.com\x01auth=Bearer token\x01\x01",
		"n,a=user,\x01port=1\x01port=2\x01auth=Bearer token\x01\x01",
		"n,a=user,\x01port=0\x01port=0\x01auth=Bearer token\x01\x01",
		"n,a=user,\x01auth=Bearer token\x01auth=Bearer token\x01\x01",
		"n,a=user,\x01foo=bar\x01auth=Bearer token\x01\x01",
		"n,a=user,\x01host=\x01auth=Bearer token\x01\x01",
		"n,a=user,\x01port=65536\x01auth=Bearer token\x01\x01",
		"n,a=user,\x01port=abc\x01auth=Bearer token\x01\x01",
		"n,a=user,\x01auth=MAC token\x01\x01",
		"n,a=user,\x01auth=Bearer bad token\x01\x01",
		"n,a=user,\x01auth=Bearer \x01\x01",
		"n,a=user,\x01host=server.example.com\x01\x01",
	}
	for _, response := range malformed {
		response := response
		t.Run("", func(t *testing.T) {
			var calls int
			s := sasl.NewOAuthBearerServerStrict(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
				calls++
				return nil
			})
			assertInvalidRequest(t, s, []byte(response), &calls)
		})
	}
}

// The lenient constructor keeps the historical behavior: it still accepts
// well-formed initial responses, decodes GS2-escaped usernames and only
// rejects structurally unusable messages.
func TestOAuthBearerLenientServerCompatibility(t *testing.T) {
	okResponses := [][]byte{
		// Client-produced message with escaped authzid.
		[]byte("n,a=a=2Cb=3Dc,\x01host=server.example.com\x01auth=Bearer token\x01\x01"),
		// Historical, unescaped authzid containing an equals sign.
		[]byte("n,a=user=name,\x01auth=Bearer token\x01\x01"),
		// No host or port.
		[]byte("n,,\x01auth=Bearer token\x01\x01"),
	}
	for _, response := range okResponses {
		var called bool
		s := sasl.NewOAuthBearerServer(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
			called = true
			return nil
		})
		challenge, done, err := s.Next(response)
		if err != nil || !done || challenge != nil || !called {
			t.Errorf("lenient server rejected %q: challenge=%q done=%v err=%v called=%v",
				response, challenge, done, err, called)
		}
	}

	// A raw comma in the authzid never reached the username even under the
	// historical parser; such messages stay invalid.
	var calls int
	s := sasl.NewOAuthBearerServer(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		calls++
		return nil
	})
	if challenge, done, err := s.Next([]byte("n,a=us,er,\x01auth=Bearer token\x01\x01")); err != nil || done {
		t.Fatalf("expected invalid_request challenge, got challenge=%q done=%v err=%v", challenge, done, err)
	}
	if calls != 0 {
		t.Fatalf("authenticator called %d times", calls)
	}
}

func TestOAuthBearerClientRejectsStructurallyInvalidOptions(t *testing.T) {
	bad := []*sasl.OAuthBearerOptions{
		{Token: "bad token"},
		{Token: ""},
		{Token: "token", Port: 70000},
		{Token: "token", Host: "host\x01evil"},
		{Token: "token", Username: "user\x00name"},
	}
	for _, opts := range bad {
		c := sasl.NewOAuthBearerClient(opts)
		if _, _, err := c.Start(); err == nil {
			t.Errorf("Start() expected error for %+v", opts)
		}
	}
}

// Fuzzing the server with arbitrary bytes must never panic, neither while
// parsing an initial response nor while confirming a failure challenge.
func FuzzOAuthBearerServer(f *testing.F) {
	f.Add([]byte("n,a=user,\x01auth=Bearer token\x01\x01"))
	f.Add([]byte{})
	f.Add([]byte{0x01})
	f.Fuzz(func(t *testing.T, response []byte) {
		auth := func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
			return nil
		}
		// Initial response path, both parsers.
		s := sasl.NewOAuthBearerServer(auth)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("lenient server panicked on %q: %v", response, r)
				}
			}()
			_, _, _ = s.Next(response)
		}()

		st := sasl.NewOAuthBearerServerStrict(auth)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("strict server panicked on %q: %v", response, r)
				}
			}()
			_, _, _ = st.Next(response)
		}()

		// Failure challenge confirmation path.
		failed := sasl.NewOAuthBearerServer(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
			return &sasl.OAuthBearerError{Status: "invalid_token"}
		})
		_, _, _ = failed.Next([]byte("n,,\x01auth=Bearer tok\x01\x01"))
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("confirmation handler panicked on %q: %v", response, r)
				}
			}()
			_, _, _ = failed.Next(response)
		}()
	})
}
