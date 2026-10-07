//go:build go1.8
// +build go1.8

package sqlx

import (
	"context"
	"database/sql"
	"errors"
)

// NamedExecChunksContext is the context-aware variant of NamedExecChunks.
// It executes the input in chunks of at most maxParams parameters, using the
// supplied context for every statement.  As with NamedExecChunks, a *Tx is
// never committed or rolled back, while a *DB provides no whole-batch
// atomicity and the returned ChunkExecResult reports progress on failure.
func NamedExecChunksContext(ctx context.Context, e ExtContext, query string, arg interface{}, maxParams int, fixed ...interface{}) (ChunkExecResult, error) {
	var fixedArg interface{}
	switch len(fixed) {
	case 0:
	case 1:
		fixedArg = fixed[0]
	default:
		return ChunkExecResult{}, errors.New("sqlx: at most one fixed argument may be supplied")
	}

	return namedExecChunks(e, query, arg, maxParams, fixedArg, func(q string, args []interface{}) (sql.Result, error) {
		return e.ExecContext(ctx, q, args...)
	})
}

// NamedExecChunksContext using this DB.
func (db *DB) NamedExecChunksContext(ctx context.Context, query string, arg interface{}, maxParams int, fixed ...interface{}) (ChunkExecResult, error) {
	return NamedExecChunksContext(ctx, db, query, arg, maxParams, fixed...)
}

// NamedExecChunksContext using this Tx.  The transaction is never committed
// or rolled back by this call.
func (tx *Tx) NamedExecChunksContext(ctx context.Context, query string, arg interface{}, maxParams int, fixed ...interface{}) (ChunkExecResult, error) {
	return NamedExecChunksContext(ctx, tx, query, arg, maxParams, fixed...)
}
