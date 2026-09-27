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

// AccessTokenTypeJWT is the media type RFC 9068 requires in the "typ" header
// of JWT access tokens.
//
// See: https://www.rfc-editor.org/rfc/rfc9068.html#section-2.1
const AccessTokenTypeJWT = "at+jwt"

// AccessTokenVerifier provides verification for JWT access tokens as defined
// by RFC 9068. It is intended for resource servers validating tokens presented
// by clients, and is deliberately separate from [IDTokenVerifier]: an ID Token
// that is valid for login is NOT a valid access token for a resource API.
type AccessTokenVerifier struct {
	keySet KeySet
	config *AccessTokenConfig
	issuer string
}

// AccessTokenConfig is the configuration for an AccessTokenVerifier.
type AccessTokenConfig struct {
	// ExpectedAudience is the resource indicator of the API validating the
	// token. The token's "aud" claim must contain this value.
	//
	// This field is required.
	ExpectedAudience string

	// If specified, only this set of algorithms may be used to sign the JWT.
	//
	// If the AccessTokenVerifier is created from a provider with
	// [Provider.AccessTokenVerifier], this defaults to the set of algorithms
	// the provider supports. Otherwise this value defaults to RS256.
	SupportedSigningAlgs []string

	// Time function to check Token expiry. Defaults to time.Now
	Now func() time.Time
}

// NewAccessTokenVerifier returns a verifier for RFC 9068 JWT access tokens,
// manually constructed from a key set and issuer URL.
//
// It's easier to use provider discovery to construct an AccessTokenVerifier
// than creating one directly. This method is intended to be used with
// providers that don't support metadata discovery, or to avoid round trips
// when the key set URL is already known.
func NewAccessTokenVerifier(issuerURL string, keySet KeySet, config *AccessTokenConfig) *AccessTokenVerifier {
	return &AccessTokenVerifier{keySet: keySet, config: config, issuer: issuerURL}
}

// AccessTokenVerifierContext returns an AccessTokenVerifier that uses the
// provider's key set to verify JWT access tokens. As opposed to
// [Provider.AccessTokenVerifier], the context is used to configure requests to
// the upstream JWKs endpoint. The provided context's cancellation is ignored.
func (p *Provider) AccessTokenVerifierContext(ctx context.Context, config *AccessTokenConfig) *AccessTokenVerifier {
	return p.newAccessTokenVerifier(NewRemoteKeySet(ctx, p.jwksURL), config)
}

// AccessTokenVerifier returns an AccessTokenVerifier that uses the provider's
// key set to verify JWT access tokens.
//
// The returned verifier uses a background context for all requests to the
// upstream JWKs endpoint. To control that context, use
// [Provider.AccessTokenVerifierContext] instead.
func (p *Provider) AccessTokenVerifier(config *AccessTokenConfig) *AccessTokenVerifier {
	return p.newAccessTokenVerifier(p.remoteKeySet(), config)
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

// AccessToken is a validated RFC 9068 JWT access token.
//
// The AccessToken only holds fields RFC 9068 requires. To access additional
// claims returned by the authorization server, use the Claims method.
type AccessToken struct {
	// The URL of the authorization server which issued this token.
	Issuer string

	// The resource indicator(s) this token is intended for. This package
	// ensures the audience contains the configured expected value.
	Audience []string

	// A unique string which identifies the principal the token was issued to.
	Subject string

	// The identifier of the client the token was issued to.
	ClientID string

	// The scopes granted to the client, parsed from the space-delimited
	// "scope" claim.
	Scopes []string

	// Expiry of the token. This package will not process tokens that have
	// expired.
	Expiry time.Time
	// When the token was issued by the authorization server.
	IssuedAt time.Time

	// Unique identifier of the token, suitable for replay detection.
	JWTID string

	// Raw payload of the access token.
	claims []byte
}

// Claims unmarshals the raw JSON payload of the access token into a provided
// struct.
//
//	token, err := accessTokenVerifier.Verify(rawAccessToken)
//	if err != nil {
//		// handle error
//	}
//	var claims struct {
//		Email string `json:"email"`
//	}
//	if err := token.Claims(&claims); err != nil {
//		// handle error
//	}
func (t *AccessToken) Claims(v any) error {
	if t.claims == nil {
		return errors.New("oidc: claims not set")
	}
	return json.Unmarshal(t.claims, v)
}

// accessTokenClaims mirrors the claims defined by RFC 9068. Field types are
// strict: a claim with an unexpected JSON type (e.g. a numeric "client_id" or
// an array "scope") fails to unmarshal instead of being coerced.
type accessTokenClaims struct {
	Issuer    string          `json:"iss"`
	Subject   string          `json:"sub"`
	Audience  audience        `json:"aud"`
	ClientID  string          `json:"client_id"`
	Scope     string          `json:"scope"`
	Expiry    *strictJSONTime `json:"exp"`
	IssuedAt  *strictJSONTime `json:"iat"`
	NotBefore *strictJSONTime `json:"nbf"`
	JWTID     string          `json:"jti"`
}

// strictJSONTime is a stricter variant of jsonTime for access token
// validation: the value must be a JSON number, quoted numbers are rejected.
type strictJSONTime time.Time

func (j *strictJSONTime) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return fmt.Errorf("oidc: time value must be a JSON number, got %s", string(b))
	}
	var t jsonTime
	if err := t.UnmarshalJSON(b); err != nil {
		return err
	}
	*j = strictJSONTime(t)
	return nil
}

