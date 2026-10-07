package squirrel

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSetBuilderOperators(t *testing.T) {
	tests := []struct {
		name     string
		set      SetQuery
		args     []interface{}
		expected string
	}{
		{"union", Union(ordersSelect("formal"), ordersSelect("backfill")), []interface{}{"formal", "backfill"}, "(SELECT order_ref FROM orders WHERE kind = ?) UNION (SELECT order_ref FROM orders WHERE kind = ?)"},
		{"union all", UnionAll(ordersSelect("formal"), ordersSelect("backfill")), []interface{}{"formal", "backfill"}, "(SELECT order_ref FROM orders WHERE kind = ?) UNION ALL (SELECT order_ref FROM orders WHERE kind = ?)"},
		{"intersect", Intersect(ordersSelect("formal"), ordersSelect("backfill")), []interface{}{"formal", "backfill"}, "(SELECT order_ref FROM orders WHERE kind = ?) INTERSECT (SELECT order_ref FROM orders WHERE kind = ?)"},
		{"intersect all", IntersectAll(ordersSelect("formal"), ordersSelect("backfill")), []interface{}{"formal", "backfill"}, "(SELECT order_ref FROM orders WHERE kind = ?) INTERSECT ALL (SELECT order_ref FROM orders WHERE kind = ?)"},
		{"except", Except(ordersSelect("formal"), ordersSelect("archived")), []interface{}{"formal", "archived"}, "(SELECT order_ref FROM orders WHERE kind = ?) EXCEPT (SELECT order_ref FROM orders WHERE kind = ?)"},
		{"except all", ExceptAll(ordersSelect("formal"), ordersSelect("archived")), []interface{}{"formal", "archived"}, "(SELECT order_ref FROM orders WHERE kind = ?) EXCEPT ALL (SELECT order_ref FROM orders WHERE kind = ?)"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sql, args, err := test.set.ToSql()
			assert.NoError(t, err)
			assert.Equal(t, test.expected, sql)
			assert.Equal(t, test.args, args)
		})
	}
}

func TestSetBuilderPreservesExplicitGroups(t *testing.T) {
	formal := ordersSelect("formal")
	backfill := ordersSelect("backfill")
	archived := ordersSelect("archived")

	leftGrouped := UnionAll(formal, backfill).Except(archived)
	rightGrouped := formal.UnionAll(backfill.Except(archived))

	leftSQL, leftArgs, err := leftGrouped.ToSql()
	assert.NoError(t, err)
	rightSQL, rightArgs, err := rightGrouped.ToSql()
	assert.NoError(t, err)

	assert.Equal(t,
		"((SELECT order_ref FROM orders WHERE kind = ?) UNION ALL (SELECT order_ref FROM orders WHERE kind = ?)) EXCEPT (SELECT order_ref FROM orders WHERE kind = ?)",
		leftSQL)
	assert.Equal(t,
		"(SELECT order_ref FROM orders WHERE kind = ?) UNION ALL ((SELECT order_ref FROM orders WHERE kind = ?) EXCEPT (SELECT order_ref FROM orders WHERE kind = ?))",
		rightSQL)
	assert.Equal(t, []interface{}{"formal", "backfill", "archived"}, leftArgs)
	assert.Equal(t, []interface{}{"formal", "backfill", "archived"}, rightArgs)

	dollarSQL, dollarArgs, err := StatementBuilder.PlaceholderFormat(Dollar).UnionAll(formal, backfill).Except(archived).ToSql()
	assert.NoError(t, err)
	assert.Equal(t,
		"((SELECT order_ref FROM orders WHERE kind = $1) UNION ALL (SELECT order_ref FROM orders WHERE kind = $2)) EXCEPT (SELECT order_ref FROM orders WHERE kind = $3)",
		dollarSQL)
	assert.Equal(t, []interface{}{"formal", "backfill", "archived"}, dollarArgs)
}

func TestSetBuilderBranchAndRootOrderLimit(t *testing.T) {
	formal := Select("order_ref").From("orders").Where("kind = ?", "formal").OrderBy("created_at DESC").Limit(10)
	backfill := Select("order_ref").From("orders").Where("kind = ?", "backfill").OrderBy("created_at DESC").Limit(10)
	archived := ordersSelect("archived")

	set := UnionAll(formal, backfill).
		Except(archived).
		OrderBy("order_ref").
		Limit(20).
		Offset(5)

	sql, args, err := set.ToSql()
	assert.NoError(t, err)
	expected :=
		"((SELECT order_ref FROM orders WHERE kind = ? ORDER BY created_at DESC LIMIT 10) UNION ALL " +
			"(SELECT order_ref FROM orders WHERE kind = ? ORDER BY created_at DESC LIMIT 10)) EXCEPT " +
			"(SELECT order_ref FROM orders WHERE kind = ?) ORDER BY order_ref LIMIT 20 OFFSET 5"
	assert.Equal(t, expected, sql)
	assert.Equal(t, []interface{}{"formal", "backfill", "archived"}, args)
}

