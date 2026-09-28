package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

const (
	issuerGoogleAccounts         = "https://accounts.google.com"
	issuerGoogleAccountsNoScheme = "accounts.google.com"
)

// TokenExpiredError indicates that [IDTokenVerifier.Verify] or
// [IDTokenVerifier.VerifyLogout] failed because the token was expired. This
// error does NOT indicate that the token is not also invalid for other reasons.
// Other checks might have failed if the expiration check had not failed.
type TokenExpiredError struct {
	// Expiry is the time when the token expired.
	Expiry time.Time
}

func (e *TokenExpiredError) Error() string {
	return fmt.Sprintf("oidc: token is expired (Token Expiry: %v)", e.Expiry)
}

// ClaimSourceError indicates that [IDTokenVerifier.Verify] failed to resolve a
// JWT ("aggregated claims") entry in the ID Token's "_claim_sources" member.
type ClaimSourceError struct {
	// Source is the name of the claim source in "_claim_sources".
	Source string
	// Claims are the claim names in "_claim_names" that reference the source
	// and are therefore affected by the failure.
	Claims []string
	// Err is the underlying failure.
	Err error
}

func (e *ClaimSourceError) Error() string {
	return fmt.Sprintf("oidc: failed to resolve claims %q from JWT claim source %q: %v",
		strings.Join(e.Claims, ", "), e.Source, e.Err)
}

func (e *ClaimSourceError) Unwrap() error {
	return e.Err
}

// KeySet is a set of public JSON Web Keys that can be used to validate the signature
// of JSON web tokens. This is expected to be backed by a remote key set through
// provider metadata discovery or an in-memory set of keys delivered out-of-band.
type KeySet interface {
	// VerifySignature parses the JSON web token, verifies the signature, and returns
	// the raw payload. Header and claim fields are validated by other parts of the
	// package. For example, the KeySet does not need to check values such as signature
	// algorithm, issuer, and audience since the IDTokenVerifier validates these values
	// independently.
	//
	// If VerifySignature makes HTTP requests to verify the token, it's expected to
	// use any HTTP client associated with the context through ClientContext.
	VerifySignature(ctx context.Context, jwt string) (payload []byte, err error)
}

// IDTokenVerifier provides verification for ID Tokens and Logout Tokens.
type IDTokenVerifier struct {
	keySet KeySet
	config *Config
	issuer string
}

// NewVerifier returns a verifier manually constructed from a key set and issuer URL.
//
// It's easier to use provider discovery to construct an IDTokenVerifier than creating
// one directly. This method is intended to be used with providers that don't support
// metadata discovery, or to avoid round trips when the key set URL is already known.
//
// This constructor can be used to create a verifier directly using the issuer URL and
// JSON Web Key Set URL without using discovery:
//
//	keySet := oidc.NewRemoteKeySet(ctx, "https://www.googleapis.com/oauth2/v3/certs")
//	verifier := oidc.NewVerifier("https://accounts.google.com", keySet, config)
//
// Or a static key set (e.g. for testing):
//
//	keySet := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{pub1, pub2}}
//	verifier := oidc.NewVerifier("https://accounts.google.com", keySet, config)
func NewVerifier(issuerURL string, keySet KeySet, config *Config) *IDTokenVerifier {
	return &IDTokenVerifier{keySet: keySet, config: config, issuer: issuerURL}
}

