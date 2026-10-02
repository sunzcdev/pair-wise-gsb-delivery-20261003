package federation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPClientFetchesAndResolves verifies resolution over real HTTP by
// serving an in-memory graph from an httptest server.
func TestHTTPClientFetchesAndResolves(t *testing.T) {
	client := NewLocalClient()

	srv := httptest.NewServer(nil)
	defer srv.Close()

	origin := srv.URL
	anchor := origin + "/anchor"
	inter := origin + "/inter"
	leaf := origin + "/op"

	entities, err := AddEntities(client, testNow,
		&EntityInput{ID: anchor, FederationMetadata: []byte(`{"federation_fetch_endpoint":"` + origin + `/fetch/anchor"}`)},
		&EntityInput{
			ID:                 inter,
			AuthorityHints:     []string{anchor},
			FederationMetadata: []byte(`{"federation_fetch_endpoint":"` + origin + `/fetch/inter"}`),
		},
		&EntityInput{
			ID:             leaf,
			AuthorityHints: []string{inter},
			ProviderMetadata: []byte(`{
				"issuer":"` + leaf + `",
				"scopes_supported":["openid"],
				"grant_types_supported":["authorization_code"]
			}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	mustTestStatement(t, client, entities[anchor], inter, `{"scopes_supported":{"subset_of":["openid","profile"]}}`)
	mustTestStatement(t, client, entities[inter], leaf, `{"scopes_supported":{"add":["profile"]}}`)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, federationWellKnown):
			entityID := strings.TrimSuffix(r.URL.Path, federationWellKnown)
			raw, err := client.FetchEntityConfiguration(r.Context(), origin+entityID)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/entity-statement+jwt")
			_, _ = w.Write([]byte(raw))
		case strings.HasPrefix(r.URL.Path, "/fetch/"):
			issuer := origin + "/" + strings.TrimPrefix(r.URL.Path, "/fetch/")
			raw, err := client.FetchEntityStatement(r.Context(), issuer, r.URL.Query().Get("sub"))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/entity-statement+jwt")
			_, _ = w.Write([]byte(raw))
		default:
			http.NotFound(w, r)
		}
	})
	srv.Config.Handler = mux

	httpClient := NewHTTPClient(srv.Client())
	result, err := NewResolver(httpClient, anchor).WithClock(testClock()).ResolveProvider(context.Background(), leaf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.ProviderMetadata), `"openid","profile"`) {
		t.Fatalf("metadata policy was not applied: %s", result.ProviderMetadata)
	}

	// The leaf alone is not a valid trust anchor even over HTTP.
	if _, err := NewResolver(httpClient, leaf).WithClock(testClock()).ResolveProvider(context.Background(), leaf); err == nil {
		t.Fatal("resolution unexpectedly succeeded with the discovery URL as sole anchor")
	}
}

func TestHTTPClient404IsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := NewHTTPClient(srv.Client()).FetchEntityConfiguration(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want not found", err)
	}
}
