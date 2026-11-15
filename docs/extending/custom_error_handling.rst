Custom error handling
=====================

By default, `Schema.load <marshmallow.Schema.load>` will raise a :exc:`ValidationError <marshmallow.exceptions.ValidationError>` if passed invalid data.

You can specify a custom error-handling function for a :class:`Schema` by overriding the `handle_error <marshmallow.Schema.handle_error>`  method. The method receives the :exc:`ValidationError <marshmallow.exceptions.ValidationError>` and the original input data to be deserialized.

.. code-block:: python

    import logging
    from marshmallow import Schema, fields


    class AppError(Exception):
        pass


    class UserSchema(Schema):
        email = fields.Email()

        def handle_error(self, exc, data, **kwargs):
            """Log and raise our custom exception when (de)serialization fails."""
            logging.error(exc.messages)
            raise AppError("An error occurred with input: {0}".format(data))


    schema = UserSchema()
    schema.load({"email": "invalid-email"})  # raises AppError

Typed, structured error paths
-----------------------------

The ``messages`` structure only keeps strings and integers as container keys.
That loses the origin of a key: an integer can be a ``List``/``Tuple`` position
*or* a real ``Dict`` key, and the literal ``"key"``/``"value"`` markers used by
mapping fields are indistinguishable from ordinary field names. The type
information cannot be reconstructed from the flattened string messages.

:attr:`ValidationError.structured <marshmallow.exceptions.ValidationError.structured>`
exposes the same errors as a list of
:class:`StructuredError <marshmallow.structured.StructuredError>` objects. Each
one carries an ordered, typed ``path`` of
:class:`PathSegment <marshmallow.structured.PathSegment>` objects, where every
segment has an explicit ``kind``:

- ``"field"`` – a field selected on a schema,
- ``"index"`` – a positional element of a `List <marshmallow.fields.List>`,
- ``"position"`` – a positional element of a `Tuple <marshmallow.fields.Tuple>`,
- ``"mapping_key"`` – a key of a `Dict <marshmallow.fields.Dict>`,
- ``"mapping_value"`` – the value associated with a mapping key,
- ``"schema"`` – a schema-level (``_schema``) error.

An empty path marks a top-level schema-level error, so schema-level and
field-level errors can coexist.

.. code-block:: python

    from marshmallow import Schema, fields
    from marshmallow.exceptions import ValidationError


    class MySchema(Schema):
        nums = fields.List(fields.Int())
        pair = fields.Tuple((fields.Int(), fields.Int()))
        mapping = fields.Dict(keys=fields.Str(), values=fields.Int())


    try:
        MySchema().load(
            {"nums": ["x"], "pair": ("x", 1), "mapping": {1: "bad"}}
        )
    except ValidationError as error:
        for structured_error in error.structured:
            print(
                [(segment.kind, segment.value) for segment in structured_error.path],
                structured_error.messages,
            )

The original ``messages`` structure is left untouched.

Non-string mapping keys
-----------------------

JSON object keys are always strings, so arbitrary mapping keys (integers,
tuples, custom objects, ...) cannot be represented directly. Use
:meth:`ValidationError.structured_jsonable
<marshmallow.exceptions.ValidationError.structured_jsonable>` (or
:func:`marshmallow.structured.to_jsonable`) to obtain JSON-serializable data.
Non-string keys are wrapped in an explicit, type-tagged envelope rather than
coerced with ``str()``, which would make distinct keys collide:

.. code-block:: python

    import json

    from marshmallow.structured import encode_key

    json.dumps(encode_key((1, 2)))
    # {"__marshmallow_key__": true, "type": "tuple", "value": [1, 2]}