// Verify parses a raw JWT access token, verifies it's been signed by the
// provider, and validates it according to RFC 9068: the "typ" header must be
// "at+jwt", the issuer must match, the audience must contain the configured
// resource indicator, the token must be within its validity window, and the
// claims required by RFC 9068 (iss, exp, aud, sub, client_id, iat, jti) must
// be present.
//
// See: https://www.rfc-editor.org/rfc/rfc9068.html#section-4
func (v *AccessTokenVerifier) Verify(ctx context.Context, rawAccessToken string) (*AccessToken, error) {
	if v.config.ExpectedAudience == "" {
		return nil, fmt.Errorf("oidc: invalid configuration, ExpectedAudience must be provided")
	}

	var supportedSigAlgs []jose.SignatureAlgorithm
	for _, alg := range v.config.SupportedSigningAlgs {
		supportedSigAlgs = append(supportedSigAlgs, jose.SignatureAlgorithm(alg))
	}
	if len(supportedSigAlgs) == 0 {
		// If no algorithms were specified by both the config and discovery, default
		// to the one mandatory algorithm "RS256".
		supportedSigAlgs = []jose.SignatureAlgorithm{jose.RS256}
	}

	// Parse and verify the signature first. This at least forces the client to
	// present a valid, signed access token before we do any other processing.
	jws, err := jose.ParseSigned(rawAccessToken, supportedSigAlgs)
	if err != nil {
		return nil, fmt.Errorf("oidc: malformed jwt: %v", err)
	}
	switch len(jws.Signatures) {
	case 0:
		return nil, fmt.Errorf("oidc: access token not signed")
	case 1:
	default:
		return nil, fmt.Errorf("oidc: multiple signatures on access token not supported")
	}
	sig := jws.Signatures[0]

	// RFC 9068 requires the "typ" header to be "at+jwt". The
	// "application/at+jwt" form is accepted for compatibility.
	//
	// https://www.rfc-editor.org/rfc/rfc9068.html#section-2.1
	typ, _ := sig.Header.ExtraHeaders[jose.HeaderType].(string)
	typ = strings.TrimPrefix(strings.ToLower(typ), "application/")
	if typ != AccessTokenTypeJWT {
		return nil, fmt.Errorf("oidc: invalid access token typ header %q, expected %q", sig.Header.ExtraHeaders[jose.HeaderType], AccessTokenTypeJWT)
	}

	payload, err := v.keySet.VerifySignature(ctx, rawAccessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to verify signature: %v", err)
	}

	var token accessTokenClaims
	if err := json.Unmarshal(payload, &token); err != nil {
		return nil, fmt.Errorf("oidc: failed to unmarshal claims: %v", err)
	}

	// RFC 9068 requires the iss, exp, aud, sub, client_id, iat, and jti claims.
	//
	// https://www.rfc-editor.org/rfc/rfc9068.html#section-2.2
	switch {
	case token.Issuer == "":
		return nil, fmt.Errorf("oidc: access token missing required claim %q", "iss")
	case token.Expiry == nil:
		return nil, fmt.Errorf("oidc: access token missing required claim %q", "exp")
	case len(token.Audience) == 0:
		return nil, fmt.Errorf("oidc: access token missing required claim %q", "aud")
	case token.Subject == "":
		return nil, fmt.Errorf("oidc: access token missing required claim %q", "sub")
	case token.ClientID == "":
		return nil, fmt.Errorf("oidc: access token missing required claim %q", "client_id")
	case token.IssuedAt == nil:
		return nil, fmt.Errorf("oidc: access token missing required claim %q", "iat")
	case token.JWTID == "":
		return nil, fmt.Errorf("oidc: access token missing required claim %q", "jti")
	}

	t := &AccessToken{
		Issuer:   token.Issuer,
		Audience: []string(token.Audience),
		Subject:  token.Subject,
		ClientID: token.ClientID,
		Scopes:   strings.Fields(token.Scope),
		Expiry:   time.Time(*token.Expiry),
		IssuedAt: time.Time(*token.IssuedAt),
		JWTID:    token.JWTID,
		claims:   payload,
	}

	// Check issuer.
	if t.Issuer != v.issuer {
		return nil, fmt.Errorf("oidc: access token issued by a different provider, expected %q got %q", v.issuer, t.Issuer)
	}

	// Check that the token is intended for this resource server.
	if !slices.Contains(t.Audience, v.config.ExpectedAudience) {
		return nil, fmt.Errorf("oidc: expected audience %q got %q", v.config.ExpectedAudience, t.Audience)
	}

	// Make sure the token is not expired.
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
		// https://github.com/AzureAD/azure-activedirectory-identitymodel-extensions-for-dotnet/blob/6.12.2/src/Microsoft.IdentityModel.Tokens/TokenValidationParameters.cs#L149-L153
		leeway := 5 * time.Minute

		if nowTime.Add(leeway).Before(nbfTime) {
			return nil, fmt.Errorf("oidc: current time %v before the nbf (not before) time: %v", nowTime, nbfTime)
		}
	}

	return t, nil
}
