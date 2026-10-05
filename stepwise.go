package acmez

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mholt/acmez/v3/acme"
)

// IssuanceState is a checkpoint of a stepwise (manual) certificate issuance.
// It contains everything needed to resume an in-progress ACME order after the
// process has exited: the order location, its last known status, and the state
// of each authorization including the challenges offered by the CA.
//
// The state is safe to serialize (e.g. as JSON) and persist between runs. It
// deliberately contains no private key material: the ACME account is supplied
// separately to each step, and the certificate private key is only needed by
// the caller to produce a CSR when collecting the certificate.
type IssuanceState struct {
	// The URL of the order resource, used to resume the order with the CA.
	OrderLocation string `json:"orderLocation"`

	// The last known status of the order ("pending", "ready", "processing",
	// "valid", or "invalid"), refreshed from the CA on every step.
	OrderStatus string `json:"orderStatus"`

	// When the order expires, as last reported by the CA.
	OrderExpires time.Time `json:"orderExpires,omitempty"`

	// The authorizations belonging to the order, with their challenges.
	Authorizations []AuthorizationState `json:"authorizations"`
}

// AuthorizationState is the checkpoint of a single authorization within an
// IssuanceState.
type AuthorizationState struct {
	// The URL of the authorization resource.
	Location string `json:"location"`

	// The identifier this authorization is for.
	Identifier acme.Identifier `json:"identifier"`

	// Whether the identifier is a wildcard.
	Wildcard bool `json:"wildcard,omitempty"`

	// The last known status of the authorization, refreshed from the CA
	// on every call to AdvanceIssuance.
	Status string `json:"status"`

	// When the authorization expires, as last reported by the CA.
	Expires time.Time `json:"expires,omitempty"`

	// The challenges offered by the CA for this authorization. For
	// pending authorizations, the caller completes one of these
	// out-of-band and marks it ready.
	Challenges []ManualChallenge `json:"challenges"`
}

// ManualChallenge is a challenge offered by the CA, annotated with caller
// input for the stepwise flow. The embedded acme.Challenge carries everything
// needed to provision the response out-of-band (for example,
// DNS01TXTRecordName() and DNS01KeyAuthorization() for dns-01).
type ManualChallenge struct {
	acme.Challenge

	// Ready must be set to true by the caller once the challenge response
	// has been fully provisioned (e.g. the DNS record has been created and
	// has propagated). AdvanceIssuance only asks the CA to validate
	// challenges that are marked ready.
	Ready bool `json:"ready,omitempty"`
}

// Complete reports whether every authorization in the state is valid, meaning
// the order can be finalized by CollectCertificate.
func (s *IssuanceState) Complete() bool {
	if len(s.Authorizations) == 0 {
		return false
	}
	for _, authz := range s.Authorizations {
		if authz.Status != acme.StatusValid {
			return false
		}
	}
	return true
}

// PendingChallenges returns the challenges of all authorizations that are not
// yet valid. These are the challenges the caller still has to choose from and
// complete before the issuance can advance.
func (s *IssuanceState) PendingChallenges() []ManualChallenge {
	var pending []ManualChallenge
	for _, authz := range s.Authorizations {
		if authz.Status == acme.StatusValid {
			continue
		}
		pending = append(pending, authz.Challenges...)
	}
	return pending
}

// MarkReady marks the challenge of the given type as ready for validation.
// Call this after the challenge response has been provisioned out-of-band.
// It returns an error if the CA did not offer a challenge of that type.
func (a *AuthorizationState) MarkReady(challengeType string) error {
	for i := range a.Challenges {
		if a.Challenges[i].Type == challengeType {
			a.Challenges[i].Ready = true
			return nil
		}
	}
	return fmt.Errorf("no %s challenge offered for %s", challengeType, a.Identifier.Value)
}

// refresh replaces the authorization's status and challenges with the ones
// currently reported by the CA, preserving the caller's Ready marks (matched
// by challenge URL).
func (a *AuthorizationState) refresh(authz acme.Authorization) {
	ready := make(map[string]bool)
	for _, chal := range a.Challenges {
		if chal.Ready {
			ready[chal.URL] = true
		}
	}
	a.Status = authz.Status
	a.Expires = authz.Expires
	a.Challenges = make([]ManualChallenge, 0, len(authz.Challenges))
	for _, chal := range authz.Challenges {
		a.Challenges = append(a.Challenges, ManualChallenge{
			Challenge: chal,
			Ready:     ready[chal.URL],
		})
	}
}

// firstReadyChallenge returns the first challenge the caller marked ready
// that the CA has not already rejected, or nil if there is none.
func (a *AuthorizationState) firstReadyChallenge() *ManualChallenge {
	for i := range a.Challenges {
		if a.Challenges[i].Ready && a.Challenges[i].Status != acme.StatusInvalid {
			return &a.Challenges[i]
		}
	}
	return nil
}