// Config is the configuration for an IDTokenVerifier.
type Config struct {
	// Expected audience of the token. For a majority of the cases this is expected to be
	// the ID of the client that initialized the login flow. It may occasionally differ if
	// the provider supports the authorizing party (azp) claim.
	//
	// If not provided, users must explicitly set SkipClientIDCheck.
	ClientID string
	// If specified, only this set of algorithms may be used to sign the JWT.
	//
	// If the IDTokenVerifier is created from a provider with [Provider.Verifier], this
	// defaults to the set of algorithms the provider supports. Otherwise this value
	// defaults to RS256.
	SupportedSigningAlgs []string

	// If true, no ClientID check performed. Must be true if ClientID field is empty.
	SkipClientIDCheck bool
	// If true, token expiry is not checked.
	SkipExpiryCheck bool

	// SkipIssuerCheck is intended for specialized cases where the caller wishes to
	// defer issuer validation. When enabled, callers MUST independently verify the Token's
	// Issuer is a known good value.
	//
	// Mismatched issuers often indicate client mis-configuration. If mismatches are
	// unexpected, evaluate if the provided issuer URL is incorrect instead of enabling
	// this option.
	SkipIssuerCheck bool

	// Time function to check Token expiry. Defaults to time.Now
	Now func() time.Time

	// InsecureSkipSignatureCheck causes this package to skip JWT signature validation.
	// It's intended for special cases where providers (such as Azure), use the "none"
	// algorithm.
	//
	// This option can only be enabled safely when the ID Token is received directly
	// from the provider after the token exchange.
	//
	// This option MUST NOT be used when receiving an ID Token from sources other
	// than the token endpoint.
	InsecureSkipSignatureCheck bool

	// ClaimSourceVerifiers maps names in the ID Token's "_claim_sources" member
	// to verifiers for JWT ("aggregated claims") claim sources. Callers are
	// responsible for configuring each verifier with the trusted issuer and
	// keys of the claims provider.
	//
	// Claims from a verified JWT source are merged into the ID Token's claims,
	// but only the claims explicitly named by the ID Token's "_claim_names"
	// member are accepted. Other claims in the source JWT, such as "iss" or
	// "sub", never override the ID Token's own claims.
	//
	// A JWT claim source without a configured verifier causes Verify to fail.
	ClaimSourceVerifiers map[string]*IDTokenVerifier
}

// VerifierContext returns an IDTokenVerifier that uses the provider's key set to
// verify JWTs. As opposed to [Provider.Verifier], the context is used to configure
// requests to the upstream JWKs endpoint. The provided context's cancellation is
// ignored.
func (p *Provider) VerifierContext(ctx context.Context, config *Config) *IDTokenVerifier {
	return p.newVerifier(NewRemoteKeySet(ctx, p.jwksURL), config)
}

// Verifier returns an IDTokenVerifier that uses the provider's key set to verify JWTs.
//
// The returned verifier uses a background context for all requests to the upstream
// JWKs endpoint. To control that context, use [Provider.VerifierContext] instead.
func (p *Provider) Verifier(config *Config) *IDTokenVerifier {
	return p.newVerifier(p.remoteKeySet(), config)
}

func (p *Provider) newVerifier(keySet KeySet, config *Config) *IDTokenVerifier {
	if len(config.SupportedSigningAlgs) == 0 && len(p.algorithms) > 0 {
		// Make a copy so we don't modify the config values.
		cp := &Config{}
		*cp = *config
		cp.SupportedSigningAlgs = p.algorithms
		config = cp
	}
	return NewVerifier(p.issuer, keySet, config)
}

// Returns the Claims from the distributed JWT token
func resolveDistributedClaim(ctx context.Context, verifier *IDTokenVerifier, src claimSource) ([]byte, error) {
	req, err := http.NewRequest("GET", src.Endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("malformed request: %v", err)
	}
	if src.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+src.AccessToken)
	}

	resp, err := doRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("oidc: Request to endpoint failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("unable to read response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc: request failed: %v", resp.StatusCode)
	}

	token, err := verifier.Verify(ctx, string(body))
	if err != nil {
		return nil, fmt.Errorf("malformed response body: %v", err)
	}

	return token.claims, nil
}

