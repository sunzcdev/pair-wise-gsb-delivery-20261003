package federation

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// EntityStatementType is the required value of the JWS "typ" header of every
// entity statement, including entity configurations.
const EntityStatementType = "entity-statement+jwt"

// federationSignatureAlgs are the asymmetric JWS algorithms accepted on
// entity statements. "none" and symmetric algorithms are never accepted.
var federationSignatureAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

const (
	metadataTypeOpenIDProvider = "openid_provider"
)

// EntityStatement is a parsed and signature-verified entity statement.
type EntityStatement struct {
	// Issuer and Subject are entity identifiers.
	Issuer  string
	Subject string

	IssuedAt  time.Time
	NotBefore time.Time
	ExpiresAt time.Time

	// Keys publishes the issuer's federation keys. It is always non-nil for
	// an entity configuration; subordinate statements normally omit it.
	Keys *jose.JSONWebKeySet

	// Metadata holds the raw metadata objects keyed by metadata type
	// (e.g. "openid_provider"). Only present on an entity configuration.
	Metadata map[string]json.RawMessage

	// MetadataPolicy holds the metadata policies keyed by metadata type.
	// Only present on a subordinate statement.
	MetadataPolicy map[string]Policy

	// AuthorityHints are the entity identifiers that may issue a
	// subordinate statement for this entity. Only present on an entity
	// configuration.
	AuthorityHints []string

	// Raw is the signed compact JWT the statement was parsed from.
	Raw string

	// Header is the verified protected JWS header.
	Header jose.Header

	payload []byte
}

// IsConfiguration reports whether the statement is an entity configuration
// (a self-issued statement, iss == sub).
func (s *EntityStatement) IsConfiguration() bool {
	return s.Issuer == s.Subject
}

// ProviderMetadata returns the raw openid_provider metadata, if present.
func (s *EntityStatement) ProviderMetadata() (json.RawMessage, bool) {
	if s.Metadata == nil {
		return nil, false
	}
	raw, ok := s.Metadata[metadataTypeOpenIDProvider]
	return raw, ok
}

type statementClaims struct {
	Issuer         string                     `json:"iss"`
	Subject        string                     `json:"sub"`
	IAT            *int64                     `json:"iat"`
	NBF            *int64                     `json:"nbf"`
	EXP            *int64                     `json:"exp"`
	JWKS           *jose.JSONWebKeySet        `json:"jwks"`
	Metadata       map[string]json.RawMessage `json:"metadata"`
	MetadataPolicy map[string]json.RawMessage `json:"metadata_policy"`
	AuthorityHints []string                   `json:"authority_hints"`
}

// parseAndVerify parses a compact entity-statement JWT, enforces the JWS
// headers mandated by the federation specification, verifies the signature
// against the supplied keys and, when expectedIssuer/expectedSubject are
// non-empty, enforces the subject relationship.
func parseAndVerify(raw string, keys *jose.JSONWebKeySet, expectedIssuer, expectedSubject string, now time.Time) (*EntityStatement, error) {
	obj, err := jose.ParseSigned(raw, federationSignatureAlgs)
	if err != nil {
		return nil, fmt.Errorf("federation: parsing entity statement: %v", err)
	}
	if len(obj.Signatures) != 1 {
		return nil, fmt.Errorf("federation: entity statement must carry exactly one signature, got %d", len(obj.Signatures))
	}

	sig := obj.Signatures[0]
	hdr := sig.Protected
	if typ, _ := hdr.ExtraHeaders[jose.HeaderType].(string); typ != EntityStatementType {
		return nil, fmt.Errorf("federation: unexpected entity statement typ %q, want %q", typ, EntityStatementType)
	}
	if crit, ok := hdr.ExtraHeaders["crit"]; ok {
		// Defense in depth: go-jose already rejects "crit" at parse time.
		parts := make([]string, 0)
		switch names := crit.(type) {
		case []interface{}:
			for _, name := range names {
				if s, ok := name.(string); ok {
					parts = append(parts, s)
				}
			}
		case []string:
			parts = append(parts, names...)
		}
		return nil, fmt.Errorf("federation: unsupported critical JWS header parameters: %s", strings.Join(parts, ", "))
	}

	payload, err := verifyWithKeys(obj, keys, hdr.KeyID)
	if err != nil {
		return nil, err
	}

	var claims statementClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("federation: decoding entity statement claims: %v", err)
	}
	if claims.Issuer == "" || claims.Subject == "" {
		return nil, errors.New("federation: entity statement missing iss or sub")
	}
	if expectedIssuer != "" && claims.Issuer != expectedIssuer {
		return nil, fmt.Errorf("federation: entity statement issuer %q does not match expected issuer %q", claims.Issuer, expectedIssuer)
	}
	if expectedSubject != "" && claims.Subject != expectedSubject {
		return nil, fmt.Errorf("federation: entity statement subject %q does not match expected subject %q", claims.Subject, expectedSubject)
	}

	stmt := &EntityStatement{
		Issuer:         claims.Issuer,
		Subject:        claims.Subject,
		Keys:           claims.JWKS,
		Metadata:       claims.Metadata,
		AuthorityHints: claims.AuthorityHints,
		Raw:            raw,
		Header:         hdr,
		payload:        payload,
	}
	if claims.IAT != nil {
		stmt.IssuedAt = time.Unix(*claims.IAT, 0).UTC()
	}
	if claims.NBF != nil {
		stmt.NotBefore = time.Unix(*claims.NBF, 0).UTC()
	}
	if claims.EXP != nil {
		stmt.ExpiresAt = time.Unix(*claims.EXP, 0).UTC()
	}
	if err := stmt.checkTime(now); err != nil {
		return nil, err
	}

	if claims.MetadataPolicy != nil {
		stmt.MetadataPolicy = make(map[string]Policy, len(claims.MetadataPolicy))
		for metadataType, rawPolicy := range claims.MetadataPolicy {
			policy, err := parsePolicy(rawPolicy)
			if err != nil {
				return nil, fmt.Errorf("federation: metadata policy for %q: %v", metadataType, err)
			}
			stmt.MetadataPolicy[metadataType] = policy
		}
	}

	return stmt, nil
}

