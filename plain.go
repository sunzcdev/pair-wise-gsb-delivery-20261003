package sasl

import (
	"bytes"
	"errors"
)

// The PLAIN mechanism name.
const Plain = "PLAIN"

type plainClient struct {
	Identity string
	Username string
	Password string
}

func (a *plainClient) Start() (mech string, ir []byte, err error) {
	mech = "PLAIN"
	ir = []byte(a.Identity + "\x00" + a.Username + "\x00" + a.Password)
	return
}

func (a *plainClient) Next(challenge []byte) (response []byte, err error) {
	return nil, ErrUnexpectedServerChallenge
}

// A client implementation of the PLAIN authentication mechanism, as described
// in RFC 4616. Authorization identity may be left blank to indicate that it is
// the same as the username.
func NewPlainClient(identity, username, password string) Client {
	return &plainClient{identity, username, password}
}

// Authenticates users with an identity, a username and a password. If the
// identity is left blank, it indicates that it is the same as the username.
// If identity is not empty and the server doesn't support it, an error must be
// returned.
type PlainAuthenticator func(identity, username, password string) error

// PlainAuthentication verifies the credentials supplied by the client. The
// authenticated identity is the username as established by the credentials. It
// must return an error if the username or the password is invalid.
//
// This is the first stage of a two-phase server: a correct password only
// establishes the authenticated identity, it never authorizes the client to
// act on behalf of another identity.
type PlainAuthentication func(username, password string) error

// PlainAuthorization decides whether the authenticated identity authc is
// allowed to act as the requested authorization identity authz.
//
// The client requests an authorization identity by sending it explicitly; an
// empty request means the client wants to act as itself, in which case authz
// is resolved to authc before this callback is invoked. A correct password
// must never be enough on its own to authorize a non-empty authz: this
// callback is always consulted and must return an error to deny the request.
type PlainAuthorization func(authc, authz string) error

type plainServer struct {
	done         bool
	authenticate PlainAuthenticator
}

func (a *plainServer) Next(response []byte) (challenge []byte, done bool, err error) {
	if a.done {
		err = ErrUnexpectedClientResponse
		return
	}

	// No initial response, send an empty challenge
	if response == nil {
		return []byte{}, false, nil
	}

	a.done = true

	parts := bytes.Split(response, []byte("\x00"))
	if len(parts) != 3 {
		err = errors.New("sasl: invalid response")
		return
	}

	identity := string(parts[0])
	username := string(parts[1])
	password := string(parts[2])

	err = a.authenticate(identity, username, password)
	done = true
	return
}

// A server implementation of the PLAIN authentication mechanism, as described
// in RFC 4616.
func NewPlainServer(authenticator PlainAuthenticator) Server {
	return &plainServer{authenticate: authenticator}
}

type splitPlainServer struct {
	done         bool
	authenticate PlainAuthentication
	authorize    PlainAuthorization
}

func (s *splitPlainServer) Next(response []byte) (challenge []byte, done bool, err error) {
	if s.done {
		return nil, false, ErrUnexpectedClientResponse
	}

	// No initial response, send an empty challenge.
	if response == nil {
		return []byte{}, false, nil
	}

	s.done = true

	parts := bytes.Split(response, []byte("\x00"))
	if len(parts) != 3 {
		return nil, true, errors.New("sasl: invalid response")
	}

	authz := string(parts[0])
	authc := string(parts[1])
	password := string(parts[2])

	// Stage 1: establish the authenticated identity from the credentials.
	if err = s.authenticate(authc, password); err != nil {
		return nil, true, err
	}

	// An empty authorization identity maps to the authenticated identity.
	if authz == "" {
		authz = authc
	}

	// Stage 2: independently decide whether authc may act as authz.
	if err = s.authorize(authc, authz); err != nil {
		return nil, true, err
	}

	return nil, true, nil
}

// NewPlainServerWithAuthorization creates a server implementation of the
// PLAIN authentication mechanism, as described in RFC 4616, that separates
// authentication from authorization.
//
// authenticate verifies the username and password and establishes the
// authenticated identity; authorize then decides whether that identity may act
// as the requested authorization identity. Both stages must succeed for the
// authentication to complete.
func NewPlainServerWithAuthorization(authenticate PlainAuthentication, authorize PlainAuthorization) Server {
	return &splitPlainServer{
		authenticate: authenticate,
		authorize:    authorize,
	}
}
