package sasl

import (
	"bytes"
	"errors"
)

// The EXTERNAL mechanism name.
const External = "EXTERNAL"

type externalClient struct {
	Identity string
}

func (a *externalClient) Start() (mech string, ir []byte, err error) {
	mech = External
	ir = []byte(a.Identity)
	return
}

func (a *externalClient) Next(challenge []byte) (response []byte, err error) {
	return nil, ErrUnexpectedServerChallenge
}

// An implementation of the EXTERNAL authentication mechanism, as described in
// RFC 4422. Authorization identity may be left blank to indicate that the
// client is requesting to act as the identity associated with the
// authentication credentials.
func NewExternalClient(identity string) Client {
	return &externalClient{identity}
}

// ExternalAuthenticator authenticates users with the EXTERNAL mechanism. If
// the identity is left blank, it indicates that it is the same as the one used
// in the external credentials. If identity is not empty and the server doesn't
// support it, an error must be returned.
type ExternalAuthenticator func(identity string) error

// ExternalAuthentication verifies the authentication identity that the
// connection has been established with, as injected by the party that accepted
// the connection (for instance a TLS peer certificate). The identity is not
// supplied by the client and therefore cannot be spoofed by it. It must return
// an error if the external credentials are not valid.
type ExternalAuthentication func() error

// ExternalAuthorization decides whether the authenticated identity authc,
// derived from the connection credentials, is allowed to act as the requested
// authorization identity authz.
//
// An empty request from the client means it wants to act as the identity of
// the external credentials; in that case authz is resolved to authc before
// this callback is invoked. A non-empty authz is always passed to this
// callback and must be explicitly allowed.
type ExternalAuthorization func(authc, authz string) error

type externalServer struct {
	done         bool
	authenticate ExternalAuthenticator
}

func (a *externalServer) Next(response []byte) (challenge []byte, done bool, err error) {
	if a.done {
		return nil, false, ErrUnexpectedClientResponse
	}

	// No initial response, send an empty challenge
	if response == nil {
		return []byte{}, false, nil
	}

	a.done = true

	if bytes.Contains(response, []byte("\x00")) {
		return nil, false, errors.New("sasl: identity contains a NUL character")
	}

	return nil, true, a.authenticate(string(response))
}

// NewExternalServer creates a server implementation of the EXTERNAL
// authentication mechanism, as described in RFC 4422.
func NewExternalServer(authenticator ExternalAuthenticator) Server {
	return &externalServer{authenticate: authenticator}
}

type splitExternalServer struct {
	done         bool
	authc        string
	authenticate ExternalAuthentication
	authorize    ExternalAuthorization
}

func (s *splitExternalServer) Next(response []byte) (challenge []byte, done bool, err error) {
	if s.done {
		return nil, false, ErrUnexpectedClientResponse
	}

	// No initial response, send an empty challenge.
	if response == nil {
		return []byte{}, false, nil
	}

	s.done = true

	if bytes.Contains(response, []byte("\x00")) {
		return nil, true, errors.New("sasl: identity contains a NUL character")
	}

	// Stage 1: the authentication identity comes from the connection
	// credentials, never from the client response.
	if err = s.authenticate(); err != nil {
		return nil, true, err
	}

	authz := string(response)

	// An empty authorization identity maps to the authenticated identity.
	if authz == "" {
		authz = s.authc
	}

	// Stage 2: independently decide whether authc may act as authz.
	if err = s.authorize(s.authc, authz); err != nil {
		return nil, true, err
	}

	return nil, true, nil
}

// NewExternalServerWithAuthorization creates a server implementation of the
// EXTERNAL authentication mechanism, as described in RFC 4422, that separates
// authentication from authorization.
//
// authc is the authentication identity associated with the connection
// credentials (such as a TLS client certificate) and is injected by the
// server; the client cannot override it. authenticate verifies those
// credentials and establishes authc, then authorize decides whether authc may
// act as the requested authorization identity. Both stages must succeed for
// the authentication to complete.
func NewExternalServerWithAuthorization(authc string, authenticate ExternalAuthentication, authorize ExternalAuthorization) Server {
	return &splitExternalServer{
		authc:        authc,
		authenticate: authenticate,
		authorize:    authorize,
	}
}
