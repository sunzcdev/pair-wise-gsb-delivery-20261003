import pytest

from marshmallow import Schema, class_registry, fields
from marshmallow.experimental.reference_preparation import (
    NestedReference,
    ReferencePreparationError,
    ReferenceReport,
    check_references,
    prepare_references,
)


class LeafSchema(Schema):
    value = fields.String()


class RootSchema(Schema):
    name = fields.String()
    leaf = fields.Nested("tests.test_reference_preparation.LeafSchema")
    leaves = fields.List(
        fields.Nested("tests.test_reference_preparation.LeafSchema")
    )


def test_prepare_lists_candidates_and_final_binding():
    report = prepare_references(RootSchema)

    assert isinstance(report, ReferenceReport)
    assert report.root is RootSchema
    assert LeafSchema in report.visited

    leaf_ref = next(ref for ref in report.references if ref.field_name == "leaf")
    assert leaf_ref.ref == "tests.test_reference_preparation.LeafSchema"
    assert leaf_ref.candidates == (LeafSchema,)
    assert leaf_ref.bound is LeafSchema
    assert leaf_ref.problem is None
    assert leaf_ref.is_back_edge is False


def test_prepare_does_not_instantiate_nested_schemas():
    class ExplodingSchema(Schema):
        def __init__(self, *args, **kwargs):
            raise AssertionError("nested schemas must not be instantiated")

    class HolderSchema(Schema):
        child = fields.Nested(ExplodingSchema)
        named = fields.Nested(
            "tests.test_reference_preparation.LeafSchema"
        )

    report = prepare_references(HolderSchema)

    bound = {ref.field_name: ref.bound for ref in report.references}
    assert bound["child"] is ExplodingSchema
    assert bound["named"] is LeafSchema


def test_prepare_does_not_mutate_registry():
    before = dict(class_registry._registry)

    prepare_references(RootSchema)

    assert class_registry._registry == before


def test_missing_reference_is_reported_with_declaration_path():
    class MissingSchema(Schema):
        child = fields.List(fields.Nested("DoesNotExistAnywhere"))

    report = prepare_references(MissingSchema)

    (problem,) = report.problems
    assert isinstance(problem, NestedReference)
    assert problem.problem == "missing"
    assert problem.bound is None
    assert problem.candidates == ()
    assert problem.ref == "DoesNotExistAnywhere"
    assert problem.declaration_path == "child[]"

    with pytest.raises(ReferencePreparationError) as exc_info:
        check_references(MissingSchema)
    assert exc_info.value.problems == [problem]
    message = str(exc_info.value)
    assert "child[]" in message
    assert "DoesNotExistAnywhere" in message


def test_ambiguous_bare_name_is_reported_with_candidates():
    type("AmbiguousNameSchema", (Schema,), {"__module__": "module.alpha"})
    type("AmbiguousNameSchema", (Schema,), {"__module__": "module.beta"})

    class AmbiguousHolderSchema(Schema):
        child = fields.Nested("AmbiguousNameSchema")

    report = prepare_references(AmbiguousHolderSchema)
    (problem,) = report.problems

    assert problem.problem == "ambiguous"
    assert problem.bound is None
    assert len(problem.candidates) == 2
    assert {candidate.__module__ for candidate in problem.candidates} == {
        "module.alpha",
        "module.beta",
    }

    with pytest.raises(ReferencePreparationError):
        check_references(AmbiguousHolderSchema)


def test_module_qualified_path_resolves_uniquely_even_with_duplicate_names():
    alpha = type(
        "QualifiedNameSchema", (Schema,), {"__module__": "qualified.alpha"}
    )
    type("QualifiedNameSchema", (Schema,), {"__module__": "qualified.beta"})

    class QualifiedHolderSchema(Schema):
        child = fields.Nested("qualified.alpha.QualifiedNameSchema")

    report = check_references(QualifiedHolderSchema)

    (reference,) = report.references
    assert reference.problem is None
    assert reference.bound is alpha


def test_self_reference_is_recorded_as_back_edge_not_an_error():
    class SelfSchema(Schema):
        name = fields.String()
        parent = fields.Nested(
            "tests.test_reference_preparation.SelfSchema"
        )
        children = fields.List(
            fields.Nested("tests.test_reference_preparation.SelfSchema")
        )

    report = check_references(SelfSchema)

    assert report.visited == (SelfSchema,)
    parent_ref = next(ref for ref in report.references if ref.field_name == "parent")
    children_ref = next(
        ref for ref in report.references if ref.field_name == "children"
    )
    assert parent_ref.is_back_edge is True
    assert parent_ref.bound is SelfSchema
    assert children_ref.is_back_edge is True
    assert children_ref.declaration_path == "children[]"
    assert report.back_edges == report.references
    assert report.problems == ()