// BeginIssuance creates a new ACME order and returns its state along with the
// challenges that must be completed before a certificate can be issued. It is
// the first step of a stepwise (manual) issuance, for use when challenges are
// fulfilled out-of-band - for example, when DNS records are changed by hand -
// and the blocking ObtainCertificate flow is not suitable.
//
// The returned state can be serialized and persisted; the process may exit
// and resume later with AdvanceIssuance. No private keys are stored in the
// state: the account is passed to each step separately, and the certificate
// private key is only needed to produce a CSR for CollectCertificate.
//
// The order must list at least one identifier. The account must be registered
// and have a "valid" status.
func (c *Client) BeginIssuance(ctx context.Context, account acme.Account, order acme.Order) (*IssuanceState, error) {
	if account.Status != acme.StatusValid {
		return nil, fmt.Errorf("account status is not valid: %s", account.Status)
	}
	if len(order.Identifiers) == 0 {
		return nil, errors.New("order does not list any identifiers")
	}
	if order.NotBefore != nil {
		// partial seconds are not permitted
		trunc := order.NotBefore.Truncate(time.Second)
		order.NotBefore = &trunc
	}
	if order.NotAfter != nil {
		// partial seconds are not permitted
		trunc := order.NotAfter.Truncate(time.Second)
		order.NotAfter = &trunc
	}

	order, err := c.Client.NewOrder(ctx, account, order)
	if err != nil {
		return nil, fmt.Errorf("creating new order: %w", err)
	}

	state := &IssuanceState{
		OrderLocation: order.Location,
		OrderStatus:   order.Status,
		OrderExpires:  order.Expires,
	}
	for _, authzURL := range order.Authorizations {
		authz, err := c.Client.GetAuthorization(ctx, account, authzURL)
		if err != nil {
			return nil, fmt.Errorf("getting authorization at %s: %w", authzURL, err)
		}
		authzState := AuthorizationState{
			Location:   authzURL,
			Identifier: authz.Identifier,
			Wildcard:   authz.Wildcard,
		}
		authzState.refresh(authz)
		state.Authorizations = append(state.Authorizations, authzState)
	}

	if c.Logger != nil {
		c.Logger.Info("began stepwise issuance; complete the challenges, then advance",
			slog.String("order", state.OrderLocation),
			slog.Int("authorizations", len(state.Authorizations)))
	}

	return state, nil
}

// AdvanceIssuance resumes a stepwise issuance from a previously persisted
// state and pushes it toward completion. It never creates a new order; it
// always reconciles the state with the CA first:
//
//   - Authorizations that are already valid are left alone.
//   - Authorizations that are still pending have their ready-marked challenge
//     initiated with the CA (unless the CA already started validating it),
//     which is then polled to completion.
//   - Authorizations that are invalid, expired, deactivated, or revoked make
//     the order unusable, and an error is returned; start over with
//     BeginIssuance.
//
// If a pending authorization has no challenge marked ready yet, it is simply
// left pending and no error is returned, so AdvanceIssuance can also be used
// to poll for the latest status. Inspect the state (or Complete) to decide
// what to do next. Once all authorizations are valid, collect the certificate
// with CollectCertificate.
func (c *Client) AdvanceIssuance(ctx context.Context, account acme.Account, state *IssuanceState) error {
	if state == nil || state.OrderLocation == "" {
		return errors.New("missing or empty issuance state")
	}

	order, err := c.Client.GetOrder(ctx, account, acme.Order{Location: state.OrderLocation})
	if err != nil {
		return fmt.Errorf("getting order %s: %w", state.OrderLocation, err)
	}
	state.OrderStatus = order.Status
	state.OrderExpires = order.Expires

	switch order.Status {
	case acme.StatusInvalid:
		if order.Error != nil {
			return fmt.Errorf("order is invalid; begin a new issuance: %w", order.Error)
		}
		return errors.New("order is invalid; begin a new issuance")
	case acme.StatusValid, acme.StatusReady, acme.StatusProcessing:
		// nothing to advance; the order is awaiting finalization or issuance,
		// so it can be collected right away
		return nil
	}

	// the order is pending; reconcile its authorizations with the CA
	if len(state.Authorizations) == 0 {
		for _, authzURL := range order.Authorizations {
			state.Authorizations = append(state.Authorizations, AuthorizationState{Location: authzURL})
		}
	}

	for i := range state.Authorizations {
		authzState := &state.Authorizations[i]
		authz, err := c.Client.GetAuthorization(ctx, account, authzState.Location)
		if err != nil {
			return fmt.Errorf("getting authorization at %s: %w", authzState.Location, err)
		}
		authzState.refresh(authz)

		switch authz.Status {
		case acme.StatusValid:
			if c.Logger != nil {
				c.Logger.Info("authorization already valid",
					slog.String("identifier", authz.IdentifierValue()),
					slog.String("authz", authzState.Location))
			}
			continue
		case acme.StatusPending:
			// handled below
		default:
			// invalid, expired, deactivated, or revoked: this authorization
			// (and thus the order) can no longer be used
			return fmt.Errorf("authorization for %s is %s; the order can no longer be used, begin a new issuance (authz=%s)",
				authz.IdentifierValue(), authz.Status, authzState.Location)
		}

		chal := authzState.firstReadyChallenge()
		if chal == nil {
			if c.Logger != nil {
				c.Logger.Info("authorization still pending; no challenge marked ready yet",
					slog.String("identifier", authz.IdentifierValue()),
					slog.String("authz", authzState.Location))
			}
			continue
		}

		// only initiate the challenge if the CA hasn't already started (or
		// finished) validating it, e.g. because a previous run was
		// interrupted after initiating it
		if chal.Status == acme.StatusPending {
			updated, err := c.Client.InitiateChallenge(ctx, account, chal.Challenge)
			if err != nil {
				return fmt.Errorf("initiating %s challenge for %s: %w", chal.Type, authz.IdentifierValue(), err)
			}
			chal.Challenge = updated
		}

		updatedAuthz, err := c.Client.PollAuthorization(ctx, account, authz)
		authzState.refresh(updatedAuthz)
		if err != nil {
			return fmt.Errorf("[%s] %w", authz.IdentifierValue(), err)
		}

		if c.Logger != nil {
			c.Logger.Info("authorization finalized",
				slog.String("identifier", authz.IdentifierValue()),
				slog.String("authz_status", authzState.Status))
		}
	}

	// reflect the latest order status in the state
	order, err = c.Client.GetOrder(ctx, account, acme.Order{Location: state.OrderLocation})
	if err != nil {
		return fmt.Errorf("refreshing order %s: %w", state.OrderLocation, err)
	}
	state.OrderStatus = order.Status
	state.OrderExpires = order.Expires

	return nil
}

