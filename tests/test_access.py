"""Tests for attribute access policies (marshmallow.access)."""

from __future__ import annotations

import copy
import typing

import pytest

from marshmallow import Schema, fields, missing
from marshmallow.access import (
    ACCESS_MODES,
    INSTANCE_MODE,
    AccessPolicy,
    AccessPolicyError,
    ReadPlan,
    forbidden_access,
    get_active_policy,
)
from marshmallow.experimental.access import AccessPolicy as ExperimentalAccessPolicy


class ORMModel:
    """Object mimicking an ORM entity with a lazy property/relationship."""

    def __init__(self, **values) -> None:
        self.__dict__.update(values)
        self.property_calls = 0
        self.relationship_calls = 0

    @property
    def computed(self) -> str:
        self.property_calls += 1
        return typing.cast("str", self.__dict__["name"]).upper()

    @property
    def relationship(self) -> dict[str, str]:
        self.relationship_calls += 1
        return {"city": "Berlin"}


class _LazyDescriptor:
    """Data descriptor emulating an ORM instrumented relationship."""

    def __get__(self, obj, objtype=None):
        if obj is None:
            return self
        obj.descriptor_calls += 1
        return {"tag": "loaded"}

    def __set__(self, obj, value) -> None:
        obj.__dict__["descriptor_relationship"] = value


class DescriptorModel:
    descriptor_relationship = _LazyDescriptor()

    def __init__(self) -> None:
        self.descriptor_calls = 0
        self.id = 3


class SlottedModel:
    __slots__ = ("assigned", "unassigned")

    def __init__(self) -> None:
        self.assigned = "value"

    @property
    def slotted_property(self) -> int:
        raise RuntimeError("must never be invoked")


class RelatedSchema(Schema):
    city = fields.Str()
    tag = fields.Str()


class ModelSchema(Schema):
    id = fields.Int()
    name = fields.Str()
    computed = fields.Str()
    relationship = fields.Nested(RelatedSchema)
    descriptor_relationship = fields.Nested(RelatedSchema)
    title = fields.Method("get_title")
    label = fields.Function(lambda obj: f"label-{obj.__dict__['name']}")
    absent = fields.Str()
    dotted = fields.Str(attribute="relationship.city")

    def get_title(self, obj) -> str:
        return f"title-{obj.__dict__['name']}"


def make_model() -> ORMModel:
    return ORMModel(id=1, name="monty")


def plan(policy: AccessPolicy) -> ReadPlan:
    """Return the attached read plan, failing loudly if no dump ran."""
    assert policy.read_plan is not None
    return policy.read_plan


class TestSentinel:
    def test_forbidden_is_distinct_from_missing(self):
        assert forbidden_access is not missing
        assert forbidden_access != missing
        assert not forbidden_access
        assert repr(forbidden_access) == "<marshmallow.forbidden_access>"

    def test_sentinel_is_singleton_under_copy(self):
        assert copy.copy(forbidden_access) is forbidden_access
        assert copy.deepcopy(forbidden_access) is forbidden_access


class TestPolicyConfiguration:
    def test_invalid_mode_raises(self):
        with pytest.raises(AccessPolicyError, match="Invalid access mode"):
            AccessPolicy("not-a-mode")  # type: ignore[arg-type]

    def test_valid_modes_exposed(self):
        assert frozenset({"instance", "allowlist"}) == ACCESS_MODES
        assert INSTANCE_MODE == "instance"

    def test_allowed_fields_normalized_to_frozenset(self):
        policy = AccessPolicy("allowlist", allowed_fields=["a", "b", "a"])
        assert policy.allowed_fields == frozenset({"a", "b"})

    def test_repr(self):
        policy = AccessPolicy("instance")
        assert "instance" in repr(policy)

    def test_context_activation_and_reset(self):
        assert get_active_policy() is None
        policy = AccessPolicy()
        with policy:
            assert get_active_policy() is policy
        assert get_active_policy() is None

    def test_nested_policies_restore_outer(self):
        outer = AccessPolicy("instance")
        inner = AccessPolicy("allowlist", allowed_fields=["x"])
        with outer:
            assert get_active_policy() is outer
            with inner:
                assert get_active_policy() is inner
            assert get_active_policy() is outer
        assert get_active_policy() is None

    def test_experimental_reexport_is_same_class(self):
        assert ExperimentalAccessPolicy is AccessPolicy


