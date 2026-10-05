package acmez_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"code.pfad.fr/check"
	"github.com/letsencrypt/challtestsrv"
	"github.com/letsencrypt/pebble/v2/ca"
	"github.com/letsencrypt/pebble/v2/db"
	"github.com/letsencrypt/pebble/v2/va"
	"github.com/letsencrypt/pebble/v2/wfe"
	"github.com/mholt/acmez/v3"
	"github.com/mholt/acmez/v3/acme"
)

// newDNSChallServer starts a local DNS server that plays the role of the
// customer's DNS zone: TXT records are provisioned "by hand" through it.
func newDNSChallServer(t *testing.T) (*challtestsrv.ChallSrv, string) {
	// find a free port for the DNS server to bind
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	check.Equal(t, nil, err)
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv, err := challtestsrv.New(challtestsrv.Config{
		Log:      log.New(io.Discard, "", 0),
		DNSAddrs: []string{addr},
	})
	check.Equal(t, nil, err)
	srv.Run()
	t.Cleanup(srv.Shutdown)
	return srv, addr
}

// newPebbleServerWithResolver starts a local ACME CA (Pebble) that validates
// challenges using the given DNS resolver.
func newPebbleServerWithResolver(t *testing.T, resolverAddr string) *httptest.Server {
	t.Setenv("PEBBLE_VA_NOSLEEP", "1")
	logger := log.New(io.Discard, "test", log.Llongfile)
	db := db.NewMemoryStore()
	profiles := map[string]ca.Profile{
		"default": {
			Description:    "The default profile",
			ValidityPeriod: 0,
		},
	}
	ca := ca.New(logger, db, "", "rsa", 0, 1, profiles)
	va := va.New(logger, 0, 0, true, resolverAddr, db)
	wfeImpl := wfe.New(logger, db, va, ca, nil, true, false, 0, 0)

	s := httptest.NewUnstartedServer(wfeImpl.Handler())
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// TestStepwiseIssuance demonstrates the stepwise issuance flow against a
// local ACME stand-in: the order is begun, the process "exits" (the state is
// serialized and the client discarded), a fresh client reloads the state,
// advances the order once the DNS change has been made by hand, and finally
// collects the certificate. The certificate private key never leaves the
// caller.
func TestStepwiseIssuance(t *testing.T) {
	challSrv, resolverAddr := newDNSChallServer(t)
	s := newPebbleServerWithResolver(t, resolverAddr)

	// each "run" of the program uses a brand-new client with no memory of
	// previous runs; only the persisted state carries over
	newClient := func() *acmez.Client {
		return &acmez.Client{Client: &acme.Client{
			Directory:  s.URL + wfe.DirectoryPath,
			HTTPClient: s.Client(),
			Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		}}
	}
	ctx := t.Context()

	// the account is long-lived and managed by the caller
	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check.Equal(t, nil, err)
	account, err := newClient().NewAccount(ctx, acme.Account{
		TermsOfServiceAgreed: true,
		PrivateKey:           accountKey,
	})
	check.Equal(t, nil, err)

	// the certificate private key is generated and kept by the caller; it is
	// only needed to sign the CSR when collecting the certificate
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check.Equal(t, nil, err)
	csr, err := acmez.NewCSR(certKey, []string{"example.com"})
	check.Equal(t, nil, err)

	// step 1: begin the issuance; this returns the order status and the
	// challenges to complete, without blocking
	state, err := newClient().BeginIssuance(ctx, account, acme.Order{
		Identifiers: []acme.Identifier{{Type: "dns", Value: "example.com"}},
	})
	check.Equal(t, nil, err)
	check.Equal(t, acme.StatusPending, state.OrderStatus)
	check.Equal(t, 1, len(state.Authorizations))
	check.Equal(t, false, state.Complete())

	// advancing without having completed anything is harmless: it only
	// refreshes the status from the CA and never creates a new order
	err = newClient().AdvanceIssuance(ctx, account, state)
	check.Equal(t, nil, err)
	check.Equal(t, acme.StatusPending, state.OrderStatus)

	// the operator manually creates the TXT record requested by the CA
	var dns01 *acme.Challenge
	for _, chal := range state.PendingChallenges() {
		if chal.Type == acme.ChallengeTypeDNS01 {
			c := chal.Challenge
			dns01 = &c
		}
	}
	if dns01 == nil {
		t.Fatal("CA did not offer a dns-01 challenge")
	}
	challSrv.AddDNSTXTRecord(dns01.DNS01TXTRecordName(), dns01.DNS01KeyAuthorization())
	err = state.Authorizations[0].MarkReady(acme.ChallengeTypeDNS01)
	check.Equal(t, nil, err)

	// the process exits here; only the serialized state is kept
	saved, err := json.Marshal(state)
	check.Equal(t, nil, err)

	// ... later, in a new run: reload the state and advance the order
	var restored acmez.IssuanceState
	check.Equal(t, nil, json.Unmarshal(saved, &restored))
	err = newClient().AdvanceIssuance(ctx, account, &restored)
	check.Equal(t, nil, err)
	check.Equal(t, true, restored.Complete())
	check.Equal(t, acme.StatusReady, restored.OrderStatus)
	check.Equal(t, state.OrderLocation, restored.OrderLocation) // same order, not a new one

	// step 3: collect the certificate; the CSR (and thus the private key)
	// is only needed now
	certs, err := newClient().CollectCertificate(ctx, account, &restored, acmez.StaticCSR(csr))
	check.Equal(t, nil, err)
	check.Equal(t, acme.StatusValid, restored.OrderStatus)
	if len(certs) == 0 {
		t.Fatal("expected at least one certificate chain")
	}
	block, _ := pem.Decode(certs[0].ChainPEM)
	if block == nil {
		t.Fatal("failed to decode certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	check.Equal(t, nil, err)
	check.EqualSlice(t, []string{"example.com"}, cert.DNSNames)

	// collecting again is safe: the already-issued certificate is simply
	// downloaded once more, without finalizing a new order
	again, err := newClient().CollectCertificate(ctx, account, &restored, nil)
	check.Equal(t, nil, err)
	check.Equal(t, len(certs), len(again))
}

// TestStepwiseIssuanceChallengeFails shows how failed authorizations are
// surfaced when resuming: the order cannot be reused and a new issuance must
// be begun.
func TestStepwiseIssuanceChallengeFails(t *testing.T) {
	_, resolverAddr := newDNSChallServer(t)
	s := newPebbleServerWithResolver(t, resolverAddr)
	client := acmez.Client{Client: &acme.Client{
		Directory:  s.URL + wfe.DirectoryPath,
		HTTPClient: s.Client(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	ctx := t.Context()

	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check.Equal(t, nil, err)
	account, err := client.NewAccount(ctx, acme.Account{
		TermsOfServiceAgreed: true,
		PrivateKey:           accountKey,
	})
	check.Equal(t, nil, err)

	state, err := client.BeginIssuance(ctx, account, acme.Order{
		Identifiers: []acme.Identifier{{Type: "dns", Value: "example.org"}},
	})
	check.Equal(t, nil, err)

	// the operator marks the challenge ready, but the TXT record was never
	// actually created, so the CA's validation fails
	err = state.Authorizations[0].MarkReady(acme.ChallengeTypeDNS01)
	check.Equal(t, nil, err)
	err = client.AdvanceIssuance(ctx, account, state)
	if err == nil {
		t.Fatal("expected advancing with a failing challenge to return an error")
	}
	check.Equal(t, acme.StatusInvalid, state.Authorizations[0].Status)

	// the failed authorization cannot be revived; advancing again reports
	// that a new issuance is required instead of silently retrying
	err = client.AdvanceIssuance(ctx, account, state)
	if err == nil || !strings.Contains(err.Error(), "begin a new issuance") {
		t.Fatalf("expected error requiring a new issuance, got: %v", err)
	}

	// and there is no certificate to collect from the failed order
	_, err = client.CollectCertificate(ctx, account, state, nil)
	if err == nil {
		t.Fatal("expected collecting from a failed order to return an error")
	}
}