func TestSetBuilderGroupedBranchOrderLimit(t *testing.T) {
	formal := Select("order_ref").From("orders").Where("kind = ?", "formal")
	backfill := Select("order_ref").From("orders").Where("kind = ?", "backfill")
	archived := Select("order_ref").From("orders").Where("kind = ?", "archived")

	active := UnionAll(formal, backfill).OrderBy("created_at DESC").Limit(10)
	set := active.Except(archived).OrderBy("order_ref").Limit(20)

	sql, args, err := set.ToSql()
	assert.NoError(t, err)
	expected :=
		"((SELECT order_ref FROM orders WHERE kind = ?) UNION ALL (SELECT order_ref FROM orders WHERE kind = ?) " +
			"ORDER BY created_at DESC LIMIT 10) EXCEPT (SELECT order_ref FROM orders WHERE kind = ?) " +
			"ORDER BY order_ref LIMIT 20"
	assert.Equal(t, expected, sql)
	assert.Equal(t, []interface{}{"formal", "backfill", "archived"}, args)
}

func TestSelectBuilderFromSet(t *testing.T) {
	formal := Select("order_ref").From("orders").Where("kind = ?", "formal")
	backfill := Select("order_ref").From("orders").Where("kind = ?", "backfill")
	archived := Select("order_ref").From("orders").Where("kind = ?", "archived")

	query := StatementBuilder.PlaceholderFormat(Dollar).
		Select("combined.order_ref").
		FromSet(UnionAll(formal, backfill).Except(archived), "combined").
		Where("combined.source = ?", "online")

	sql, args, err := query.ToSql()
	assert.NoError(t, err)
	expected :=
		"SELECT combined.order_ref FROM (((SELECT order_ref FROM orders WHERE kind = $1) UNION ALL " +
			"(SELECT order_ref FROM orders WHERE kind = $2)) EXCEPT (SELECT order_ref FROM orders WHERE kind = $3)) " +
			"AS combined WHERE combined.source = $4"
	assert.Equal(t, expected, sql)
	assert.Equal(t, []interface{}{"formal", "backfill", "archived", "online"}, args)
}

func TestSetBuilderErrors(t *testing.T) {
	_, _, err := Union(ordersSelect("formal"), nil).ToSql()
	assert.Error(t, err)

	var empty SetBuilder
	_, _, err = empty.ToSql()
	assert.Error(t, err)
	_, err = empty.Query()
	assert.Equal(t, RunnerNotSet, err)
	err = empty.Scan()
	assert.Equal(t, RunnerNotSet, err)

	_, _, err = Select("order_ref").FromSet(Union(ordersSelect("formal"), ordersSelect("backfill")), "").ToSql()
	assert.Error(t, err)
}

func TestSetBuilderRunners(t *testing.T) {
	db := &DBStub{}
	set := StatementBuilder.RunWith(db).UnionAll(ordersSelect("formal"), ordersSelect("backfill"))

	expectedSQL := "(SELECT order_ref FROM orders WHERE kind = ?) UNION ALL (SELECT order_ref FROM orders WHERE kind = ?)"
	expectedArgs := []interface{}{"formal", "backfill"}

	_, err := set.Query()
	assert.NoError(t, err)
	assert.Equal(t, expectedSQL, db.LastQuerySql)
	assert.Equal(t, expectedArgs, db.LastQueryArgs)

	set.Scan()
	assert.Equal(t, expectedSQL, db.LastQueryRowSql)
	assert.Equal(t, expectedArgs, db.LastQueryRowArgs)

	_, err = UnionAll(ordersSelect("formal"), ordersSelect("backfill")).Query()
	assert.Equal(t, RunnerNotSet, err)

	err = UnionAll(ordersSelect("formal"), ordersSelect("backfill")).Scan()
	assert.Equal(t, RunnerNotSet, err)
}

func TestSetBuilderSemanticsWithDuplicateRows(t *testing.T) {
	formal := []string{"A", "A"}
	backfill := []string{"B"}
	archived := []string{"A"}

	leftGrouped := exceptRows(unionAllRows(formal, backfill), archived)
	rightGrouped := unionAllRows(formal, exceptRows(backfill, archived))

	assert.Equal(t, []string{"B"}, leftGrouped)
	assert.Equal(t, []string{"A", "A", "B"}, rightGrouped)
}

func ordersSelect(kind string) SelectBuilder {
	return Select("order_ref").From("orders").Where("kind = ?", kind)
}

func unionAllRows(left, right []string) []string {
	result := make([]string, 0, len(left)+len(right))
	result = append(result, left...)
	result = append(result, right...)
	return result
}

func exceptRows(left, right []string) []string {
	excluded := make(map[string]bool)
	for _, row := range right {
		excluded[row] = true
	}
	result := make([]string, 0, len(left))
	for _, row := range left {
		if excluded[row] {
			continue
		}
		result = append(result, row)
	}
	return result
}
