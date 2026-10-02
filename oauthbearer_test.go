package sasl_test

import (
	"bytes"
	"strings"
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

func TestOAuthBearerAuthzidEscaping(t *testing.T) {
	username := "us,er=na=me,e@example.com"

	c := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
		Username: username,
		Token:    "token",
	})
	_, ir, err := c.Start()
	if err != nil {
		t.Fatal(err)
	}

	// ',' and '=' must be escaped so they cannot break the gs2-header
	// or key-value pair structure.
	parts := bytes.SplitN(ir, []byte{','}, 3)
	if len(parts) != 3 {
		t.Fatal("Escaped authzid broke the gs2-header structure:", ir)
	}
	if string(parts[1]) != "a=us=2Cer=3Dna=3Dme=2Ce@example.com" {
		t.Error("Invalid escaped authzid:", string(parts[1]))
	}

	// Both servers must decode the original username.
	for name, newServer := range map[string]func(sasl.OAuthBearerAuthenticator) sasl.Server{
		"lenient": sasl.NewOAuthBearerServer,
		"strict":  sasl.NewOAuthBearerServerStrict,
	} {
		t.Run(name, func(t *testing.T) {
			var got sasl.OAuthBearerOptions
			s := newServer(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
				got = opts
				return nil
			})
			if _, done, err := s.Next(ir); err != nil {
				t.Fatal("Unexpected error:", err)
			} else if !done {
				t.Fatal("Exchange is not complete")
			}
			if got.Username != username {
				t.Error("Invalid decoded username:", got.Username)
			}
		})
	}
}

func TestOAuthBearerServerStrict(t *testing.T) {
	called := 0
	authenticator := func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		called++
		return nil
	}

	cases := []struct {
		name     string
		response string
	}{
		{"missing gs2-cb-flag", "y,,\x01auth=Bearer t\x01\x01"},
		{"missing gs2-header separator", "n,a=u\x01auth=Bearer t\x01\x01"},
		{"missing a= prefix", "n,u,\x01auth=Bearer t\x01\x01"},
		{"invalid authzid escape", "n,a=us=2Xer,\x01auth=Bearer t\x01\x01"},
		{"truncated authzid escape", "n,a=user=3,\x01auth=Bearer t\x01\x01"},
		{"unescaped comma in authzid", "n,a=us,er,\x01auth=Bearer t\x01\x01"},
		{"missing field separator", "n,,auth=Bearer t\x01\x01"},
		{"missing trailing separators", "n,,\x01auth=Bearer t"},
		{"single trailing separator", "n,,\x01auth=Bearer t\x01"},
		{"empty field", "n,,\x01\x01auth=Bearer t\x01\x01"},
		{"missing equals", "n,,\x01auth\x01\x01"},
		{"missing auth", "n,,\x01host=server.example.com\x01\x01"},
		{"no fields at all", "n,,\x01\x01"},
		{"empty auth token", "n,,\x01auth=Bearer \x01\x01"},
		{"unsupported token type", "n,,\x01auth=Basic d\x01\x01"},
		{"duplicate host", "n,,\x01host=a\x01host=b\x01auth=Bearer t\x01\x01"},
		{"duplicate port", "n,,\x01port=143\x01port=993\x01auth=Bearer t\x01\x01"},
		{"duplicate auth", "n,,\x01auth=Bearer t\x01auth=Bearer t\x01\x01"},
		{"malformed port", "n,,\x01port=http\x01auth=Bearer t\x01\x01"},
		{"unknown parameter", "n,,\x01wtf=1\x01auth=Bearer t\x01\x01"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			called = 0
			s := sasl.NewOAuthBearerServerStrict(authenticator)
			challenge, done, err := s.Next([]byte(c.response))
			if err != nil {
				t.Fatal("Unexpected error:", err)
			}
			if done {
				t.Fatal("Exchange is marked complete")
			}
			if !bytes.Contains(challenge, []byte("invalid_request")) {
				t.Error("Expected invalid_request challenge, got:", string(challenge))
			}
			if called != 0 {
				t.Error("Authenticator called for malformed response")
			}

			// After the failure challenge, only a single 0x01 byte is
			// an acceptable confirmation.
			if _, _, err := s.Next([]byte{0x01}); err == nil {
				t.Fatal("Expected error after confirmation")
			}
		})
	}
}

func TestOAuthBearerServerStrictValid(t *testing.T) {
	var got sasl.OAuthBearerOptions
	s := sasl.NewOAuthBearerServerStrict(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		got = opts
		return nil
	})

	// Example from RFC 7628 section 4.1.
	ir := []byte("n,a=user@example.com,\x01host=server.example.com\x01port=143" +
		"\x01auth=Bearer vF9dft4qmTc2Nvb3RlckBhbHRhdmlzdGEuY29tCg==\x01\x01")
	_, done, err := s.Next(ir)
	if err != nil {
		t.Fatal("Unexpected error:", err)
	}
	if !done {
		t.Fatal("Exchange is not complete")
	}
	if got.Username != "user@example.com" {
		t.Error("Invalid username:", got.Username)
	}
	if got.Host != "server.example.com" {
		t.Error("Invalid host:", got.Host)
	}
	if got.Port != 143 {
		t.Error("Invalid port:", got.Port)
	}
	if got.Token != "vF9dft4qmTc2Nvb3RlckBhbHRhdmlzdGEuY29tCg==" {
		t.Error("Invalid token:", got.Token)
	}
}

