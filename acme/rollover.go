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
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrKeyRolloverUncertain is returned when the outcome of an account key
// rollover cannot be determined: the keyChange request failed in a way
// that leaves it unknown whether the CA committed the new key (for
// example, the response was lost in transit), and the verification
// probes could not establish which key the CA now associates with the
// account. While a KeyRollover is in this state it refuses to hand out
// any key and refuses to roll again, so an uncertain rollover can never
// silently cascade into repeated key changes. Call KeyRollover.Resolve
// to run the verification process again once the CA is reachable.
var ErrKeyRolloverUncertain = errors.New("account key rollover outcome is uncertain")

// KeyRollover coordinates use of one ACME account's private key across
// any number of goroutines, so that a key rollover has well-defined
// boundaries with respect to requests signed with the old and new keys.
//
// Goroutines that use the account (for example, to issue certificates)
// must obtain the current key through AcquireKey and use it for the
// whole request, instead of reading the account's PrivateKey field
// directly. This defines three phases that the CA can observe:
//
//  1. Old-key phase: requests acquire the old key freely.
//  2. Rollover: Rollover first stops handing out keys and waits for
//     all in-flight old-key requests to finish, so no old-key request
//     can reach the CA after the keyChange. It then sends exactly one
//     keyChange request; while it is in flight, no other account
//     request is sent at all.
//  3. New-key phase: once the rollover is known to be committed (or
//     aborted), waiting goroutines unblock and acquire whichever key
//     is current.
//
// The hardest case is a keyChange whose response is lost: the CA may
// or may not have committed the new key. Rollover then runs a
// controlled verification process — read-only POST-as-GET probes of
// the account, signed with the candidate keys — instead of blindly
// retrying the keyChange. If the probes prove the new key was
// committed, the new key is adopted; if they prove it was not, the old
// key is kept. If they cannot prove either (for example, the CA is
// unreachable), Rollover returns ErrKeyRolloverUncertain and the
// coordinator enters an uncertain state: it hands out no keys and
// rejects further rollovers until Resolve determines the account's
// actual key. A rollover is never automatically retried.
//
// The zero value is not usable; create one with NewKeyRollover.
type KeyRollover struct {
	mu         sync.Mutex
	key        crypto.Signer // the account's current key
	pendingKey crypto.Signer // the new key of an unverified rollover
	inFlight   int           // requests currently using the current key
	rolling    bool          // a rollover or resolution is in progress
	uncertain  bool          // the last rollover's outcome is unknown
	changed    chan struct{} // closed on every state change; lazily created
}

// NewKeyRollover returns a KeyRollover that hands out initialKey as the
// account's current key. initialKey must be the key the CA currently
// associates with the account.
func NewKeyRollover(initialKey crypto.Signer) *KeyRollover {
	return &KeyRollover{key: initialKey}
}

