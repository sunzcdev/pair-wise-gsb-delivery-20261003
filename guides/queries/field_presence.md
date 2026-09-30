---
layout: guide
doc_stub: false
search: true
section: Queries
title: Will a Field Be In the Response?
desc: Symbolically determine whether a response path can appear, honoring @skip, @include, variables and fragments
index: 6
---

Sometimes a service wants to know, _before_ executing an operation, whether
a field (for example `auditTrail`) might appear in the response. A naive scan
for the field name ignores `@skip`, `@include`, fragment spreads and fragment
type conditions, so it gives wrong answers.

{{ "GraphQL::Analysis::FieldPresence" | api_doc }} performs that check
symbolically, using only the operation, the variables and the schema type
structure. It never calls a resolver.

```ruby
query = GraphQL::Query.new(MySchema, query_string, variables: variables)
result = GraphQL::Analysis::FieldPresence.analyze(query, ["node", "auditTrail"])

result.status   # :always, :never, or :variable
```

- `:always` — at least one path reaches the field unconditionally.
- `:never` — no path reaches the field (it is absent, or every path is ruled
  out by a constant-false directive or a schema-impossible type condition).
- `:variable` — the field is reachable, but only depending on a Boolean
  variable, the concrete runtime object type of an abstract return type, or
  a directive the analyzer does not understand.

The path is an array of response keys (a dot-separated string works too), and
aliases are matched. A named operation may be chosen with
`operation_name:`.

## Variables

`@skip(if:)` and `@include(if:)` accept a Boolean literal or a Boolean
variable. Values provided at request time and variable definition defaults
are applied, so a condition may reduce to a constant. Remaining free Boolean
variables are listed in `result.variables`.

## Reviewable witnesses

When a path is reachable, `result.witnesses` contains one or more
{{ "GraphQL::Analysis::FieldPresence::Witness" | api_doc }} objects. Each
witness is a concrete assignment that demonstrates reachability:

- `variables` maps the Boolean variables to the values they must have.
- `types` is aligned with the analyzed path; each entry is either a concrete
  object type the parent runtime object must be, or `nil` when any possible
  type works.
- `unknown_gates` lists directives the analyzer could not decide.
- `route` is the sequence of fields and fragments taken, for review.

## Unknown directives

Only the built-in `@skip` and `@include` affect whether a selection runs.
Any other directive is treated as an _opaque gate_: the analyzer does not
assume it passes, so a field guarded by an unknown directive is reported as
`:variable`, and the gate is attached to the witness and collected in
`result.unknown_gates`.
