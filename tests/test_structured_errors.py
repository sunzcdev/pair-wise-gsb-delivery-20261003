import json

import pytest

from marshmallow import Schema, fields, structured, validates_schema
from marshmallow.exceptions import ValidationError


def _kinds(error: structured.StructuredError):
    return tuple(segment.kind for segment in error.path)


def _values(error: structured.StructuredError):
    return tuple(segment.value for segment in error.path)


class TestTypedPaths:
    def test_list_index_and_tuple_position_are_distinct(self):
        class MySchema(Schema):
            nums = fields.List(fields.Int())
            pair = fields.Tuple((fields.Int(), fields.Int()))

        with pytest.raises(ValidationError) as exc_info:
            MySchema().load({"nums": ["x"], "pair": ("x", "y")})

        by_field = {}
        for error in exc_info.value.structured:
            by_field.setdefault(error.path[0].value, error)
        list_error = by_field["nums"]
        tuple_error = by_field["pair"]

        assert _kinds(list_error) == ("field", "index")
        assert _values(list_error) == ("nums", 0)
        assert _kinds(tuple_error) == ("field", "position")
        assert _values(tuple_error) == ("pair", 0)

    def test_integer_dict_key_is_mapping_key_not_index(self):
        class MySchema(Schema):
            mapping = fields.Dict(keys=fields.Str(), values=fields.Int())

        with pytest.raises(ValidationError) as exc_info:
            MySchema().load({"mapping": {1: "x"}})

        kinds = {_kinds(error) for error in exc_info.value.structured}
        assert ("field", "mapping_key") in kinds
        assert ("field", "mapping_key", "mapping_value") in kinds
        value_error = next(
            error
            for error in exc_info.value.structured
            if _kinds(error) == ("field", "mapping_key", "mapping_value")
        )
        assert value_error.path[1].value == 1

    def test_mapping_key_error_keeps_original_key_object(self):
        class MySchema(Schema):
            mapping = fields.Dict(keys=fields.Str())

        tuple_key = (1, 2)
        with pytest.raises(ValidationError) as exc_info:
            MySchema().load({"mapping": {tuple_key: "v"}})

        key_segments = [
            segment
            for error in exc_info.value.structured
            for segment in error.path
            if segment.kind == "mapping_key"
        ]
        assert key_segments
        assert key_segments[0].value == tuple_key

    def test_mapping_key_and_value_segments(self):
        field = fields.Dict(keys=fields.Str, values=fields.Int)
        with pytest.raises(ValidationError) as exc_info:
            field.deserialize({1: "bad"})

        kinds = {_kinds(error) for error in exc_info.value.structured}
        assert ("mapping_key",) in kinds
        assert ("mapping_key", "mapping_value") in kinds

    def test_nested_schema_path(self):
        class Child(Schema):
            age = fields.Int()

        class Parent(Schema):
            child = fields.Nested(Child)

        with pytest.raises(ValidationError) as exc_info:
            Parent().load({"child": {"age": "nope"}})

        (error,) = exc_info.value.structured
        assert _kinds(error) == ("field", "field")
        assert _values(error) == ("child", "age")

    def test_many_collection_index_is_outermost(self):
        class Item(Schema):
            nums = fields.List(fields.Int())

        with pytest.raises(ValidationError) as exc_info:
            Item(many=True).load([{"nums": ["x"]}, "not-a-dict"])

        paths = [
            (_kinds(error), _values(error))
            for error in exc_info.value.structured
        ]
        assert (
            ("index", "field", "index"),
            (0, "nums", 0),
        ) in paths
        assert (("index",), (1,)) in paths


class TestSchemaAndFieldErrorsCoexist:
    def test_schema_error_has_empty_path(self):
        class MySchema(Schema):
            age = fields.Int()

            @validates_schema(skip_on_field_errors=False)
            def check(self, data, **kwargs):
                raise ValidationError("global boom")

        with pytest.raises(ValidationError) as exc_info:
            MySchema().load({"age": "nope"})

        path_kinds = {_kinds(error) for error in exc_info.value.structured}
        assert () in path_kinds  # schema-level error
        assert ("field",) in path_kinds  # field-level error
        # Messages structure is unchanged.
        assert set(exc_info.value.messages) == {"age", "_schema"}


class TestJsonEncoding:
    def test_non_string_key_is_explicitly_tagged(self):
        class MySchema(Schema):
            mapping = fields.Dict(keys=fields.Str())

        with pytest.raises(ValidationError) as exc_info:
            MySchema().load({"mapping": {(1, 2): "v"}})

        encoded = exc_info.value.structured_jsonable()
        # Must be JSON serializable even though raw messages are not.
        json.dumps(encoded)
        key_segment = next(
            segment
            for error in encoded
            for segment in error["path"]
            if segment["kind"] == "mapping_key"
        )
        assert key_segment["value"] == {
            "__marshmallow_key__": True,
            "type": "tuple",
            "value": [1, 2],
        }

    def test_str_coercion_collisions_are_avoided(self):
        class Weird:
            def __repr__(self):
                return "1"

        payload = [1, "1", Weird()]
        encoded = {
            json.dumps(structured.encode_key(key), sort_keys=True)
            for key in payload
        }
        assert len(encoded) == len(payload)


class TestBackwardCompatibility:
    def test_messages_structure_unchanged(self):
        class MySchema(Schema):
            nums = fields.List(fields.Int())
            mapping = fields.Dict(keys=fields.Str(), values=fields.Int())

        with pytest.raises(ValidationError) as exc_info:
            MySchema().load(
                {"nums": ["x"], "mapping": {1: "bad", "ok": 2}}
            )

        assert exc_info.value.messages == {
            "nums": {0: ["Not a valid integer."]},
            "mapping": {
                1: {
                    "key": ["Not a valid string."],
                    "value": ["Not a valid integer."],
                }
            },
        }

    def test_structured_cannot_be_guessed_from_flat_messages(self):
        # Both a list index and an integer dict key appear as the integer 0 in
        # the flat messages, but the typed paths keep them distinct.
        class MySchema(Schema):
            nums = fields.List(fields.Int())
            mapping = fields.Dict(keys=fields.Str())

        with pytest.raises(ValidationError) as exc_info:
            MySchema().load({"nums": ["x"], "mapping": {0: "v"}})

        segment_kinds = {
            (error.path[0].value, error.path[1].kind)
            for error in exc_info.value.structured
        }
        assert ("nums", "index") in segment_kinds
        assert ("mapping", "mapping_key") in segment_kinds
