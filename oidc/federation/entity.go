// Package federation resolves OpenID Provider metadata through OpenID
// Federation trust chains.
//
// A Resolver starts from an OP's entity configuration (its discovery URL,
// which is not itself a trust anchor) and walks authority hints upward until
// it reaches a configured trust anchor. Every entity statement on the
// resulting chain has its signature, issuer/subject relationship, and
// validity window verified, and the metadata policies of the chain are
// combined and applied to the OP's self-asserted metadata.
package federation

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// signature algorithms accepted for entity statements.
var supportedAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.EdDSA,
}

// EntityStatement is a verified OpenID Federation entity statement: either a
// self-signed entity configuration or a statement a superior entity issued
// about a subordinate.
type EntityStatement struct {
	Issuer  string
	Subject string

	IssuedAt  time.Time
	ExpiresAt time.Time

	// JWKS is the entity's key set, only present in entity configurations.
	JWKS jose.JSONWebKeySet

	// AuthorityHints lists the entities above this one, only present in
	// entity configurations of subordinate entities.
	AuthorityHints []string

	// Metadata maps an entity type (e.g. "openid_provider") to its claims.
	Metadata map[string]map[string]any

	// MetadataPolicy maps an entity type to per-claim policies imposed on
	// subordinate entities.
	MetadataPolicy map[string]map[string]ClaimPolicy

	// MaxPathLength, if non-nil, limits the number of intermediate entities
	// permitted between this entity and a leaf.
	MaxPathLength *int

	raw []byte
}

type entityStatementClaims struct {
	Issuer         string              `json:"iss"`
	Subject        string              `json:"sub"`
	IssuedAt       int64               `json:"iat"`
	Expiry         int64               `json:"exp"`
	JWKS           *jose.JSONWebKeySet `json:"jwks,omitempty"`
	AuthorityHints []string            `json:"authority_hints,omitempty"`

	Metadata       map[string]map[string]any         `json:"metadata,omitempty"`
	MetadataPolicy map[string]map[string]ClaimPolicy `json:"metadata_policy,omitempty"`
	Constraints    *struct {
		MaxPathLength *int `json:"max_path_length,omitempty"`
	} `json:"constraints,omitempty"`
}

func (c *entityStatementClaims) statement(raw []byte) (*EntityStatement, error) {
	if c.Issuer == "" || c.Subject == "" {
		return nil, errors.New("federation: entity statement missing iss or sub")
	}
	if c.Expiry == 0 {
		return nil, errors.New("federation: entity statement missing exp")
	}
	stmt := &EntityStatement{
		Issuer:         c.Issuer,
		Subject:        c.Subject,
		IssuedAt:       time.Unix(c.IssuedAt, 0),
		ExpiresAt:      time.Unix(c.Expiry, 0),
		AuthorityHints: c.AuthorityHints,
		Metadata:       c.Metadata,
		MetadataPolicy: c.MetadataPolicy,
		raw:            raw,
	}
	if c.JWKS != nil {
		stmt.JWKS = *c.JWKS
	}
	if c.Constraints != nil {
		stmt.MaxPathLength = c.Constraints.MaxPathLength
	}
	if err := validatePolicyOperators(stmt.MetadataPolicy); err != nil {
		return nil, err
	}
	return stmt, nil
}

// parseUnverified decodes the claims of an entity statement without checking
// its signature. It is only used to locate the keys a self-signed entity
// configuration should be verified against.
func parseUnverified(raw []byte) (*entityStatementClaims, error) {
	jws, err := jose.ParseSigned(string(raw), supportedAlgs)
	if err != nil {
		return nil, fmt.Errorf("federation: malformed entity statement: %v", err)
	}
	var claims entityStatementClaims
	if err := json.Unmarshal(jws.UnsafePayloadWithoutVerification(), &claims); err != nil {
		return nil, fmt.Errorf("federation: decoding entity statement: %v", err)
	}
	return &claims, nil
}

// verifySignature verifies the compact JWS against the key set and returns
// the decoded claims.
func verifySignature(raw []byte, keys jose.JSONWebKeySet) (*entityStatementClaims, error) {
	jws, err := jose.ParseSigned(string(raw), supportedAlgs)
	if err != nil {
		return nil, fmt.Errorf("federation: malformed entity statement: %v", err)
	}
	for i := range keys.Keys {
		payload, err := jws.Verify(&keys.Keys[i])
		if err != nil {
			continue
		}
		var claims entityStatementClaims
		if err := json.Unmarshal(payload, &claims); err != nil {
			return nil, fmt.Errorf("federation: decoding entity statement: %v", err)
		}
		return &claims, nil
	}
	return nil, errors.New("federation: no key able to verify entity statement signature")
}

// ParseEntityStatement verifies the signature of a raw entity statement with
// the provided keys and checks its validity window against now.
func ParseEntityStatement(raw []byte, keys jose.JSONWebKeySet, now time.Time) (*EntityStatement, error) {
	claims, err := verifySignature(raw, keys)
	if err != nil {
		return nil, err
	}
	stmt, err := claims.statement(raw)
	if err != nil {
		return nil, err
	}
	if err := stmt.checkValidity(now); err != nil {
		return nil, err
	}
	return stmt, nil
}

// ParseEntityConfiguration parses a self-signed entity configuration. The
// statement is verified against the key set it carries, and its issuer and
// subject must be identical.
func ParseEntityConfiguration(raw []byte, now time.Time) (*EntityStatement, error) {
	unverified, err := parseUnverified(raw)
	if err != nil {
		return nil, err
	}
	if unverified.JWKS == nil || len(unverified.JWKS.Keys) == 0 {
		return nil, fmt.Errorf("federation: entity configuration for %q carries no jwks", unverified.Issuer)
	}
	stmt, err := ParseEntityStatement(raw, *unverified.JWKS, now)
	if err != nil {
		return nil, err
	}
	if stmt.Issuer != stmt.Subject {
		return nil, fmt.Errorf("federation: entity configuration iss %q does not match sub %q", stmt.Issuer, stmt.Subject)
	}
	return stmt, nil
}

func (s *EntityStatement) checkValidity(now time.Time) error {
	if !s.ExpiresAt.After(now) {
		return fmt.Errorf("federation: entity statement issued by %q for %q expired at %s", s.Issuer, s.Subject, s.ExpiresAt)
	}
	if s.IssuedAt.After(now.Add(time.Minute)) {
		return fmt.Errorf("federation: entity statement issued by %q for %q has iat in the future", s.Issuer, s.Subject)
	}
	return nil
}

// verifyWith re-verifies the statement's raw signature against a different
// key set. It is used to pin a trust anchor's configuration to configured
// keys rather than self-asserted ones.
func (s *EntityStatement) verifyWith(keys jose.JSONWebKeySet) error {
	claims, err := verifySignature(s.raw, keys)
	if err != nil {
		return err
	}
	if claims.Issuer != s.Issuer || claims.Subject != s.Subject {
		return fmt.Errorf("federation: re-verified statement for %q does not match", s.Subject)
	}
	return nil
}
