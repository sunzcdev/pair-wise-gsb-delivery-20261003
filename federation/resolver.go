package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultMaxPathLength bounds how many intermediate entities a trust chain
// may contain.
const DefaultMaxPathLength = 10

// ErrNoTrustPath is returned when no candidate path from the leaf reaches a
// configured trust anchor.
var ErrNoTrustPath = errors.New("federation: no valid trust path to any configured trust anchor")

// Resolver resolves leaf metadata through OpenID Federation trust chains.
//
// The leaf URL is never treated as a trust anchor: a chain only succeeds by
// ending at one of the trust anchors explicitly passed to NewResolver.
type Resolver struct {
	client        Client
	trustAnchors  map[string]bool
	now           func() time.Time
	maxPathLength int

	// per-resolution caches
	configs   map[string]*EntityStatement
	configErr map[string]error
}

// NewResolver constructs a Resolver using client for all fetch operations and
// the given trust anchor entity identifiers.
func NewResolver(client Client, trustAnchors ...string) *Resolver {
	set := make(map[string]bool, len(trustAnchors))
	for _, anchor := range trustAnchors {
		set[strings.TrimRight(anchor, "/")] = true
	}
	return &Resolver{
		client:        client,
		trustAnchors:  set,
		now:           time.Now,
		maxPathLength: DefaultMaxPathLength,
	}
}

// WithClock overrides the clock used for validity checks. Mainly useful for
// tests; production code normally leaves it unset.
func (r *Resolver) WithClock(now func() time.Time) *Resolver {
	r.now = now
	return r
}

// WithMaxPathLength overrides DefaultMaxPathLength.
func (r *Resolver) WithMaxPathLength(n int) *Resolver {
	if n > 0 {
		r.maxPathLength = n
	}
	return r
}

// Chain is one verified trust chain, ordered from the leaf to the trust
// anchor.
type Chain struct {
	// Configurations are the verified entity configurations ordered
	// leaf -> ... -> trust anchor.
	Configurations []*EntityStatement
	// Subordinates are the verified subordinate statements ordered
	// leaf -> anchor: Subordinates[i] is issued by Configurations[i+1]
	// about Configurations[i].
	Subordinates []*EntityStatement
}

// Entities returns the chain's entity identifiers, leaf first.
func (c Chain) Entities() []string {
	out := make([]string, len(c.Configurations))
	for i, cfg := range c.Configurations {
		out[i] = cfg.Subject
	}
	return out
}

// PolicyContribution records the policy one statement contributed.
type PolicyContribution struct {
	Issuer  string
	Subject string
	Policy  Policy
}

// PathAttempt describes one explored candidate edge and why it did or did not
// extend a chain.
type PathAttempt struct {
	// Path is the sequence of entity identifiers walked so far, leaf first,
	// including the superior the attempt was reaching for.
	Path []string
	// Reason is nil for edges that were traversed successfully; otherwise
	// it explains why the edge was unusable (cycle, missing statement,
	// bad signature, expiry, ...).
	Reason error
}

// Trace exposes the resolver's path discovery for provenance output.
type Trace struct {
	Leaf       string
	Attempts   []PathAttempt
	ValidPaths [][]string
}

// Result is the outcome of a successful resolution.
type Result struct {
	// ProviderMetadata is the leaf's openid_provider metadata after the
	// composed metadata policy was applied.
	ProviderMetadata json.RawMessage

	Chain Chain
	// PolicyContributions are the openid_provider policies applied along
	// the chosen chain, ordered trust anchor -> leaf.
	PolicyContributions []PolicyContribution
	// Origins explains where the final value of every metadata parameter
	// came from.
	Origins []ClaimOrigin

	Trace *Trace
}

type searchState struct {
	trace    *Trace
	chains   []Chain
	attempts []PathAttempt
}

