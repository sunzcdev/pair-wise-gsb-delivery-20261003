package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// JARMConfig is the configuration for a [JARMVerifier].
type JARMConfig struct {
	// ClientID is the OAuth 2.0 client identifier that initiated the
	// authorization flow. The "aud" claim of the signed authorization
	// response must contain this value. Required.
	ClientID string

	// SupportedSigningAlgs lists the JWS algorithms accepted for the signed
	// authorization response. This is the explicit algorithm policy enforced
	// before any claim is trusted.
	//
	// If the JARMVerifier is created from a provider with
	// [Provider.JARMVerifier], this defaults to the set of algorithms the
	// provider supports. Otherwise this value defaults to RS256.
	SupportedSigningAlgs []string

	// Time function to check response expiry. Defaults to time.Now.
	Now func() time.Time
}

// JARMVerifier provides verification for JWT Secured Authorization Response
// Mode (JARM) responses, where the authorization server returns the OAuth 2.0
// authorization response parameters, such as "code" and "state", as claims of
// a signed JWT delivered through the "response" parameter of the client's
// redirect URI.
//
// A JARM response is NOT an ID Token and must not be verified with
// [IDTokenVerifier]. Use [Provider.JARMVerifier] or [NewJARMVerifier] to
// construct a dedicated verifier instead.
//
// See: https://openid.net/specs/oauth-v2-jarm.html
type JARMVerifier struct {
	// jwtVerifier enforces the signature and algorithm policy. Its claim
	// validations are disabled; JARM responses are validated by Verify and
	// are never passed through ID token validation.
	jwtVerifier *IDTokenVerifier
	issuer      string
	clientID    string
	now         func() time.Time
}

// NewJARMVerifier returns a verifier manually constructed from a key set and
// issuer URL.
//
// It's easier to use [Provider.JARMVerifier] to construct a JARMVerifier than
// creating one directly. This constructor is intended for providers that
// don't support metadata discovery, or to avoid round trips when the key set
// is already known.
func NewJARMVerifier(issuerURL string, keySet KeySet, config *JARMConfig) *JARMVerifier {
	algs := config.SupportedSigningAlgs
	if len(algs) == 0 {
		algs = []string{RS256}
	}
	return &JARMVerifier{
		jwtVerifier: NewVerifier(issuerURL, keySet, &Config{
			SupportedSigningAlgs: algs,
			SkipClientIDCheck:    true,
			SkipIssuerCheck:      true,
			SkipExpiryCheck:      true,
		}),
		issuer:   issuerURL,
		clientID: config.ClientID,
		now:      config.Now,
	}
}

// JARMVerifierContext returns a JARMVerifier that uses the provider's key set
// to verify signed authorization responses. As opposed to
// [Provider.JARMVerifier], the context is used to configure requests to the
// upstream JWKs endpoint. The provided context's cancellation is ignored.
func (p *Provider) JARMVerifierContext(ctx context.Context, config *JARMConfig) *JARMVerifier {
	return p.newJARMVerifier(NewRemoteKeySet(ctx, p.jwksURL), config)
}

// JARMVerifier returns a JARMVerifier that uses the provider's key set to
// verify signed authorization responses.
//
// The returned verifier uses a background context for all requests to the
// upstream JWKs endpoint. To control that context, use
// [Provider.JARMVerifierContext] instead.
func (p *Provider) JARMVerifier(config *JARMConfig) *JARMVerifier {
	return p.newJARMVerifier(p.remoteKeySet(), config)
}

func (p *Provider) newJARMVerifier(keySet KeySet, config *JARMConfig) *JARMVerifier {
	if len(config.SupportedSigningAlgs) == 0 && len(p.algorithms) > 0 {
		// Make a copy so we don't modify the config values.
		cp := &JARMConfig{}
		*cp = *config
		cp.SupportedSigningAlgs = p.algorithms
		config = cp
	}
	return NewJARMVerifier(p.issuer, keySet, config)
}

// AuthorizationResponse is a successfully verified JARM authorization
// response. The authorization code is only exposed through this type, after
// the response's signature, issuer, audience, expiry, and state have all been
// validated.
type AuthorizationResponse struct {
	// The authorization code returned by the authorization server, to be
	// redeemed at the token endpoint.
	Code string
	// The state value bound to the authorization request. This package
	// ensures it matches the value passed to [JARMVerifier.Verify].
	State string

	// The issuer of the response, identical to the provider's issuer URL.
	Issuer string
	// The audience of the response, containing the client ID.
	Audience []string
	// Expiry of the response.
	Expiry time.Time

	// Raw payload of the signed response.
	claims []byte
}

