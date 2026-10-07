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

package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// RolloverPhase describes the outcome of a key rollover attempt.
type RolloverPhase string

const (
	// RolloverCommitted means the CA confirmed that the new key is now
	// associated with the account, either from a successful keyChange
	// response or from a successful reconciliation probe.
	RolloverCommitted RolloverPhase = "committed"

	// RolloverRejected means the CA definitively rejected the keyChange
	// request and the old key is still the account key.
	RolloverRejected RolloverPhase = "rejected"

	// RolloverUncertain means the client could not determine whether the
	// CA applied the key change (typically because the keyChange response
	// was lost) and reconciliation could not resolve the question either.
	RolloverUncertain RolloverPhase = "uncertain"
)

// ErrRolloverUncertain is returned when it is unknown whether the CA
// applied an account key change. A shared account in this state refuses
// to sign further requests until the caller explicitly resolves the
// situation with Client.Resolve; signing with either the old or the new
// key might hit an account whose key is unknown, and blindly re-sending
// the keyChange could cycle the key several times.
var ErrRolloverUncertain = errors.New("account key rollover state is uncertain; resolve before continuing")

// RolloverResult reports the result of a key rollover.
type RolloverResult struct {
	// Account is the account as it stands after the rollover attempt.
	// Its PrivateKey is the new key when Phase is RolloverCommitted, and
	// the old key otherwise.
	Account Account

	// Phase is the outcome of the rollover attempt.
	Phase RolloverPhase

	// Err carries the keyChange error when Phase is not RolloverCommitted.
	// For RolloverUncertain it wraps ErrRolloverUncertain.
	Err error
}

// UncertainRollover is the error reported by a RolloverResult whose
// Phase is RolloverUncertain. The CA may have applied the key change
// without the client ever seeing the response, so both the old and the
// new key are retained for a later, explicit reconciliation.
type UncertainRollover struct {
	// Location is the account URL whose key is in question.
	Location string

	// OldKey is the account key in effect before the keyChange request.
	OldKey crypto.Signer

	// NewKey is the key the CA was asked to switch to.
	NewKey crypto.Signer

	// Cause is the transport or probe error that left the state unknown.
	Cause error
}

func (e *UncertainRollover) Error() string {
	return fmt.Sprintf("key rollover for account %s may have been applied by the server but its response was lost: %v", e.Location, e.Cause)
}

// Unwrap returns ErrRolloverUncertain so callers can detect this class
// of error with errors.Is, and chains the underlying cause.
func (e *UncertainRollover) Unwrap() []error {
	return []error{ErrRolloverUncertain, e.Cause}
}

// NewAccountKeyManager wraps a registered account so that many goroutines
// can share it safely across a key rollover. The returned value is safe
// for concurrent use and implements crypto.Signer: pass it to NewOrder,
// FinalizeOrder, GetCertificateChain and the other account-taking Client
// methods exactly as the underlying Account would be passed.
//
// While a rollover is in progress, new requests block until one of the
// well-defined boundaries is reached:
//
//   - requests already signing with the old key are allowed to finish
//     (the CA still accepts them until it processes the keyChange);
//   - the keyChange request is serialized against those requests;
//   - requests after the boundary exclusively use the new key.
func NewAccountKeyManager(account Account) *AccountKeyManager {
	m := &AccountKeyManager{Account: account, key: account.PrivateKey}
	// The account hands out a stable proxy instead of the concrete key.
	// Account values are copied into every issuance call, so coordination
	// has to ride along inside Account.PrivateKey itself: the proxy takes
	// a read lease for the whole JWS request and delegates to whichever
	// concrete key is current at that boundary.
	m.Account.PrivateKey = &accountKeyHandle{m: m}
	m.gate.init()
	return m
}

// AccountKeyManager coordinates access to an account key shared by
// concurrent signing goroutines. A zero-value AccountKeyManager is not
// usable; create one with NewAccountKeyManager.
//
// AccountKeyManager must not be copied after first use.
type AccountKeyManager struct {
	Account

	// key is the concrete account key currently in effect; it is only
	// read or written while holding the gate (read lease or writer).
	key crypto.Signer

	gate accountGate

	// uncertain holds the pending old/new key pair while a rollover is
	// in the uncertain state; it is nil at all other times.
	uncertain *UncertainRollover
}

// CurrentKey returns the concrete account key in effect right now. It is
// intended for callers that need the actual key material (for example to
// build the inner/outer JWS of a rollover); ordinary issuance goes
// through the manager's Account and its coordinating proxy.
func (m *AccountKeyManager) CurrentKey() crypto.Signer {
	m.gate.mu.Lock()
	defer m.gate.mu.Unlock()
	return m.key
}

// currentKey takes a read lease on the account gate and returns the
// key to sign with. The returned release function must be called when
// the key is no longer needed.
func (m *AccountKeyManager) currentKey(ctx context.Context) (crypto.Signer, error) {
	if err := m.gate.beginRead(ctx); err != nil {
		return nil, err
	}
	if m.uncertain != nil {
		m.gate.endRead()
		return nil, fmt.Errorf("refusing to sign for account %s: %w", m.Location, ErrRolloverUncertain)
	}
	return m.key, nil
}