class TestInstanceMode:
    def test_instance_dict_data_is_read(self):
        with AccessPolicy("instance") as policy:
            result = ModelSchema().dump(make_model())
        assert result == {"id": 1, "name": "monty"}
        assert policy.read_plan is not None
        assert plan(policy).evaluated_fields == {"id", "name"}

    def test_property_getter_is_never_invoked(self):
        model = make_model()
        with AccessPolicy("instance") as policy:
            result = ModelSchema().dump(model)
        assert model.property_calls == 0
        assert "computed" not in result
        assert "computed" in plan(policy).forbidden_fields

    def test_nested_relationship_getter_is_never_invoked(self):
        model = make_model()
        with AccessPolicy("instance") as policy:
            result = ModelSchema().dump(model)
        assert model.relationship_calls == 0
        assert "relationship" not in result
        assert "relationship" in plan(policy).forbidden_fields
        # dot paths are recorded under their declared field name
        assert "dotted" in plan(policy).forbidden_fields

    def test_data_descriptor_is_never_invoked(self):
        model = DescriptorModel()
        with AccessPolicy("instance") as policy:
            result = ModelSchema().dump(model)
        assert model.descriptor_calls == 0
        assert "descriptor_relationship" not in result
        assert "descriptor_relationship" in plan(policy).forbidden_fields

    def test_method_field_is_gated_without_calling_schema_method(self):
        calls = []

        class LocalSchema(ModelSchema):
            def get_title(self, obj):
                calls.append(obj)
                return super().get_title(obj)

        with AccessPolicy("instance") as policy:
            result = LocalSchema().dump(make_model())
        assert calls == []
        assert "title" not in result
        assert "title" in plan(policy).forbidden_fields

    def test_function_field_is_gated_without_calling_function(self):
        with AccessPolicy("instance") as policy:
            result = ModelSchema().dump(make_model())
        assert "label" not in result
        assert "label" in plan(policy).forbidden_fields

    def test_missing_attribute_distinguished_from_forbidden(self):
        with AccessPolicy("instance") as policy:
            ModelSchema().dump(make_model())
        plan = policy.read_plan
        assert plan is not None
        assert "absent" in plan.missing_fields
        assert "absent" not in plan.forbidden_fields
        assert plan.forbidden_fields.isdisjoint(plan.missing_fields)
        assert plan.unevaluated == plan.forbidden_fields | plan.missing_fields

    def test_does_not_invoke_custom_getattr(self):
        class DynamicAttrs(ORMModel):
            def __getattr__(self, item):
                raise AssertionError(f"lazy load triggered for {item}")

        with AccessPolicy("instance") as policy:
            result = ModelSchema().dump(DynamicAttrs(id=1, name="x"))
        assert result == {"id": 1, "name": "x"}
        assert "absent" in plan(policy).missing_fields

    def test_slotted_instance_data_is_read_and_properties_blocked(self):
        class SlotSchema(Schema):
            assigned = fields.Str()
            unassigned = fields.Str()
            slotted_property = fields.Int()

        with AccessPolicy("instance") as policy:
            result = SlotSchema().dump(SlottedModel())
        assert result == {"assigned": "value"}
        assert "unassigned" in plan(policy).missing_fields
        assert "slotted_property" in plan(policy).forbidden_fields

    def test_mapping_inputs_are_always_readable(self):
        class MapSchema(Schema):
            present = fields.Str()
            gone = fields.Int()

        with AccessPolicy("instance") as policy:
            result = MapSchema().dump({"present": "yes"})
        assert result == {"present": "yes"}
        assert plan(policy).missing_fields == {"gone"}
        assert plan(policy).forbidden_fields == set()

    def test_dump_default_still_applies_for_forbidden_field(self):
        class DefaultSchema(Schema):
            id = fields.Int()
            computed = fields.Str(dump_default="fallback")

        with AccessPolicy("instance") as policy:
            result = DefaultSchema().dump(make_model())
        assert result == {"id": 1, "computed": "fallback"}
        assert "computed" in plan(policy).forbidden_fields

    def test_many_dump_attaches_single_plan(self):
        models = [make_model(), ORMModel(id=2, name="python")]
        with AccessPolicy("instance") as policy:
            result = ModelSchema(many=True).dump(models)
        assert [item["name"] for item in result] == ["monty", "python"]
        assert all("computed" not in item for item in result)
        assert policy.read_plan is not None
        assert "computed" in plan(policy).forbidden_fields


