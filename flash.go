package scs

import (
	"context"
	"errors"

	"github.com/alexedwards/scs/v2/memstore"
)

var (
	_ FlashStore    = (*memstore.MemStore)(nil)
	_ FlashCtxStore = (*memstore.MemStore)(nil)
)

// ErrFlashNotSupported is returned by SessionManager.Flash when the configured
// session store does not implement FlashStore (or FlashCtxStore). Such stores
// cannot provide a cross-request one-time guarantee, so Flash deliberately
// fails instead of falling back to behavior (like Pop) that could deliver the
// same value to more than one concurrent request.
var ErrFlashNotSupported = errors.New("scs: the session store does not support atomic flash operations")

// FlashFunc is invoked by a FlashStore.Flash call with the data currently
// stored for a session token. If the token does not exist (or has expired)
// then found is false, b is nil and the returned byte slice is discarded.
//
// An implementation should decode b, extract and delete any one-time values,
// re-encode the remaining data and return it. The read-modify-write performed
// by the store is atomic with respect to all other store operations, so the
// same value can never be handed to two concurrent callers.
type FlashFunc func(b []byte, found bool) (newB []byte, err error)

// FlashStore is implemented by session stores which can atomically read and
// modify a single session's data. Stores that cannot perform the
// read-modify-write atomically (for example because they only expose separate
// find and commit operations) must NOT implement this interface.
type FlashStore interface {
	Store

	// Flash atomically retrieves the data for the given session token, invokes
	// fn with it, and stores the byte slice returned by fn, preserving the
	// existing expiry time. If the token is not found or has expired, fn is
	// invoked with found set to false and no data is written.
	Flash(token string, fn func(b []byte, found bool) (newB []byte, err error)) error
}

// FlashCtxStore is the same as FlashStore, except that it also exposes a
// context.Context aware variant.
type FlashCtxStore interface {
	FlashStore

	// FlashCtx is the same as FlashStore.Flash, except it takes a
	// context.Context.
	FlashCtx(ctx context.Context, token string, fn func(b []byte, found bool) (newB []byte, err error)) error
}

// Flash atomically extracts a one-time value (a "flash message") from the
// session so that it is delivered to exactly one request.
//
// Unlike Pop, which only removes the value from the current request's
// in-memory copy of the session, Flash performs an atomic read-modify-write
// against the session store. Two parallel requests that both loaded the
// session before the value was consumed cannot both receive it: only one call
// returns found == true. Once a value has been flashed, any subsequent commit
// of a stale copy of the session (including copies loaded by other requests
// before the flash) has the value stripped out, so it can never be written
// back to the store.
//
// The return values are:
//   - val: the extracted value (nil when found is false).
//   - found: true if a value was present and extracted, false if there is no
//     such value.
//   - err: a non-nil error if the store operation failed. In that case nothing
//     is removed locally or in the store, so the call may be retried. If the
//     configured store cannot perform an atomic flash, err is
//     ErrFlashNotSupported.
func (s *SessionManager) Flash(ctx context.Context, key string) (val interface{}, found bool, err error) {
	sd := s.getSessionDataFromContext(ctx)

	sd.mu.Lock()
	defer sd.mu.Unlock()

	token := sd.token

	// If the session has never been committed then no other request can hold a
	// copy of it, so a local pop is already safe and atomic.
	if token == "" {
		v, ok := sd.values[key]
		if !ok {
			return nil, false, nil
		}
		delete(sd.values, key)
		sd.status = Modified
		return v, true, nil
	}

	var extracted interface{}
	var exists bool

	fn := FlashFunc(func(b []byte, stored bool) ([]byte, error) {
		if !stored {
			return b, nil
		}

		deadline, values, decodeErr := s.Codec.Decode(b)
		if decodeErr != nil {
			return b, decodeErr
		}

		v, ok := values[key]
		if !ok {
			return b, nil
		}

		delete(values, key)

		newB, encodeErr := s.Codec.Encode(deadline, values)
		if encodeErr != nil {
			return b, encodeErr
		}

		extracted = v
		exists = true
		return newB, nil
	})

	storeToken := token
	if s.HashTokenInStore {
		storeToken = hashToken(token)
	}

	var flashErr error
	if c, ok := s.Store.(FlashCtxStore); ok {
		flashErr = c.FlashCtx(ctx, storeToken, fn)
	} else if c, ok := s.Store.(FlashStore); ok {
		flashErr = c.Flash(storeToken, fn)
	} else {
		return nil, false, ErrFlashNotSupported
	}

	if flashErr != nil {
		return nil, false, flashErr
	}

	if exists {
		// Keep this request's in-memory copy consistent and record the consumed
		// key so that stale copies (including in other requests) cannot write
		// the value back when they are committed.
		delete(sd.values, key)
		s.recordFlashConsumedLocked(token, key)
	}

	return extracted, exists, nil
}

// stripConsumedFlashesLocked removes every value that has been flashed out of
// this session token from the supplied session data. It must be called with
// sd.mu held.
func (s *SessionManager) stripConsumedFlashesLocked(sd *sessionData) {
	if sd.token == "" {
		return
	}

	s.flashMu.Lock()
	defer s.flashMu.Unlock()

	keys, ok := s.flashes[sd.token]
	if !ok {
		return
	}
	for key := range keys {
		delete(sd.values, key)
	}
}

// recordFlashConsumedLocked records that key has been atomically flashed from
// the session identified by token. It must be called with sd.mu held.
func (s *SessionManager) recordFlashConsumedLocked(token, key string) {
	s.flashMu.Lock()
	defer s.flashMu.Unlock()

	if s.flashes == nil {
		s.flashes = make(map[string]map[string]struct{})
	}

	keys, ok := s.flashes[token]
	if !ok {
		keys = make(map[string]struct{})
		s.flashes[token] = keys
	}
	keys[key] = struct{}{}
}

// clearConsumedFlashesLocked discards the flash bookkeeping for a token. It
// must be called with sd.mu held.
func (s *SessionManager) clearConsumedFlashesLocked(token string) {
	s.flashMu.Lock()
	defer s.flashMu.Unlock()
	delete(s.flashes, token)
}

// moveConsumedFlashesLocked re-keys flash bookkeeping when a session token is
// renewed. It must be called with sd.mu held.
func (s *SessionManager) moveConsumedFlashesLocked(oldToken, newToken string) {
	s.flashMu.Lock()
	defer s.flashMu.Unlock()

	keys, ok := s.flashes[oldToken]
	if !ok {
		return
	}

	existing, _ := s.flashes[newToken]
	if existing == nil {
		s.flashes[newToken] = keys
	} else {
		for key := range keys {
			existing[key] = struct{}{}
		}
	}
	delete(s.flashes, oldToken)
}
