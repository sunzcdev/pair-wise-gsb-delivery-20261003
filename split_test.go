package sasl_test

import (
	"errors"
	"testing"

	"github.com/emersion/go-sasl"
)

func plainResponse(authz, authc, password string) []byte {
	return []byte(authz + "\x00" + authc + "\x00" + password)
}

func TestPlainServerWithAuthorizationSuccess(t *testing.T) {
	var authenticatedAuthc string
	var authorized [2]string

	s := sasl.NewPlainServerWithAuthorization(
		func(username, password string) error {
			if username != "alice" || password != "secret" {
				return errors.New("bad credentials")
			}
			authenticatedAuthc = username
			return nil
		},
		func(authc, authz string) error {
			authorized = [2]string{authc, authz}
			return nil
		},
	)

	if challenge, done, err := s.Next(nil); err != nil || done || len(challenge) != 0 {
		t.Fatalf("unexpected start: challenge=%q done=%v err=%v", challenge, done, err)
	}

	if challenge, done, err := s.Next(plainResponse("", "alice", "secret")); err != nil || !done || challenge != nil {
		t.Fatalf("unexpected result: challenge=%q done=%v err=%v", challenge, done, err)
	}

	if authenticatedAuthc != "alice" {
		t.Errorf("authenticated identity = %q, want alice", authenticatedAuthc)
	}
	// Empty authzid must map to the authenticated identity.
	if authorized != [2]string{"alice", "alice"} {
		t.Errorf("authorized = %v, want [alice alice]", authorized)
	}
}

func TestPlainServerWithAuthorizationExplicitAuthz(t *testing.T) {
	var authorizeCalled bool

	s := sasl.NewPlainServerWithAuthorization(
		func(username, password string) error {
			return nil
		},
		func(authc, authz string) error {
			authorizeCalled = true
			if authc != "alice" || authz != "bob" {
				t.Errorf("authorize(%q, %q), want (alice, bob)", authc, authz)
			}
			return nil
		},
	)

	s.Next(nil)
	if _, done, err := s.Next(plainResponse("bob", "alice", "secret")); err != nil || !done {
		t.Fatalf("expected success, done=%v err=%v", done, err)
	}
	if !authorizeCalled {
		t.Fatal("authorizer not consulted for explicit authzid")
	}
}

func TestPlainServerWithAuthorizationBadPassword(t *testing.T) {
	var authorizeCalled bool

	s := sasl.NewPlainServerWithAuthorization(
		func(username, password string) error {
			return errors.New("invalid username or password")
		},
		func(authc, authz string) error {
			authorizeCalled = true
			return nil
		},
	)

	s.Next(nil)
	if _, done, err := s.Next(plainResponse("", "alice", "wrong")); err == nil || !done {
		t.Fatalf("expected authentication failure, done=%v err=%v", done, err)
	}
	if authorizeCalled {
		t.Fatal("authorizer must not be consulted when authentication failed")
	}

	// A repeated Next must not reuse the previous identity.
	if _, _, err := s.Next(plainResponse("", "alice", "secret")); err != sasl.ErrUnexpectedClientResponse {
		t.Fatalf("repeated Next err = %v, want ErrUnexpectedClientResponse", err)
	}
}

func TestPlainServerWithAuthorizationDenied(t *testing.T) {
	s := sasl.NewPlainServerWithAuthorization(
		func(username, password string) error { return nil },
		func(authc, authz string) error {
			return errors.New("not authorized to act as " + authz)
		},
	)

	s.Next(nil)
	// Even with a correct password, a non-empty authzid must not be auto-approved.
	if _, done, err := s.Next(plainResponse("bob", "alice", "secret")); err == nil || !done {
		t.Fatalf("expected authorization failure, done=%v err=%v", done, err)
	}

	if _, _, err := s.Next(plainResponse("bob", "alice", "secret")); err != sasl.ErrUnexpectedClientResponse {
		t.Fatalf("repeated Next err = %v, want ErrUnexpectedClientResponse", err)
	}
}

func TestPlainServerWithAuthorizationInvalidResponse(t *testing.T) {
	var callbacks int
	s := sasl.NewPlainServerWithAuthorization(
		func(username, password string) error {
			callbacks++
			return nil
		},
		func(authc, authz string) error {
			callbacks++
			return nil
		},
	)

	s.Next(nil)
	if _, done, err := s.Next([]byte("nodelimiter")); err == nil || !done {
		t.Fatalf("expected invalid response error, done=%v err=%v", done, err)
	}
	if callbacks != 0 {
		t.Fatalf("callbacks invoked %d time(s) for invalid response", callbacks)
	}
}