func TestOAuthBearerFailureConfirmation(t *testing.T) {
	for name, newServer := range map[string]func(sasl.OAuthBearerAuthenticator) sasl.Server{
		"lenient": sasl.NewOAuthBearerServer,
		"strict":  sasl.NewOAuthBearerServerStrict,
	} {
		t.Run(name, func(t *testing.T) {
			called := 0
			s := newServer(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
				called++
				return &sasl.OAuthBearerError{Status: "invalid_token", Schemes: "bearer"}
			})

			_, _, err := s.Next([]byte("n,a=u,\x01auth=Bearer bad\x01\x01"))
			if err != nil {
				t.Fatal("Unexpected error:", err)
			}
			if called != 1 {
				t.Fatal("Authenticator not called exactly once:", called)
			}

			// Only a single 0x01 byte confirms the failure. Anything
			// else must be rejected without panicking and without
			// calling the authenticator again.
			rejected := [][]byte{
				{},
				[]byte(""),
				[]byte{0x00},
				[]byte{0x01, 0x01},
				[]byte{0x01, 0x01, 0x01},
				[]byte("cancel"),
			}
			for _, resp := range rejected {
				if _, done, err := s.Next(resp); err == nil {
					t.Errorf("Expected error for response %q", resp)
				} else if !done {
					t.Errorf("Exchange not marked done for response %q", resp)
				}
			}
			if called != 1 {
				t.Fatal("Authenticator called again after failure:", called)
			}

			if _, done, err := s.Next([]byte{0x01}); err == nil {
				t.Fatal("Expected error after confirmation")
			} else if !done {
				t.Fatal("Exchange not marked done after confirmation")
			}
			if called != 1 {
				t.Fatal("Authenticator called again after confirmation:", called)
			}
		})
	}
}

// TestOAuthBearerExchangeRFC7628 exercises the normal and failing message
// exchanges described in RFC 7628 sections 4.1 and 4.3, entirely in memory.
func TestOAuthBearerExchangeRFC7628(t *testing.T) {
	oauthErr := sasl.OAuthBearerError{
		Status:  "invalid_token",
		Schemes: "bearer",
		Scope:   "example_scope",
	}
	authenticator := func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		if opts.Username == "user@example.com" &&
			opts.Token == "vF9dft4qmTc2Nvb3RlckBhbHRhdmlzdGEuY29tCg==" {
			return nil
		}
		return &oauthErr
	}

	t.Run("success", func(t *testing.T) {
		s := sasl.NewOAuthBearerServerStrict(authenticator)
		c := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
			Username: "user@example.com",
			Token:    "vF9dft4qmTc2Nvb3RlckBhbHRhdmlzdGEuY29tCg==",
			Host:     "server.example.com",
			Port:     143,
		})

		mech, ir, err := c.Start()
		if err != nil {
			t.Fatal(err)
		}
		if mech != "OAUTHBEARER" {
			t.Fatal("Invalid mechanism name:", mech)
		}

		_, done, err := s.Next(ir)
		if err != nil {
			t.Fatal("Unexpected error:", err)
		}
		if !done {
			t.Fatal("Exchange is not complete")
		}
	})

	t.Run("failure", func(t *testing.T) {
		s := sasl.NewOAuthBearerServerStrict(authenticator)
		c := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
			Username: "user@example.com",
			Token:    "expired",
		})

		_, ir, err := c.Start()
		if err != nil {
			t.Fatal(err)
		}

		challenge, done, err := s.Next(ir)
		if err != nil {
			t.Fatal("Unexpected error:", err)
		}
		if done {
			t.Fatal("Exchange is marked complete")
		}

		// The client decodes the JSON error challenge.
		if _, err := c.Next(challenge); err == nil {
			t.Fatal("Expected error from handling challenge")
		} else if authzErr, ok := err.(*sasl.OAuthBearerError); !ok {
			t.Fatal("Not OAuthBearerError:", err)
		} else if authzErr.Status != "invalid_token" {
			t.Fatal("Wrong status:", authzErr.Status)
		}

		// The client confirms the failure with a single 0x01 byte and
		// the server terminates the exchange with the original error.
		if _, done, err := s.Next([]byte{0x01}); err == nil {
			t.Fatal("Expected error after confirmation")
		} else if !done {
			t.Fatal("Exchange not marked done after confirmation")
		} else if !strings.Contains(err.Error(), "invalid_token") {
			t.Fatal("Unexpected confirmation error:", err)
		}
	})
}
