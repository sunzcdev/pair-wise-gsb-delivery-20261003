"""Tests for the preparation-only conversion planning API."""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
from typing import Annotated, Any, Generic, List, NewType, Optional, TypeVar, Union

import pytest
from attr import define, field

from cattrs import BaseConverter, ConversionPlan, Converter
from cattrs.planning import (
    FactoryInvocation,
    HookSource,
    TypeEdge,
    TypeNode,
    plan_structure,
    plan_unstructure,
)


@define
class Inner:
    x: int


@define
class Outer:
    inner: Inner
    maybe: Optional[str]
    numbers: list[int]
    anything: Any


@define
class RecNode:
    children: List[RecNode]
    payload: int


@define
class MutualA:
    b: Optional[MutualB]


@define
class MutualB:
    a: Optional[MutualA]


T = TypeVar("T")


@define
class GenericBox(Generic[T]):
    value: T
    boxes: list[T]


UserId = NewType("UserId", int)


class PlainUnsupported:
    """A class with no registered structure hook."""


@define
class UnsupportedHolder:
    thing: PlainUnsupported


@define
class AttribConverterHolder:
    thing: PlainUnsupported = field(converter=PlainUnsupported)


@define
class Account:
    user_id: UserId


from typing_extensions import TypedDict


class SampleTypedDict(TypedDict):
    name: str
    age: int


class SampleEnum(Enum):
    RED = 1
    BLUE = 2


@define
class UnionMemberA:
    tag: str


@define
class UnionMemberB:
    n: int


@define
class UnionHolder:
    choice: Union[UnionMemberA, UnionMemberB]


@define
class EnumHolder:
    color: SampleEnum


@define
class AnnotatedHolder:
    v: Annotated[int, field(default=0)]


@pytest.fixture(params=[BaseConverter, Converter])
def converter(request):
    return request.param()


def test_plan_is_a_conversion_plan(converter):
    plan = converter.plan_structure(Outer)
    assert isinstance(plan, ConversionPlan)
    assert plan.root is Outer
    assert all(isinstance(node, TypeNode) for node in plan.nodes.values())
    assert all(isinstance(edge, TypeEdge) for edge in plan.edges)


def test_every_field_position_is_listed(converter):
    plan = converter.plan_structure(Outer)
    assert Outer in plan.nodes
    assert Inner in plan.nodes
    assert int in plan.nodes
    assert str in plan.nodes
    for node in plan.nodes.values():
        assert isinstance(node.source, HookSource)
        assert callable(node.source.hook)
    outer_fields = plan.fields[Outer]
    assert set(outer_fields) == {"inner", "maybe", "numbers", "anything"}


def test_planning_does_not_invoke_hooks_or_constructors(converter):
    constructed = []
    hook_calls = []

    def tracking_hook(value, _):
        hook_calls.append(value)
        result = Inner(value)
        constructed.append(result)
        return result

    converter.register_structure_hook(Inner, tracking_hook)
    plan = converter.plan_structure(Outer)

    assert hook_calls == []
    assert constructed == []
    assert plan.nodes[Inner].source.hook is tracking_hook


def test_any_is_unverified_not_a_validated_concrete_type(converter):
    plan = converter.plan_structure(Outer)
    assert plan.nodes[Any].unverified is True
    assert Any in plan.unverified_types
    assert all(edge.parent is not Any for edge in plan.edges)


def test_missing_structure_handler_is_reported(converter):
    plan = converter.plan_structure(UnsupportedHolder)
    assert plan.missing_handlers == [PlainUnsupported]
    source = plan.nodes[PlainUnsupported].source
    assert source.missing is True
    assert source.kind == "fallback"
    assert source.custom is False
    assert plan.nodes[UnsupportedHolder].source.missing is False


def test_unstructure_has_no_missing_handlers_by_default(converter):
    plan = converter.plan_unstructure(UnsupportedHolder)
    assert plan.missing_handlers == []
    source = plan.nodes[PlainUnsupported].source
    assert source.kind == "fallback"
    assert source.custom is False
    assert source.missing is False


def test_custom_fallback_is_not_missing():
    class Unsupported:
        pass

    converter = Converter(
        structure_fallback_factory=lambda t: (lambda v, _: Unsupported())
    )
    plan = converter.plan_structure(Unsupported)
    source = plan.nodes[Unsupported].source
    assert source.custom is True
    assert source.missing is False


def test_recursive_graph_is_finite_and_records_back_edges(converter):
    plan = converter.plan_structure(RecNode)
    assert RecNode in plan.nodes
    assert len(plan.nodes) == 3  # RecNode, list[RecNode], int
    assert plan.back_edges, "a recursive edge must be reported"
    assert all(edge.child is RecNode for edge in plan.back_edges)


def test_mutual_recursion_terminates(converter):
    plan = converter.plan_structure(MutualA)
    assert plan.back_edges
    assert len(plan.nodes) == len(set(plan.nodes))


def test_generic_arguments_are_preserved(converter):
    plan = converter.plan_structure(GenericBox[int])
    assert GenericBox[int] in plan.nodes
    value_edges = [edge for edge in plan.edges if edge.label == "value"]
    assert value_edges and value_edges[0].child is int
    assert not plan.unverified_types


