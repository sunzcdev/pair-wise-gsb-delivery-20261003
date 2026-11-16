# Planning Conversions

Before deploying a model, it is useful to know exactly which fields have
conversion support and which hooks would handle each type position--without
actually converting any data. *cattrs* supports this with **conversion
planning**, available through {meth}`BaseConverter.plan_structure` and
{meth}`BaseConverter.plan_unstructure` (or the {mod}`cattrs.planning` module
functions).

A plan is preparation only: it never calls a field conversion hook and never
constructs an object. It walks the type graph starting from a root type and
records:

- every distinct type position as a {class}`~cattrs.TypeNode`, including the
  {class}`~cattrs.HookSource` selected for it;
- the directed {class}`~cattrs.TypeEdge` edges between parent types and their
  fields, generic arguments, union members and collection elements;
- the **recursive edges** ({attr}`~cattrs.ConversionPlan.back_edges`) that
  close cycles, so recursive and mutually recursive type graphs stay finite;
- the **missing handlers** ({attr}`~cattrs.ConversionPlan.missing_handlers`),
  i.e. positions where structuring would fail because no hook is registered;
- the **unverified** positions ({attr}`~cattrs.ConversionPlan.unverified_types`),
  which are `Any` or unresolved `TypeVar` annotations. These accept any input
  and are deliberately not treated as validated concrete types;
- every **hook factory invocation**
  ({attr}`~cattrs.ConversionPlan.factory_invocations`) performed while
  preparing the plan.

```{doctest}
>>> from attrs import define
>>> from cattrs import Converter

>>> @define
... class Inner:
...     x: int

>>> @define
... class Outer:
...     inner: Inner
...     things: list[int]

>>> converter = Converter()
>>> plan = converter.plan_structure(Outer)
>>> Inner in plan.nodes and int in plan.nodes
True
>>> plan.fields[Outer] == {"inner": Inner, "things": list[int]}
True
>>> plan.missing_handlers
[]
```

## Generic Types

Generic arguments are preserved throughout the graph. Planning
`Box[int]` keeps the bound `int` rather than collapsing the position to the
bare class, while planning an unparameterized generic reports its unresolved
`TypeVar` as unverified instead of guessing a concrete type.

## Hook Sources and Custom Hooks

Each node's {class}`~cattrs.HookSource` reports where its hook came from
(`singledispatch`, `direct`, `function`, `factory` or `fallback`) and whether
it is a converter built-in or a hook registered by your code. This makes it
easy to confirm that a custom hook is actually the one selected for a field.

## Preparation Is Not Side-Effect Free

Selecting a hook may require running a registered **hook factory**, and a
factory can perform side effects (the built-in generating factories, for
example, register additional direct hooks). Planning does not hide this: every
factory run is appended to
{attr}`~cattrs.ConversionPlan.factory_invocations`, including whether the
factory was custom. The hooks the factories produce are never invoked, and no
structured objects are created.

## Missing Handlers

When structuring, a type with no registered hook reaches the converter's
default raising fallback and is listed in
{attr}`~cattrs.ConversionPlan.missing_handlers`, with its node's source marked
`missing=True`. Unstructuring uses an identity passthrough fallback by default,
so unstructure plans report no missing handlers unless a custom fallback that
fails is configured.
