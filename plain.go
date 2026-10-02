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

// PlainCredentialAuthenticator verifies a username and password and returns
// the authentication identity associated with the credentials.
type PlainCredentialAuthenticator func(username, password string) (authcid string, err error)

// PlainAuthorizer decides whether the authenticated identity may act as the
// requested authorization identity.
type PlainAuthorizer func(authcid, authzid string) error

type separatedPlainServer struct {
	done         bool
	identity     string
	authenticate PlainCredentialAuthenticator
	authorize    PlainAuthorizer
}

func (a *separatedPlainServer) Identity() string {
	return a.identity
}

func (a *separatedPlainServer) Next(response []byte) (challenge []byte, done bool, err error) {
	if a.done {
		a.identity = ""
		return nil, false, ErrUnexpectedClientResponse
	}

	// No initial response, send an empty challenge
	if response == nil {
		return []byte{}, false, nil
	}

	a.done = true
	a.identity = ""

	parts := bytes.Split(response, []byte("\x00"))
	if len(parts) != 3 {
		return nil, true, errors.New("sasl: invalid response")
	}

	authzid := string(parts[0])
	username := string(parts[1])
	password := string(parts[2])

	authcid, err := a.authenticate(username, password)
	if err != nil {
		return nil, true, err
	}
	if authcid == "" {
		return nil, true, errors.New("sasl: authenticator returned an empty authentication identity")
	}

	if authzid == "" {
		authzid = authcid
	}

	if err := a.authorize(authcid, authzid); err != nil {
		return nil, true, err
	}

	a.identity = authzid
	return nil, true, nil
}

// NewPlainServerWithAuth creates a PLAIN server which verifies the credentials
// before authorizing the requested identity. The authorization identity is
// available from the returned server only after both steps succeed.
func NewPlainServerWithAuth(authenticate PlainCredentialAuthenticator, authorize PlainAuthorizer) IdentityServer {
	return &separatedPlainServer{
		authenticate: authenticate,
		authorize:    authorize,
	}
}
