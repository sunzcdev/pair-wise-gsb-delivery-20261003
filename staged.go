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

package acmez

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mholt/acmez/v3/acme"
)

// IssuanceState is the resumable state of a staged (multi-step) certificate
// issuance. It is created by BeginOrder, advanced by AdvanceOrder, and
// completed by CollectCertificate.
//
// The state is safe to serialize (e.g. as JSON) and persist between steps,
// so a process may exit after BeginOrder and another process (or a later
// run of the same one) may reload the state and continue where it left off.
// It contains no secret key material: the certificate private key never
// leaves the caller (only the public CSR is stored here), and the account
// key is supplied by the caller to each step.
type IssuanceState struct {
	// The ACME order as last observed. Use OrderLocation to re-fetch
	// the current order object from the server.
	Order         acme.Order `json:"order"`
	OrderLocation string     `json:"order_location"`

	// The DER-encoded CSR that will be sent to the server when the
	// order is finalized. A CSR is public data; it is stored here so
	// that the finalization step does not need the private key.
	CSR []byte `json:"csr"`

	// The challenges that must be completed before the order can be
	// finalized. The caller is responsible for provisioning each one
	// (for example, creating the DNS TXT record described by the
	// challenge) before calling AdvanceOrder.
	Challenges []PendingChallenge `json:"challenges"`
}

// PendingChallenge describes one challenge, selected from those offered by
// the server, that the caller must complete out-of-band. Helper methods on
// acme.Challenge such as DNS01TXTRecordName, DNS01KeyAuthorization, and
// HTTP01ResourcePath describe exactly what to provision.
type PendingChallenge struct {
	// The identifier this challenge proves control over.
	Identifier acme.Identifier `json:"identifier"`

	// The URL of the authorization object this challenge belongs to.
	AuthorizationURL string `json:"authorization_url"`

	// The selected challenge, including its token and key authorization.
	// Its Status field is updated as AdvanceOrder makes progress.
	Challenge acme.Challenge `json:"challenge"`
}

// BeginOrder starts a staged certificate issuance: it creates an order with
// the ACME server and returns the order status along with the challenges
// that must be completed before the certificate can be issued. Unlike
// ObtainCertificate, it does not solve any challenges and does not block;
// it simply returns the state so the caller can complete the challenges
// at its own pace (for example, when DNS records must be changed manually).
//
// The returned state can be persisted and later resumed with AdvanceOrder
// and CollectCertificate, possibly by a different process. The certificate
// private key is only needed to create the CSR in params; it is not stored
// in the state and remains solely in the caller's custody.
//
// If the client has solvers configured in ChallengeSolvers, a challenge
// type with a configured solver is preferred for each authorization;
// otherwise the first offered challenge is selected.
func (c *Client) BeginOrder(ctx context.Context, params OrderParameters) (*IssuanceState, error) {
	if err := validateOrderParams(params); err != nil {
		return nil, err
	}

	// get the CSR now, since the private key might not be available
	// to the later steps; the CSR itself is not secret
	csr, err := params.CSR.CSR(ctx, params.Identifiers)
	if err != nil {
		return nil, fmt.Errorf("getting CSR from source: %w", err)
	}
	if csr == nil {
		return nil, errors.New("source did not provide CSR")
	}

	order, err := orderFromParams(params)
	if err != nil {
		return nil, err
	}

	// create the ACME order
	order, err = c.Client.NewOrder(ctx, params.Account, order)
	if err != nil {
		return nil, fmt.Errorf("creating new order: %w", err)
	}

	// ensure the order identifiers match the CSR
	if err := validateOrderIdentifiers(&order, csr); err != nil {
		return nil, fmt.Errorf("validating order identifiers: %w", err)
	}

	state := &IssuanceState{
		Order:         order,
		OrderLocation: order.Location,
		CSR:           csr.Raw,
	}

	// collect the challenges the caller needs to complete
	for _, authzURL := range order.Authorizations {
		authz, err := c.Client.GetAuthorization(ctx, params.Account, authzURL)
		if err != nil {
			return nil, fmt.Errorf("getting authorization at %s: %w", authzURL, err)
		}

		switch authz.Status {
		case acme.StatusValid:
			// the identifier is already authorized (e.g. cached by the
			// server from a previous order); nothing to do for it
			if c.Logger != nil {
				c.Logger.Info("authorization already valid",
					slog.String("identifier", authz.IdentifierValue()),
					slog.String("authz_url", authz.Location))
			}
			continue
		case acme.StatusPending:
			// this is the expected case; handled below
		default:
			return nil, fmt.Errorf("authz %s has unexpected status %s; cannot begin order", authzURL, authz.Status)
		}

		chal, err := c.selectChallenge(authz)
		if err != nil {
			return nil, err
		}

		state.Challenges = append(state.Challenges, PendingChallenge{
			Identifier:       authz.Identifier,
			AuthorizationURL: authzURL,
			Challenge:        chal,
		})
	}

	if c.Logger != nil {
		c.Logger.Info("order created; waiting on challenges",
			slog.String("order", order.Location),
			slog.Int("pending_challenges", len(state.Challenges)))
	}

	return state, nil
}