func TestExternalServerWithAuthorizationSuccess(t *testing.T) {
	var authenticateCalled bool
	var authorized [2]string

	s := sasl.NewExternalServerWithAuthorization(
		"CN=alice",
		func() error {
			authenticateCalled = true
			return nil
		},
		func(authc, authz string) error {
			authorized = [2]string{authc, authz}
			return nil
		},
	)

	if challenge, done, err := s.Next(nil); err != nil || done || len(challenge) != 0 {
		t.Fatalf("unexpected start: challenge=%q done=%v err=%v", challenge, done, err)
	}

	// Empty authzid from the client must resolve to the injected authc.
	if challenge, done, err := s.Next([]byte{}); err != nil || !done || challenge != nil {
		t.Fatalf("unexpected result: challenge=%q done=%v err=%v", challenge, done, err)
	}
	if !authenticateCalled {
		t.Fatal("authenticator not called")
	}
	if authorized != [2]string{"CN=alice", "CN=alice"} {
		t.Errorf("authorized = %v, want [CN=alice CN=alice]", authorized)
	}
}

func TestExternalServerWithAuthorizationClientCannotOverrideAuthc(t *testing.T) {
	var seenAuthc string

	s := sasl.NewExternalServerWithAuthorization(
		"CN=alice",
		func() error { return nil },
		func(authc, authz string) error {
			seenAuthc = authc
			if authc != "CN=alice" {
				t.Errorf("authc = %q, want injected CN=alice", authc)
			}
			return nil
		},
	)

	s.Next(nil)
	// The client tries to claim a different identity; the authc stays injected.
	if _, done, err := s.Next([]byte("CN=bob")); err != nil || !done {
		t.Fatalf("unexpected result: done=%v err=%v", done, err)
	}
	if seenAuthc != "CN=alice" {
		t.Errorf("authc seen by authorizer = %q, want CN=alice", seenAuthc)
	}
}

func TestExternalServerWithAuthorizationDenied(t *testing.T) {
	s := sasl.NewExternalServerWithAuthorization(
		"CN=alice",
		func() error { return nil },
		func(authc, authz string) error {
			return errors.New("not authorized to act as " + authz)
		},
	)

	s.Next(nil)
	if _, done, err := s.Next([]byte("CN=bob")); err == nil || !done {
		t.Fatalf("expected authorization failure, done=%v err=%v", done, err)
	}

	if _, _, err := s.Next([]byte("CN=bob")); err != sasl.ErrUnexpectedClientResponse {
		t.Fatalf("repeated Next err = %v, want ErrUnexpectedClientResponse", err)
	}

}

func TestExternalServerWithAuthorizationCredentialFailure(t *testing.T) {
	var authorizeCalled bool

	s := sasl.NewExternalServerWithAuthorization(
		"CN=alice",
		func() error { return errors.New("invalid client certificate") },
		func(authc, authz string) error {
			authorizeCalled = true
			return nil
		},
	)

	s.Next(nil)
	if _, done, err := s.Next([]byte{}); err == nil || !done {
		t.Fatalf("expected credential failure, done=%v err=%v", done, err)
	}
	if authorizeCalled {
		t.Fatal("authorizer must not be consulted when authentication failed")
	}

	if _, _, err := s.Next([]byte{}); err != sasl.ErrUnexpectedClientResponse {
		t.Fatalf("repeated Next err = %v, want ErrUnexpectedClientResponse", err)
	}
}

func TestExternalServerWithAuthorizationNUL(t *testing.T) {
	var callbacks int
	s := sasl.NewExternalServerWithAuthorization(
		"CN=alice",
		func() error {
			callbacks++
			return nil
		},
		func(authc, authz string) error {
			callbacks++
			return nil
		},
	)

	s.Next(nil)
	if _, done, err := s.Next([]byte("CN=alice\x00evil")); err == nil || !done {
		t.Fatalf("expected NUL error, done=%v err=%v", done, err)
	}
	if callbacks != 0 {
		t.Fatalf("callbacks invoked %d time(s) for NUL identity", callbacks)
	}
}
