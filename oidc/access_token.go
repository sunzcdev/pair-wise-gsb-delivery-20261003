package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// AccessTokenVerifier verifies JWT access tokens as defined by RFC 9068
// (JSON Web Token (JWT) Profile for OAuth 2.0 Access Tokens).
//
// It is intended for resource servers validating access tokens presented by
// clients. It is NOT a replacement for [IDTokenVerifier]: an ID Token that is
// valid for login is not a valid access token, and vice versa.
type AccessTokenVerifier struct {
	keySet KeySet
	config *AccessTokenConfig
	issuer string
}

// AccessTokenConfig is the configuration for an [AccessTokenVerifier].
type AccessTokenConfig struct {
	// ExpectedAudience is the set of audience values the resource server
	// accepts, typically its resource indicator. A token is accepted if its
	// "aud" claim contains at least one of these values.
	//
	// At least one value must be provided.
	ExpectedAudience []string

	// If specified, only this set of algorithms may be used to sign the JWT.
	//
	// If the AccessTokenVerifier is created from a provider with
	// [Provider.AccessTokenVerifier], this defaults to the set of algorithms
	// the provider supports. Otherwise this value defaults to RS256.
	SupportedSigningAlgs []string

	// If true, token expiry is not checked.
	SkipExpiryCheck bool

	// SkipIssuerCheck is intended for specialized cases where the caller wishes
	// to defer issuer validation. When enabled, callers MUST independently
	// verify the token's Issuer is a known good value.
	SkipIssuerCheck bool

	// Time function to check token expiry. Defaults to time.Now.
	Now func() time.Time
}

// NewAccessTokenVerifier returns an [AccessTokenVerifier] manually constructed
// from an issuer URL and key set.
//
// It's easier to use [Provider.AccessTokenVerifier] for providers that support
// discovery. This constructor is intended for providers that don't, or to
// avoid round trips when the key set is already known.
func NewAccessTokenVerifier(issuerURL string, keySet KeySet, config *AccessTokenConfig) *AccessTokenVerifier {
	return &AccessTokenVerifier{keySet: keySet, config: config, issuer: issuerURL}
}

// AccessTokenVerifier returns an [AccessTokenVerifier] that uses the
// provider's key set to verify JWT access tokens.
//
// The returned verifier uses a background context for all requests to the
// upstream JWKs endpoint. To control that context, use
// [Provider.AccessTokenVerifierContext] instead.
func (p *Provider) AccessTokenVerifier(config *AccessTokenConfig) *AccessTokenVerifier {
	return p.newAccessTokenVerifier(p.remoteKeySet(), config)
}

// AccessTokenVerifierContext returns an [AccessTokenVerifier] that uses the
// provider's key set to verify JWT access tokens. As opposed to
// [Provider.AccessTokenVerifier], the context is used to configure requests to
// the upstream JWKs endpoint. The provided context's cancellation is ignored.
func (p *Provider) AccessTokenVerifierContext(ctx context.Context, config *AccessTokenConfig) *AccessTokenVerifier {
	return p.newAccessTokenVerifier(NewRemoteKeySet(ctx, p.jwksURL), config)
}

func (p *Provider) newAccessTokenVerifier(keySet KeySet, config *AccessTokenConfig) *AccessTokenVerifier {
	if len(config.SupportedSigningAlgs) == 0 && len(p.algorithms) > 0 {
		// Make a copy so we don't modify the config values.
		cp := &AccessTokenConfig{}
		*cp = *config
		cp.SupportedSigningAlgs = p.algorithms
		config = cp
	}
	return NewAccessTokenVerifier(p.issuer, keySet, config)
}

// AccessToken is a verified JWT access token as defined by RFC 9068.
//
// The AccessToken only holds fields RFC 9068 requires. To access additional
// claims returned by the authorization server, use the Claims method.
type AccessToken struct {
	// The URL of the authorization server which issued this token.
	Issuer string

	// The audience, or set of audiences, this token is intended for. This
	// package ensures the audience contains an expected value.
	Audience []string

	// A unique identifier for the user or client this token was issued to.
	Subject string

	// Expiry of the token. This package will not process tokens that have
	// expired unless that validation is explicitly turned off.
	Expiry time.Time
	// When the token was issued by the authorization server.
	IssuedAt time.Time

	// The OAuth 2.0 client this token was issued to.
	ClientID string

	// Unique identifier of the token, usable for replay detection.
	JWTID string

	// Scopes granted to this token, parsed from the space-delimited "scope"
	// claim. Empty if the token carries no "scope" claim.
	Scopes []string

	// Raw payload of the access token.
	claims []byte
}

// Claims unmarshals the raw JSON payload of the access token into a provided
// struct.
//
//	accessToken, err := accessTokenVerifier.Verify(ctx, rawAccessToken)
//	if err != nil {
//		// handle error
//	}
//	var claims struct {
//		Email string `json:"email"`
//	}
//	if err := accessToken.Claims(&claims); err != nil {
//		// handle error
//	}
func (t *AccessToken) Claims(v any) error {
	if t.claims == nil {
		return errors.New("oidc: claims not set")
	}
	return json.Unmarshal(t.claims, v)
}

