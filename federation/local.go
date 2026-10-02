package federation

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// LocalClient is an in-memory [Client]. It serves signed entity statements
// without touching the network, which makes trust-chain behavior (including
// broken links and cycles) fully deterministic for examples and tests.
type LocalClient struct {
	mu sync.RWMutex

	configs    map[string]string // entityID -> signed entity configuration
	statements map[string]string // issuer "\x00" subject -> signed statement

	// BrokenConfig marks an entity whose configuration endpoint behaves as
	// missing (404 equivalent).
	brokenConfig map[string]bool
	// BrokenStatement marks an (issuer, subject) edge as missing.
	brokenStatement map[string]bool
}

// NewLocalClient returns an empty LocalClient.
func NewLocalClient() *LocalClient {
	return &LocalClient{
		configs:         map[string]string{},
		statements:      map[string]string{},
		brokenConfig:    map[string]bool{},
		brokenStatement: map[string]bool{},
	}
}

// PutEntityConfiguration stores a signed entity configuration for entityID.
func (c *LocalClient) PutEntityConfiguration(entityID, rawJWT string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.configs[trimRightSlash(entityID)] = rawJWT
}

// PutEntityStatement stores a signed subordinate statement.
func (c *LocalClient) PutEntityStatement(issuer, subject, rawJWT string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statements[statementKey(trimRightSlash(issuer), trimRightSlash(subject))] = rawJWT
}

// BreakConfiguration makes entityID's configuration endpoint answer as
// missing, simulating a broken chain node.
func (c *LocalClient) BreakConfiguration(entityID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.brokenConfig[trimRightSlash(entityID)] = true
}

// BreakStatement makes the subordinate statement of issuer about subject
// answer as missing.
func (c *LocalClient) BreakStatement(issuer, subject string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.brokenStatement[statementKey(trimRightSlash(issuer), trimRightSlash(subject))] = true
}

// FetchEntityConfiguration implements Client.
func (c *LocalClient) FetchEntityConfiguration(_ context.Context, entityID string) (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entityID = trimRightSlash(entityID)
	if c.brokenConfig[entityID] {
		return "", fmt.Errorf("%w: local configuration for %s", ErrNotFound, entityID)
	}
	raw, ok := c.configs[entityID]
	if !ok {
		return "", fmt.Errorf("%w: local configuration for %s", ErrNotFound, entityID)
	}
	return raw, nil
}

// FetchEntityStatement implements Client.
func (c *LocalClient) FetchEntityStatement(_ context.Context, issuer, subject string) (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	issuer = trimRightSlash(issuer)
	subject = trimRightSlash(subject)
	key := statementKey(issuer, subject)
	if c.brokenStatement[key] {
		return "", fmt.Errorf("%w: local statement %s -> %s", ErrNotFound, issuer, subject)
	}
	raw, ok := c.statements[key]
	if !ok {
		return "", fmt.Errorf("%w: local statement %s -> %s", ErrNotFound, issuer, subject)
	}
	return raw, nil
}

func statementKey(issuer, subject string) string {
	return issuer + "\x00" + subject
}

func trimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// EntityInput describes an entity while building a local federation graph.
type EntityInput struct {
	// ID is the entity identifier (an HTTPS URL).
	ID string

	// PrivateKey signs the entity configuration. When nil a fresh P-256 key
	// is generated; the generated key is available on the returned Entity.
	PrivateKey *ecdsa.PrivateKey
	KeyID      string

	// ProviderMetadata is the openid_provider metadata object (raw JSON,
	// e.g. {"issuer":"...","scopes_supported":[...]}).
	ProviderMetadata []byte

	// FederationMetadata is the federation_entity metadata object (e.g.
	// {"federation_fetch_endpoint":"https://..."}).
	FederationMetadata []byte

	// AuthorityHints are the entity identifiers that may issue a
	// subordinate statement for this entity.
	AuthorityHints []string

	// Expiry/NotBefore/IAT control the validity window of the entity
	// configuration. Zero Expiry defaults to now+1h in SignEntityConfiguration.
	IAT    time.Time
	NBF    time.Time
	Expiry time.Time
}

// Entity is a built local entity: its key, signed configuration and a helper
// to issue subordinate statements.
type Entity struct {
	ID        string
	Key       *ecdsa.PrivateKey
	KeyID     string
	JWKS      *jose.JSONWebKeySet
	ConfigJWT string
}