// AdvanceOrder resumes a staged issuance begun by BeginOrder and pushes it
// forward: it tells the server to validate each challenge that is still
// pending and waits for the validations to complete. It should be called
// only after the caller has provisioned all the challenges described in
// the state (for example, after the DNS records have been created).
//
// AdvanceOrder never creates a new order; it always reconciles the saved
// state with the real state on the server first. Authorizations that the
// server reports as already valid are skipped; authorizations or challenges
// that have failed, and orders or authorizations that have expired, are
// reported as errors (a failed or expired order cannot be reused; begin a
// new one with BeginOrder).
//
// The state is updated in place to reflect the latest server state, so the
// caller should persist it again after each call. It is safe to call
// AdvanceOrder multiple times; calls after all challenges are valid are
// no-ops.
func (c *Client) AdvanceOrder(ctx context.Context, account acme.Account, state *IssuanceState) error {
	if state == nil || state.OrderLocation == "" {
		return errors.New("missing or incomplete issuance state")
	}

	// reconcile with the real order state on the server
	order, err := c.Client.GetOrder(ctx, account, acme.Order{Location: state.OrderLocation})
	if err != nil {
		return fmt.Errorf("getting order %s: %w", state.OrderLocation, err)
	}
	state.Order = order

	switch order.Status {
	case acme.StatusInvalid:
		return fmt.Errorf("order %s is invalid; begin a new order: %w", order.Location, orderError(order))
	case acme.StatusValid, acme.StatusReady, acme.StatusProcessing:
		// nothing left to advance; the certificate can be collected
		// (or is already being issued)
		return nil
	case acme.StatusPending:
		// continue below
	default:
		return fmt.Errorf("unrecognized order status: %s", order.Status)
	}

	if !order.Expires.IsZero() && time.Now().After(order.Expires) {
		return fmt.Errorf("order %s expired at %s; begin a new order", order.Location, order.Expires)
	}

	// initiate and await validation of each pending challenge
	for i := range state.Challenges {
		pc := &state.Challenges[i]

		// check the real state of the authorization on the server
		authz, err := c.Client.GetAuthorization(ctx, account, pc.AuthorizationURL)
		if err != nil {
			return fmt.Errorf("getting authorization at %s: %w", pc.AuthorizationURL, err)
		}

		switch authz.Status {
		case acme.StatusValid:
			// already validated (perhaps by an earlier AdvanceOrder
			// call, or reused from a previous order)
			pc.Challenge.Status = acme.StatusValid
			continue
		case acme.StatusPending:
			// continue below
		case acme.StatusInvalid:
			return fmt.Errorf("authorization for %s failed: %w", authz.IdentifierValue(), authzError(authz))
		default:
			// expired, deactivated, or revoked
			return fmt.Errorf("authorization for %s is %s; begin a new order", authz.IdentifierValue(), authz.Status)
		}

		// find the tracked challenge within the current authz object
		chal := findChallenge(authz, pc.Challenge)
		if chal == nil {
			return fmt.Errorf("challenge %s (%s) no longer offered by authorization %s",
				pc.Challenge.Type, pc.Challenge.URL, pc.AuthorizationURL)
		}
		pc.Challenge = *chal

		switch chal.Status {
		case acme.StatusValid:
			continue
		case acme.StatusInvalid:
			return fmt.Errorf("challenge %s for %s failed: %w", chal.Type, authz.IdentifierValue(), challengeError(*chal))
		case acme.StatusPending:
			// tell the server the challenge is ready to be validated
			pc.Challenge, err = c.Client.InitiateChallenge(ctx, account, *chal)
			if err != nil {
				return fmt.Errorf("initiating challenge %s for %s: %w", chal.Type, authz.IdentifierValue(), err)
			}
		case acme.StatusProcessing:
			// already initiated (e.g. by an earlier call); just poll
		}

		// wait for the server to finish validating
		authz, err = c.Client.PollAuthorization(ctx, account, authz)
		if final := findChallenge(authz, pc.Challenge); final != nil {
			pc.Challenge = *final
		}
		if err != nil {
			return fmt.Errorf("validating %s: %w", authz.IdentifierValue(), err)
		}

		if c.Logger != nil {
			c.Logger.Info("authorization finalized",
				slog.String("identifier", authz.IdentifierValue()),
				slog.String("authz_status", authz.Status))
		}
	}

	// refresh the order one last time so the caller sees the latest status
	order, err = c.Client.GetOrder(ctx, account, acme.Order{Location: state.OrderLocation})
	if err != nil {
		return fmt.Errorf("getting order %s: %w", state.OrderLocation, err)
	}
	state.Order = order

	return nil
}

