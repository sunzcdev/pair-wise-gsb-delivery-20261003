from __future__ import annotations

from decimal import Decimal

from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

from jsonschema import Draft202012Validator, Draft4Validator
from jsonschema._containment import (
    COMPATIBLE,
    INCOMPATIBLE,
    UNKNOWN,
    ContainmentResult,
    check_scalar_numeric_containment as check,
)


def assert_compatible(result):
    assert isinstance(result, ContainmentResult)
    assert result.status == COMPATIBLE
    assert result.witness is None


def assert_incompatible(result, old_validator, new_validator):
    assert result.status == INCOMPATIBLE
    assert isinstance(result.witness, Decimal)
    # The defining requirement: the very same Decimal witness must be
    # re-validated by both validators, accepted by the old one and rejected
    # by the new one.
    assert old_validator.is_valid(result.witness)
    assert not new_validator.is_valid(result.witness)
    assert result.old_accepts_witness is True
    assert result.new_accepts_witness is False


def assert_unknown(result, keyword=None):
    assert result.status == UNKNOWN
    assert result.witness is None
    if keyword is not None:
        assert result.unsupported_keyword == keyword


class TestCompatible:
    def test_identical_closed_interval(self):
        schema = {"type": "number", "minimum": 0, "maximum": 10}
        assert_compatible(check(schema, dict(schema)))

    def test_widened_interval_is_contained(self):
        old = {"type": "integer", "minimum": 2, "maximum": 8}
        new = {"type": "number", "minimum": 0, "maximum": 10}
        assert_compatible(check(old, new))

    def test_open_old_boundary_is_contained_in_closed_new(self):
        old = {"type": "number", "exclusiveMinimum": 5}
        new = {"type": "number", "minimum": 5}
        assert_compatible(check(old, new))

    def test_every_old_enum_member_survives(self):
        old = {"type": "integer", "enum": [1, 2, 3]}
        new = {"type": "number", "enum": [3, 2, 1, 4]}
        assert_compatible(check(old, new))

    def test_annotations_and_unused_defs_are_ignored(self):
        old = {
            "type": "integer",
            "minimum": 0,
            "title": "old",
            "description": "legacy contract",
            "$defs": {"unused": {"type": "string"}},
        }
        new = {"type": "integer", "minimum": 0, "$comment": "new contract"}
        assert_compatible(check(old, new))

    def test_empty_old_domain_is_vacuously_contained(self):
        old = {"type": "integer", "minimum": 10, "maximum": 5}
        assert_compatible(check(old, {"type": "integer"}))

    def test_false_schema_old_is_vacuously_contained(self):
        assert_compatible(check(False, {"minimum": 0}))

    def test_non_integral_integer_interval_is_empty(self):
        old = {
            "type": "integer",
            "minimum": Decimal("1.5"),
            "maximum": Decimal("1.5"),
        }
        assert_compatible(check(old, {"type": "integer"}))

    def test_resolvable_local_ref_chain(self):
        old = {
            "$defs": {
                "c": {"type": "integer", "minimum": 1},
                "b": {"$ref": "#/$defs/c"},
            },
            "$ref": "#/$defs/b",
        }
        assert_compatible(check(old, {"type": "integer", "minimum": 0}))

    def test_draft4_boolean_exclusive_old_is_narrower(self):
        old = {
            "$schema": "http://json-schema.org/draft-04/schema#",
            "type": "number",
            "minimum": 5,
            "exclusiveMinimum": True,
        }
        new = {
            "$schema": "http://json-schema.org/draft-04/schema#",
            "type": "number",
            "minimum": 5,
        }
        assert_compatible(check(old, new))