// Claims unmarshals the raw JSON payload of the authorization response into a
// provided struct.
func (r *AuthorizationResponse) Claims(v any) error {
	if r.claims == nil {
		return errors.New("oidc: claims not set")
	}
	return json.Unmarshal(r.claims, v)
}

// AuthorizationError is an OAuth 2.0 error response delivered through a
// successfully verified JARM response. It is returned as an error by
// [JARMVerifier.Verify] when the authorization server denied the request, and
// can be extracted with errors.As.
type AuthorizationError struct {
	// The OAuth 2.0 "error" code, such as "access_denied".
	Code string
	// The optional human readable "error_description" value.
	Description string
	// The optional "error_uri" value identifying a page with additional
	// information about the error.
	URI string
	// The state value bound to the authorization request. This package
	// ensures it matches the value passed to [JARMVerifier.Verify].
	State string
}

func (e *AuthorizationError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("oidc: authorization server returned error %q: %s", e.Code, e.Description)
	}
	return fmt.Sprintf("oidc: authorization server returned error %q", e.Code)
}

type jarmResponse struct {
	Issuer           string    `json:"iss"`
	Audience         audience  `json:"aud"`
	Expiry           *jsonTime `json:"exp"`
	NotBefore        *jsonTime `json:"nbf"`
	State            string    `json:"state"`
	Code             string    `json:"code"`
	Error            string    `json:"error"`
	ErrorDescription string    `json:"error_description"`
	ErrorURI         string    `json:"error_uri"`
}

// Verify parses a raw JARM response, the value of the "response" parameter
// received at the client's redirect URI, and verifies it against the
// provider's keys and the verifier's algorithm policy. It then validates the
// response's issuer, audience, and expiry, and requires the "state" claim to
// match expectedState, the value stored when the authorization request was
// initiated.
//
// The authorization code is never exposed before every check has passed. If
// the response carries an OAuth 2.0 error instead of a code, an
// [*AuthorizationError] is returned as the error. Responses containing
// contradictory fields, such as both "code" and "error", are rejected.
func (v *JARMVerifier) Verify(ctx context.Context, rawResponse, expectedState string) (*AuthorizationResponse, error) {
	if v.clientID == "" {
		return nil, errors.New("oidc: invalid configuration, JARMConfig.ClientID must be provided")
	}

	// Verify the signature and algorithm policy first. No claim, including
	// the authorization code, is trusted before this succeeds.
	payload, _, err := v.jwtVerifier.verifyJWT(ctx, rawResponse)
	if err != nil {
		return nil, err
	}
	var resp jarmResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("oidc: failed to unmarshal jarm claims: %v", err)
	}

	if resp.Issuer != v.issuer {
		return nil, fmt.Errorf("oidc: jarm response issued by a different provider, expected %q got %q", v.issuer, resp.Issuer)
	}
	if !slices.Contains(resp.Audience, v.clientID) {
		return nil, fmt.Errorf("oidc: jarm response expected audience %q got %q", v.clientID, []string(resp.Audience))
	}
	if resp.Expiry == nil {
		return nil, errors.New("oidc: jarm response missing exp claim")
	}

	now := time.Now
	if v.now != nil {
		now = v.now
	}
	nowTime := now()

	expiry := time.Time(*resp.Expiry)
	if expiry.Before(nowTime) {
		return nil, &TokenExpiredError{Expiry: expiry}
	}
	if resp.NotBefore != nil {
		nbfTime := time.Time(*resp.NotBefore)
		// Match the leeway used for ID tokens to deal with clock skew.
		leeway := 5 * time.Minute
		if nowTime.Add(leeway).Before(nbfTime) {
			return nil, fmt.Errorf("oidc: current time %v before the nbf (not before) time: %v", nowTime, nbfTime)
		}
	}

	if subtle.ConstantTimeCompare([]byte(resp.State), []byte(expectedState)) != 1 {
		return nil, errors.New("oidc: jarm response state does not match the state of the authorization request")
	}

	if resp.Code != "" && resp.Error != "" {
		return nil, errors.New("oidc: jarm response contains both a code and an error")
	}
	if resp.Error != "" {
		return nil, &AuthorizationError{
			Code:        resp.Error,
			Description: resp.ErrorDescription,
			URI:         resp.ErrorURI,
			State:       resp.State,
		}
	}
	if resp.Code == "" {
		return nil, errors.New("oidc: jarm response contains neither a code nor an error")
	}

	return &AuthorizationResponse{
		Code:     resp.Code,
		State:    resp.State,
		Issuer:   resp.Issuer,
		Audience: []string(resp.Audience),
		Expiry:   expiry,
		claims:   payload,
	}, nil
}