// ResolveProvider finds a verified trust chain from leafEntityID to a
// configured trust anchor, composes the openid_provider metadata policy
// along it and returns the leaf metadata after policy application.
func (r *Resolver) ResolveProvider(ctx context.Context, leafEntityID string) (*Result, error) {
	leafEntityID = strings.TrimRight(leafEntityID, "/")

	r.configs = map[string]*EntityStatement{}
	r.configErr = map[string]error{}

	leaf, err := r.configuration(ctx, leafEntityID)
	if err != nil {
		return nil, fmt.Errorf("federation: leaf entity configuration: %v", err)
	}
	if _, hasProvider := leaf.ProviderMetadata(); !hasProvider {
		return nil, fmt.Errorf("federation: leaf %q publishes no openid_provider metadata", leafEntityID)
	}
	// The discovery URL is never assumed to be a trust anchor: even when an
	// operator lists the leaf itself in the trust-anchor set, resolution
	// must still traverse at least one issued subordinate statement up to a
	// configured superior. The anchor match is therefore only evaluated on
	// a node reached after an edge, not on the starting configuration.

	state := &searchState{trace: &Trace{Leaf: leafEntityID}}
	r.search(ctx, []*EntityStatement{leaf}, nil, state, false)

	if len(state.chains) == 0 {
		return nil, fmt.Errorf("%w for %q: %s", ErrNoTrustPath, leafEntityID, joinAttemptReasons(state.attempts))
	}

	sortChains(state.chains)
	chosen := state.chains[0]

	for _, chain := range state.chains {
		state.trace.ValidPaths = append(state.trace.ValidPaths, chain.Entities())
	}
	state.trace.Attempts = state.attempts

	return r.buildResult(leaf, chosen, state.trace)
}

// search performs a depth-first walk over entity configurations following
// authority_hints. visited prevents cycles; every rejected edge is recorded
// so that a failure can explain all the paths that were tried.
func (r *Resolver) search(ctx context.Context, configs []*EntityStatement, subordinates []*EntityStatement, state *searchState, reachedByStatement bool) {
	current := configs[len(configs)-1]

	if reachedByStatement && r.trustAnchors[current.Subject] {
		state.chains = append(state.chains, Chain{
			Configurations: append([]*EntityStatement(nil), configs...),
			Subordinates:   append([]*EntityStatement(nil), subordinates...),
		})
		return
	}

	if len(configs)-1 >= r.maxPathLength {
		state.attempts = append(state.attempts, PathAttempt{
			Path:   pathOf(configs, ""),
			Reason: fmt.Errorf("max path length %d exceeded", r.maxPathLength),
		})
		return
	}

	if len(current.AuthorityHints) == 0 {
		state.attempts = append(state.attempts, PathAttempt{
			Path:   pathOf(configs, ""),
			Reason: fmt.Errorf("path ends at %q, which is not a configured trust anchor and publishes no authority_hints", current.Subject),
		})
		return
	}

	for _, superior := range current.AuthorityHints {
		superior = strings.TrimRight(superior, "/")
		path := pathOf(configs, superior)

		if reason := r.visitEdge(current.Subject, superior, configs); reason != nil {
			state.attempts = append(state.attempts, PathAttempt{Path: path, Reason: reason})
			continue
		}

		superiorCfg, cfgErr := r.configuration(ctx, superior)
		if cfgErr != nil {
			state.attempts = append(state.attempts, PathAttempt{Path: path, Reason: cfgErr})
			continue
		}

		stmtRaw, fetchErr := r.client.FetchEntityStatement(ctx, superior, current.Subject)
		if fetchErr != nil {
			state.attempts = append(state.attempts, PathAttempt{Path: path, Reason: fmt.Errorf("fetching subordinate statement: %v", fetchErr)})
			continue
		}

		stmt, verifyErr := parseAndVerify(stmtRaw, superiorCfg.Keys, superior, current.Subject, r.now())
		if verifyErr != nil {
			state.attempts = append(state.attempts, PathAttempt{Path: path, Reason: verifyErr})
			continue
		}

		r.search(ctx,
			append(configs, superiorCfg),
			append(subordinates, stmt),
			state, true)
	}
}

