// Copyright 2026 Matthew Holt
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package acmez_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"

	"code.pfad.fr/check"
	"github.com/mholt/acmez/v3"
	"github.com/mholt/acmez/v3/acme"
)

// TestStagedIssuance demonstrates the multi-step issuance flow against a
// local ACME stand-in (pebble): the order is begun, the process "exits"
// (state is serialized), the state is reloaded by a fresh client, the
// caller provisions the challenges out-of-band, the order is advanced,
// and finally the certificate is collected.
func TestStagedIssuance(t *testing.T) {
	solverPort, solver := newHttpSolver(t)

	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check.Equal(t, nil, err)

	// the certificate private key is generated and kept by the caller;
	// it is never stored in the issuance state
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check.Equal(t, nil, err)
	sans := []string{"127.0.0.1"}

	ctx := context.Background()

	// --- step 1: begin the order, then "exit" -------------------------

	client := acmez.Client{
		Client: newAcmeClient(t, solverPort, 0),
		ChallengeSolvers: map[string]acmez.Solver{
			acme.ChallengeTypeHTTP01: solver,
		},
	}
	account, err := client.NewAccount(ctx, acme.Account{
		TermsOfServiceAgreed: true,
		PrivateKey:           accountKey,
	})
	check.Equal(t, nil, err)

	csr, err := acmez.NewCSR(certKey, sans)
	check.Equal(t, nil, err)
	params, err := acmez.OrderParametersFromCSR(account, csr)
	check.Equal(t, nil, err)

	state, err := client.BeginOrder(ctx, params)
	check.Equal(t, nil, err)
	check.Equal(t, acme.StatusPending, state.Order.Status)
	if len(state.Challenges) != 1 {
		t.Fatalf("expected 1 pending challenge, got %d", len(state.Challenges))
	}
	check.Equal(t, acme.ChallengeTypeHTTP01, state.Challenges[0].Challenge.Type)
	orderLocation := state.OrderLocation

	// simulate the process exiting: only the serialized state survives
	saved, err := json.Marshal(state)
	check.Equal(t, nil, err)

	// --- step 2: reload and provision the challenges ------------------

	var resumed acmez.IssuanceState
	check.Equal(t, nil, json.Unmarshal(saved, &resumed))
	check.Equal(t, orderLocation, resumed.OrderLocation)

	// the caller completes the challenges out-of-band (here: provision
	// the http-01 resource; in the manual DNS case, this is where a
	// human would create the TXT record described by the challenge)
	for _, pc := range resumed.Challenges {
		check.Equal(t, nil, solver.Present(ctx, pc.Challenge))
	}

	// --- step 3: advance the order with a fresh client ----------------

	resumedClient := acmez.Client{Client: newAcmeClient(t, solverPort, 0)}
	check.Equal(t, nil, resumedClient.AdvanceOrder(ctx, account, &resumed))

	// the same order must be reused, never a new one
	check.Equal(t, orderLocation, resumed.OrderLocation)
	check.Equal(t, acme.StatusReady, resumed.Order.Status)
	check.Equal(t, acme.StatusValid, resumed.Challenges[0].Challenge.Status)

	// advancing again is a no-op once all challenges are valid
	check.Equal(t, nil, resumedClient.AdvanceOrder(ctx, account, &resumed))
	check.Equal(t, orderLocation, resumed.OrderLocation)

	// --- step 4: collect the certificate ------------------------------

	certChains, err := resumedClient.CollectCertificate(ctx, account, &resumed)
	check.Equal(t, nil, err)
	check.Equal(t, orderLocation, resumed.OrderLocation)
	check.Equal(t, acme.StatusValid, resumed.Order.Status)
	if len(certChains) == 0 {
		t.Fatal("expected at least one certificate chain")
	}

	block, _ := pem.Decode(certChains[0].ChainPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	check.Equal(t, nil, err)
	if got := cert.IPAddresses[0].String(); got != sans[0] {
		t.Fatalf("expected cert SAN %s, got %s", sans[0], got)
	}

	// collecting again from the final state downloads the same order's
	// certificate without creating a new order
	_, err = resumedClient.CollectCertificate(ctx, account, &resumed)
	check.Equal(t, nil, err)
	check.Equal(t, orderLocation, resumed.OrderLocation)
}

// TestStagedCollectBeforeAdvance ensures the final step refuses to run
// while challenges are still outstanding.
func TestStagedCollectBeforeAdvance(t *testing.T) {
	solverPort, _ := newHttpSolver(t)

	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check.Equal(t, nil, err)
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check.Equal(t, nil, err)

	ctx := context.Background()
	client := acmez.Client{Client: newAcmeClient(t, solverPort, 0)}
	account, err := client.NewAccount(ctx, acme.Account{
		TermsOfServiceAgreed: true,
		PrivateKey:           accountKey,
	})
	check.Equal(t, nil, err)

	csr, err := acmez.NewCSR(certKey, []string{"127.0.0.1"})
	check.Equal(t, nil, err)
	params, err := acmez.OrderParametersFromCSR(account, csr)
	check.Equal(t, nil, err)

	state, err := client.BeginOrder(ctx, params)
	check.Equal(t, nil, err)

	_, err = client.CollectCertificate(ctx, account, state)
	if err == nil {
		t.Fatal("expected error when collecting before advancing, got nil")
	}
}