// leaseKey is used by Client.httpPostJWS to hold one consistent key for
// the whole JWS request (including its badNonce retries).
func (m *AccountKeyManager) leaseKey(ctx context.Context) (crypto.Signer, func(), error) {
	key, err := m.currentKey(ctx)
	if err != nil {
		return nil, func() {}, err
	}
	return key, m.gate.endRead, nil
}

// accountKeyHandle is the stable crypto.Signer stored on the managed
// Account. It never holds key material itself; each operation takes a
// read lease and delegates to the manager's current concrete key. The
// handle stays valid across rollovers even though Account is copied by
// value into every issuance call.
type accountKeyHandle struct {
	m *AccountKeyManager
}

func (h *accountKeyHandle) Public() crypto.PublicKey {
	key, err := h.m.currentKey(context.Background())
	if err != nil {
		return nil
	}
	defer h.m.gate.endRead()
	return key.Public()
}

func (h *accountKeyHandle) Sign(randReader io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	key, err := h.m.currentKey(context.Background())
	if err != nil {
		return nil, err
	}
	defer h.m.gate.endRead()
	// jwsSign renders ECDSA signatures as raw R||S rather than ASN.1, so
	// the handle must do the same when delegating to an ecdsa key.
	if ecKey, ok := key.(*ecdsa.PrivateKey); ok {
		r, s, err := ecdsa.Sign(randReader, ecKey, digest)
		if err != nil {
			return nil, err
		}
		size := ecKey.Params().BitSize / 8
		if ecKey.Params().BitSize%8 > 0 {
			size++
		}
		sig := make([]byte, size*2)
		rb, sb := r.Bytes(), s.Bytes()
		copy(sig[size-len(rb):], rb)
		copy(sig[size*2-len(sb):], sb)
		return sig, nil
	}
	return key.Sign(randReader, digest, opts)
}

// leaseKey lets httpPostJWS pin one concrete key for a whole request.
func (h *accountKeyHandle) leaseKey(ctx context.Context) (crypto.Signer, func(), error) {
	return h.m.leaseKey(ctx)
}

// Rollover changes the account key with full coordination: in-flight
// requests signed with the old key drain first, then exactly one
// keyChange is sent, then all subsequent requests use newKey.
//
// If the keyChange response is lost, the result has Phase
// RolloverUncertain instead of triggering automatic retries; call
// Resolve to perform a controlled reconciliation once connectivity is
// restored.
func (c *Client) Rollover(ctx context.Context, manager *AccountKeyManager, newKey crypto.Signer) (RolloverResult, error) {
	if err := c.provision(ctx); err != nil {
		res := RolloverResult{Account: manager.Account, Phase: RolloverRejected, Err: err}
		return res, err
	}

	if sameSignerKey(manager.CurrentKey(), newKey) {
		return RolloverResult{Account: manager.Account, Phase: RolloverCommitted}, nil
	}

	oldKey := manager.CurrentKey()
	uncertain := &UncertainRollover{Location: manager.Location, OldKey: oldKey, NewKey: newKey}

	// Cross the boundary: block until every in-flight old-key request has
	// released its lease, and hold the gate closed across the keyChange and
	// any reconciliation so no request can sign with the wrong key.
	endWrite, err := manager.gate.beginWrite(ctx)
	if err != nil {
		res := RolloverResult{Account: manager.Account, Phase: RolloverRejected, Err: err}
		return res, err
	}
	defer endWrite()

	phase, cause := c.performKeyChange(ctx, manager.Account, oldKey, newKey)
	switch phase {
	case RolloverCommitted:
		manager.key = newKey
		manager.uncertain = nil
		return RolloverResult{Account: manager.Account, Phase: RolloverCommitted}, nil
	case RolloverRejected:
		manager.uncertain = nil
		res := RolloverResult{Account: manager.Account, Phase: RolloverRejected, Err: cause}
		return res, res.Err
	}

	// The response was lost. Run the controlled reconciliation described in
	// RFC 8555 §7.3.5 while still holding the exclusive gate.
	switch c.reconcile(ctx, manager.Location, oldKey, newKey) {
	case RolloverCommitted:
		manager.key = newKey
		manager.uncertain = nil
		return RolloverResult{Account: manager.Account, Phase: RolloverCommitted}, nil
	case RolloverRejected:
		manager.uncertain = nil
		res := RolloverResult{Account: manager.Account, Phase: RolloverRejected, Err: cause}
		return res, res.Err
	}

	// Truly undeterminable: park the account and hand both keys back.
	manager.uncertain = uncertain
	res := RolloverResult{
		Account: manager.Account,
		Phase:   RolloverUncertain,
		Err:     uncertain,
	}
	return res, res.Err
}

