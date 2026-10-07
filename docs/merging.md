# Merging

A [`MergeDataset`](https://godoc.org/github.com/doug-martin/goqu/#MergeDataset) builds an
SQL standard `MERGE` statement. It syncs a target table from a source table (or parameterized
subquery) by declaring `ON` join condition and one or more `WHEN MATCHED` /
`WHEN NOT MATCHED` branches.

> MERGE is only enabled for dialects that support it: `postgres` (PostgreSQL 15+) and
> `sqlserver`. Generating a MERGE statement with another dialect returns an error.

## Branches and actions

Each branch is matched in declaration order:

* `WhenMatched()` / `WhenMatchedAnd(cond)` support the `UpdateSet` and `Delete` actions.
* `WhenNotMatched()` / `WhenNotMatchedAnd(cond)` support the `Insert` and `InsertRow` actions.

Appending another branch of the same category after an **unconditional** branch returns an
`unreachable MERGE branch` error, since the unconditional branch always wins. Using an action
that is not legal for the branch category is also rejected.

## Basic sync

```go
ds := goqu.Dialect("postgres").
    Merge("accounts").
    Using("staging").
    On(goqu.I("accounts.id").Eq(goqu.I("staging.id"))).
    WhenMatched().UpdateSet(goqu.Record{"name": goqu.I("staging.name")}).
    WhenNotMatched().InsertRow(goqu.Record{"id": goqu.I("staging.id"), "name": goqu.I("staging.name")})

sql, args, _ := ds.ToSQL()
fmt.Println(sql, args)
```

Output:

```sql
MERGE INTO "accounts" USING "staging" ON ("accounts"."id" = "staging"."id") WHEN MATCHED THEN UPDATE SET "name"="staging"."name" WHEN NOT MATCHED THEN INSERT ("id", "name") VALUES ("staging"."id", "staging"."name"); []
```

## Conditional branches, aliases and RETURNING

```go
ds := goqu.Dialect("postgres").
    Merge(goqu.T("accounts").As("a")).
    Using(goqu.T("staging").As("s")).
    On(goqu.I("a.id").Eq(goqu.I("s.id"))).
    WhenMatchedAnd(goqu.I("s.deleted").Is(true)).Delete().
    WhenMatched().UpdateSet(goqu.Record{"name": goqu.I("s.name")}).
    WhenNotMatchedAnd(goqu.I("s.active").Is(true)).
    Insert([]interface{}{"id", "name"}, []interface{}{goqu.I("s.id"), goqu.I("s.name")}).
    Returning(goqu.I("a.id"))
```

## Parameterized subquery source

```go
source := goqu.From("employees").
    Where(goqu.I("updated_at").Gt("2020-01-01")).
    As("e")

sql, args, _ := goqu.Dialect("postgres").
    Merge(goqu.T("accounts").As("a")).
    Using(source).
    On(goqu.I("a.id").Eq(goqu.I("e.account_id"))).
    WhenMatched().UpdateSet(goqu.Record{"name": goqu.I("e.name")}).
    WhenNotMatched().InsertRow(goqu.Record{"id": goqu.I("e.account_id"), "name": goqu.I("e.name")}).
    Prepared(true).
    ToSQL()
```

The number of insert columns and values, and the presence of update assignments, are validated
before the SQL is generated.