class TestAllowlistMode:
    def test_allowed_descriptor_is_invoked(self):
        model = make_model()
        with AccessPolicy(
            "allowlist",
            allowed_fields={
                "id",
                "name",
                "computed",
                "relationship",
                "relationship.city",
                "title",
                "label",
                "city",
            },
        ) as policy:
            result = ModelSchema().dump(model)
        assert result["computed"] == "MONTY"
        assert result["relationship"] == {"city": "Berlin"}
        assert result["title"] == "title-monty"
        assert result["label"] == "label-monty"
        assert model.property_calls == 1
        # The nested dump and the dotted field both traverse the relationship.
        assert model.relationship_calls == 2
        assert plan(policy).forbidden_fields == set()
        assert plan(policy).missing_fields == {
            "absent",
            "tag",
            "descriptor_relationship",
        }

    def test_unlisted_descriptor_stays_blocked(self):
        model = make_model()
        with AccessPolicy("allowlist", allowed_fields={"id", "name"}) as policy:
            result = ModelSchema().dump(model)
        assert model.property_calls == 0
        assert model.relationship_calls == 0
        assert "computed" not in result
        assert "relationship" not in result
        assert "title" not in result
        assert plan(policy).forbidden_fields == {
            "computed",
            "relationship",
            "title",
            "label",
            "dotted",
        }
        # `descriptor_relationship` does not exist on ORMModel at all, so it is
        # missing rather than forbidden; the inner nested `tag` is never
        # reached because the relationship itself is blocked.
        assert plan(policy).missing_fields == {
            "absent",
            "descriptor_relationship",
        }

    def test_allowed_data_descriptor_is_invoked(self):
        model = DescriptorModel()
        with AccessPolicy(
            "allowlist", allowed_fields={"id", "descriptor_relationship", "tag"}
        ) as policy:
            result = ModelSchema().dump(model)
        assert model.descriptor_calls == 1
        assert result["descriptor_relationship"] == {"tag": "loaded"}
        assert "descriptor_relationship" not in plan(policy).forbidden_fields

    def test_none_allowed_fields_permits_every_declared_field(self):
        model = make_model()
        with AccessPolicy("allowlist") as policy:
            result = ModelSchema().dump(model)
        assert result["computed"] == "MONTY"
        assert result["title"] == "title-monty"
        assert plan(policy).forbidden_fields == set()
        assert plan(policy).missing_fields == {
            "absent",
            "tag",
            "descriptor_relationship",
        }

    def test_allowed_dot_path(self):
        class DotSchema(Schema):
            id = fields.Int()
            dotted = fields.Str(attribute="relationship.city")

        model = make_model()
        with AccessPolicy(
            "allowlist", allowed_fields={"id", "relationship", "relationship.city"}
        ) as policy:
            result = DotSchema().dump(model)
        assert result == {"id": 1, "dotted": "Berlin"}
        assert plan(policy).forbidden_fields == set()

    def test_attribute_kwarg_is_checked_against_policy(self):
        class AttrSchema(Schema):
            id = fields.Int()
            renamed = fields.Str(attribute="computed")

        model = make_model()
        with AccessPolicy("allowlist", allowed_fields={"id"}) as policy:
            result = AttrSchema().dump(model)
        assert model.property_calls == 0
        assert "renamed" not in result
        # Plan speaks the declared field name.
        assert "renamed" in plan(policy).forbidden_fields

        model2 = make_model()
        with AccessPolicy("allowlist", allowed_fields={"id", "computed"}) as _policy2:
            result2 = AttrSchema().dump(model2)
        assert result2["renamed"] == "MONTY"
        assert model2.property_calls == 1


class TestNestedPropagation:
    def test_nested_schema_inherits_policy(self):
        class Child:
            def __init__(self):
                self.kept = "ok"

            @property
            def lazy(self):
                raise AssertionError("database hit")

        class ChildSchema(Schema):
            kept = fields.Str()
            lazy = fields.Str()

        class Parent:
            def __init__(self):
                self.child = Child()

        class ParentSchema(Schema):
            child = fields.Nested(ChildSchema)

        with AccessPolicy("instance") as policy:
            result = ParentSchema().dump(Parent())
        assert result == {"child": {"kept": "ok"}}
        assert "lazy" in plan(policy).forbidden_fields

    def test_list_of_nested_inherits_policy(self):
        class Child:
            def __init__(self, value):
                self.value = value

            @property
            def expensive(self):
                raise AssertionError("database hit")

        class ChildSchema(Schema):
            value = fields.Int()
            expensive = fields.Int()

        class Holder:
            def __init__(self):
                self.children = [Child(1), Child(2)]

        class HolderSchema(Schema):
            children = fields.List(fields.Nested(ChildSchema))

        with AccessPolicy("instance") as policy:
            result = HolderSchema().dump(Holder())
        assert result == {"children": [{"value": 1}, {"value": 2}]}
        assert "expensive" in plan(policy).forbidden_fields

    def test_nested_allowlist_permits_inner_descriptor(self):
        class Child:
            def __init__(self):
                self.value = 1

            @property
            def lazy(self):
                return "resolved"

        class ChildSchema(Schema):
            value = fields.Int()
            lazy = fields.Str()

        class Holder:
            def __init__(self):
                self.child = Child()

        class HolderSchema(Schema):
            child = fields.Nested(ChildSchema)

        with AccessPolicy("allowlist", allowed_fields={"child", "value", "lazy"}):
            result = HolderSchema().dump(Holder())
        assert result == {"child": {"value": 1, "lazy": "resolved"}}


