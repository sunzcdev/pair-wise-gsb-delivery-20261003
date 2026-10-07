# Expression reduction

The `github.com/doug-martin/goqu/v9/exp/reduce` package is an **optional**
pre-pass that simplifies a boolean expression before it reaches the SQL
generator. It is not applied automatically; call it explicitly when the WHERE
expression assembled by the permission layer has grown large.

```go
import "github.com/doug-martin/goqu/v9/exp/reduce"

r := reduce.New(
    reduce.WithNullability(map[string]reduce.Nullability{
        "users.id":   reduce.NonNullable,
        "users.name": reduce.Nullable,
    }),
    reduce.WithPureFunctions("COALESCE", "UPPER"),
)

where := r.Reduce(where)
```

`reduce.Fold(e)` is the zero-option shortcut for folds that need no schema
knowledge.

## Three-valued logic

Reduction follows SQL three-valued logic (3VL): every predicate evaluates to
`TRUE`, `FALSE`, or `UNKNOWN` (a.k.a. `NULL`).

| AND     | T | F | U |   | OR      | T | F | U |   | NOT |   |
|---------|---|---|---|---|---------|---|---|---|---|-----|---|
| **T**   | T | F | U |   | **T**   | T | T | T |   | T   | F |
| **F**   | F | F | F |   | **F**   | T | F | U |   | F   | T |
| **U**   | U | F | U |   | **U**   | T | U | U |   | U   | U |

Notable folds (no nullability information needed):

- `TRUE AND x`  -> `x`,   `FALSE AND x` -> `FALSE`
- `FALSE OR x`  -> `x`,   `TRUE OR x`   -> `TRUE`
- `FALSE AND U` -> `FALSE`; `TRUE OR U` -> `TRUE`
- `U AND x` is **not** a constant, because `x` might be `FALSE`
  (`U AND FALSE = FALSE`). Likewise `U OR x` is not a constant.
- De Morgan: `NOT (a AND b) = (NOT a) OR (NOT b)` and dual; these hold with
  `UNKNOWN` too, along with associative flattening and double-negation.

`Reducer.Not(e)` performs structural negation: operators flip to their
three-valued inverses (`=`/`<>`, `>`/`<=`, `IS`/`IS NOT`, `IN`/`NOT IN`,
`LIKE`/`NOT LIKE`, `BETWEEN`/`NOT BETWEEN`, ...). Those inversions are exact
in 3VL without a non-null proof. When no structural negation exists (Literal
SQL, an unknown function, CASE, ...) the expression is reduced but returned
unchanged rather than emitting opaque `NOT (...)` text.

## Folds that require a non-null proof

The identities below are true in ordinary boolean algebra but **false** when
the column can be NULL, so the reducer performs them only when the column is
declared `NonNullable`:

- `x = x`  -> `TRUE`, and `x <> x` -> `FALSE`. On a nullable column these are
  `TRUE`/`FALSE` for non-null rows but `UNKNOWN` when `x IS NULL`.
- `x IS NULL` -> `FALSE`, and `x IS NOT NULL` -> `TRUE` need a NOT NULL
  constraint; on a nullable column both are row-dependent.

Column keys match identifier spelling: `"col"`, `"table.col"`, or
`"schema.table.col"`. `Nullable` and `UnknownNullability` both block these
folds; `Nullable` exists to make assumptions explicit.

## What is kept conservatively

To stay sound the reducer never interprets:

- Literal SQL (`goqu.L(...)`), including literal text that happens to spell
  `TRUE`/`FALSE` — only constants produced by the reducer are folded.
- Unknown SQL functions; declare a function pure with `WithPureFunctions` to
  allow it to participate in elimination.
- Subqueries, window functions, `CASE`, `Ex`/`ExOr` maps, and other opaque
  nodes.

## Side effects and evaluation order

`driver.Valuer` arguments are invoked later, during SQL generation, not by
the reducer. When a fold would erase an operand (e.g. `TRUE OR side_effect()`),
the operand is removed only if the whole erased subtree is proven pure; if it
contains a Valuer, Literal, or undeclared function the list shape is retained
so the generated SQL performs the same `Value()` calls. Operands are never
reordered and no node is evaluated twice. Constants produced by the reducer
are immutable shared singletons, and reduction is idempotent.
