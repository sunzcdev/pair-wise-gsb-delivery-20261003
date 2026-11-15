"""Tests for patch-mode loading (`Schema.load_patch`)."""

import pytest

from marshmallow import (
    DELETE,
    EXCLUDE,
    INCLUDE,
    PatchOp,
    PatchValue,
    Schema,
    ValidationError,
    fields,
    validate,
    validates,
)


class PatchSchema(Schema):
    name = fields.Str(required=True)
    age = fields.Int(load_default=0)
    nickname = fields.Str(deletable=True, allow_none=True)
    email = fields.Str(allow_none=True)


class TestPatchModeBasics:
    def test_absent_fields_are_not_in_result(self):
        result = PatchSchema().load_patch({"name": "Alice"})
        assert result == {"name": PatchValue.set("Alice")}
        assert "age" not in result
        assert "nickname" not in result

    def test_absent_field_does_not_apply_load_default(self):
        result = PatchSchema().load_patch({})
        assert result == {}

    def test_absent_required_field_does_not_error(self):
        # required is a full-object rule; patch mode does not enforce it
        result = PatchSchema().load_patch({"age": 3})
        assert result == {"age": PatchValue.set(3)}

    def test_load_still_enforces_required(self):
        # The full-object required rules are not globally modified.
        with pytest.raises(ValidationError) as excinfo:
            PatchSchema().load({"age": 3})
        assert excinfo.value.messages == {"name": ["Missing data for required field."]}

    def test_explicit_none_is_a_set_operation(self):
        result = PatchSchema().load_patch({"email": None})
        assert result == {"email": PatchValue.set(None)}

    def test_explicit_none_validated_against_allow_none(self):
        with pytest.raises(ValidationError) as excinfo:
            PatchSchema().load_patch({"name": None})
        assert excinfo.value.messages == {"name": ["Field may not be null."]}

    def test_delete_marker_produces_delete_operation(self):
        result = PatchSchema().load_patch({"nickname": DELETE})
        assert result == {"nickname": PatchValue.delete()}
        assert result["nickname"].op is PatchOp.DELETE

    def test_delete_not_allowed_by_default(self):
        with pytest.raises(ValidationError) as excinfo:
            PatchSchema().load_patch({"name": DELETE})
        assert excinfo.value.messages == {"name": ["Field may not be deleted."]}

    def test_delete_marker_does_not_collide_with_business_values(self):
        # The DELETE marker is a unique sentinel object; plain values such as
        # strings or None are never interpreted as deletions.
        result = PatchSchema().load_patch({"nickname": "DELETE"})
        assert result == {"nickname": PatchValue.set("DELETE")}

    def test_set_values_run_full_validation(self):
        class S(Schema):
            age = fields.Int(validate=validate.Range(min=0), deletable=True)

        with pytest.raises(ValidationError) as excinfo:
            S().load_patch({"age": -1})
        assert excinfo.value.messages == {
            "age": ["Must be greater than or equal to 0."]
        }
        with pytest.raises(ValidationError) as excinfo:
            S().load_patch({"age": "not-an-int"})
        assert excinfo.value.messages == {"age": ["Not a valid integer."]}

    def test_delete_skips_value_conversion(self):
        class S(Schema):
            # DELETE would fail int conversion and range validation if it ran
            age = fields.Int(validate=validate.Range(min=0), deletable=True)

        result = S().load_patch({"age": DELETE})
        assert result == {"age": PatchValue.delete()}

    def test_set_values_run_pre_load_and_post_load(self):
        class S(Schema):
            name = fields.Str(
                pre_load=lambda v: v.strip(),
                post_load=lambda v: v.upper(),
            )

        result = S().load_patch({"name": "  alice  "})
        assert result == {"name": PatchValue.set("ALICE")}

    def test_data_key_and_attribute_are_respected(self):
        class S(Schema):
            name = fields.Str(data_key="Name", attribute="user_name")

        result = S().load_patch({"Name": "Alice"})
        assert result == {"user_name": PatchValue.set("Alice")}

    def test_unknown_raise_by_default(self):
        with pytest.raises(ValidationError) as excinfo:
            PatchSchema().load_patch({"unknown_field": 1})
        assert excinfo.value.messages == {"unknown_field": ["Unknown field."]}

    def test_unknown_exclude(self):
        result = PatchSchema().load_patch({"name": "Alice", "x": 1}, unknown=EXCLUDE)
        assert result == {"name": PatchValue.set("Alice")}

    def test_unknown_include_wraps_raw_set_operation(self):
        result = PatchSchema().load_patch({"x": 1}, unknown=INCLUDE)
        assert result == {"x": PatchValue.set(1)}

    def test_many(self):
        result = PatchSchema().load_patch(
            [{"name": "Alice"}, {"nickname": DELETE}], many=True
        )
        assert result == [
            {"name": PatchValue.set("Alice")},
            {"nickname": PatchValue.delete()},
        ]

    def test_validates_decorator_receives_unwrapped_set_value(self):
        class S(Schema):
            age = fields.Int(deletable=True)

            @validates("age")
            def validate_age(self, value, **kwargs):
                if value is not None and value < 0:
                    raise ValidationError("Must be non-negative.")

        assert S().load_patch({"age": 3}) == {"age": PatchValue.set(3)}
        assert S().load_patch({"age": DELETE}) == {"age": PatchValue.delete()}
        with pytest.raises(ValidationError) as excinfo:
            S().load_patch({"age": -1})
        assert excinfo.value.messages == {"age": ["Must be non-negative."]}


