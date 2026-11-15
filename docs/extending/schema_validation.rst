.. _schema_validation:

Schema-level validation
=======================

You can register schema-level validation functions for a :class:`Schema` using the `marshmallow.validates_schema <marshmallow.decorators.validates_schema>` decorator. By default, schema-level validation errors will be stored on the ``_schema`` key of the errors dictionary.

.. code-block:: python

    from marshmallow import Schema, fields, validates_schema, ValidationError


    class NumberSchema(Schema):
        field_a = fields.Integer()
        field_b = fields.Integer()

        @validates_schema
        def validate_numbers(self, data, **kwargs):
            if data["field_b"] >= data["field_a"]:
                raise ValidationError("field_a must be greater than field_b")


    schema = NumberSchema()
    try:
        schema.load({"field_a": 1, "field_b": 2})
    except ValidationError as err:
        err.messages["_schema"]
    # => ["field_a must be greater than field_b"]

Storing errors on specific fields
---------------------------------

It is possible to report errors on fields and subfields using a `dict`.

When multiple schema-leval validator return errors, the error structures are merged together in the :exc:`ValidationError <marshmallow.exceptions.ValidationError>` raised at the end of the validation.

.. code-block:: python

    from marshmallow import Schema, fields, validates_schema, ValidationError


    class NumberSchema(Schema):
        field_a = fields.Integer()
        field_b = fields.Integer()
        field_c = fields.Integer()
        field_d = fields.Integer()

        @validates_schema
        def validate_lower_bound(self, data, **kwargs):
            errors = {}
            if data["field_b"] <= data["field_a"]:
                errors["field_b"] = ["field_b must be greater than field_a"]
            if data["field_c"] <= data["field_a"]:
                errors["field_c"] = ["field_c must be greater than field_a"]
            if errors:
                raise ValidationError(errors)

        @validates_schema
        def validate_upper_bound(self, data, **kwargs):
            errors = {}
            if data["field_b"] >= data["field_d"]:
                errors["field_b"] = ["field_b must be lower than field_d"]
            if data["field_c"] >= data["field_d"]:
                errors["field_c"] = ["field_c must be lower than field_d"]
            if errors:
                raise ValidationError(errors)


    schema = NumberSchema()
    try:
        schema.load({"field_a": 3, "field_b": 2, "field_c": 1, "field_d": 0})
    except ValidationError as err:
        err.messages
    # => {
    #     'field_b': [
    #         'field_b must be greater than field_a',
    #         'field_b must be lower than field_d'
    #     ],
    #     'field_c': [
    #         'field_c must be greater than field_a',
    #         'field_c must be lower than field_d'
    #     ]
    #    }

Declaring field dependencies
----------------------------

When the same schema is used with ``only`` or ``exclude`` to produce different
views of a collection, a schema-level validator may depend on fields that have
been projected out. Validators can declare their field dependencies with the
``requires`` argument, and the ``on_missing_dependencies`` option controls what
happens when a dependency is not part of the schema:

- ``"error"`` (default): The validator does not run and a schema-level
  validation error is reported for the missing dependencies.
- ``"skip"``: The validator does not run and no error is reported.
- ``"run"``: The validator runs anyway.

.. code-block:: python

    from marshmallow import Schema, fields, validates_schema, ValidationError


    class NumberSchema(Schema):
        field_a = fields.Integer()
        field_b = fields.Integer()

        @validates_schema(requires=("field_a", "field_b"))
        def validate_numbers(self, data, **kwargs):
            if data["field_b"] >= data["field_a"]:
                raise ValidationError("field_a must be greater than field_b")


    schema = NumberSchema(exclude=("field_b",))
    schema.validate({"field_a": 1})
    # => {"_schema": ["Schema-level validator 'validate_numbers' was not run:
    #     missing required fields: 'field_b'."]}

Dependencies are declared (canonical) field names; a field's ``data_key`` does
not change its identity. Dependencies of validators on nested schemas are
resolved against the nested schema's own fields.

Whenever a schema-level validator does not run -- either because of missing
dependencies or because ``skip_on_field_errors`` is in effect -- a record is
added to `Schema.validator_diagnostics <marshmallow.schema.Schema.validator_diagnostics>`
explaining why. A skipped validator is never counted as a validation success.

.. versionadded:: 4.5.0
