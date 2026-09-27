package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// JARMConfig is the configuration for a JARMVerifier.
type JARMConfig struct {
	// ClientID is the expected audience of the authorization response. This
	// is the ID of the client that initiated the authorization request and is
	// required.
	ClientID string

	// SupportedSigningAlgs is the explicit set of algorithms that may sign
	// authorization responses. Responses signed with any other algorithm,
	// including "none", are rejected.
	//
	// If the JARMVerifier is created from a provider with
	// [Provider.JARMVerifier] and this value is empty, it defaults to the set
	// of algorithms the provider supports. Otherwise this value defaults to
	// RS256.
	SupportedSigningAlgs []string

	// Time function to check response expiry. Defaults to time.Now.
	Now func() time.Time
}

// JARMVerifier verifies JWT Secured Authorization Response Mode (JARM)
// responses, where the authorization server returns the OAuth 2.0
// authorization response parameters (such as "code" and "state") as claims of
// a signed JWT delivered in the "response" parameter of the callback.
//
// A JARM response is NOT an ID Token. It carries no identity claims and must
// not be processed by an [IDTokenVerifier].
//
// See: https://openid.net/specs/oauth-v2-jarm.html
type JARMVerifier struct {
	keySet KeySet
	issuer string
	config *JARMConfig
}

// NewJARMVerifier returns a JARMVerifier manually constructed from a key set
// and issuer URL.
//
// It's easier to use [Provider.JARMVerifier] to construct a verifier through
// provider discovery. This constructor is intended for providers that don't
// support metadata discovery, or to avoid round trips when the key set is
// already known.
func NewJARMVerifier(issuerURL string, keySet KeySet, config *JARMConfig) *JARMVerifier {
	return &JARMVerifier{keySet: keySet, issuer: issuerURL, config: config}
}

// JARMVerifier returns a JARMVerifier that uses the provider's key set to
// verify JARM responses.
//
// The returned verifier uses a background context for all requests to the
// upstream JWKs endpoint. To control that context, use
// [Provider.JARMVerifierContext] instead.
func (p *Provider) JARMVerifier(config *JARMConfig) *JARMVerifier {
	return p.newJARMVerifier(p.remoteKeySet(), config)
}

// JARMVerifierContext returns a JARMVerifier that uses the provider's key set
// to verify JARM responses. As opposed to [Provider.JARMVerifier], the
// context is used to configure requests to the upstream JWKs endpoint. The
// provided context's cancellation is ignored.
func (p *Provider) JARMVerifierContext(ctx context.Context, config *JARMConfig) *JARMVerifier {
	return p.newJARMVerifier(NewRemoteKeySet(ctx, p.jwksURL), config)
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

// JARMError is an OAuth 2.0 error response conveyed by a verified JARM
// response, such as "access_denied" when the user declines consent.
//
// See: https://datatracker.ietf.org/doc/html/rfc6749#section-4.1.2.1
type JARMError struct {
	// Code is the OAuth 2.0 "error" value, for example "invalid_request" or
	// "access_denied".
	Code string
	// Description is the optional human readable "error_description" value.
	Description string
	// URI is the optional "error_uri" value identifying a web page with
	// additional information about the error.
	URI string
}

func (e *JARMError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("oidc: authorization server returned error %q: %s", e.Code, e.Description)
	}
	return fmt.Sprintf("oidc: authorization server returned error %q", e.Code)
}

// AuthorizationResponse is a verified JARM authorization response.
//
// Exactly one of Code or Error is set. Code is only populated after the
// response signature, issuer, audience, expiry, and state have all been
// validated.
type AuthorizationResponse struct {
	// The issuer of the response, identical to the provider's issuer URL.
	Issuer string
	// The audience of the response, which contains the client ID.
	Audience []string
	// Expiry of the response.
	Expiry time.Time
	// When the response was issued by the provider, if indicated.
	IssuedAt time.Time
	// State is the "state" value bound to the authorization request. The
	// verifier guarantees it matches the value passed to Verify.
	State string

	// Code is the authorization code to exchange at the token endpoint. Only
	// set for successful responses, when Error is nil.
	Code string
	// Error is the OAuth 2.0 error returned by the authorization server, if
	// the authorization request failed.
	Error *JARMError

	// Raw payload of the response.
	claims []byte
}

// Claims unmarshals the raw JSON payload of the authorization response into
// the provided struct, for accessing response parameters not exposed directly.
func (r *AuthorizationResponse) Claims(v any) error {
	if r.claims == nil {
		return errors.New("oidc: claims not set")
	}
	return json.Unmarshal(r.claims, v)
}