// Resolve performs a controlled reconciliation after an uncertain
// rollover. It probes the account with the new key and, if necessary,
// the old key to determine which key the CA currently holds, exactly
// once per probe; it never re-sends a keyChange. The returned result's
// Phase is RolloverCommitted, RolloverRejected or RolloverUncertain.
func (c *Client) Resolve(ctx context.Context, manager *AccountKeyManager) (RolloverResult, error) {
	if err := c.provision(ctx); err != nil {
		return RolloverResult{Account: manager.Account, Phase: RolloverUncertain, Err: ErrRolloverUncertain}, err
	}

	endWrite, err := manager.gate.beginWrite(ctx)
	if err != nil {
		return RolloverResult{Account: manager.Account, Phase: RolloverUncertain, Err: err}, err
	}
	defer endWrite()

	pending := manager.uncertain
	if pending == nil {
		return RolloverResult{Account: manager.Account, Phase: RolloverCommitted}, nil
	}

	switch c.reconcile(ctx, pending.Location, pending.OldKey, pending.NewKey) {
	case RolloverCommitted:
		manager.key = pending.NewKey
		manager.uncertain = nil
		return RolloverResult{Account: manager.Account, Phase: RolloverCommitted}, nil
	case RolloverRejected:
		manager.key = pending.OldKey
		manager.uncertain = nil
		return RolloverResult{Account: manager.Account, Phase: RolloverRejected, Err: pending.Cause}, nil
	}

	res := RolloverResult{Account: manager.Account, Phase: RolloverUncertain, Err: pending}
	return res, res.Err
}

// performKeyChange sends a single keyChange request. Unlike ordinary
// requests it is never retried (RFC 8555 §7.3.5: a keyChange response
// cannot be safely replayed blindly): a lost response is reported as
// uncertain instead.
func (c *Client) performKeyChange(ctx context.Context, account Account, oldKey, newKey crypto.Signer) (RolloverPhase, error) {
	oldPublicKeyJWK, err := jwkEncode(oldKey.Public())
	if err != nil {
		return RolloverRejected, fmt.Errorf("encoding old account key: %v", err)
	}

	innerJWS, err := jwsEncodeJSON(keyChangeRequest{
		Account: account.Location,
		OldKey:  []byte(oldPublicKeyJWK),
	}, newKey, "", "", c.dir.KeyChange)
	if err != nil {
		return RolloverRejected, fmt.Errorf("encoding inner JWS: %v", err)
	}

	if err := c.postKeyChangeOnce(ctx, oldKey, account.Location, json.RawMessage(innerJWS)); err != nil {
		if isDefinitiveRolloverRejection(err) {
			return RolloverRejected, fmt.Errorf("rolling key on server: %w", err)
		}
		return RolloverUncertain, fmt.Errorf("keyChange response was lost: %w", err)
	}

	return RolloverCommitted, nil
}

// isDefinitiveRolloverRejection reports whether err is a CA response that
// unambiguously means the account key did not change. An ACME Problem
// document (a status the server chose to describe) is definitive because
// the request reached the server and was answered; a transport, timeout
// or malformed response is not.
func isDefinitiveRolloverRejection(err error) bool {
	var problem Problem
	return errors.As(err, &problem)
}

// reconcile determines which key the CA currently associates with the
// account using POST-as-GET probes, as suggested by RFC 8555 §7.3.5.
// Each probe is signed independently and neither one replays the
// keyChange request.
func (c *Client) reconcile(ctx context.Context, location string, oldKey, newKey crypto.Signer) RolloverPhase {
	// "If the new key verifies, then the key change has occurred." §7.3.5
	if c.probeAccountKey(ctx, location, newKey) {
		return RolloverCommitted
	}
	// "If the old key verifies, then the key change has not occurred." §7.3.5
	if c.probeAccountKey(ctx, location, oldKey) {
		return RolloverRejected
	}
	// Neither probe gave a definitive answer (network down, etc.); the
	// caller must not guess and must not re-send the keyChange.
	return RolloverUncertain
}

// probeAccountKey returns true if a POST-as-GET to the account URL
// authenticated with key succeeds. A CA rejection is indistinguishable
// from a failed probe here; reconcile() settles that by probing the
// other key as well.
func (c *Client) probeAccountKey(ctx context.Context, location string, key crypto.Signer) bool {
	_, err := c.httpPostJWS(ctx, key, location, location, nil, nil)
	return err == nil
}

// sameSignerKey reports whether two signers expose the same public key,
// comparing JWK thumbprints so RSA and ECDSA keys compare uniformly.
func sameSignerKey(a, b crypto.Signer) bool {
	ta, err := jwkThumbprint(a.Public())
	if err != nil {
		return false
	}
	tb, err := jwkThumbprint(b.Public())
	if err != nil {
		return false
	}
	return ta == tb
}

// keyLeaser is implemented by coordinated account keys. httpPostJWS
// holds the returned lease for the whole request so the signing key
// cannot change between encoding and transmission (or across badNonce
// retries).
type keyLeaser interface {
	leaseKey(ctx context.Context) (crypto.Signer, func(), error)
}
