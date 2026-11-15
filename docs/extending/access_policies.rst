Attribute access policies (experimental)
========================================

When dumping ORM/ODM objects, reading an attribute can have side effects:
a ``property`` may run arbitrary code and a relationship descriptor may issue
a lazy database query. Marshmallow normally accesses attributes freely, but
you can opt into an explicit **attribute access policy** that constrains what
a dump is allowed to touch.

The API lives in the experimental module and may change in minor releases:

.. code-block:: python

    from marshmallow.experimental.access import AccessPolicy

Two modes are available:

- ``"instance"``: only data already present on the instance (its
  ``__dict__``, assigned ``__slots__`` or mapping entries) is read.
  Descriptors (``property``, ORM relationships, custom descriptors) are never
  invoked.
- ``"allowlist"``: behaves like ``"instance"``, but descriptors listed in
  ``allowed_fields`` *may* be invoked. Passing ``allowed_fields=None`` allows
  every descriptor corresponding to a declared field, treating the schema's
  field list as the trusted scope.

Basic usage
-----------

.. code-block:: python

    from marshmallow import Schema, fields
    from marshmallow.experimental.access import AccessPolicy


    class OrderSchema(Schema):
        id = fields.Int()
        # a lazy SQLAlchemy relationship
        items = fields.Nested(ItemSchema, many=True)


    with AccessPolicy("instance") as policy:
        data = OrderSchema().dump(order)

In ``"instance"`` mode the ``items`` relationship is never loaded, so the
dump cannot trigger a query you did not ask for. You can also call
``policy.dump(schema, obj)`` instead of using the context manager directly.

Allowing specific descriptors
-----------------------------

.. code-block:: python

    with AccessPolicy(
        "allowlist",
        allowed_fields={"id", "items"},
    ) as policy:
        data = OrderSchema().dump(order)

Only descriptors named in the allowlist may execute; every other attribute is
read from existing instance data or skipped.

The read plan
-------------

A policy records why each field did not produce a value, distinguishing a
**forbidden** descriptor access from an attribute that is genuinely
**missing** on the object:

.. code-block:: python

    policy.read_plan.evaluated_fields   # fields read from existing data
    policy.read_plan.forbidden_fields   # descriptors the policy blocked
    policy.read_plan.missing_fields     # attributes not set on the object
    policy.read_plan.unevaluated        # union of the two sets above

The forbidden/missing distinction is also available programmatically through
the ``marshmallow.access.forbidden_access`` sentinel, which is deliberately
different from ``marshmallow.missing``.

Scope of the policy
-------------------

Policies apply to every field path involved in a dump without ever running a
getter merely to check whether an attribute exists:

- regular fields and dot-delimited ``attribute`` paths are checked segment by
  segment;
- :class:`~marshmallow.fields.Nested` schemas (including lists of nested
  schemas) inherit the active policy automatically;
- :class:`~marshmallow.fields.Method` and
  :class:`~marshmallow.fields.Function` fields execute schema-declared
  callables, so they are gated by the same declaration scope and are skipped
  when not allowed.

Without an active policy, ``Schema.dump`` behaves exactly as before, so
existing schemas keep working unchanged. Custom
:meth:`~marshmallow.Schema.get_attribute` implementations can keep honoring a
policy by delegating to :func:`marshmallow.access.access_value`.
