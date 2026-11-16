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

When the same schema is used with ``only``/``exclude`` to produce different
views of a collection, a schema-level validator may lose access to the fields
it needs. Validators can declare their field dependencies with the
``requires`` parameter, and the ``on_missing`` parameter controls what happens
when a dependency is projected out of the schema:

- ``"skip"`` (default): the validator does not run.
- ``"error"``: a schema-level validation error is reported, listing the
  missing dependencies.
- ``"run"``: the validator runs anyway and must handle absent keys itself.

.. code-block:: python

    from marshmallow import Schema, fields, validates_schema, ValidationError


    class RangeSchema(Schema):
        start = fields.Integer()
        end = fields.Integer()

        @validates_schema(requires=("start", "end"), on_missing="error")
        def validate_range(self, data, **kwargs):
            if data["start"] > data["end"]:
                raise ValidationError("start must not exceed end")


    RangeSchema(exclude=("end",)).validate({"start": 5})
    # => {'_schema': ["Schema-level validator 'validate_range' did not run:
    #                  missing required fields: end."]}

Dependencies are canonical field names: a field's ``data_key`` does not change
its identity. Dependencies of validators on ``Nested`` schemas are resolved
against the nested schema's own fields. Depending on a field that is not
declared on the schema raises `ValueError`.

This mechanism is independent of ``skip_on_field_errors``: a dependency that
failed to deserialize at runtime is a field error, not a missing dependency.

Whenever a schema-level validator does not run, a diagnostic is recorded on
``schema.validation_diagnostics`` explaining why, so a skipped validator is
never silently treated as a validation success. The list is reset on every
``load``/``validate`` call.

.. code-block:: python

    schema = RangeSchema(exclude=("end",))
    schema.validate({"start": 5})  # => {}
    schema.validation_diagnostics
    # => [{'validator': 'validate_range',
    #      'reason': 'missing_dependencies',
    #      'missing_fields': ['end'],
    #      'policy': 'skip'}]

.. versionadded:: 4.5.0
