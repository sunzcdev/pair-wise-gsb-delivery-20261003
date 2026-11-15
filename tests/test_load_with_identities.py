import pytest

from marshmallow import (
    IdentityItem,
    IdentityLoadError,
    IdentityLoadResult,
    Schema,
    ValidationError,
    fields,
    post_load,
    pre_load,
    validates_schema,
)
from marshmallow.exceptions import MarshmallowError


class SimpleSchema(Schema):
    x = fields.Integer(required=True)


class ReorderingSchema(SimpleSchema):
    @pre_load(pass_collection=True, preserve_identities=True)
    def reverse_items(self, items, many, **kwargs):
        return list(reversed(items))


class FilteringSchema(SimpleSchema):
    @pre_load(pass_collection=True, preserve_identities=True)
    def drop_skipped(self, items, many, **kwargs):
        return [item for item in items if not item.data.get("skip")]


class TestLoadWithIdentities:
    def test_success_returns_result_keyed_by_identity(self):
        result = SimpleSchema().load_with_identities(
            [("a", {"x": 1}), ("b", {"x": 2})]
        )
        assert isinstance(result, IdentityLoadResult)
        assert result.data == {"a": {"x": 1}, "b": {"x": 2}}
        assert result.filtered == []

    def test_accepts_mapping_input(self):
        result = SimpleSchema().load_with_identities({"a": {"x": 1}, "b": {"x": 2}})
        assert result.data == {"a": {"x": 1}, "b": {"x": 2}}
        assert result.filtered == []

    def test_opaque_identities_are_not_inspected(self):
        identities = [("req", 1), 42, (1, 2)]
        records = [{"x": 1}, {"x": 2}, {"x": 3}]
        result = SimpleSchema().load_with_identities(zip(identities, records))
        assert result.data == {
            ("req", 1): {"x": 1},
            42: {"x": 2},
            (1, 2): {"x": 3},
        }

    def test_reordered_records_map_back_to_identity(self):
        result = ReorderingSchema().load_with_identities(
            [("a", {"x": 1}), ("b", {"x": 2}), ("c", {"x": 3})]
        )
        assert result.data == {"c": {"x": 3}, "b": {"x": 2}, "a": {"x": 1}}
        assert result.filtered == []

    def test_filtered_records_have_separate_status(self):
        result = FilteringSchema().load_with_identities(
            [("a", {"x": 1}), ("b", {"x": 2, "skip": True}), ("c", {"x": 3})]
        )
        assert result.data == {"a": {"x": 1}, "c": {"x": 3}}
        assert result.filtered == ["b"]

    def test_errors_are_keyed_by_identity_after_reordering(self):
        class Schema(FilteringSchema, ReorderingSchema):
            pass

        with pytest.raises(IdentityLoadError) as excinfo:
            Schema().load_with_identities(
                [
                    ("a", {"x": 1}),
                    ("b", {"x": 2, "skip": True}),
                    ("c", {"x": "bad"}),
                    ("d", {}),
                ]
            )
        exc = excinfo.value
        assert isinstance(exc, ValidationError)
        assert exc.messages == {
            "d": {"x": ["Missing data for required field."]},
            "c": {"x": ["Not a valid integer."]},
        }
        # Partial results carry the original identity, not a position.
        assert exc.valid_data == {"a": {"x": 1}}
        assert exc.filtered == ["b"]

    def test_duplicate_identities_rejected_before_processing(self):
        called = False

        class Schema(SimpleSchema):
            @pre_load(pass_collection=True, preserve_identities=True)
            def hook(self, items, many, **kwargs):
                nonlocal called
                called = True
                return items

        with pytest.raises(ValueError, match="Duplicate identities"):
            Schema().load_with_identities([("a", {"x": 1}), ("a", {"x": 2})])
        assert not called

    def test_unhashable_identity_rejected(self):
        with pytest.raises(TypeError):
            SimpleSchema().load_with_identities([(["a"], {"x": 1})])

    @pytest.mark.parametrize("hook_tag", ["pre_load", "post_load"])
    def test_plain_collection_hooks_are_rejected(self, hook_tag):
        decorator = {"pre_load": pre_load, "post_load": post_load}[hook_tag]

        class Schema(SimpleSchema):
            @decorator(pass_collection=True)
            def plain_hook(self, data, many, **kwargs):
                return data

        with pytest.raises(MarshmallowError, match="preserve_identities"):
            Schema().load_with_identities([("a", {"x": 1})])

    def test_opted_in_post_load_hook_may_filter(self):
        class Schema(SimpleSchema):
            @post_load(pass_collection=True, preserve_identities=True)
            def drop_odd(self, items, many, **kwargs):
                return [item for item in items if item.data["x"] % 2 == 0]

        result = Schema().load_with_identities(
            [("a", {"x": 1}), ("b", {"x": 2}), ("c", {"x": 4})]
        )
        assert result.data == {"b": {"x": 2}, "c": {"x": 4}}
        assert result.filtered == ["a"]

    def test_per_item_hooks_still_apply(self):
        class Schema(SimpleSchema):
            @pre_load
            def strip_x(self, item, many, **kwargs):
                item["x"] = str(item["x"]).strip()
                return item

            @post_load
            def double_x(self, item, many, **kwargs):
                item["x"] *= 2
                return item

        result = Schema().load_with_identities([("a", {"x": " 1 "})])
        assert result.data == {"a": {"x": 2}}

    def test_hook_receives_identity_items(self):
        received = []

        class Schema(SimpleSchema):
            @pre_load(pass_collection=True, preserve_identities=True)
            def inspect(self, items, many, **kwargs):
                received.extend(items)
                return items

        Schema().load_with_identities([("a", {"x": 1})])
        assert received == [IdentityItem(identity="a", data={"x": 1})]

    def test_hook_may_return_plain_pairs(self):
        class Schema(SimpleSchema):
            @pre_load(pass_collection=True, preserve_identities=True)
            def as_pairs(self, items, many, **kwargs):
                return [(item.identity, item.data) for item in items]

        result = Schema().load_with_identities([("a", {"x": 1})])
        assert result.data == {"a": {"x": 1}}

    def test_hook_returning_duplicate_identities_is_rejected(self):
        class Schema(SimpleSchema):
            @pre_load(pass_collection=True, preserve_identities=True)
            def duplicate(self, items, many, **kwargs):
                return [items[0], items[0]]

        with pytest.raises(MarshmallowError, match="duplicate identities"):
            Schema().load_with_identities([("a", {"x": 1}), ("b", {"x": 2})])

    def test_pre_load_hook_errors_are_remapped_to_identities(self):
        class Schema(SimpleSchema):
            @pre_load(pass_collection=True, preserve_identities=True)
            def fail_second(self, items, many, **kwargs):
                raise ValidationError({1: {"x": ["Nope."]}})

        with pytest.raises(IdentityLoadError) as excinfo:
            Schema().load_with_identities([("a", {"x": 1}), ("b", {"x": 2})])
        assert excinfo.value.messages == {"b": {"x": ["Nope."]}}
        assert excinfo.value.valid_data == {}

    def test_post_load_hook_errors_are_remapped_to_identities(self):
        class Schema(FilteringSchema):
            @post_load(pass_collection=True, preserve_identities=True)
            def fail_first_remaining(self, items, many, **kwargs):
                raise ValidationError({0: {"x": ["Nope."]}})

        with pytest.raises(IdentityLoadError) as excinfo:
            Schema().load_with_identities(
                [("a", {"x": 1, "skip": True}), ("b", {"x": 2})]
            )
        assert excinfo.value.messages == {"b": {"x": ["Nope."]}}
        assert excinfo.value.filtered == ["a"]

    def test_schema_level_validation_errors_are_keyed_by_identity(self):
        class Schema(ReorderingSchema):
            @validates_schema
            def validate_x(self, data, **kwargs):
                if data["x"] > 1:
                    raise ValidationError("Too big.", "x")

        with pytest.raises(IdentityLoadError) as excinfo:
            Schema().load_with_identities([("a", {"x": 1}), ("b", {"x": 2})])
        assert excinfo.value.messages == {"b": {"x": ["Too big."]}}
        assert excinfo.value.valid_data == {"a": {"x": 1}}

    def test_collection_level_schema_validation_error(self):
        class Schema(SimpleSchema):
            @validates_schema(pass_collection=True)
            def validate_all(self, data, **kwargs):
                raise ValidationError({"_schema": ["Always fails."]})

        with pytest.raises(IdentityLoadError) as excinfo:
            Schema().load_with_identities([("a", {"x": 1})])
        assert excinfo.value.messages == {"_schema": ["Always fails."]}
        assert excinfo.value.valid_data == {"a": {"x": 1}}

    def test_index_errors_disabled_is_rejected(self):
        class Schema(SimpleSchema):
            class Meta:
                index_errors = False

        with pytest.raises(MarshmallowError, match="index_errors"):
            Schema().load_with_identities([("a", {"x": 1})])

    def test_partial_and_unknown_are_passed_through(self):
        class Schema(SimpleSchema):
            class Meta:
                unknown = "raise"

        result = Schema().load_with_identities(
            [("a", {"x": 1, "extra": "kept"})], unknown="include"
        )
        assert result.data == {"a": {"x": 1, "extra": "kept"}}

    def test_empty_batch(self):
        result = SimpleSchema().load_with_identities([])
        assert result.data == {}
        assert result.filtered == []
