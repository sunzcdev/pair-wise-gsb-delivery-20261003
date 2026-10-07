//go:build go1.8
// +build go1.8

package squirrel

import (
	"context"
	"database/sql"

	"github.com/lann/builder"
)

func (d *setData) QueryContext(ctx context.Context) (*sql.Rows, error) {
	if d.RunWith == nil {
		return nil, RunnerNotSet
	}
	ctxRunner, ok := d.RunWith.(QueryerContext)
	if !ok {
		return nil, NoContextSupport
	}
	return QueryContextWith(ctx, ctxRunner, d)
}

func (d *setData) QueryRowContext(ctx context.Context) RowScanner {
	if d.RunWith == nil {
		return &Row{err: RunnerNotSet}
	}
	queryRower, ok := d.RunWith.(QueryRowerContext)
	if !ok {
		if _, ok := d.RunWith.(QueryerContext); !ok {
			return &Row{err: RunnerNotQueryRunner}
		}
		return &Row{err: NoContextSupport}
	}
	return QueryRowContextWith(ctx, queryRower, d)
}

// QueryContext builds and QueryContexts the complete set query.
func (b SetBuilder) QueryContext(ctx context.Context) (*sql.Rows, error) {
	data := builder.GetStruct(b).(setData)
	return data.QueryContext(ctx)
}

// QueryRowContext builds and QueryRowContexts a single row from the set query.
func (b SetBuilder) QueryRowContext(ctx context.Context) RowScanner {
	data := builder.GetStruct(b).(setData)
	return data.QueryRowContext(ctx)
}

// ScanContext is a shortcut for QueryRowContext().Scan.
func (b SetBuilder) ScanContext(ctx context.Context, dest ...interface{}) error {
	return b.QueryRowContext(ctx).Scan(dest...)
}
