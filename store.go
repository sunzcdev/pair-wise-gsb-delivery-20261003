package scs

import (
	"context"
	"errors"
	"time"
)

// ErrFlashNotSupported is returned by SessionManager.Flash when the configured
// session store does not support the atomic operations required to consume a
// flash value across concurrent requests. Stores that cannot provide this
// guarantee must not pretend otherwise; callers can fall back to Pop for
// best-effort, request-local behavior.
var ErrFlashNotSupported = errors.New("scs: session store does not support atomic flash operations")

// FlashFunc is invoked atomically by a store implementing FlashStore while
// reading and updating a session record. It receives the currently stored
// encoded session data (or nil if the token does not exist or has expired) and
// returns the updated encoded data after the flash key has been removed. The ok
// return value reports whether the flash key was present.
type FlashFunc = func(b []byte) (updated []byte, ok bool, err error)

// MergeFunc is invoked atomically by a store implementing MergeCommitStore
// while committing a session. It receives the currently stored encoded session
// data (or nil if the token does not exist or has expired) and returns the
// encoded data that should be stored.
type MergeFunc = func(current []byte) (updated []byte, err error)

// Store is the interface for session stores.
type Store interface {
	// Delete should remove the session token and corresponding data from the
	// session store. If the token does not exist then Delete should be a no-op
	// and return nil (not an error).
	Delete(token string) (err error)

	// Find should return the data for a session token from the store. If the
	// session token is not found or is expired, the found return value should
	// be false (and the err return value should be nil). Similarly, tampered
	// or malformed tokens should result in a found return value of false and a
	// nil err value. The err return value should be used for system errors only.
	Find(token string) (b []byte, found bool, err error)

	// Commit should add the session token and data to the store, with the given
	// expiry time. If the session token already exists, then the data and
	// expiry time should be overwritten.
	Commit(token string, b []byte, expiry time.Time) (err error)
}

// IterableStore is the interface for session stores which support iteration.
type IterableStore interface {
	// All should return a map containing data for all active sessions (i.e.
	// sessions which have not expired). The map key should be the session
	// token and the map value should be the session data. If no active
	// sessions exist this should return an empty (not nil) map.
	All() (map[string][]byte, error)
}

// CtxStore is an interface for session stores which take a context.Context
// parameter.
type CtxStore interface {
	Store

	// DeleteCtx is the same as Store.Delete, except it takes a context.Context.
	DeleteCtx(ctx context.Context, token string) (err error)

	// FindCtx is the same as Store.Find, except it takes a context.Context.
	FindCtx(ctx context.Context, token string) (b []byte, found bool, err error)

	// CommitCtx is the same as Store.Commit, except it takes a context.Context.
	CommitCtx(ctx context.Context, token string, b []byte, expiry time.Time) (err error)
}

// IterableCtxStore is the interface for session stores which support iteration
// and which take a context.Context parameter.
type IterableCtxStore interface {
	// AllCtx is the same as IterableStore.All, expect it takes a
	// context.Context.
	AllCtx(ctx context.Context) (map[string][]byte, error)
}

// FlashStore is implemented by session stores which can atomically extract a
// flash value for a given session token. "Atomically" means that when multiple
// concurrent requests attempt to flash the same key, exactly one will observe
// it as present, and the updated session data (with the key removed) is
// persisted by the store before the operation returns.
type FlashStore interface {
	Store

	// Flash atomically invokes fn for the session data associated with token
	// and, if fn reports that the flash key was present, persists the updated
	// data returned by fn without changing the session expiry. The ok return
	// value mirrors the value reported by fn. A system error should be returned
	// as err.
	Flash(token string, key string, fn FlashFunc) (ok bool, err error)
}

// FlashCtxStore is the same as FlashStore, except that its methods take a
// context.Context parameter.
type FlashCtxStore interface {
	CtxStore

	// FlashCtx is the same as FlashStore.Flash, except it takes a
	// context.Context.
	FlashCtx(ctx context.Context, token string, key string, fn FlashFunc) (ok bool, err error)
}

// MergeCommitStore is implemented by session stores which can atomically
// perform a read-modify-write when committing session data. This allows a
// commit made from a stale, earlier-loaded session copy to be merged with the
// current stored data instead of blindly overwriting it (which would otherwise
// allow already-consumed flash values to be written back).
type MergeCommitStore interface {
	Store

	// MergeCommit atomically invokes fn for the session data associated with
	// token and persists the updated data returned by fn with the given expiry.
	MergeCommit(token string, fn MergeFunc, expiry time.Time) error
}

// MergeCommitCtxStore is the same as MergeCommitStore, except that its methods
// take a context.Context parameter.
type MergeCommitCtxStore interface {
	CtxStore

	// MergeCommitCtx is the same as MergeCommitStore.MergeCommit, except it
	// takes a context.Context.
	MergeCommitCtx(ctx context.Context, token string, fn MergeFunc, expiry time.Time) error
}
