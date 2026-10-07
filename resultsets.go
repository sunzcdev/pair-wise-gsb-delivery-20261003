package sqlx

import (
	"context"
	"errors"
	"fmt"
)

// ResultSetMode controls how SelectResultSets and SelectResultSetsContext
// behave when the number of result sets returned by a query does not match
// the number of ResultSet specs provided by the caller.
type ResultSetMode int

const (
	// ResultSetsStrict requires the query to return exactly as many result
	// sets as there are ResultSet specs.  Missing or extra result sets are
	// reported as an error.
	ResultSetsStrict ResultSetMode = iota
	// ResultSetsIgnoreExtra allows the query to return more result sets than
	// there are ResultSet specs; the trailing result sets are discarded.
	// Missing result sets are still reported as an error.
	ResultSetsIgnoreExtra
)

// ResultSet describes how a single result set of a multi-result query (for
// instance a stored procedure returning several result sets) is consumed.
// Exactly one of Dest, RowFunc or Skip must be set.
type ResultSet struct {
	// Dest, when set, must be a pointer to a slice.  Every row of this
	// result set is scanned into a new element of the slice, exactly like
	// Select.  An empty result set leaves the slice empty; it still
	// occupies a position in the spec list.
	Dest interface{}
	// RowFunc, when set, is called once for every row of this result set.
	// The *Rows argument is positioned on the current row, so RowFunc may
	// use StructScan, SliceScan, MapScan or Scan directly.  Returning an
	// error aborts consumption of the remaining result sets.
	RowFunc func(*Rows) error
	// Skip, when true, discards this result set without scanning it.
	Skip bool
}

// ScanResultSet returns a ResultSet spec which scans every row of a result
// set into dest, which must be a pointer to a slice, like Select.
func ScanResultSet(dest interface{}) ResultSet {
	return ResultSet{Dest: dest}
}

// EachResultSetRow returns a ResultSet spec which calls fn once for every
// row of a result set.
func EachResultSetRow(fn func(*Rows) error) ResultSet {
	return ResultSet{RowFunc: fn}
}

// SkipResultSet returns a ResultSet spec which discards a result set.
func SkipResultSet() ResultSet {
	return ResultSet{Skip: true}
}

func (s ResultSet) validate() error {
	n := 0
	if s.Dest != nil {
		n++
	}
	if s.RowFunc != nil {
		n++
	}
	if s.Skip {
		n++
	}
	if n == 0 {
		return errors.New("sqlx: result set spec must set one of Dest, RowFunc or Skip")
	}
	if n > 1 {
		return errors.New("sqlx: result set spec must set only one of Dest, RowFunc or Skip")
	}
	return nil
}

// SelectResultSets executes a query which returns multiple result sets using
// the provided Queryer and consumes each result set according to the
// matching entry in sets.  See SelectResultSetsContext for details.
func SelectResultSets(q Queryer, mode ResultSetMode, sets []ResultSet, query string, args ...interface{}) error {
	if err := validateResultSets(mode, sets); err != nil {
		return err
	}
	rows, err := q.Queryx(query, args...)
	if err != nil {
		return err
	}
	return consumeResultSets(context.Background(), rows, mode, sets)
}

// SelectResultSetsContext executes a query which returns multiple result
// sets using the provided QueryerContext and consumes each result set
// according to the matching entry in sets.
//
// Every result set returned by the query, including empty ones, occupies a
// position and is matched against the ResultSet spec at the same index.  The
// column-to-field mapping is rebuilt for every result set, so result sets
// may reuse column names with different types or a different column order.
//
// Consumption stops, the rows are closed and an error is returned when the
// context is canceled, when a RowFunc returns an error, when scanning fails
// or when the underlying rows report an error.  If the query returns fewer
// result sets than expected an error is always returned; extra trailing
// result sets are an error unless mode is ResultSetsIgnoreExtra.
func SelectResultSetsContext(ctx context.Context, q QueryerContext, mode ResultSetMode, sets []ResultSet, query string, args ...interface{}) error {
	if err := validateResultSets(mode, sets); err != nil {
		return err
	}
	rows, err := q.QueryxContext(ctx, query, args...)
	if err != nil {
		return err
	}
	return consumeResultSets(ctx, rows, mode, sets)
}

func validateResultSets(mode ResultSetMode, sets []ResultSet) error {
	if mode != ResultSetsStrict && mode != ResultSetsIgnoreExtra {
		return fmt.Errorf("sqlx: invalid result set mode %d", mode)
	}
	for i, set := range sets {
		if err := set.validate(); err != nil {
			return fmt.Errorf("sqlx: result set %d: %v", i, err)
		}
	}
	return nil
}

// consumeResultSets consumes the result sets of rows according to sets and
// closes rows before returning.
func consumeResultSets(ctx context.Context, rows *Rows, mode ResultSetMode, sets []ResultSet) error {
	defer rows.Close()
	for i, set := range sets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i > 0 {
			if !rows.NextResultSet() {
				if err := rows.Err(); err != nil {
					return err
				}
				return fmt.Errorf("sqlx: expected %d result sets, but query returned only %d", len(sets), i)
			}
			// The columns of the new result set may differ in name, type or
			// order, so any cached StructScan state from the previous result
			// set must be discarded to force a fresh mapping.
			rows.started = false
			rows.fields = nil
			rows.values = nil
		}
		if err := consumeResultSet(rows, set); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	if rows.NextResultSet() && mode != ResultSetsIgnoreExtra {
		return fmt.Errorf("sqlx: query returned more result sets than the %d expected", len(sets))
	}
	return rows.Err()
}

func consumeResultSet(rows *Rows, set ResultSet) error {
	switch {
	case set.Skip:
		// nothing to do: NextResultSet discards the remaining rows of this
		// result set when the next spec is consumed.
		return nil
	case set.Dest != nil:
		return scanAll(rows, set.Dest, false)
	default:
		for rows.Next() {
			if err := set.RowFunc(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
}

// SelectResultSets executes a query returning multiple result sets using
// this DB and consumes each of them according to sets.
func (db *DB) SelectResultSets(mode ResultSetMode, sets []ResultSet, query string, args ...interface{}) error {
	return SelectResultSets(db, mode, sets, query, args...)
}

// SelectResultSetsContext executes a query returning multiple result sets
// using this DB and consumes each of them according to sets.
func (db *DB) SelectResultSetsContext(ctx context.Context, mode ResultSetMode, sets []ResultSet, query string, args ...interface{}) error {
	return SelectResultSetsContext(ctx, db, mode, sets, query, args...)
}

// SelectResultSets executes a query returning multiple result sets using
// this Tx and consumes each of them according to sets.
func (tx *Tx) SelectResultSets(mode ResultSetMode, sets []ResultSet, query string, args ...interface{}) error {
	return SelectResultSets(tx, mode, sets, query, args...)
}

// SelectResultSetsContext executes a query returning multiple result sets
// using this Tx and consumes each of them according to sets.
func (tx *Tx) SelectResultSetsContext(ctx context.Context, mode ResultSetMode, sets []ResultSet, query string, args ...interface{}) error {
	return SelectResultSetsContext(ctx, tx, mode, sets, query, args...)
}

// SelectResultSetsContext executes a query returning multiple result sets
// using this Conn and consumes each of them according to sets.
func (c *Conn) SelectResultSetsContext(ctx context.Context, mode ResultSetMode, sets []ResultSet, query string, args ...interface{}) error {
	return SelectResultSetsContext(ctx, c, mode, sets, query, args...)
}