// resolveAggregatedClaims verifies the JWT entries in the ID Token's
// "_claim_sources" member and merges the claims referenced by "_claim_names"
// into the token's claims. Each source is verified at most once, no matter how
// many claim names reference it. Only claims explicitly named by "_claim_names"
// are merged; all other claims in the source JWT, including "iss" and "sub",
// are ignored.
func (v *IDTokenVerifier) resolveAggregatedClaims(ctx context.Context, t *IDToken, token *idToken) error {
	// Group the claim names in "_claim_names" by JWT claim source.
	claimsBySource := make(map[string][]string)
	for claimName, sourceName := range token.ClaimNames {
		src, ok := token.ClaimSources[sourceName]
		if !ok || src.JWT == "" {
			// Unknown sources are rejected by Verify. Endpoint sources are
			// resolved through resolveDistributedClaim instead.
			continue
		}
		claimsBySource[sourceName] = append(claimsBySource[sourceName], claimName)
	}
	if len(claimsBySource) == 0 {
		return nil
	}

	merged := make(map[string]json.RawMessage)
	if err := json.Unmarshal(t.claims, &merged); err != nil {
		return fmt.Errorf("oidc: failed to unmarshal claims: %v", err)
	}

	sourceNames := make([]string, 0, len(claimsBySource))
	for sourceName := range claimsBySource {
		sourceNames = append(sourceNames, sourceName)
	}
	slices.Sort(sourceNames)

	for _, sourceName := range sourceNames {
		claimNames := claimsBySource[sourceName]
		slices.Sort(claimNames)

		verifier, ok := v.config.ClaimSourceVerifiers[sourceName]
		if !ok {
			return &ClaimSourceError{
				Source: sourceName,
				Claims: claimNames,
				Err:    errors.New("no verifier configured for JWT claim source"),
			}
		}

		srcToken, err := verifier.Verify(ctx, token.ClaimSources[sourceName].JWT)
		if err != nil {
			return &ClaimSourceError{Source: sourceName, Claims: claimNames, Err: err}
		}
		var srcClaims map[string]json.RawMessage
		if err := json.Unmarshal(srcToken.claims, &srcClaims); err != nil {
			return &ClaimSourceError{
				Source: sourceName,
				Claims: claimNames,
				Err:    fmt.Errorf("failed to unmarshal claims: %v", err),
			}
		}
		for _, claimName := range claimNames {
			raw, ok := srcClaims[claimName]
			if !ok {
				return &ClaimSourceError{
					Source: sourceName,
					Claims: claimNames,
					Err:    fmt.Errorf("JWT does not contain claim %q", claimName),
				}
			}
			merged[claimName] = raw
		}
	}

	payload, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("oidc: failed to marshal merged claims: %v", err)
	}
	t.claims = payload
	return nil
}

// Verify parses a raw ID Token, verifies it's been signed by the provider, performs
// any additional checks depending on the Config, and returns the payload.
//
// Verify does NOT do nonce validation, which is the caller's responsibility.
//
// See: https://openid.net/specs/openid-connect-core-1_0.html#IDTokenValidation
//
//	oauth2Token, err := oauth2Config.Exchange(ctx, r.URL.Query().Get("code"))
//	if err != nil {
//	    // handle error
//	}
//
//	// Extract the ID Token from oauth2 token.
//	rawIDToken, ok := oauth2Token.Extra("id_token").(string)
//	if !ok {
//	    // handle error
//	}
//
//	token, err := verifier.Verify(ctx, rawIDToken)
func (v *IDTokenVerifier) Verify(ctx context.Context, rawIDToken string) (*IDToken, error) {
	payload, sig, err := v.verifyJWT(ctx, rawIDToken)
	if err != nil {
		return nil, err
	}
	var token idToken
	if err := json.Unmarshal(payload, &token); err != nil {
		return nil, fmt.Errorf("oidc: failed to unmarshal claims: %v", err)
	}

	distributedClaims := make(map[string]claimSource)

	//step through the token to map claim names to claim sources"
	for cn, src := range token.ClaimNames {
		if src == "" {
			return nil, fmt.Errorf("oidc: failed to obtain source from claim name")
		}
		s, ok := token.ClaimSources[src]
		if !ok {
			return nil, fmt.Errorf("oidc: source does not exist")
		}
		distributedClaims[cn] = s
	}

	t := &IDToken{
		Issuer:            token.Issuer,
		Subject:           token.Subject,
		Audience:          []string(token.Audience),
		Expiry:            time.Time(token.Expiry),
		IssuedAt:          time.Time(token.IssuedAt),
		Nonce:             token.Nonce,
		AccessTokenHash:   token.AtHash,
		claims:            payload,
		distributedClaims: distributedClaims,
		sigAlgorithm:      sig.Header.Algorithm,
	}

	// Check issuer.
	if !v.config.SkipIssuerCheck && t.Issuer != v.issuer {
		// Google sometimes returns "accounts.google.com" as the issuer claim instead of
		// the required "https://accounts.google.com". Detect this case and allow it only
		// for Google.
		//
		// We will not add hooks to let other providers go off spec like this.
		if !(v.issuer == issuerGoogleAccounts && t.Issuer == issuerGoogleAccountsNoScheme) {
			return nil, fmt.Errorf("oidc: id token issued by a different provider, expected %q got %q", v.issuer, t.Issuer)
		}
	}

	// If a client ID has been provided, make sure it's part of the audience. SkipClientIDCheck must be true if ClientID is empty.
	//
	// This check DOES NOT ensure that the ClientID is the party to which the ID Token was issued (i.e. Authorized party).
	if !v.config.SkipClientIDCheck {
		if v.config.ClientID != "" {
			if !slices.Contains(t.Audience, v.config.ClientID) {
				return nil, fmt.Errorf("oidc: expected audience %q got %q", v.config.ClientID, t.Audience)
			}
		} else {
			return nil, fmt.Errorf("oidc: invalid configuration, clientID must be provided or SkipClientIDCheck must be set")
		}
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
			// https://github.com/AzureAD/azure-activedirectory-identitymodel-extensions-for-dotnet/blob/6.12.2/src/Microsoft.IdentityModel.Tokens/TokenValidationParameters.cs#L149-L153
			leeway := 5 * time.Minute

			if nowTime.Add(leeway).Before(nbfTime) {
				return nil, fmt.Errorf("oidc: current time %v before the nbf (not before) time: %v", nowTime, nbfTime)
			}
		}
	}

	if err := v.resolveAggregatedClaims(ctx, t, &token); err != nil {
		return nil, err
	}

	return t, nil
}

