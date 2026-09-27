package scs

import (
	"context"
	"time"
)

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

// VersionedStore is the interface for session stores which support optimistic
// concurrency control. It is an optional extension to the Store interface.
//
// When the store set on a SessionManager implements VersionedStore, committing
// a session which was loaded from the store performs a three-way, field-level
// merge between the values as they were when the session was loaded, the
// values currently held in the store, and the changes made during the request
// cycle. Changes to different fields are merged together; conflicting changes
// to the same field cause the commit to fail with a *ConflictError instead of
// silently applying last-write-wins semantics. A commit also fails with a
// *ConflictError if the session was deleted from the store (e.g. by a
// concurrent Destroy call) after it was loaded, so that an in-flight request
// can never resurrect a destroyed session.
type VersionedStore interface {
	Store

	// FindVersioned is the same as Store.Find, except it also returns the
	// current version of the stored data. The version is an opaque,
	// monotonically increasing identifier for the stored data.
	FindVersioned(token string) (b []byte, version uint64, found bool, err error)

	// CommitVersioned is the same as Store.Commit, except the commit only
	// succeeds if the version of the data currently held in the store is
	// equal to expectedVersion (a compare-and-swap operation). If the
	// versions do not match, or the token does not exist in the store, the
	// commit must be a no-op and the committed return value must be false.
	// In particular, CommitVersioned must never create a token which does
	// not already exist in the store.
	CommitVersioned(token string, b []byte, expiry time.Time, expectedVersion uint64) (committed bool, err error)
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
