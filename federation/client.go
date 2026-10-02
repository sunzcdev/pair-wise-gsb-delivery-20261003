package federation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client abstracts all network access performed by the resolver so that
// fetching is fully replaceable (mocks, caches, in-memory test graphs).
type Client interface {
	// FetchEntityConfiguration returns the signed entity configuration JWT
	// published by entityID at
	// {entityID}/.well-known/openid-federation.
	FetchEntityConfiguration(ctx context.Context, entityID string) (string, error)

	// FetchEntityStatement asks issuer's federation_fetch_endpoint for the
	// subordinate statement about subject:
	// {endpoint}?sub={subject}.
	FetchEntityStatement(ctx context.Context, issuer, subject string) (string, error)
}

// ErrNotFound is returned by a Client when a remote endpoint answers 404
// (or the local equivalent). The resolver treats it as "no statement along
// this edge", while other transport errors are hard failures.
var ErrNotFound = fmt.Errorf("federation: entity statement not found")

const (
	federationWellKnown = "/.well-known/openid-federation"
)

// HTTPClient fetches entity statements over HTTPS following the URL rules of
// OpenID Federation 1.0.
type HTTPClient struct {
	// Client is used for all requests. Defaults to http.DefaultClient.
	Client *http.Client

	// UserAgent is sent as the User-Agent header when non-empty.
	UserAgent string
}

// NewHTTPClient returns an HTTPClient using httpc (or http.DefaultClient
// when nil).
func NewHTTPClient(httpc *http.Client) *HTTPClient {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	return &HTTPClient{Client: httpc}
}

func (c *HTTPClient) client() *http.Client {
	if c.Client == nil {
		return http.DefaultClient
	}
	return c.Client
}

// FetchEntityConfiguration implements Client.
func (c *HTTPClient) FetchEntityConfiguration(ctx context.Context, entityID string) (string, error) {
	endpoint, err := entityConfigurationURL(entityID)
	if err != nil {
		return "", err
	}
	return c.fetchJWT(ctx, endpoint)
}

// FetchEntityStatement implements Client.
func (c *HTTPClient) FetchEntityStatement(ctx context.Context, issuer, subject string) (string, error) {
	endpoint, err := c.lookupFetchEndpoint(ctx, issuer)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("federation: invalid federation_fetch_endpoint %q published by %q: %v", endpoint, issuer, err)
	}
	q := u.Query()
	q.Set("sub", subject)
	u.RawQuery = q.Encode()
	return c.fetchJWT(ctx, u.String())
}

// lookupFetchEndpoint fetches the issuer's entity configuration just to read
// federation_entity.federation_fetch_endpoint. The resolver separately
// verifies the configuration it chains through.
func (c *HTTPClient) lookupFetchEndpoint(ctx context.Context, issuer string) (string, error) {
	raw, err := c.FetchEntityConfiguration(ctx, issuer)
	if err != nil {
		return "", err
	}

	// federation_fetch_endpoint lives outside "jwks"; re-read the payload.
	var envelope struct {
		Metadata map[string]struct {
			FetchEndpoint string `json:"federation_fetch_endpoint"`
		} `json:"metadata"`
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("federation: entity configuration of %q is not a compact JWT", issuer)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("federation: decoding entity configuration payload: %v", err)
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", fmt.Errorf("federation: reading federation_fetch_endpoint from %q: %v", issuer, err)
	}
	if endpoint := envelope.Metadata["federation_entity"].FetchEndpoint; endpoint != "" {
		return endpoint, nil
	}
	return "", fmt.Errorf("federation: %q publishes no federation_fetch_endpoint", issuer)
}

func (c *HTTPClient) fetchJWT(ctx context.Context, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/entity-statement+jwt")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("federation: fetching %s: %v", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("federation: reading %s: %v", endpoint, err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return strings.TrimSpace(string(body)), nil
	case resp.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("%w: GET %s", ErrNotFound, endpoint)
	default:
		return "", fmt.Errorf("federation: GET %s returned %s", endpoint, resp.Status)
	}
}

func entityConfigurationURL(entityID string) (string, error) {
	u, err := url.Parse(entityID)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("federation: entity ID %q is not an absolute URL", entityID)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("federation: entity ID %q must not contain a query or fragment", entityID)
	}
	return strings.TrimRight(entityID, "/") + federationWellKnown, nil
}
