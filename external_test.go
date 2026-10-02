package sasl_test

import (
	"errors"
	"testing"

	"github.com/emersion/go-sasl"
)

func TestNewPlainServerWithAuth(t *testing.T) {
	t.Run("empty authzid uses authenticated identity", func(t *testing.T) {
		var authenticatedAuthcid string
		var authorizedAuthzid string
		s := sasl.NewPlainServerWithAuth(
			func(username, password string) (string, error) {
				if username != "username" || password != "password" {
					return "", errors.New("invalid credentials")
				}
				return "canonical-user", nil
			},
			func(authcid, authzid string) error {
				authenticatedAuthcid = authcid
				authorizedAuthzid = authzid
				return nil
			},
		)

		_, done, err := s.Next([]byte("\x00username\x00password"))
		if err != nil || !done {
			t.Fatalf("Next() = %v, %v, %v", nil, done, err)
		}
		if authenticatedAuthcid != "canonical-user" {
			t.Fatalf("authenticator returned %q", authenticatedAuthcid)
		}
		if authorizedAuthzid != "canonical-user" {
			t.Fatalf("authorizer received authzid %q", authorizedAuthzid)
		}
		if got := s.Identity(); got != "canonical-user" {
			t.Fatalf("Identity() = %q", got)
		}
	})

	t.Run("non-empty authzid is authorized", func(t *testing.T) {
		s := sasl.NewPlainServerWithAuth(
			func(username, password string) (string, error) {
				return "user", nil
			},
			func(authcid, authzid string) error {
				if authcid != "user" || authzid != "admin" {
					return errors.New("not authorized")
				}
				return nil
			},
		)

		_, _, err := s.Next([]byte("admin\x00user\x00password"))
		if err != nil {
			t.Fatal("Next() returned error:", err)
		}
		if got := s.Identity(); got != "admin" {
			t.Fatalf("Identity() = %q", got)
		}
	})

	t.Run("authorization failure does not expose identity", func(t *testing.T) {
		authorized := false
		s := sasl.NewPlainServerWithAuth(
			func(username, password string) (string, error) {
				return "user", nil
			},
			func(authcid, authzid string) error {
				authorized = true
				return errors.New("not authorized")
			},
		)

		_, done, err := s.Next([]byte("admin\x00user\x00password"))
		if err == nil || !done {
			t.Fatalf("Next() = %v, %v, %v", nil, done, err)
		}
		if !authorized {
			t.Fatal("authorizer was not called")
		}
		if got := s.Identity(); got != "" {
			t.Fatalf("Identity() = %q", got)
		}
	})

	t.Run("authentication failure skips authorization", func(t *testing.T) {
		authorized := false
		s := sasl.NewPlainServerWithAuth(
			func(username, password string) (string, error) {
				return "", errors.New("invalid credentials")
			},
			func(authcid, authzid string) error {
				authorized = true
				return nil
			},
		)

		_, _, err := s.Next([]byte("admin\x00user\x00password"))
		if err == nil {
			t.Fatal("Next() did not return an error")
		}
		if authorized {
			t.Fatal("authorizer was called after authentication failure")
		}
		if got := s.Identity(); got != "" {
			t.Fatalf("Identity() = %q", got)
		}
	})

	t.Run("repeated Next clears identity", func(t *testing.T) {
		s := sasl.NewPlainServerWithAuth(
			func(username, password string) (string, error) {
				return "user", nil
			},
			func(authcid, authzid string) error {
				return nil
			},
		)

		_, _, err := s.Next([]byte("\x00user\x00password"))
		if err != nil {
			t.Fatal("first Next() returned error:", err)
		}

		_, _, err = s.Next([]byte("\x00user\x00password"))
		if err != sasl.ErrUnexpectedClientResponse {
			t.Fatalf("second Next() error = %v", err)
		}
		if got := s.Identity(); got != "" {
			t.Fatalf("Identity() = %q", got)
		}
	})
}

func TestNewExternalServerWithAuth(t *testing.T) {
	t.Run("empty authzid uses injected identity", func(t *testing.T) {
		var receivedAuthcid string
		var receivedAuthzid string
		s := sasl.NewExternalServerWithAuth("peer", func(authcid, authzid string) error {
			receivedAuthcid = authcid
			receivedAuthzid = authzid
			return nil
		})

		_, done, err := s.Next([]byte{})
		if err != nil || !done {
			t.Fatalf("Next() = %v, %v, %v", nil, done, err)
		}
		if receivedAuthcid != "peer" || receivedAuthzid != "peer" {
			t.Fatalf("authorizer received %q, %q", receivedAuthcid, receivedAuthzid)
		}
		if got := s.Identity(); got != "peer" {
			t.Fatalf("Identity() = %q", got)
		}
	})

	t.Run("client cannot override authenticated identity", func(t *testing.T) {
		s := sasl.NewExternalServerWithAuth("peer", func(authcid, authzid string) error {
			if authcid != "peer" {
				t.Fatalf("authcid = %q", authcid)
			}
			if authzid != "requested" {
				t.Fatalf("authzid = %q", authzid)
			}
			return nil
		})

		_, _, err := s.Next([]byte("requested"))
		if err != nil {
			t.Fatal("Next() returned error:", err)
		}
		if got := s.Identity(); got != "requested" {
			t.Fatalf("Identity() = %q", got)
		}
	})

	t.Run("authorization failure does not expose identity", func(t *testing.T) {
		s := sasl.NewExternalServerWithAuth("peer", func(authcid, authzid string) error {
			return errors.New("not authorized")
		})

		_, _, err := s.Next([]byte("requested"))
		if err == nil {
			t.Fatal("Next() did not return an error")
		}
		if got := s.Identity(); got != "" {
			t.Fatalf("Identity() = %q", got)
		}
	})

	t.Run("empty injected identity fails", func(t *testing.T) {
		authorized := false
		s := sasl.NewExternalServerWithAuth("", func(authcid, authzid string) error {
			authorized = true
			return nil
		})

		_, _, err := s.Next([]byte{})
		if err == nil {
			t.Fatal("Next() did not return an error")
		}
		if authorized {
			t.Fatal("authorizer was called without an authentication identity")
		}
	})

	t.Run("repeated Next clears identity", func(t *testing.T) {
		s := sasl.NewExternalServerWithAuth("peer", func(authcid, authzid string) error {
			return nil
		})

		_, _, err := s.Next([]byte{})
		if err != nil {
			t.Fatal("first Next() returned error:", err)
		}

		_, _, err = s.Next([]byte{})
		if err != sasl.ErrUnexpectedClientResponse {
			t.Fatalf("second Next() error = %v", err)
		}
		if got := s.Identity(); got != "" {
			t.Fatalf("Identity() = %q", got)
		}
	})
}
