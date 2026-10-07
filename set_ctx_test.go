//go:build go1.8
// +build go1.8

package squirrel

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSetBuilderContext(t *testing.T) {
	db := &DBStub{}
	b := UnionAll(Select("a").From("t"), Select("b").From("u")).RunWith(db)

	_, err := b.ExecContext(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "SELECT a FROM t UNION ALL SELECT b FROM u", db.LastExecSql)

	_, err = b.QueryContext(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "SELECT a FROM t UNION ALL SELECT b FROM u", db.LastQuerySql)

	b.QueryRowContext(ctx)
	assert.Equal(t, "SELECT a FROM t UNION ALL SELECT b FROM u", db.LastQueryRowSql)

	_, err = UnionAll(Select("a").From("t"), Select("b").From("u")).ExecContext(ctx)
	assert.Equal(t, RunnerNotSet, err)
}