// CollectCertificate completes a staged issuance: it finalizes the order
// with the CSR captured by BeginOrder (if not already finalized) and
// downloads the issued certificate chains. All challenges must have been
// validated first (see AdvanceOrder). The certificate private key is not
// needed for this step; it remains in the caller's custody and is only
// needed to use the certificate.
//
// The state is updated in place with the final order state.
func (c *Client) CollectCertificate(ctx context.Context, account acme.Account, state *IssuanceState) ([]acme.Certificate, error) {
	if state == nil || state.OrderLocation == "" {
		return nil, errors.New("missing or incomplete issuance state")
	}
	if len(state.CSR) == 0 {
		return nil, errors.New("issuance state has no CSR")
	}

	// reconcile with the real order state on the server
	order, err := c.Client.GetOrder(ctx, account, acme.Order{Location: state.OrderLocation})
	if err != nil {
		return nil, fmt.Errorf("getting order %s: %w", state.OrderLocation, err)
	}
	state.Order = order

	switch order.Status {
	case acme.StatusValid:
		// already finalized; fall through to the download below
	case acme.StatusReady:
		csr, err := x509.ParseCertificateRequest(state.CSR)
		if err != nil {
			return nil, fmt.Errorf("parsing stored CSR: %w", err)
		}
		if err := validateOrderIdentifiers(&order, csr); err != nil {
			return nil, fmt.Errorf("validating order identifiers: %w", err)
		}

		// finalize the order, which requests the CA to issue the certificate
		order, err = c.Client.FinalizeOrder(ctx, account, order, csr.Raw)
		if err != nil {
			return nil, fmt.Errorf("finalizing order %s: %w", order.Location, err)
		}
		state.Order = order
	case acme.StatusPending:
		return nil, fmt.Errorf("order %s still has pending authorizations; complete the challenges and call AdvanceOrder first", order.Location)
	case acme.StatusInvalid:
		return nil, fmt.Errorf("order %s is invalid; begin a new order: %w", order.Location, orderError(order))
	default:
		return nil, fmt.Errorf("order %s has unexpected status: %s", order.Location, order.Status)
	}

	if order.Certificate == "" {
		return nil, fmt.Errorf("order %s has no certificate URL (status: %s)", order.Location, order.Status)
	}

	// download the certificate
	certChains, err := c.Client.GetCertificateChain(ctx, account, order.Certificate)
	if err != nil {
		return nil, fmt.Errorf("downloading certificate chain from %s: %w (order=%s)",
			order.Certificate, err, order.Location)
	}

	if c.Logger != nil {
		c.Logger.Info("successfully downloaded available certificate chains",
			slog.Int("count", len(certChains)))
	}

	return certChains, nil
}

// selectChallenge chooses a challenge from those offered by the authz. A
// challenge type with a configured solver is preferred; otherwise the first
// offered challenge is returned.
func (c *Client) selectChallenge(authz acme.Authorization) (acme.Challenge, error) {
	for _, chal := range authz.Challenges {
		if solver, ok := c.ChallengeSolvers[chal.Type]; ok && solver != nil {
			return chal, nil
		}
	}
	if len(authz.Challenges) > 0 {
		return authz.Challenges[0], nil
	}
	return acme.Challenge{}, fmt.Errorf("%s: no challenges offered by authorization %s",
		authz.IdentifierValue(), authz.Location)
}

// findChallenge returns the challenge within authz that matches the tracked
// challenge, identified by URL (falling back to type), or nil if there is
// no match.
func findChallenge(authz acme.Authorization, tracked acme.Challenge) *acme.Challenge {
	for i, chal := range authz.Challenges {
		if tracked.URL != "" && chal.URL == tracked.URL {
			return &authz.Challenges[i]
		}
	}
	for i, chal := range authz.Challenges {
		if chal.Type == tracked.Type {
			return &authz.Challenges[i]
		}
	}
	return nil
}

// authzError returns the first problem recorded on the authz's challenges,
// or a generic error if none is present.
func authzError(authz acme.Authorization) error {
	for _, chal := range authz.Challenges {
		if chal.Error != nil {
			return *chal.Error
		}
	}
	return errors.New("no error details provided by server")
}

// orderError returns the problem recorded on the order, or a generic error
// if none is present.
func orderError(order acme.Order) error {
	if order.Error != nil {
		return *order.Error
	}
	return errors.New("no error details provided by server")
}

// challengeError returns the problem recorded on the challenge, or a
// generic error if none is present.
func challengeError(chal acme.Challenge) error {
	if chal.Error != nil {
		return *chal.Error
	}
	return errors.New("no error details provided by server")
}
