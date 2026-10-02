package sasl

import (
	"encoding/json"
	"errors"
	"fmt"
)

// The OAUTHBEARER mechanism name.
const OAuthBearer = "OAUTHBEARER"

type OAuthBearerError struct {
	Status  string `json:"status"`
	Schemes string `json:"schemes"`
	Scope   string `json:"scope"`
}

type OAuthBearerOptions struct {
	Username string
	Token    string
	Host     string
	Port     int
}

// Implements error
func (err *OAuthBearerError) Error() string {
	return fmt.Sprintf("OAUTHBEARER authentication error (%v)", err.Status)
}

type oauthBearerClient struct {
	OAuthBearerOptions
}

func (a *oauthBearerClient) Start() (mech string, ir []byte, err error) {
	ir, err = encodeOAuthBearerInitialResponse(a.OAuthBearerOptions)
	return OAuthBearer, ir, err
}

func (a *oauthBearerClient) Next(challenge []byte) ([]byte, error) {
	authBearerErr := &OAuthBearerError{}
	if err := json.Unmarshal(challenge, authBearerErr); err != nil {
		return nil, err
	} else {
		return nil, authBearerErr
	}
}

// An implementation of the OAUTHBEARER authentication mechanism, as
// described in RFC 7628.
func NewOAuthBearerClient(opt *OAuthBearerOptions) Client {
	return &oauthBearerClient{*opt}
}

type OAuthBearerAuthenticator func(opts OAuthBearerOptions) *OAuthBearerError

type oauthBearerServer struct {
	done         bool
	failErr      error
	strict       bool
	authenticate OAuthBearerAuthenticator
}

// ErrInvalidClientResponse is returned when the client fails to send the
// single 0x01 confirmation defined by RFC 7628 section 3.2.2 after a
// failure challenge.
var ErrInvalidClientResponse = errors.New("sasl: invalid response")

func (a *oauthBearerServer) fail(descr string) ([]byte, bool, error) {
	blob, err := json.Marshal(OAuthBearerError{
		Status:  "invalid_request",
		Schemes: "bearer",
	})
	if err != nil {
		panic(err) // wtf
	}
	a.failErr = errors.New("sasl: client error: " + descr)
	return blob, false, nil
}

func (a *oauthBearerServer) Next(response []byte) (challenge []byte, done bool, err error) {
	// Per RFC, we cannot just send an error, we need to return JSON-structured
	// value as a challenge and then after getting dummy response from the
	// client stop the exchange.
	if a.failErr != nil {
		// Server libraries (go-smtp, go-imap) will not call Next on
		// protocol-specific SASL cancel response ('*'). However, GS2 (and
		// indirectly OAUTHBEARER) defines a protocol-independent way to do so
		// using a single 0x01 byte. RFC 7628 section 3.2.2: after a JSON
		// error challenge the client MUST answer with exactly that one
		// byte; nil, an empty slice or any longer input is invalid. The
		// length is checked first so an empty slice cannot panic, and the
		// authenticator is never invoked again.
		if len(response) != 1 || response[0] != oauthBearerSep {
			return nil, true, ErrInvalidClientResponse
		}
		return nil, true, a.failErr
	}

	if a.done {
		err = ErrUnexpectedClientResponse
		return
	}

	// Generate empty challenge.
	if response == nil {
		return []byte{}, false, nil
	}

	a.done = true

	opts, parseErr := parseOAuthBearerInitialResponse(response, a.strict)
	if parseErr != nil {
		return a.fail(parseErr.Error())
	}

	authzErr := a.authenticate(opts)
	if authzErr != nil {
		blob, err := json.Marshal(authzErr)
		if err != nil {
			panic(err) // wtf
		}
		a.failErr = authzErr
		return blob, false, nil
	}

	return nil, true, nil
}

// NewOAuthBearerServer returns a server with the lenient, backwards
// compatible client message parser: it accepts the historical field
// layout and keeps accepting well-formed messages produced by older
// clients.
func NewOAuthBearerServer(auth OAuthBearerAuthenticator) Server {
	return &oauthBearerServer{authenticate: auth}
}

// NewOAuthBearerServerStrict returns a server that strictly enforces
// RFC 7628: the GS2 authzid MUST use the RFC 5801 =2C/=3D escaping,
// parameter fields MUST be exactly bounded by 0x01 separators and end
// with 0x01 0x01, the "auth" parameter is required, and duplicate
// "host", "port" or "auth" parameters are rejected. Malformed messages
// produce an "invalid_request" challenge without calling the
// authenticator.
func NewOAuthBearerServerStrict(auth OAuthBearerAuthenticator) Server {
	return &oauthBearerServer{authenticate: auth, strict: true}
}