class TestIncompatible:
    def test_tightened_lower_bound(self):
        old_schema = {"type": "number", "minimum": 0}
        new_schema = {"type": "number", "minimum": 5}
        result = check(old_schema, new_schema)
        old_v = _validator(Draft202012Validator, old_schema)
        new_v = _validator(Draft202012Validator, new_schema)
        assert_incompatible(result, old_v, new_v)
        assert Decimal(0) <= result.witness < Decimal(5)

    def test_tightened_upper_bound_integer(self):
        old_schema = {"type": "integer", "maximum": 10}
        new_schema = {"type": "integer", "maximum": 8}
        result = check(old_schema, new_schema)
        old_v = _validator(Draft202012Validator, old_schema)
        new_v = _validator(Draft202012Validator, new_schema)
        assert_incompatible(result, old_v, new_v)
        assert result.witness == Decimal(9)

    def test_closed_to_open_lower_boundary_value(self):
        old_schema = {"type": "number", "minimum": 5}
        new_schema = {"type": "number", "exclusiveMinimum": 5}
        result = check(old_schema, new_schema)
        old_v = _validator(Draft202012Validator, old_schema)
        new_v = _validator(Draft202012Validator, new_schema)
        assert_incompatible(result, old_v, new_v)
        assert result.witness == Decimal(5)

    def test_closed_to_open_upper_boundary_value(self):
        old_schema = {"type": "number", "maximum": 5}
        new_schema = {"type": "number", "exclusiveMaximum": 5}
        result = check(old_schema, new_schema)
        old_v = _validator(Draft202012Validator, old_schema)
        new_v = _validator(Draft202012Validator, new_schema)
        assert_incompatible(result, old_v, new_v)
        assert result.witness == Decimal(5)

    def test_integer_narrowing_exposes_fractional_witness(self):
        old_schema = {"type": "number", "minimum": 0}
        new_schema = {"type": "integer", "minimum": 0}
        result = check(old_schema, new_schema)
        old_v = _validator(Draft202012Validator, old_schema)
        new_v = _validator(Draft202012Validator, new_schema)
        assert_incompatible(result, old_v, new_v)
        assert result.witness == Decimal("0.5")

    def test_dropped_enum_member_is_the_witness(self):
        old_schema = {"type": "integer", "enum": [1, 2, 3]}
        new_schema = {"type": "integer", "enum": [1, 2]}
        result = check(old_schema, new_schema)
        old_v = _validator(Draft202012Validator, old_schema)
        new_v = _validator(Draft202012Validator, new_schema)
        assert_incompatible(result, old_v, new_v)
        assert result.witness == Decimal(3)

    def test_new_enum_cannot_cover_infinite_old_domain(self):
        old_schema = {"type": "integer", "minimum": 0}
        new_schema = {"type": "integer", "enum": [1, 2, 3]}
        result = check(old_schema, new_schema)
        old_v = _validator(Draft202012Validator, old_schema)
        new_v = _validator(Draft202012Validator, new_schema)
        assert_incompatible(result, old_v, new_v)
        assert result.witness == Decimal(0)

    def test_draft4_closed_old_vs_open_new(self):
        old_schema = {
            "$schema": "http://json-schema.org/draft-04/schema#",
            "type": "number",
            "minimum": 5,
        }
        new_schema = {
            "$schema": "http://json-schema.org/draft-04/schema#",
            "type": "number",
            "minimum": 5,
            "exclusiveMinimum": True,
        }
        result = check(old_schema, new_schema)
        old_v = _validator(Draft4Validator, old_schema)
        new_v = _validator(Draft4Validator, new_schema)
        assert_incompatible(result, old_v, new_v)
        assert result.witness == Decimal(5)

    def test_string_bounds_keep_full_decimal_precision(self):
        old_schema = {"type": "number", "minimum": "0.30000000000000004"}
        new_schema = {"type": "number", "minimum": "0.30000000000000005"}
        result = check(old_schema, new_schema)
        old_v = _validator(Draft202012Validator, old_schema)
        new_v = _validator(Draft202012Validator, new_schema)
        assert_incompatible(result, old_v, new_v)
        # The witness sits strictly between the two decimal bounds.
        assert Decimal("0.30000000000000004") <= result.witness
        assert result.witness < Decimal("0.30000000000000005")

    def test_witness_uses_supplied_validators_and_registry(self):
        registry = Registry().with_resource(
            "https://contracts.test/band.json",
            Resource.from_contents(
                {"type": "integer", "minimum": 5},
                default_specification=DRAFT202012,
            ),
        )
        old_v = Draft202012Validator(
            {"$ref": "https://contracts.test/band.json"},
            registry=registry,
        )
        new_v = Draft202012Validator(
            {"type": "integer", "minimum": 7},
            registry=registry,
        )
        result = check(old_validator=old_v, new_validator=new_v)
        assert_incompatible(result, _decimal_validator(old_v),
                            _decimal_validator(new_v))
        assert result.witness == Decimal(6)

    def test_coexisting_inclusive_and_exclusive_lower_bounds(self):
        # Both keywords are independent in draft 6+; neither may overwrite
        # the other when extracting the effective bound.
        old_schema = {
            "type": "number",
            "minimum": Decimal("2.5"),
            "maximum": Decimal("2.5"),
        }
        new_schema = {
            "type": "integer",
            "minimum": 0,
            "maximum": 1,
        }
        result = check(old_schema, new_schema)
        old_v = _validator(Draft202012Validator, old_schema)
        new_v = _validator(Draft202012Validator, new_schema)
        assert_incompatible(result, old_v, new_v)
        assert result.witness == Decimal("2.5")

    def test_old_domain_entirely_below_new_lower_bound(self):
        old_schema = {"maximum": Decimal("-10")}
        new_schema = {
            "minimum": Decimal("-1.5"),
            "maximum": Decimal("10"),
        }
        result = check(old_schema, new_schema)
        old_v = _validator(Draft202012Validator, old_schema)
        new_v = _validator(Draft202012Validator, new_schema)
        assert_incompatible(result, old_v, new_v)
        assert result.witness <= Decimal("-10")

    def test_open_interval_is_not_covered_by_discrete_enum(self):
        old_schema = {
            "minimum": Decimal("9.99"),
            "exclusiveMaximum": Decimal("10"),
        }
        new_schema = {
            "type": "number",
            "exclusiveMinimum": 0,
            "enum": [5, 10, 1, Decimal("9.99")],
        }
        # The old domain is the open interval [9.99, 10); the new schema
        # only accepts discrete enum members, so 9.995 escapes and the
        # correct verdict is incompatibility, not compatibility.
        result = check(old_schema, new_schema)
        old_v = _validator(Draft202012Validator, old_schema)
        new_v = _validator(Draft202012Validator, new_schema)
        assert_incompatible(result, old_v, new_v)
        assert Decimal("9.99") < result.witness < Decimal(10)

    def test_collapsed_point_inside_enum_is_compatible(self):
        old_schema = {
            "minimum": Decimal("9.99"),
            "maximum": Decimal("9.99"),
        }
        new_schema = {"enum": [Decimal("9.99"), Decimal("10")]}
        assert_compatible(check(old_schema, new_schema))

    def test_string_numeric_enum_members_keep_precision(self):
        old_schema = {"type": "integer", "enum": ["1", "2", "3"]}
        new_schema = {"type": "integer", "enum": ["1", "2"]}
        result = check(old_schema, new_schema)
        old_v = _validator(Draft202012Validator, old_schema)
        new_v = _validator(Draft202012Validator, new_schema)
        assert_incompatible(result, old_v, new_v)
        assert result.witness == Decimal(3)

    def test_bounded_integer_domain_against_enum_is_enumerated(self):
        old_schema = {"type": "integer", "minimum": 1, "maximum": 3}
        new_schema = {"type": "integer", "enum": [1, 2, 3, 4]}
        assert_compatible(check(old_schema, new_schema))

        narrowed = {"type": "integer", "enum": [1, 2]}
        result = check(old_schema, narrowed)
        old_v = _validator(Draft202012Validator, old_schema)
        new_v = _validator(Draft202012Validator, narrowed)
        assert_incompatible(result, old_v, new_v)
        assert result.witness == Decimal(3)