class InnerSchema(Schema):
    x = fields.Int(required=True)
    y = fields.Str(deletable=True, load_default="default-y")


class OuterSchema(Schema):
    inner = fields.Nested(InnerSchema, deletable=True)
    name = fields.Str()


class TestNestedPatch:
    def test_nested_fields_loaded_in_patch_mode(self):
        result = OuterSchema().load_patch({"inner": {"x": 1}})
        assert result == {"inner": PatchValue.set({"x": PatchValue.set(1)})}

    def test_nested_missing_field_does_not_apply_load_default(self):
        result = OuterSchema().load_patch({"inner": {"x": 1}})
        assert "y" not in result["inner"].value

    def test_nested_required_not_enforced_in_patch_mode(self):
        result = OuterSchema().load_patch({"inner": {"y": "a"}})
        assert result == {"inner": PatchValue.set({"y": PatchValue.set("a")})}

    def test_nested_load_still_enforces_required(self):
        with pytest.raises(ValidationError) as excinfo:
            OuterSchema().load({"inner": {"y": "a"}})
        assert excinfo.value.messages == {
            "inner": {"x": ["Missing data for required field."]}
        }

    def test_nested_delete(self):
        result = OuterSchema().load_patch({"inner": {"y": DELETE}})
        assert result == {"inner": PatchValue.set({"y": PatchValue.delete()})}

    def test_nested_delete_not_allowed(self):
        with pytest.raises(ValidationError) as excinfo:
            OuterSchema().load_patch({"inner": {"x": DELETE}})
        assert excinfo.value.messages == {"inner": {"x": ["Field may not be deleted."]}}

    def test_nested_field_itself_deleted(self):
        result = OuterSchema().load_patch({"inner": DELETE})
        assert result == {"inner": PatchValue.delete()}

    def test_nested_field_delete_not_allowed(self):
        class S(Schema):
            inner = fields.Nested(InnerSchema)

        with pytest.raises(ValidationError) as excinfo:
            S().load_patch({"inner": DELETE})
        assert excinfo.value.messages == {"inner": ["Field may not be deleted."]}

    def test_nested_set_value_still_validated(self):
        with pytest.raises(ValidationError) as excinfo:
            OuterSchema().load_patch({"inner": {"x": "not-an-int"}})
        assert excinfo.value.messages == {"inner": {"x": ["Not a valid integer."]}}

    def test_nested_many(self):
        class S(Schema):
            items = fields.Nested(InnerSchema, many=True)

        result = S().load_patch({"items": [{"x": 1}, {"y": DELETE}]})
        assert result == {
            "items": PatchValue.set(
                [{"x": PatchValue.set(1)}, {"y": PatchValue.delete()}]
            )
        }

    def test_deeply_nested_presence_preserved_at_each_level(self):
        class Mid(Schema):
            leaf = fields.Str()
            inner = fields.Nested(InnerSchema)

        class Top(Schema):
            mid = fields.Nested(Mid)

        result = Top().load_patch({"mid": {"inner": {"x": 5}}})
        assert result == {
            "mid": PatchValue.set({"inner": PatchValue.set({"x": PatchValue.set(5)})})
        }


class TestPluckPatch:
    def test_pluck_set_value(self):
        class S(Schema):
            inner = fields.Pluck(InnerSchema, "x", deletable=True)

        result = S().load_patch({"inner": 7})
        assert result == {"inner": PatchValue.set({"x": 7})}

    def test_pluck_delete(self):
        class S(Schema):
            inner = fields.Pluck(InnerSchema, "x", deletable=True)

        result = S().load_patch({"inner": DELETE})
        assert result == {"inner": PatchValue.delete()}


class TestDeleteSentinel:
    def test_sentinel_repr_and_falsiness(self):
        assert repr(DELETE) == "<marshmallow.DELETE>"
        assert not DELETE

    def test_sentinel_survives_copy(self):
        import copy

        assert copy.copy(DELETE) is DELETE
        assert copy.deepcopy(DELETE) is DELETE
