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

// VersionedStore is an optional interface for session stores which support
// versioned reads and compare-and-swap commits. When the store used by a
// SessionManager implements this interface, commits are performed using a
// three-way merge between the data loaded at the start of the request cycle
// (the baseline), the data currently in the store, and the changes made
// during the request cycle. This prevents concurrent requests for the same
// session from silently overwriting each other's changes (the "last write
// wins" problem): changes to different keys are merged, and conflicting
// changes to the same key are reported to the caller via a ConflictError
// instead of being silently resolved.
type VersionedStore interface {
	Store

	// FindVersioned is the same as Store.Find, except it also returns a
	// version number which uniquely identifies the stored data. The version
	// must change every time the data for the token is modified or deleted.
	// If the token is not found the version return value should be 0.
	FindVersioned(token string) (b []byte, version uint64, found bool, err error)

	// CommitVersioned is the same as Store.Commit, except the commit only
	// succeeds if the version of the data currently stored for the token
	// equals expectedVersion (an expectedVersion of 0 means the token must
	// not currently exist). The committed return value reports whether the
	// commit was applied; a false value (with a nil error) indicates a
	// version mismatch, i.e. the stored data changed between the
	// corresponding FindVersioned call and this commit.
	CommitVersioned(token string, b []byte, expiry time.Time, expectedVersion uint64) (committed bool, err error)
}
