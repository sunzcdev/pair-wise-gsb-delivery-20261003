package federation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// TrustAnchor is a pre-configured federation root. Statements are only
// trusted if the chain terminates at one of these entities and the anchor's
// own configuration verifies against the pinned keys.
type TrustAnchor struct {
	EntityID string
	JWKS     jose.JSONWebKeySet
}

// Fetcher retrieves raw entity statements. All network access during
// resolution goes through this interface, so tests and deployments can
// substitute their own transport.
type Fetcher interface {
	// FetchEntityConfiguration returns the self-signed entity configuration
	// published by entityID.
	FetchEntityConfiguration(ctx context.Context, entityID string) ([]byte, error)
	// FetchSubordinateStatement returns the statement issued by the entity
	// serving fetchEndpoint about subordinate.
	FetchSubordinateStatement(ctx context.Context, fetchEndpoint, subordinate string) ([]byte, error)
}

// HTTPFetcher is the default Fetcher. It resolves entity configurations from
// the well-known location and subordinate statements from federation fetch
// endpoints.
type HTTPFetcher struct {
	Client *http.Client
}

// WellKnownURL returns the entity configuration location for an entity ID.
func WellKnownURL(entityID string) string {
	return strings.TrimSuffix(entityID, "/") + "/.well-known/openid-federation"
}

func (f *HTTPFetcher) httpClient() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return http.DefaultClient
}

func (f *HTTPFetcher) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/entity-statement+jwt")
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("federation: GET %s: unexpected status %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (f *HTTPFetcher) FetchEntityConfiguration(ctx context.Context, entityID string) ([]byte, error) {
	return f.get(ctx, WellKnownURL(entityID))
}

func (f *HTTPFetcher) FetchSubordinateStatement(ctx context.Context, fetchEndpoint, subordinate string) ([]byte, error) {
	u, err := url.Parse(fetchEndpoint)
	if err != nil {
		return nil, fmt.Errorf("federation: invalid fetch endpoint %q: %v", fetchEndpoint, err)
	}
	q := u.Query()
	q.Set("sub", subordinate)
	u.RawQuery = q.Encode()
	return f.get(ctx, u.String())
}

// Resolver builds and validates trust chains from OPs to configured trust
// anchors.
type Resolver struct {
	// TrustAnchors are the configured federation roots.
	TrustAnchors []TrustAnchor

	// Fetcher retrieves entity statements. Defaults to HTTPFetcher.
	Fetcher Fetcher

	// Now is the time function used for validity checks. Defaults to
	// time.Now.
	Now func() time.Time

	// MaxChainLength bounds the number of entities on a chain, guarding
	// against unbounded walks. Defaults to 8.
	MaxChainLength int
}

// ResolvedProvider is the outcome of resolving an OP's issuer through a
// trust chain.
type ResolvedProvider struct {
	// Issuer is the OP's entity ID.
	Issuer string
	// TrustAnchor is the entity ID of the anchor the chain terminates at.
	TrustAnchor string
	// Chain lists the entity IDs from the trust anchor down to the OP. It
	// records where the trusted metadata came from.
	Chain []string
	// Metadata is the OP's "openid_provider" metadata after the chain's
	// combined metadata policy has been applied.
	Metadata map[string]any
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Resolver) fetcher() Fetcher {
	if r.Fetcher != nil {
		return r.Fetcher
	}
	return &HTTPFetcher{}
}

func (r *Resolver) maxChainLength() int {
	if r.MaxChainLength > 0 {
		return r.MaxChainLength
	}
	return 8
}

// candidate is a partially built chain, from the OP up to entityID.
type candidate struct {
	entityID string
	// config is the verified configuration of entityID.
	config *EntityStatement
	// leaf is the OP's own verified configuration.
	leaf *EntityStatement
	// subStmts[i] is the verified statement issued by path[i+1] about
	// path[i].
	subStmts []*EntityStatement
	// path lists entity IDs from the OP up to entityID.
	path []string
}

// ResolveProvider resolves the trusted "openid_provider" metadata for an
// issuer. The issuer is the OP's discovery URL; it is not required to be a
// trust anchor. The federation is searched breadth-first, so the shortest
// valid chain wins and cycles cannot cause unbounded recursion.
func (r *Resolver) ResolveProvider(ctx context.Context, issuer string) (*ResolvedProvider, error) {
	if len(r.TrustAnchors) == 0 {
		return nil, errors.New("federation: no trust anchors configured")
	}
	fetcher := r.fetcher()
	now := r.now()

	raw, err := fetcher.FetchEntityConfiguration(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("federation: fetching configuration for %q: %w", issuer, err)
	}
	opConfig, err := ParseEntityConfiguration(raw, now)
	if err != nil {
		return nil, fmt.Errorf("federation: invalid configuration for %q: %w", issuer, err)
	}

	var errs []error
	queue := []candidate{{
		entityID: issuer,
		config:   opConfig,
		leaf:     opConfig,
		path:     []string{issuer},
	}}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]

		for _, anchor := range r.TrustAnchors {
			if anchor.EntityID != c.entityID {
				continue
			}
			resolved, err := r.validateChain(anchor, c)
			if err == nil {
				return resolved, nil
			}
			errs = append(errs, err)
		}

		if len(c.path) >= r.maxChainLength() {
			errs = append(errs, fmt.Errorf("federation: chain at %q exceeds maximum length", c.entityID))
			continue
		}
		for _, hint := range c.config.AuthorityHints {
			if slices.Contains(c.path, hint) {
				// A cycle: this path can never reach an anchor it
				// has not already passed.
				continue
			}
			next, err := r.stepUp(ctx, fetcher, now, c, hint)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			queue = append(queue, *next)
		}
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("federation: no trust chain from %q to any configured trust anchor", issuer)
	}
	return nil, fmt.Errorf("federation: no valid trust chain from %q: %w", issuer, errors.Join(errs...))
}