func verifyWithKeys(obj *jose.JSONWebSignature, keys *jose.JSONWebKeySet, kid string) ([]byte, error) {
	if keys == nil || len(keys.Keys) == 0 {
		return nil, errors.New("federation: issuer published no keys to verify the statement")
	}

	candidates := keys.Keys
	if kid != "" {
		candidates = keys.Key(kid)
		if len(candidates) == 0 {
			return nil, fmt.Errorf("federation: no issuer key matches kid %q", kid)
		}
	}

	var lastErr error
	for _, key := range candidates {
		if !key.IsPublic() {
			key = key.Public()
		}
		payload, err := obj.Verify(key.Key)
		if err == nil {
			return payload, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no key matched")
	}
	if kid != "" {
		return nil, fmt.Errorf("federation: signature not valid under kid %q: %v", kid, lastErr)
	}
	return nil, fmt.Errorf("federation: signature not valid under any issuer key: %v", lastErr)
}

func (s *EntityStatement) checkTime(now time.Time) error {
	now = now.UTC()
	if !s.IssuedAt.IsZero() && now.Before(s.IssuedAt) {
		return fmt.Errorf("federation: statement from %q about %q used before iat %s", s.Issuer, s.Subject, s.IssuedAt.Format(time.RFC3339))
	}
	if !s.NotBefore.IsZero() && now.Before(s.NotBefore) {
		return fmt.Errorf("federation: statement from %q about %q not yet valid (nbf %s)", s.Issuer, s.Subject, s.NotBefore.Format(time.RFC3339))
	}
	if s.ExpiresAt.IsZero() {
		return fmt.Errorf("federation: statement from %q about %q has no exp", s.Issuer, s.Subject)
	}
	if now.Equal(s.ExpiresAt) || now.After(s.ExpiresAt) {
		return fmt.Errorf("federation: statement from %q about %q expired at %s", s.Issuer, s.Subject, s.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

// verifyConfiguration parses and verifies a self-issued entity configuration:
// the statement must be signed by the keys it publishes itself and iss==sub.
func verifyConfiguration(raw, entityID string, now time.Time) (*EntityStatement, error) {
	boot, err := peekSelfIssued(raw)
	if err != nil {
		return nil, err
	}
	stmt, err := parseAndVerify(raw, boot.Keys, entityID, entityID, now)
	if err != nil {
		return nil, err
	}
	if stmt.Keys == nil || len(stmt.Keys.Keys) == 0 {
		return nil, fmt.Errorf("federation: entity configuration for %q publishes no federation keys", entityID)
	}
	return stmt, nil
}

// peekSelfIssued does a first unsigned pass over an entity configuration
// solely to reach the "jwks" member needed to verify the very same
// statement. Nothing from this pass is trusted after signature verification.
func peekSelfIssued(raw string) (*EntityStatement, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("federation: entity configuration is not a compact JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("federation: decoding entity configuration payload: %v", err)
	}
	var claims statementClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("federation: reading entity configuration jwks: %v", err)
	}
	if claims.JWKS == nil || len(claims.JWKS.Keys) == 0 {
		return nil, errors.New("federation: entity configuration publishes no federation keys")
	}
	return &EntityStatement{Keys: claims.JWKS}, nil
}