// CollectCertificate finalizes the order of a stepwise issuance and downloads
// the resulting certificate chains. The state must have been advanced far
// enough that all authorizations are valid (see AdvanceIssuance and
// IssuanceState.Complete).
//
// The CSR is obtained from csrSource at this point, so the certificate
// private key never has to leave the caller's custody during the earlier
// steps. If the order was already finalized (e.g. a previous run was
// interrupted during or after finalization), the CSR source is not consulted
// and the certificate is simply downloaded.
func (c *Client) CollectCertificate(ctx context.Context, account acme.Account, state *IssuanceState, csrSource CSRSource) ([]acme.Certificate, error) {
	if state == nil || state.OrderLocation == "" {
		return nil, errors.New("missing or empty issuance state")
	}

	order, err := c.Client.GetOrder(ctx, account, acme.Order{Location: state.OrderLocation})
	if err != nil {
		return nil, fmt.Errorf("getting order %s: %w", state.OrderLocation, err)
	}

	switch order.Status {
	case acme.StatusReady:
		if csrSource == nil {
			return nil, errors.New("missing CSR source")
		}
		csr, err := csrSource.CSR(ctx, order.Identifiers)
		if err != nil {
			return nil, fmt.Errorf("getting CSR from source: %w", err)
		}
		if csr == nil {
			return nil, errors.New("source did not provide CSR")
		}
		if err := validateOrderIdentifiers(&order, csr); err != nil {
			return nil, fmt.Errorf("validating order identifiers: %w", err)
		}
		order, err = c.Client.FinalizeOrder(ctx, account, order, csr.Raw)
		if err != nil {
			return nil, fmt.Errorf("finalizing order %s: %w", order.Location, err)
		}
	case acme.StatusProcessing:
		// a previous run was interrupted after finalizing; wait for issuance
		order, err = c.waitForOrder(ctx, account, order)
		if err != nil {
			return nil, err
		}
	}

	state.OrderStatus = order.Status
	state.OrderExpires = order.Expires

	switch order.Status {
	case acme.StatusValid:
		// certificate is ready to download
	case acme.StatusPending:
		return nil, errors.New("order is still pending; complete the challenges and advance the issuance first")
	case acme.StatusInvalid:
		if order.Error != nil {
			return nil, fmt.Errorf("order is invalid: %w", order.Error)
		}
		return nil, errors.New("order is invalid")
	default:
		return nil, fmt.Errorf("order is not ready for collection (status: %s)", order.Status)
	}

	if order.Certificate == "" {
		return nil, errors.New("order is valid but the CA did not provide a certificate URL")
	}

	certChains, err := c.Client.GetCertificateChain(ctx, account, order.Certificate)
	if err != nil {
		return nil, fmt.Errorf("downloading certificate chain from %s: %w (order=%s)",
			order.Certificate, err, order.Location)
	}

	if c.Logger != nil {
		c.Logger.Info("successfully downloaded available certificate chains",
			slog.Int("count", len(certChains)),
			slog.String("order", order.Location))
	}

	return certChains, nil
}

// waitForOrder polls the order until it leaves the "processing" state.
func (c *Client) waitForOrder(ctx context.Context, account acme.Account, order acme.Order) (acme.Order, error) {
	interval := c.PollInterval
	if interval == 0 {
		interval = 250 * time.Millisecond
	}
	timeout := c.PollTimeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	start := time.Now()
	for time.Since(start) < timeout {
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return order, ctx.Err()
		}

		var err error
		order, err = c.Client.GetOrder(ctx, account, order)
		if err != nil {
			return order, fmt.Errorf("polling order status: %w", err)
		}
		if order.Status != acme.StatusProcessing {
			return order, nil
		}
	}

	return order, errors.New("order took too long")
}
