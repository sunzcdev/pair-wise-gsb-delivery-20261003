package sqlx

import (
	"context"
	"errors"
	"fmt"
)

// TailPolicy controls how ScanResultSets behaves when the statement returns
// more result sets than the caller declared.
type TailPolicy int

const (
	// TailError is the default: any result set beyond the declared specs is
	// reported as an error and no trailing data is silently discarded.
	TailError TailPolicy = iota
	// TailIgnore consumes and discards every trailing result set (including
	// its rows) so that the cursor is left fully drained.
	TailIgnore
)

// ResultSetSpec describes how a single result set returned by a multi-result-set
// statement is consumed. Result sets are matched to specs positionally, so an
// empty result set still occupies its index.
//
// Exactly one of Dest, Handler or Skip must be set:
//
//   - Dest is a pointer to a slice; all rows of the result set are StructScanned
//     into it, resetting the slice length to zero first. Slices of scannable
//     types (e.g. *[]int) scan a single-column result set.
//   - Handler is called once per row with the *Rows already positioned on the
//     row, ready for StructScan, MapScan or SliceScan. Returning an error stops
//     consumption.
//   - Skip drains the result set without scanning anything.
type ResultSetSpec struct {
	Dest    interface{}
	Handler func(*Rows) error
	Skip    bool
}

// ErrInvalidResultSetSpec is returned when a ResultSetSpec does not select
// exactly one consumption mode.
var ErrInvalidResultSetSpec = errors.New("sqlx: result set spec must set exactly one of Dest, Handler or Skip")

// ScanResultSets consumes the result sets produced by a statement which returns
// multiple result sets. Each spec is applied positionally, so empty result sets
// still occupy a slot. The column-to-field mapping is rebuilt for every result
// set, so sets that share column names but differ in type or order are handled
// safely.
//
// If fewer result sets are returned than specs declared, an error is returned.
// Extra result sets are either reported (TailError) or drained (TailIgnore)
// according to tail.
//
// The rows cursor is always closed before returning, including when a handler
// fails or rows.Err reports an iteration error. For cancellation support use
// ScanResultSetsContext.
func ScanResultSets(rows *Rows, specs []ResultSetSpec, tail TailPolicy) error {
	return scanResultSets(context.Background(), rows, specs, tail)
}

func scanResultSets(ctx context.Context, rows *Rows, specs []ResultSetSpec, tail TailPolicy) error {
	// Always release the cursor, even on cancellation, handler failure or a
	// driver iteration error.
	defer rows.Close()

	if len(specs) == 0 {
		return errors.New("sqlx: ScanResultSets requires at least one result set spec")
	}
	if tail != TailError && tail != TailIgnore {
		return fmt.Errorf("sqlx: invalid tail policy %d", tail)
	}
	for i, spec := range specs {
		n := 0
		if spec.Dest != nil {
			n++
		}
		if spec.Handler != nil {
			n++
		}
		if spec.Skip {
			n++
		}
		if n != 1 {
			return fmt.Errorf("sqlx: result set spec %d: %w", i, ErrInvalidResultSetSpec)
		}
	}

	for i, spec := range specs {
		if i > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !rows.NextResultSet() {
				if err := rows.Err(); err != nil {
					return err
				}
				return fmt.Errorf("sqlx: expected %d result sets, but only %d were returned", len(specs), i)
			}
		}

		if err := consumeResultSet(ctx, rows, spec); err != nil {
			return err
		}
	}

	// Decide what to do with any result sets beyond the declared specs.
	if !rows.NextResultSet() {
		return rows.Err()
	}
	if tail == TailError {
		return fmt.Errorf("sqlx: unexpected extra result set at index %d", len(specs))
	}

	// TailIgnore: drain every remaining result set and its rows.
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := drainRows(ctxAwareRows{ctx, rows}); err != nil {
			return err
		}
		if !rows.NextResultSet() {
			return rows.Err()
		}
	}
}

// consumeResultSet handles a single result set according to spec. The Rows
// StructScan cache is reset so the mapping is rebuilt from the current result
// set's columns, even when a previous result set shared the same column names.
func consumeResultSet(ctx context.Context, rows *Rows, spec ResultSetSpec) error {
	// Force StructScan to re-derive columns, traversals and scan targets for
	// this result set instead of reusing a previous set's (possibly
	// differently typed or ordered) mapping.
	rows.started = false
	rows.fields = nil
	rows.values = nil

	switch {
	case spec.Skip:
		return drainRows(ctxAwareRows{ctx, rows})
	case spec.Dest != nil:
		return scanAll(rows, spec.Dest, false)
	default:
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := spec.Handler(rows); err != nil {
				return err
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return nil
	}
}

// rowIterator is the subset of *Rows needed to exhaust a result set.
type rowIterator interface {
	Next() bool
	Err() error
}

// drainRows advances through every remaining row of the current result set so
// that errors surfaced by Rows.Err are observed before moving on. The underlying
// driver then treats the result set as fully consumed.
func drainRows(r rowIterator) error {
	for r.Next() {
	}
	return r.Err()
}

// ctxAwareRows wraps *Rows so an early cancellation can interrupt a drain even
// when the driver Next call does not block on the context itself.
type ctxAwareRows struct {
	ctx  context.Context
	rows *Rows
}

func (c ctxAwareRows) Next() bool {
	if err := c.ctx.Err(); err != nil {
		return false
	}
	return c.rows.Next()
}

func (c ctxAwareRows) Err() error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	return c.rows.Err()
}