def test_unbound_typevar_is_unverified(converter):
    plan = converter.plan_structure(GenericBox)
    assert any(node.unverified for node in plan.nodes.values())
    assert plan.unverified_types


def test_generic_collection_arguments_are_preserved(converter):
    @define
    class Holder:
        numbers: list[int]
        mapping: dict[str, int]

    plan = converter.plan_structure(Holder)
    assert list[int] in plan.nodes
    assert dict[str, int] in plan.nodes
    assert any(edge.child is int for edge in plan.edges)


def test_custom_hook_factory_execution_is_recorded():
    converter = Converter()
    seen = []

    def custom_factory(cl):
        seen.append(cl)

        def hook(v, _):
            return cl(v)

        return hook

    class Plain:
        def __init__(self, value):
            self.value = value

    converter.register_structure_hook_factory(lambda t: t is Plain, custom_factory)
    plan = converter.plan_structure(Plain)

    assert seen == [Plain]  # the factory genuinely ran during preparation
    invocation = next(inv for inv in plan.factory_invocations if inv.type is Plain)
    assert isinstance(invocation, FactoryInvocation)
    assert invocation.factory is custom_factory
    assert invocation.custom is True


def test_builtin_factories_are_recorded_as_not_custom(converter):
    plan = converter.plan_structure(Outer)
    assert plan.factory_invocations
    assert all(
        isinstance(invocation, FactoryInvocation)
        and invocation.custom is False
        for invocation in plan.factory_invocations
    )


def test_registered_class_hook_source_is_custom(converter):
    converter.register_structure_hook(Inner, lambda v, _: Inner(v["x"]))
    plan = converter.plan_structure(Outer)
    source = plan.nodes[Inner].source
    assert source.custom is True
    assert source.kind == "singledispatch"


def test_builtin_primitive_sources_are_not_custom(converter):
    plan = converter.plan_structure(Outer)
    assert plan.nodes[int].source.custom is False
    assert plan.nodes[str].source.custom is False


def test_newtype_is_traversed_to_its_base(converter):
    plan = converter.plan_structure(Account)
    assert UserId in plan.nodes
    assert int in plan.nodes


def test_annotated_field_is_traversed(converter):
    plan = converter.plan_structure(AnnotatedHolder)
    assert int in plan.nodes


def test_typeddict_fields_are_planned(converter):
    plan = converter.plan_structure(SampleTypedDict)
    assert str in plan.nodes
    assert int in plan.nodes
    fields_view = plan.fields[SampleTypedDict]
    assert fields_view["name"] is str
    assert fields_view["age"] is int


def test_unstructure_plan_discovers_same_positions(converter):
    sp = converter.plan_structure(Outer)
    up = converter.plan_unstructure(Outer)
    assert set(sp.nodes) == set(up.nodes)


def test_module_level_functions_equivalent_to_methods(converter):
    method_plan = converter.plan_structure(Outer)
    function_plan = plan_structure(converter, Outer)
    assert set(method_plan.nodes) == set(function_plan.nodes)
    assert method_plan.back_edges == function_plan.back_edges
    assert set(plan_unstructure(converter, Outer).nodes) == set(
        converter.plan_unstructure(Outer).nodes
    )


def test_plan_reports_hook_source_kind_for_each_position(converter):
    plan = converter.plan_structure(Outer)
    kinds = {node.source.kind for node in plan.nodes.values()}
    assert "singledispatch" in kinds or "function" in kinds
    assert "factory" in kinds


def test_planning_does_not_change_conversion_behavior(converter):
    converter.plan_structure(Outer)
    converter.plan_unstructure(Outer)
    original = Outer(Inner(1), "s", [1, 2], None)
    unstructured = converter.unstructure(original)
    assert converter.structure(unstructured, Outer) == original


def test_recorder_is_reset_after_planning(converter):
    converter.plan_structure(Outer)
    converter.plan_unstructure(Outer)
    assert (
        converter._structure_func._function_dispatch.factory_recorder is None
    )
    assert (
        converter._unstructure_func._function_dispatch.factory_recorder is None
    )


def test_attrib_converter_field_with_missing_handler():
    converter = Converter()
    plan = converter.plan_structure(AttribConverterHolder)
    assert PlainUnsupported in plan.nodes


def test_dataclass_fields_are_planned(converter):
    @dataclass
    class DC:
        name: str
        scores: tuple[int, str]

    plan = converter.plan_structure(DC)
    assert DC in plan.nodes
    assert plan.fields[DC] == {"name": str, "scores": tuple[int, str]}
    assert int in plan.nodes


def test_mapping_arguments_use_key_value_labels(converter):
    @define
    class Holder:
        mapping: dict[str, int]

    plan = converter.plan_structure(Holder)
    labels = {edge.label: edge.child for edge in plan.edges}
    assert labels["[key]"] is str
    assert labels["[value]"] is int


def test_enum_positions_are_planned(converter):
    plan = converter.plan_structure(EnumHolder)
    assert SampleEnum in plan.nodes
    assert plan.nodes[SampleEnum].source.missing is False


def test_union_members_are_traversed(converter):
    plan = converter.plan_structure(UnionHolder)
    assert UnionMemberA in plan.nodes
    assert UnionMemberB in plan.nodes
    member_edges = [edge for edge in plan.edges if edge.label.startswith("<member")]
    assert len(member_edges) == 2