// GenerateKey creates a fresh P-256 signing key.
func GenerateKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// SignEntityConfiguration signs a self-issued entity configuration JWT for
// input and publishes the signer's public key in its "jwks" member.
func SignEntityConfiguration(input *EntityInput, now time.Time) (*Entity, error) {
	key := input.PrivateKey
	if key == nil {
		var err error
		key, err = GenerateKey()
		if err != nil {
			return nil, err
		}
	}
	keyID := input.KeyID
	if keyID == "" {
		keyID = "fed-key-" + trimRightSlash(input.ID)
	}

	jwk := jose.JSONWebKey{Key: key.Public(), KeyID: keyID, Algorithm: string(jose.ES256), Use: "sig"}
	jwks := &jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}}

	iat := input.IAT
	if iat.IsZero() {
		iat = now.Add(-time.Minute)
	}
	exp := input.Expiry
	if exp.IsZero() {
		exp = now.Add(time.Hour)
	}

	claims := map[string]any{
		"iss":             trimRightSlash(input.ID),
		"sub":             trimRightSlash(input.ID),
		"iat":             iat.Unix(),
		"exp":             exp.Unix(),
		"jwks":            jwks,
		"authority_hints": input.AuthorityHints,
	}
	if !input.NBF.IsZero() {
		claims["nbf"] = input.NBF.Unix()
	}

	metadata := map[string]json.RawMessage{}
	if len(input.ProviderMetadata) > 0 {
		metadata[metadataTypeOpenIDProvider] = input.ProviderMetadata
	}
	if len(input.FederationMetadata) > 0 {
		metadata["federation_entity"] = input.FederationMetadata
	}
	if len(metadata) > 0 {
		claims["metadata"] = metadata
	}

	raw, err := signStatement(key, keyID, claims)
	if err != nil {
		return nil, err
	}
	return &Entity{ID: trimRightSlash(input.ID), Key: key, KeyID: keyID, JWKS: jwks, ConfigJWT: raw}, nil
}

// StatementInput describes a subordinate statement to sign.
type StatementInput struct {
	Issuer  *Entity
	Subject string

	// ProviderPolicy is the metadata policy for openid_provider (raw JSON,
	// e.g. {"scopes_supported":{"add":["openid"]}}).
	ProviderPolicy []byte

	IAT    time.Time
	NBF    time.Time
	Expiry time.Time
}

// SignEntityStatement signs a subordinate statement issued by the issuer
// entity about the subject entity.
func SignEntityStatement(input *StatementInput, now time.Time) (string, error) {
	if input.Issuer == nil {
		return "", fmt.Errorf("federation: statement requires an issuer entity")
	}
	iat := input.IAT
	if iat.IsZero() {
		iat = now.Add(-time.Minute)
	}
	exp := input.Expiry
	if exp.IsZero() {
		exp = now.Add(time.Hour)
	}

	claims := map[string]any{
		"iss": input.Issuer.ID,
		"sub": trimRightSlash(input.Subject),
		"iat": iat.Unix(),
		"exp": exp.Unix(),
	}
	if !input.NBF.IsZero() {
		claims["nbf"] = input.NBF.Unix()
	}
	if len(input.ProviderPolicy) > 0 {
		claims["metadata_policy"] = map[string]json.RawMessage{
			metadataTypeOpenIDProvider: input.ProviderPolicy,
		}
	}

	return signStatement(input.Issuer.Key, input.Issuer.KeyID, claims)
}

func signStatement(key *ecdsa.PrivateKey, keyID string, claims map[string]any) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{}).
			WithType(EntityStatementType).
			WithHeader(jose.HeaderKey("kid"), keyID),
	)
	if err != nil {
		return "", fmt.Errorf("federation: creating signer: %v", err)
	}

	// encoding/json marshals maps with sorted keys, keeping the signed
	// payloads of the examples deterministic.
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", fmt.Errorf("federation: signing statement: %v", err)
	}
	return obj.CompactSerialize()
}

// AddEntities builds the given entities, registers their configurations on
// client and returns the entities keyed by ID.
func AddEntities(client *LocalClient, now time.Time, inputs ...*EntityInput) (map[string]*Entity, error) {
	out := make(map[string]*Entity, len(inputs))
	for _, input := range inputs {
		entity, err := SignEntityConfiguration(input, now)
		if err != nil {
			return nil, err
		}
		client.PutEntityConfiguration(entity.ID, entity.ConfigJWT)
		out[entity.ID] = entity
	}
	return out, nil
}

// AddStatement signs a subordinate statement and registers it on client.
func AddStatement(client *LocalClient, issuer *Entity, input *StatementInput, now time.Time) (string, error) {
	input.Issuer = issuer
	raw, err := SignEntityStatement(input, now)
	if err != nil {
		return "", err
	}
	client.PutEntityStatement(issuer.ID, input.Subject, raw)
	return raw, nil
}