// AcquireKey returns the account's current key for signing one logical
// request (including any internal retries of that request), along with
// a release function that must be called when the request is done.
//
// While a rollover is in progress, AcquireKey blocks until the rollover
// resolves and then returns whichever key is current, so callers never
// observe a mixed state. If the last rollover's outcome is uncertain,
// AcquireKey fails with ErrKeyRolloverUncertain rather than guessing
// which key the CA expects.
func (kr *KeyRollover) AcquireKey(ctx context.Context) (key crypto.Signer, release func(), err error) {
	for {
		kr.mu.Lock()
		if kr.uncertain {
			kr.mu.Unlock()
			return nil, nil, fmt.Errorf("cannot issue requests: %w", ErrKeyRolloverUncertain)
		}
		if !kr.rolling {
			kr.inFlight++
			key := kr.key
			kr.mu.Unlock()
			var once sync.Once
			return key, func() {
				once.Do(func() {
					kr.mu.Lock()
					kr.inFlight--
					kr.broadcastLocked()
					kr.mu.Unlock()
				})
			}, nil
		}
		changed := kr.changeChanLocked()
		kr.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

// CurrentKey returns the account's current key. The second return value
// reports whether that key is known to match the CA's state: it is false
// while a rollover is in progress or has an uncertain outcome.
func (kr *KeyRollover) CurrentKey() (crypto.Signer, bool) {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	return kr.key, !kr.rolling && !kr.uncertain
}

// Rollover changes the account's key to newKey on the CA, coordinated
// with all goroutines using this KeyRollover, as described on the type.
// The account's Location field must be set. The coordinator's current
// key is used as the old key; the account's PrivateKey field is ignored.
//
// If the keyChange is definitively rejected by the CA, the old key
// remains current and the CA's error is returned. If the keyChange's
// outcome is ambiguous, a verification process determines which key the
// CA actually committed; only if that, too, is inconclusive does this
// return ErrKeyRolloverUncertain. In no case is the keyChange retried
// automatically.
func (kr *KeyRollover) Rollover(ctx context.Context, client *Client, account Account, newKey crypto.Signer) error {
	if newKey == nil {
		return fmt.Errorf("new key is nil")
	}
	if account.Location == "" {
		return fmt.Errorf("account has no location URL")
	}

	kr.mu.Lock()
	if kr.uncertain {
		kr.mu.Unlock()
		return fmt.Errorf("a previous key rollover has an uncertain outcome; resolve it with Resolve before rolling again: %w", ErrKeyRolloverUncertain)
	}
	if kr.rolling {
		kr.mu.Unlock()
		return fmt.Errorf("a key rollover is already in progress")
	}
	kr.rolling = true

	// phase 1 ends here: no new old-key requests may start, and we wait
	// for the ones already in flight to finish, so the CA cannot observe
	// an old-key request after the keyChange
	for kr.inFlight > 0 {
		changed := kr.changeChanLocked()
		kr.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			kr.mu.Lock()
			kr.rolling = false
			kr.broadcastLocked()
			kr.mu.Unlock()
			return ctx.Err()
		}
		kr.mu.Lock()
	}
	oldKey := kr.key
	kr.mu.Unlock()

	// critical window: no account requests are in flight and AcquireKey
	// blocks new ones, so this single keyChange is the only request the
	// CA can observe from this account right now
	account.PrivateKey = oldKey
	_, err := client.AccountKeyRollover(ctx, account, newKey)
	if err == nil {
		kr.commit(newKey)
		return nil
	}

	// a definitive ACME problem (4xx) means the CA rejected the
	// keyChange, so the account key is certainly unchanged
	var problem Problem
	if errors.As(err, &problem) && problem.Status >= 400 && problem.Status < 500 {
		kr.abort()
		return err
	}

	// anything else (network error, 5xx, lost response) is ambiguous:
	// verify which key the CA has instead of retrying the keyChange; the
	// probes run on a detached context because a canceled caller context
	// is often exactly why the outcome is unknown
	verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	outcome, verifyErr := verifyAccountKey(verifyCtx, client, account, oldKey, newKey)
	cancel()

	switch outcome {
	case keyRolloverCommitted:
		kr.commit(newKey)
		return nil
	case keyRolloverNotCommitted:
		kr.abort()
		return err
	default:
		kr.mu.Lock()
		kr.pendingKey = newKey
		kr.uncertain = true
		kr.rolling = false
		kr.broadcastLocked()
		kr.mu.Unlock()
		return fmt.Errorf("%w: keyChange failed (%v) and verification was inconclusive (%v)", ErrKeyRolloverUncertain, err, verifyErr)
	}
}

// Resolve determines which key the CA currently associates with the
// account after a rollover ended with ErrKeyRolloverUncertain, using the
// same read-only verification probes. On success it clears the uncertain
// state and returns the account's actual current key, which AcquireKey
// will hand out from then on. If the CA still cannot be queried
// conclusively, the state remains uncertain and ErrKeyRolloverUncertain
// is returned. If the state is not uncertain, Resolve is a no-op and
// returns the current key.
func (kr *KeyRollover) Resolve(ctx context.Context, client *Client, account Account) (crypto.Signer, error) {
	kr.mu.Lock()
	for kr.rolling {
		changed := kr.changeChanLocked()
		kr.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		kr.mu.Lock()
	}
	if !kr.uncertain {
		key := kr.key
		kr.mu.Unlock()
		return key, nil
	}
	// exclude concurrent rollovers and resolutions while we probe
	kr.rolling = true
	oldKey, newKey := kr.key, kr.pendingKey
	kr.mu.Unlock()

	outcome, verifyErr := verifyAccountKey(ctx, client, account, oldKey, newKey)

	kr.mu.Lock()
	defer kr.mu.Unlock()
	kr.rolling = false
	switch outcome {
	case keyRolloverCommitted:
		kr.key = newKey
		kr.pendingKey = nil
		kr.uncertain = false
	case keyRolloverNotCommitted:
		kr.pendingKey = nil
		kr.uncertain = false
	}
	kr.broadcastLocked()
	if kr.uncertain {
		return nil, fmt.Errorf("%w: verification inconclusive: %v", ErrKeyRolloverUncertain, verifyErr)
	}
	return kr.key, nil
}

// commit adopts newKey as the current key and unblocks waiting callers.
func (kr *KeyRollover) commit(newKey crypto.Signer) {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	kr.key = newKey
	kr.rolling = false
	kr.broadcastLocked()
}

// abort keeps the old key as the current key and unblocks waiting callers.
func (kr *KeyRollover) abort() {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	kr.rolling = false
	kr.broadcastLocked()
}

// broadcastLocked wakes all goroutines waiting for a state change.
// kr.mu must be held.
func (kr *KeyRollover) broadcastLocked() {
	if kr.changed != nil {
		close(kr.changed)
		kr.changed = nil
	}
}

// changeChanLocked returns a channel that is closed on the next state
// change. kr.mu must be held.
func (kr *KeyRollover) changeChanLocked() chan struct{} {
	if kr.changed == nil {
		kr.changed = make(chan struct{})
	}
	return kr.changed
}

// keyRolloverOutcome is the verified result of a keyChange request.
type keyRolloverOutcome int

const (
	// keyRolloverUnknown means it could not be determined whether the
	// CA committed the new key.
	keyRolloverUnknown keyRolloverOutcome = iota
	// keyRolloverCommitted means the CA proved it uses the new key.
	keyRolloverCommitted
	// keyRolloverNotCommitted means the CA proved it still uses the old key.
	keyRolloverNotCommitted
)

// verifyAccountKey determines which key the CA currently associates with
// the account by sending read-only POST-as-GET requests to the account
// URL signed with the candidate keys. Probing is safe to repeat because
// POST-as-GET has no side effects, unlike another keyChange.
func verifyAccountKey(ctx context.Context, client *Client, account Account, oldKey, newKey crypto.Signer) (keyRolloverOutcome, error) {
	// if the new key is accepted, the rollover definitely committed
	definitive, accepted, err := probeAccountKey(ctx, client, account, newKey)
	if !definitive {
		return keyRolloverUnknown, fmt.Errorf("probing with new key: %w", err)
	}
	if accepted {
		return keyRolloverCommitted, nil
	}

	// the new key was definitively rejected; if the old key is still
	// accepted, the rollover definitely did not commit
	definitive, accepted, err = probeAccountKey(ctx, client, account, oldKey)
	if !definitive {
		return keyRolloverUnknown, fmt.Errorf("probing with old key: %w", err)
	}
	if accepted {
		return keyRolloverNotCommitted, nil
	}

	return keyRolloverUnknown, fmt.Errorf("CA rejected both the old and the new account key")
}

// probeAccountKey asks the CA whether key is the account's current key
// by sending a POST-as-GET request to the account URL signed with key.
// definitive is false if the CA's answer could not be obtained (network
// or server-internal errors), in which case accepted is meaningless.
func probeAccountKey(ctx context.Context, client *Client, account Account, key crypto.Signer) (definitive, accepted bool, err error) {
	var acct Account
	_, err = client.httpPostJWS(ctx, key, account.Location, account.Location, nil, &acct)
	if err == nil {
		return true, true, nil
	}
	var problem Problem
	if errors.As(err, &problem) && problem.Status >= 400 && problem.Status < 500 {
		// the CA gave a definitive answer: this key is not the account key
		return true, false, nil
	}
	return false, false, err
}
