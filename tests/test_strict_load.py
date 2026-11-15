import decimal

import pytest

from marshmallow import Schema, fields
from marshmallow.exceptions import ValidationError


class StrictTypesSchema(Schema):
    text = fields.String()
    integer = fields.Integer()
    floating = fields.Float()
    boolean = fields.Boolean()
    integers = fields.List(fields.Integer())
    booleans = fields.Dict(keys=fields.String(), values=fields.Boolean())
    amount = fields.Decimal()


STRICT_VALID = {
    "text": "value",
    "integer": 42,
    "floating": 12.5,
    "boolean": True,
    "integers": [1, 2],
    "booleans": {"enabled": False},
    "amount": decimal.Decimal("12.5"),
}

LENIENT_VALID = {
    "integer": "42",
    "floating": "12.5",
    "boolean": 1,
    "integers": ["1", "2"],
    "booleans": {"enabled": "true"},
    "amount": "12.5",
}


def test_strict_load_accepts_declared_types():
    assert StrictTypesSchema().load(STRICT_VALID, strict=True) == STRICT_VALID


@pytest.mark.parametrize(
    ("data", "field_name"),
    [
        ({"text": 1}, "text"),
        ({"integer": "42"}, "integer"),
        ({"floating": "12.5"}, "floating"),
        ({"boolean": 1}, "boolean"),
        ({"integers": ["1"]}, "integers"),
        ({"booleans": {"enabled": "true"}}, "booleans"),
        ({"amount": "12.5"}, "amount"),
    ],
)
def test_strict_load_rejects_lenient_conversions(data, field_name):
    with pytest.raises(ValidationError) as error:
        StrictTypesSchema().load(data, strict=True)
    assert field_name in error.value.messages


def test_lenient_load_is_default_and_leaves_no_strict_state():
    schema = StrictTypesSchema()
    assert schema.load(LENIENT_VALID) == {
        "integer": 42,
        "floating": 12.5,
        "boolean": True,
        "integers": [1, 2],
        "booleans": {"enabled": True},
        "amount": decimal.Decimal("12.5"),
    }
    with pytest.raises(ValidationError):
        schema.load(LENIENT_VALID, strict=True)
    assert schema.load(LENIENT_VALID) == {
        "integer": 42,
        "floating": 12.5,
        "boolean": True,
        "integers": [1, 2],
        "booleans": {"enabled": True},
        "amount": decimal.Decimal("12.5"),
    }


class FieldOverrideSchema(Schema):
    forced_strict = fields.Integer(strict=True)
    forced_lenient = fields.Boolean(strict=False)
    lenient_items = fields.List(fields.Integer(strict=False))


def test_field_level_overrides_take_precedence():
    strict_data = {
        "forced_strict": 1,
        "forced_lenient": 1,
        "lenient_items": ["1"],
    }
    assert FieldOverrideSchema().load(strict_data, strict=True) == {
        "forced_strict": 1,
        "forced_lenient": True,
        "lenient_items": [1],
    }

    with pytest.raises(ValidationError) as error:
        FieldOverrideSchema().load(
            {"forced_strict": "1", "forced_lenient": 1, "lenient_items": ["1"]},
            strict=True,
        )
    assert "forced_strict" in error.value.messages

    assert FieldOverrideSchema().load(
        {"forced_strict": 1, "forced_lenient": "true", "lenient_items": ["1"]}
    ) == {"forced_strict": 1, "forced_lenient": True, "lenient_items": [1]}


class InnerSchema(Schema):
    integer = fields.Integer()


class NestedSchema(Schema):
    inner = fields.Nested(InnerSchema)


def test_nested_strict_propagates_without_mutating_reused_fields():
    schema = NestedSchema()
    with pytest.raises(ValidationError) as error:
        schema.load({"inner": {"integer": "1"}}, strict=True)
    assert error.value.messages == {"inner": {"integer": ["Not a valid integer."]}}

    assert schema.load({"inner": {"integer": "1"}}) == {"inner": {"integer": 1}}
    assert schema.fields["inner"].schema.fields["integer"].strict is None


class DecimalTextSchema(Schema):
    text_allowed = fields.Decimal(allow_string=True)
    text_denied = fields.Decimal(allow_string=False)


def test_decimal_text_input_is_field_declared():
    schema = DecimalTextSchema()
    assert schema.load(
        {
            "text_allowed": "12.5",
            "text_denied": decimal.Decimal("12.5"),
        },
        strict=True,
    ) == {
        "text_allowed": decimal.Decimal("12.5"),
        "text_denied": decimal.Decimal("12.5"),
    }

    with pytest.raises(ValidationError) as error:
        schema.load(
            {
                "text_allowed": decimal.Decimal("12.5"),
                "text_denied": "12.5",
            }
        )
    assert "text_denied" in error.value.messages


def test_schema_validate_accepts_strict():
    assert StrictTypesSchema().validate(STRICT_VALID, strict=True) == {}
    assert "integer" in StrictTypesSchema().validate(
        {"integer": "42"}, strict=True
    )