type jarmClaims struct {
	Issuer   string    `json:"iss"`
	Audience audience  `json:"aud"`
	Expiry   *jsonTime `json:"exp"`
	IssuedAt jsonTime  `json:"iat"`

	State string `json:"state"`
	Code  string `json:"code"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	ErrorURI         string `json:"error_uri"`
}

// Verify parses a raw JARM response, verifies it has been signed by the
// provider with an allowed algorithm, and validates the issuer, audience,
// expiry, and state claims.
//
// The expectedState argument MUST be the "state" value saved when the
// authorization request was initiated. Responses whose state doesn't match
// are rejected.
//
// The signature is verified before any response parameter, such as the
// authorization code, is exposed to the caller.
//
//	rawResponse := r.URL.Query().Get("response")
//	authResp, err := jarmVerifier.Verify(ctx, rawResponse, stateFromCookie)
//	if err != nil {
//		// handle invalid response
//	}
//	if authResp.Error != nil {
//		// handle OAuth 2.0 error, e.g. "access_denied"
//	}
//	oauth2Token, err := oauth2Config.Exchange(ctx, authResp.Code)
func (v *JARMVerifier) Verify(ctx context.Context, rawResponse, expectedState string) (*AuthorizationResponse, error) {
	if v.config.ClientID == "" {
		return nil, errors.New("oidc: invalid configuration, JARMConfig.ClientID must be provided")
	}

	var supportedSigAlgs []jose.SignatureAlgorithm
	for _, alg := range v.config.SupportedSigningAlgs {
		supportedSigAlgs = append(supportedSigAlgs, jose.SignatureAlgorithm(alg))
	}
	if len(supportedSigAlgs) == 0 {
		// If no algorithms were specified by both the config and discovery,
		// default to the one mandatory algorithm "RS256".
		supportedSigAlgs = []jose.SignatureAlgorithm{jose.RS256}
	}

	// Parse and verify the signature first. No claim is exposed to the
	// caller before the response is authenticated.
	jws, err := jose.ParseSigned(rawResponse, supportedSigAlgs)
	if err != nil {
		return nil, fmt.Errorf("oidc: malformed jarm response: %v", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, fmt.Errorf("oidc: jarm response must contain exactly one signature, got %d", len(jws.Signatures))
	}
	payload, err := v.keySet.VerifySignature(ctx, rawResponse)
	if err != nil {
		return nil, fmt.Errorf("oidc: failed to verify jarm response signature: %v", err)
	}

	var claims jarmClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("oidc: failed to unmarshal jarm response claims: %v", err)
	}

	if claims.Issuer != v.issuer {
		return nil, fmt.Errorf("oidc: jarm response issued by a different provider, expected %q got %q", v.issuer, claims.Issuer)
	}

	if !slices.Contains([]string(claims.Audience), v.config.ClientID) {
		return nil, fmt.Errorf("oidc: jarm response expected audience %q got %q", v.config.ClientID, []string(claims.Audience))
	}

	if claims.Expiry == nil {
		return nil, errors.New("oidc: jarm response missing expiry (exp) claim")
	}
	now := time.Now
	if v.config.Now != nil {
		now = v.config.Now
	}
	expiry := time.Time(*claims.Expiry)
	if expiry.Before(now()) {
		return nil, &TokenExpiredError{Expiry: expiry}
	}

	if claims.State == "" {
		return nil, errors.New("oidc: jarm response missing state")
	}
	if subtle.ConstantTimeCompare([]byte(claims.State), []byte(expectedState)) != 1 {
		return nil, errors.New("oidc: jarm response state did not match expected state")
	}

	// A response is either a successful authorization response or an OAuth
	// 2.0 error response. Responses carrying both or neither are invalid.
	if claims.Code != "" && claims.Error != "" {
		return nil, errors.New("oidc: jarm response contains both code and error")
	}

	resp := &AuthorizationResponse{
		Issuer:   claims.Issuer,
		Audience: []string(claims.Audience),
		Expiry:   expiry,
		IssuedAt: time.Time(claims.IssuedAt),
		State:    claims.State,
		claims:   payload,
	}
	switch {
	case claims.Error != "":
		resp.Error = &JARMError{
			Code:        claims.Error,
			Description: claims.ErrorDescription,
			URI:         claims.ErrorURI,
		}
	case claims.Code != "":
		resp.Code = claims.Code
	default:
		return nil, errors.New("oidc: jarm response contains neither code nor error")
	}
	return resp, nil
}