class TestUnknown:
    def test_allof_is_unsupported(self):
        assert_unknown(
            check({"allOf": [{"minimum": 1}]}, {"minimum": 0}),
            keyword="allOf",
        )

    def test_multipleof_is_unsupported(self):
        assert_unknown(
            check({"type": "number", "multipleOf": 2}, {"minimum": 0}),
            keyword="multipleOf",
        )

    def test_non_numeric_type_is_unsupported(self):
        assert_unknown(check({"type": "string"}, {"type": "string"}))

    def test_multi_type_with_string_is_unsupported(self):
        assert_unknown(
            check({"type": ["integer", "string"]}, {"type": "integer"}),
        )

    def test_const_is_unsupported(self):
        assert_unknown(check({"const": 1}, {"minimum": 0}), keyword="const")

    def test_not_is_unsupported(self):
        assert_unknown(check({"not": {"minimum": 3}}, {}), keyword="not")

    def test_non_numeric_enum_member_is_unsupported(self):
        assert_unknown(check({"enum": ["a", "b"]}, {}))

    def test_mixed_enum_is_unsupported(self):
        assert_unknown(check({"enum": [1, "x"]}, {}))

    def test_unresolvable_pointer_ref(self):
        assert_unknown(check({"$ref": "#/$defs/missing"}, {}))

    def test_unresolvable_remote_ref(self):
        assert_unknown(
            check(
                {"$ref": "https://nope.example/x.json"},
                {"type": "integer"},
            ),
        )

    def test_cyclic_ref(self):
        schema = {
            "$defs": {
                "a": {"$ref": "#/$defs/b"},
                "b": {"$ref": "#/$defs/a"},
            },
            "$ref": "#/$defs/a",
        }
        assert_unknown(check(schema, {}))

    def test_missing_inputs(self):
        assert_unknown(check(new_schema={"minimum": 0}))

    def test_unknown_dialect_is_unsupported(self):
        assert_unknown(
            check(
                {
                    "$schema": "https://unknown.example/dialect",
                    "minimum": 0,
                },
                {"minimum": 0},
            ),
        )


