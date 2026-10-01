# Match Expressions

The `match` expression selects one branch by matching a value against a list
of patterns. It is useful when an input is one of a closed set of variants
(a sum type) and each variant carries different fields.

```expr
match event {
  Created{ID: id}            -> { "created " + id }
  Paid{Amount: a} if a > 100 -> { "large payment" }
  Paid{Amount: a}            -> { "payment of " + a }
  Closed                     -> { "closed" }
}
```

## Patterns

A branch has the form `pattern [if guard] -> { body }`:

- `Variant{Field: pattern, ...}` matches a variant and destructures its
  fields. Fields can be omitted; nested fields are matched recursively.
- `Variant` matches a variant without binding any of its fields.
- `identifier` binds the matched value to a variable visible in the optional
  guard and the body.
- `_` matches any value without binding it.
- `nil` matches a nil value.
- Literal patterns match numbers, strings and booleans (`1`, `"x"`, `true`).

Bindings are scoped to their own branch: they are not visible in other
branches and do not leak after the `match` expression.

## Guards

A branch may carry an optional boolean guard:

```expr
match shape {
  Square{Side: s} if s > 10 -> { "large" }
  Square{Side: s}          -> { "small" }
}
```

A guarded branch is conditional. It is never treated as an unconditional
cover of its variant when checking exhaustiveness, even when the guard always
holds.

## Closed Variant Sets

The host program registers the exhaustive list of variants for an interface
type together with the fields of each variant using the `variants` package:

```go
type Event interface{ event() }

type Created struct{ ID string }
type Paid struct{ Amount int }
type Closed struct{}

registry := variants.NewRegistry(
    variants.New((*Event)(nil)).
        Case("Created", Created{}).
        Case("Paid", Paid{}).
        Case("Closed", Closed{}),
)

program, err := expr.Compile(code,
    expr.Env(map[string]any{"event": (*Event)(nil)}),
    expr.Variants(registry),
)
```

Variants may be passed by value or by pointer; both are accepted at runtime.

## Exhaustiveness and Reachability

When the scrutinee has a registered variant set, the compiler checks that
every variant is covered:

- A missing variant is a compile error naming the missing variants.
- A branch that can never be selected is reported as unreachable.
- A wildcard (`_`) or identifier branch covers every remaining variant.

Only the selected branch is evaluated at runtime; bodies and guards of
non-matching branches are never executed.
