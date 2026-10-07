//go:build go1.8
// +build go1.8

package sqlx

import "context"

// ScanResultSetsContext is ScanResultSets with context support. Cancellation
// is checked before advancing to each result set and before scanning or
// draining each row; a canceled context ends consumption, closes the cursor
// and returns the context's error. A failed handler or Rows.Err likewise ends
// consumption and releases the cursor.
func ScanResultSetsContext(ctx context.Context, rows *Rows, specs []ResultSetSpec, tail TailPolicy) error {
	return scanResultSets(ctx, rows, specs, tail)
}