def _decimal_type_checker(validator_cls):
    from decimal import Decimal as _Decimal

    def is_integer(checker, instance):
        if isinstance(instance, bool):
            return False
        if isinstance(instance, int):
            return True
        if isinstance(instance, _Decimal):
            return instance == instance.to_integral_value()
        return False

    def is_number(checker, instance):
        if isinstance(instance, bool):
            return False
        from numbers import Number

        return isinstance(instance, (Number, _Decimal))

    return validator_cls.TYPE_CHECKER.redefine_many(
        {"integer": is_integer, "number": is_number},
    )


def _validator(validator_cls, schema):
    from jsonschema.validators import extend

    decimal_cls = extend(
        validator_cls,
        type_checker=_decimal_type_checker(validator_cls),
    )
    return decimal_cls(_decimal(schema))


def _decimal(schema):
    """Decimal-literal copy of a small numeric schema for re-validation."""

    if isinstance(schema, bool):
        return schema
    if isinstance(schema, dict):
        return {key: _decimal(value) for key, value in schema.items()}
    if isinstance(schema, list):
        return [_decimal(each) for each in schema]
    if isinstance(schema, int):
        return Decimal(schema)
    if isinstance(schema, float):
        return Decimal(str(schema))
    if isinstance(schema, str):
        try:
            return Decimal(schema)
        except Exception:
            return schema
    return schema


def _decimal_validator(validator):
    from jsonschema._containment import _decimal_validator_for

    return _decimal_validator_for(validator)