// accessTokenClaims mirrors the claim layout required by RFC 9068. Required
// claims use pointers so their presence can be validated, and so that claims
// with unexpected JSON types (e.g. a numeric "client_id") fail to unmarshal
// instead of being silently coerced.
type accessTokenClaims struct {
	Issuer    *string      `json:"iss"`
	Subject   *string      `json:"sub"`
	Audience  *audience    `json:"aud"`
	Expiry    *numericDate `json:"exp"`
	IssuedAt  *numericDate `json:"iat"`
	NotBefore *numericDate `json:"nbf"`
	ClientID  *string      `json:"client_id"`
	JWTID     *string      `json:"jti"`
	Scope     *string      `json:"scope"`
}

// numericDate is a JSON NumericDate as defined by RFC 7519. Unlike jsonTime,
// it rejects values that aren't JSON numbers, such as strings containing
// digits.
type numericDate time.Time

func (j *numericDate) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || b[0] == '"' {
		return fmt.Errorf("oidc: invalid NumericDate %s", b)
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	var unix int64

	if t, err := n.Int64(); err == nil {
		unix = t
	} else {
		f, err := n.Float64()
		if err != nil {
			return err
		}
		unix = int64(f)
	}
	*j = numericDate(time.Unix(unix, 0))
	return nil
}

// Verify parses a raw JWT access token, verifies it's been signed by the
// provider, and validates it against the RFC 9068 profile: the "typ" header
// must be "at+jwt", the issuer, audience, and expiry must be valid, and the
// claims required by RFC 9068 (iss, exp, aud, sub, client_id, iat, jti) must
// be present.
//
// See: https://www.rfc-editor.org/rfc/rfc9068.html#section-4
func (v *AccessTokenVerifier) Verify(ctx context.Context, rawAccessToken string) (*AccessToken, error) {
	if len(v.config.ExpectedAudience) == 0 {
		return nil, fmt.Errorf("oidc: invalid configuration, expected audience must be provided")
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

	payload, sig, err := parseAndVerifyJWT(ctx, v.keySet, rawAccessToken, supportedSigAlgs, false)
	if err != nil {
		return nil, err
	}

	// RFC 9068 requires the "typ" header to be "at+jwt". This distinguishes
	// access tokens from other JWTs (such as ID Tokens) issued by the same
	// authorization server.
	//
	// https://www.rfc-editor.org/rfc/rfc9068.html#section-2.1
	typ, ok := sig.Header.ExtraHeaders[jose.HeaderType].(string)
	if !ok {
		return nil, fmt.Errorf("oidc: access token missing \"typ\" header")
	}
	if mediaType := strings.ToLower(typ); mediaType != "at+jwt" && mediaType != "application/at+jwt" {
		return nil, fmt.Errorf("oidc: unexpected \"typ\" header %q, expected \"at+jwt\"", typ)
	}

	var token accessTokenClaims
	if err := json.Unmarshal(payload, &token); err != nil {
		return nil, fmt.Errorf("oidc: failed to unmarshal claims: %v", err)
	}

	// The claims required by RFC 9068 must be present.
	//
	// https://www.rfc-editor.org/rfc/rfc9068.html#section-2.2
	for _, required := range []struct {
		name    string
		present bool
	}{
		{"iss", token.Issuer != nil},
		{"exp", token.Expiry != nil},
		{"aud", token.Audience != nil},
		{"sub", token.Subject != nil},
		{"client_id", token.ClientID != nil},
		{"iat", token.IssuedAt != nil},
		{"jti", token.JWTID != nil},
	} {
		if !required.present {
			return nil, fmt.Errorf("oidc: access token missing required claim %q", required.name)
		}
	}

	t := &AccessToken{
		Issuer:   *token.Issuer,
		Subject:  *token.Subject,
		Audience: []string(*token.Audience),
		Expiry:   time.Time(*token.Expiry),
		IssuedAt: time.Time(*token.IssuedAt),
		ClientID: *token.ClientID,
		JWTID:    *token.JWTID,
		claims:   payload,
	}
	if token.Scope != nil {
		t.Scopes = strings.Fields(*token.Scope)
	}

	// Check issuer.
	if !v.config.SkipIssuerCheck && t.Issuer != v.issuer {
		return nil, fmt.Errorf("oidc: access token issued by a different provider, expected %q got %q", v.issuer, t.Issuer)
	}

	// Check that the token is intended for this resource server.
	if !slices.ContainsFunc(v.config.ExpectedAudience, func(expected string) bool {
		return slices.Contains(t.Audience, expected)
	}) {
		return nil, fmt.Errorf("oidc: expected audience %q got %q", v.config.ExpectedAudience, t.Audience)
	}

	// If a SkipExpiryCheck is false, make sure token is not expired.
	if !v.config.SkipExpiryCheck {
		now := time.Now
		if v.config.Now != nil {
			now = v.config.Now
		}
		nowTime := now()

		if t.Expiry.Before(nowTime) {
			return nil, &TokenExpiredError{Expiry: t.Expiry}
		}

		// If nbf claim is provided in token, ensure that it is indeed in the past.
		if token.NotBefore != nil {
			nbfTime := time.Time(*token.NotBefore)
			// Set to 5 minutes since this is what other OpenID Connect providers do to deal with clock skew.
			leeway := 5 * time.Minute

			if nowTime.Add(leeway).Before(nbfTime) {
				return nil, fmt.Errorf("oidc: current time %v before the nbf (not before) time: %v", nowTime, nbfTime)
			}
		}
	}

	return t, nil
}