class TestCompatibility:
    def test_plain_dump_is_unaffected_without_policy(self):
        model = make_model()
        result = ModelSchema().dump(model)
        assert result["computed"] == "MONTY"
        assert result["relationship"] == {"city": "Berlin"}
        assert result["title"] == "title-monty"
        assert model.property_calls == 1
        # The relationship getter is reached by the nested field and the
        # dotted attribute field.
        assert model.relationship_calls == 2

    def test_policy_context_does_not_leak_between_dumps(self):
        model = make_model()
        with AccessPolicy("instance"):
            blocked = ModelSchema().dump(model)
        assert "computed" not in blocked
        unrestricted = ModelSchema().dump(model)
        assert unrestricted["computed"] == "MONTY"

    def test_custom_get_attribute_can_reuse_access_value(self):
        from marshmallow.access import access_value  # noqa: PLC0415

        class CustomSchema(Schema):
            name = fields.Str()
            computed = fields.Str()

            def get_attribute(self, obj, attr, default):
                return access_value(obj, attr, default, policy=get_active_policy())

        model = make_model()
        with AccessPolicy("instance") as policy:
            result = CustomSchema().dump(model)
        assert result == {"name": "monty"}
        assert "computed" in plan(policy).forbidden_fields

    def test_policy_dump_helper(self):
        policy = AccessPolicy("instance")
        result = policy.dump(ModelSchema(), make_model())
        assert result == {"id": 1, "name": "monty"}
        assert policy.read_plan is not None
        assert get_active_policy() is None


class TestCallableFieldEdgeCases:
    def test_load_only_method_is_not_dumped(self):
        class LocalSchema(Schema):
            x = fields.Method(None, deserialize="load_x")

            def load_x(self, value):
                return value

        with AccessPolicy("instance"):
            assert LocalSchema().dump({"x": 1}) == {}

    def test_load_only_function_is_not_dumped(self):
        class LocalSchema(Schema):
            x = fields.Function(None, deserialize=lambda value: value)

        with AccessPolicy("instance"):
            assert LocalSchema().dump({"x": 1}) == {}

    def test_constant_fields_are_not_gated(self):
        class LocalSchema(Schema):
            c = fields.Constant(42)

        with AccessPolicy("instance") as policy:
            result = LocalSchema().dump(object())
        assert result == {"c": 42}
        assert plan(policy).forbidden_fields == set()

    def test_forbidden_method_with_dump_default_is_skipped_not_called(self):
        calls = []

        class LocalSchema(Schema):
            m = fields.Method("get_m", dump_default="dd")

            def get_m(self, obj):
                calls.append(obj)
                return "real"

        with AccessPolicy("instance") as policy:
            result = LocalSchema().dump(object())
        assert calls == []
        assert result == {}
        assert "m" in plan(policy).forbidden_fields


class TestReadPlan:
    def test_plan_equality_and_immutability(self):
        first = ReadPlan(
            evaluated_fields={"a"},
            forbidden_fields={"b"},
            missing_fields={"c"},
        )
        second = ReadPlan(
            evaluated_fields=["a"], forbidden_fields=["b"], missing_fields=["c"]
        )
        assert first == second
        assert hash(first) == hash(second)
        assert first != ReadPlan()
        assert first.evaluated_fields == frozenset({"a"})
        assert first.unevaluated == frozenset({"b", "c"})

    def test_plan_repr_lists_sections(self):
        plan = ReadPlan(forbidden_fields={"b"})
        assert "forbidden={'b'}" in repr(plan)