def test_mutual_recursion_is_recorded_as_back_edge():
    class NodeASchema(Schema):
        b = fields.Nested("tests.test_reference_preparation.NodeBSchema")

    class NodeBSchema(Schema):
        a = fields.Nested("tests.test_reference_preparation.NodeASchema")

    report = check_references(NodeASchema)

    assert set(report.visited) == {NodeASchema, NodeBSchema}
    back_edges = report.back_edges
    assert len(back_edges) == 1
    assert back_edges[0].bound is NodeASchema
    assert back_edges[0].declaration_path == "b.a"


def test_diamond_graph_revisits_only_as_back_edge():
    class DiamondTopSchema(Schema):
        left = fields.Nested(
            "tests.test_reference_preparation.DiamondMiddleSchema"
        )
        right = fields.Nested(
            "tests.test_reference_preparation.DiamondMiddleSchema"
        )

    class DiamondMiddleSchema(Schema):
        leaf = fields.Nested(
            "tests.test_reference_preparation.LeafSchema"
        )

    report = prepare_references(DiamondTopSchema)

    middle_refs = [
        ref for ref in report.references if ref.bound is DiamondMiddleSchema
    ]
    assert len(middle_refs) == 2
    assert [ref.is_back_edge for ref in middle_refs] == [False, True]
    leaf_refs = [ref for ref in report.references if ref.bound is LeafSchema]
    # LeafSchema is only traversed once: the second reach of the middle schema
    # is a back edge and its fields are not walked again.
    assert len(leaf_refs) == 1
    assert leaf_refs[0].is_back_edge is False
    assert LeafSchema in report.visited


def test_container_fields_are_traversed_with_declaration_paths():
    class ContainerSchema(Schema):
        nested_list = fields.List(
            fields.Nested("tests.test_reference_preparation.LeafSchema")
        )
        nested_tuple = fields.Tuple(
            (
                fields.String(),
                fields.Nested("tests.test_reference_preparation.LeafSchema"),
            )
        )
        nested_dict = fields.Dict(
            values=fields.Nested("tests.test_reference_preparation.LeafSchema")
        )

    report = prepare_references(ContainerSchema)
    paths = {ref.field_name: ref.declaration_path for ref in report.references}

    assert paths["nested_list"] == "nested_list[]"
    assert paths["nested_tuple"] == "nested_tuple[1]"
    assert paths["nested_dict"] == "nested_dict{}"


def test_callable_string_reference_is_resolved_without_instantiation():
    class CallableStringHolder(Schema):
        child = fields.Nested(
            lambda: "tests.test_reference_preparation.LeafSchema"
        )

    report = check_references(CallableStringHolder)
    (reference,) = report.references
    assert reference.ref == "tests.test_reference_preparation.LeafSchema"
    assert reference.bound is LeafSchema


def test_callable_returning_schema_class_is_traversed_and_cycle_safe():
    class LambdaNodeSchema(Schema):
        name = fields.String()
        child = fields.Nested(lambda: LambdaNodeSchema)

    report = check_references(LambdaNodeSchema)

    (reference,) = report.references
    assert reference.bound is LambdaNodeSchema
    assert reference.is_back_edge is True


def test_callable_returning_schema_instance_is_not_instantiated_by_check():
    class ProbeSchema(Schema):
        name = fields.String()

    instance = ProbeSchema()

    class LambdaInstanceHolder(Schema):
        child = fields.Nested(lambda: instance)

    report = check_references(LambdaInstanceHolder)
    (reference,) = report.references
    assert reference.bound is ProbeSchema


def test_direct_schema_instance_and_pluck_are_supported():
    class PluckHolderSchema(Schema):
        leaf = fields.Pluck(LeafSchema, "value")
        instance = fields.Nested(LeafSchema())

    report = prepare_references(PluckHolderSchema)

    assert all(ref.bound is LeafSchema for ref in report.references)
    assert report.problems == ()


def test_nested_in_mapping_key_is_traversed():
    class KeyHolderSchema(Schema):
        mapping = fields.Dict(
            keys=fields.Nested("tests.test_reference_preparation.LeafSchema"),
            values=fields.String(),
        )

    report = check_references(KeyHolderSchema)
    (reference,) = report.references
    assert reference.declaration_path == "mapping{key}"
    assert reference.bound is LeafSchema


def test_schema_instance_input_is_prepared_by_its_class():
    report = prepare_references(RootSchema())

    assert report.root is RootSchema
    assert LeafSchema in report.visited


def test_existing_runtime_binding_matches_preparation_binding():
    class RuntimeHolderSchema(Schema):
        leaf = fields.Nested(
            "tests.test_reference_preparation.LeafSchema"
        )

    report = prepare_references(RuntimeHolderSchema)
    runtime_class = type(
        RuntimeHolderSchema().fields["leaf"].schema  # type: ignore[attr-defined]
    )

    assert report.references[0].bound is runtime_class