// Nonce returns an auth code option which requires the ID Token created by the
// OpenID Connect provider to contain the specified nonce.
func Nonce(nonce string) oauth2.AuthCodeOption {
	return oauth2.SetAuthURLParam("nonce", nonce)
}

func (v *IDTokenVerifier) verifyJWT(ctx context.Context, rawIDToken string) ([]byte, jose.Signature, error) {
	var supportedSigAlgs []jose.SignatureAlgorithm
	for _, alg := range v.config.SupportedSigningAlgs {
		supportedSigAlgs = append(supportedSigAlgs, jose.SignatureAlgorithm(alg))
	}
	if len(supportedSigAlgs) == 0 {
		// If no algorithms were specified by both the config and discovery, default
		// to the one mandatory algorithm "RS256".
		supportedSigAlgs = []jose.SignatureAlgorithm{jose.RS256}
	}
	if v.config.InsecureSkipSignatureCheck {
		// "none" is a required value to even parse a JWT with the "none" algorithm
		// using go-jose.
		supportedSigAlgs = append(supportedSigAlgs, "none")
	}

	// Parse and verify the signature first. This at least forces the user to have
	// a valid, signed ID token before we do any other processing.
	jws, err := jose.ParseSigned(rawIDToken, supportedSigAlgs)
	if err != nil {
		return nil, jose.Signature{}, fmt.Errorf("oidc: malformed jwt: %v", err)
	}
	switch len(jws.Signatures) {
	case 0:
		return nil, jose.Signature{}, fmt.Errorf("oidc: id token not signed")
	case 1:
	default:
		return nil, jose.Signature{}, fmt.Errorf("oidc: multiple signatures on id token not supported")
	}
	sig := jws.Signatures[0]

	var payload []byte
	if v.config.InsecureSkipSignatureCheck {
		// Yolo mode.
		payload = jws.UnsafePayloadWithoutVerification()
	} else {
		// The JWT is attached here for the happy path to avoid the verifier from
		// having to parse the JWT twice.
		ctx = context.WithValue(ctx, parsedJWTKey, jws)
		payload, err = v.keySet.VerifySignature(ctx, rawIDToken)
		if err != nil {
			return nil, jose.Signature{}, fmt.Errorf("failed to verify signature: %v", err)
		}
	}
	return payload, sig, nil
}