// visitEdge rejects self-references and repeated entities, i.e. cycles in
// the authority_hints graph.
func (r *Resolver) visitEdge(subject, superior string, configs []*EntityStatement) error {
	if superior == subject {
		return fmt.Errorf("cycle: %q lists itself as an authority hint", superior)
	}
	for _, cfg := range configs {
		if cfg.Subject == superior {
			return fmt.Errorf("cycle detected: %q already present on path %s", superior, strings.Join(pathOf(configs, superior), " -> "))
		}
	}
	return nil
}

func (r *Resolver) configuration(ctx context.Context, entityID string) (*EntityStatement, error) {
	entityID = strings.TrimRight(entityID, "/")
	if cfg, ok := r.configs[entityID]; ok {
		return cfg, nil
	}
	if err, ok := r.configErr[entityID]; ok {
		return nil, err
	}

	raw, err := r.client.FetchEntityConfiguration(ctx, entityID)
	if err != nil {
		err = fmt.Errorf("fetching entity configuration for %q: %v", entityID, err)
		r.configErr[entityID] = err
		return nil, err
	}

	cfg, err := verifyConfiguration(raw, entityID, r.now())
	if err != nil {
		err = fmt.Errorf("verifying entity configuration for %q: %v", entityID, err)
		r.configErr[entityID] = err
		return nil, err
	}

	if cfg.MetadataPolicy != nil {
		// An entity configuration carries no metadata policy; a statement
		// that mixes the two statement kinds is malformed.
		err := fmt.Errorf("entity configuration for %q unexpectedly carries metadata_policy", entityID)
		r.configErr[entityID] = err
		return nil, err
	}

	r.configs[entityID] = cfg
	return cfg, nil
}

func (r *Resolver) buildResult(leaf *EntityStatement, chain Chain, trace *Trace) (*Result, error) {
	leafMetadata, _ := leaf.ProviderMetadata()

	// Subordinate statements are stored leaf -> anchor; policies are
	// composed trust anchor -> leaf.
	contributions := make([]PolicyContribution, 0, len(chain.Subordinates))
	for i := len(chain.Subordinates) - 1; i >= 0; i-- {
		stmt := chain.Subordinates[i]
		var policy Policy
		if stmt.MetadataPolicy != nil {
			policy = stmt.MetadataPolicy[metadataTypeOpenIDProvider]
		}
		contributions = append(contributions, PolicyContribution{
			Issuer:  stmt.Issuer,
			Subject: stmt.Subject,
			Policy:  policy,
		})
	}

	composed, operatorIssuer, err := composePoliciesWithOrigins(contributions)
	if err != nil {
		return nil, err
	}

	finalMetadata, origins, err := applyPolicy(leaf.Subject, leafMetadata, composed, operatorIssuer)
	if err != nil {
		return nil, err
	}

	return &Result{
		ProviderMetadata:    finalMetadata,
		Chain:               chain,
		PolicyContributions: contributions,
		Origins:             origins,
		Trace:               trace,
	}, nil
}

func pathOf(configs []*EntityStatement, next string) []string {
	out := make([]string, 0, len(configs)+1)
	for _, cfg := range configs {
		out = append(out, cfg.Subject)
	}
	if next != "" {
		out = append(out, next)
	}
	return out
}

func sortChains(chains []Chain) {
	sort.Slice(chains, func(i, j int) bool {
		a, b := chains[i].Entities(), chains[j].Entities()
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return strings.Join(a, "|") < strings.Join(b, "|")
	})
}

func joinAttemptReasons(attempts []PathAttempt) string {
	if len(attempts) == 0 {
		return "no authority hints were published"
	}
	seen := map[string]bool{}
	parts := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		line := fmt.Sprintf("[%s] %v", strings.Join(attempt.Path, " -> "), attempt.Reason)
		if !seen[line] {
			seen[line] = true
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, "; ")
}