// stepUp extends a candidate chain one level: it fetches the hinted
// superior's configuration and its statement about the current entity, and
// verifies both.
func (r *Resolver) stepUp(ctx context.Context, fetcher Fetcher, now time.Time, c candidate, hint string) (*candidate, error) {
	raw, err := fetcher.FetchEntityConfiguration(ctx, hint)
	if err != nil {
		return nil, fmt.Errorf("federation: fetching configuration for %q: %w", hint, err)
	}
	hintConfig, err := ParseEntityConfiguration(raw, now)
	if err != nil {
		return nil, fmt.Errorf("federation: invalid configuration for %q: %w", hint, err)
	}
	endpoint, err := fetchEndpoint(hintConfig)
	if err != nil {
		return nil, err
	}
	rawStmt, err := fetcher.FetchSubordinateStatement(ctx, endpoint, c.entityID)
	if err != nil {
		return nil, fmt.Errorf("federation: fetching statement by %q about %q: %w", hint, c.entityID, err)
	}
	subStmt, err := ParseEntityStatement(rawStmt, hintConfig.JWKS, now)
	if err != nil {
		return nil, fmt.Errorf("federation: invalid statement by %q about %q: %w", hint, c.entityID, err)
	}
	if subStmt.Issuer != hint || subStmt.Subject != c.entityID {
		return nil, fmt.Errorf("federation: statement by %q about %q has unexpected iss/sub %q/%q", hint, c.entityID, subStmt.Issuer, subStmt.Subject)
	}
	return &candidate{
		entityID: hint,
		config:   hintConfig,
		leaf:     c.leaf,
		subStmts: append(slices.Clone(c.subStmts), subStmt),
		path:     append(slices.Clone(c.path), hint),
	}, nil
}

// validateChain verifies a complete chain that ends at a trust anchor and
// computes the trusted provider metadata.
func (r *Resolver) validateChain(anchor TrustAnchor, c candidate) (*ResolvedProvider, error) {
	// The anchor's configuration must verify against the pinned keys, not
	// only against the keys it asserts itself.
	if err := c.config.verifyWith(anchor.JWKS); err != nil {
		return nil, fmt.Errorf("federation: trust anchor %q configuration does not verify against configured keys: %w", anchor.EntityID, err)
	}

	// Entity IDs from the trust anchor down to the OP.
	m := len(c.path)
	entityIDs := make([]string, 0, m)
	for i := m - 1; i >= 0; i-- {
		entityIDs = append(entityIDs, c.path[i])
	}

	// links[0] is the anchor's configuration, links[1..m-1] are the
	// statements about entityIDs[1..m-1], and the final link is the OP's
	// own configuration.
	links := []*EntityStatement{c.config}
	for i := len(c.subStmts) - 1; i >= 0; i-- {
		links = append(links, c.subStmts[i])
	}
	if c.leaf != c.config {
		links = append(links, c.leaf)
	}

	// Every statement below the anchor must be issued by the entity
	// directly above it about the entity directly below it.
	for i := 1; i < m; i++ {
		if links[i].Issuer != entityIDs[i-1] || links[i].Subject != entityIDs[i] {
			return nil, fmt.Errorf("federation: broken chain between %q and %q", entityIDs[i-1], entityIDs[i])
		}
	}
	leaf := links[len(links)-1]
	if leaf.Issuer != entityIDs[m-1] || leaf.Subject != entityIDs[m-1] {
		return nil, fmt.Errorf("federation: chain does not terminate at %q", entityIDs[m-1])
	}

	// Enforce max_path_length constraints: a statement at position i
	// allows at most that many intermediates between it and the OP.
	for i := 0; i < m; i++ {
		if links[i].MaxPathLength == nil {
			continue
		}
		intermediates := m - 2 - i
		if intermediates < 0 {
			intermediates = 0
		}
		if intermediates > *links[i].MaxPathLength {
			return nil, fmt.Errorf("federation: statement by %q allows at most %d intermediates, chain has %d", entityIDs[i], *links[i].MaxPathLength, intermediates)
		}
	}

	// Combine metadata policies top-down and apply them to the OP's
	// self-asserted provider metadata. The OP's own configuration carries
	// no policy over itself.
	policy := map[string]ClaimPolicy{}
	for i := 0; i < len(links)-1; i++ {
		policy = combinePolicies(policy, links[i].MetadataPolicy["openid_provider"])
	}
	metadata, err := applyMetadataPolicy(leaf.Metadata["openid_provider"], policy)
	if err != nil {
		return nil, fmt.Errorf("federation: metadata policy violation on chain rooted at %q: %w", anchor.EntityID, err)
	}

	return &ResolvedProvider{
		Issuer:      entityIDs[m-1],
		TrustAnchor: anchor.EntityID,
		Chain:       entityIDs,
		Metadata:    metadata,
	}, nil
}

// fetchEndpoint extracts the federation fetch endpoint from an entity's
// verified configuration.
func fetchEndpoint(config *EntityStatement) (string, error) {
	fedEntity, ok := config.Metadata["federation_entity"]
	if !ok {
		return "", fmt.Errorf("federation: %q publishes no federation_entity metadata", config.Issuer)
	}
	endpoint, ok := fedEntity["federation_fetch_endpoint"].(string)
	if !ok || endpoint == "" {
		return "", fmt.Errorf("federation: %q publishes no federation_fetch_endpoint", config.Issuer)
	}
	return endpoint, nil
}
