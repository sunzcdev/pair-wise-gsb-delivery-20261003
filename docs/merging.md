# Merging

The `MergeDataset` builds standalone `MERGE` statements that apply different actions depending on
whether a source row matches a target row.

`MERGE` is supported by PostgreSQL (15+) and SQL Server dialects. The `mysql` and `sqlite3`
dialects report an error when generating a `MERGE` statement because they do not support the
command.

<a name="create"></a>
## Creating A MergeDataset

Use **[`goqu.Merge`](https://godoc.org/github.com/doug-martin/goqu/#Merge)**, or
`Dialect("postgres").Merge(...)` / `db.Merge(...)`.

```go
ds := goqu.Dialect("postgres").
    Merge("accounts").
    Using("transactions").
    On(goqu.I("accounts.id").Eq(goqu.I("transactions.id")))
```

## Target Alias

Use `TargetAs` to alias the target table.

```go
ds := goqu.Dialect("postgres").
    Merge("accounts").
    TargetAs("a").
    Using("transactions").
    On(goqu.I("a.id").Eq(goqu.I("transactions.id")))
```

## Source

`Using` accepts a table name or expression. `UsingAs` accepts a subquery and an alias. The source
subquery may contain parameters.

```go
src := goqu.From("transactions").
    Select(goqu.I("user_id"), goqu.SUM(goqu.I("amount")).As("amount")).
    GroupBy(goqu.I("user_id"))

ds := goqu.Dialect("postgres").
    Merge("accounts").
    TargetAs("a").
    UsingAs(src, "t").
    On(goqu.I("a.user_id").Eq(goqu.I("t.user_id")))
```

## Match Condition

`On` joins multiple expressions with `AND`.

## Branches

Each `WhenMatched`/`WhenNotMatched` starts a branch with an optional search condition. Branches of
the same type are matched in declaration order. A branch of the same type declared after an
unconditional branch is unreachable and produces an error.

* `WHEN MATCHED` branches support `ThenUpdate`, `ThenDelete`, and `ThenDoNothing`.
* `WHEN NOT MATCHED` branches support `ThenInsert` and `ThenDoNothing`.

Mixing an action into the wrong branch category produces an error.

```go
ds := goqu.Dialect("postgres").
    Merge("accounts").
    TargetAs("a").
    UsingAs(src, "t").
    On(goqu.I("a.user_id").Eq(goqu.I("t.user_id"))).
    WhenMatched(goqu.I("t.amount").Gt(100)).
    ThenUpdate(goqu.Record{"balance": goqu.L("? + ?", goqu.I("a.balance"), goqu.I("t.amount"))}).
    WhenMatched().
    ThenDelete().
    WhenNotMatched().
    ThenInsert(goqu.Record{"user_id": goqu.I("t.user_id"), "balance": goqu.I("t.amount")})
```

`ThenInsert` accepts a `goqu.Record`, a map, or a struct. The insert column list and the supplied
value count are validated before the SQL is generated.

## Returning

Use `Returning` on dialects that support it (e.g. PostgreSQL). SQL Server does not support
`RETURNING`.
