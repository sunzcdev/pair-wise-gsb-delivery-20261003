"""Tests for the ``partial_construct`` mode of ``Schema``."""

import pytest

from marshmallow import (
    PartialConstructResult,
    Schema,
    ValidationError,
    fields,
    post_load,
    validates_schema,
)


class User:
    def __init__(self, name, age):
        self.name = name
        self.age = age

    def __eq__(self, other):
        return (
            isinstance(other, User)
            and self.name == other.name
            and self.age == other.age
        )


class UserSchema(Schema):
    name = fields.Str(required=True)
    age = fields.Int(required=True)

    @post_load
    def make_user(self, data, **kwargs):
        return User(**data)


class TestPartialConstructValidation:
    def test_valid_items_are_constructed_with_original_indices(self):
        schema = UserSchema(many=True, partial_construct=True)
        data = [
            {"name": "alice", "age": 30},
            {"name": "bob"},  # missing age
            {"name": "carol", "age": 40},
        ]
        with pytest.raises(ValidationError) as excinfo:
            schema.load(data)
        err = excinfo.value
        assert err.messages == {1: {"age": ["Missing data for required field."]}}
        assert isinstance(err.valid_data, PartialConstructResult)
        assert err.valid_data.constructed == {
            0: User("alice", 30),
            2: User("carol", 40),
        }
        assert err.valid_data.canonical == {}
        # Merged mapping view is keyed by original index
        assert err.valid_data[0] == User("alice", 30)
        assert err.valid_data[2] == User("carol", 40)
        assert len(err.valid_data) == 2
        assert set(err.valid_data) == {0, 2}

    def test_all_valid_returns_plain_list(self):
        schema = UserSchema(many=True, partial_construct=True)
        result = schema.load([{"name": "alice", "age": 30}])
        assert result == [User("alice", 30)]

    def test_schema_level_validation_isolates_items(self):
        class AdultSchema(UserSchema):
            @validates_schema
            def check_adult(self, data, **kwargs):
                if data["age"] < 18:
                    raise ValidationError("Too young", "age")

        schema = AdultSchema(many=True, partial_construct=True)
        data = [{"name": "alice", "age": 30}, {"name": "bob", "age": 10}]
        with pytest.raises(ValidationError) as excinfo:
            schema.load(data)
        err = excinfo.value
        assert err.messages == {1: {"age": ["Too young"]}}
        assert err.valid_data.constructed == {0: User("alice", 30)}

    def test_without_post_load_hooks_valid_items_are_canonical(self):
        class PlainSchema(Schema):
            name = fields.Str(required=True)

        schema = PlainSchema(many=True, partial_construct=True)
        with pytest.raises(ValidationError) as excinfo:
            schema.load([{"name": "alice"}, {}])
        err = excinfo.value
        assert err.valid_data.constructed == {}
        assert err.valid_data.canonical == {0: {"name": "alice"}}

    def test_many_false_is_unaffected(self):
        schema = UserSchema(partial_construct=True)
        assert schema.load({"name": "alice", "age": 30}) == User("alice", 30)
        with pytest.raises(ValidationError) as excinfo:
            schema.load({"name": "alice"})
        assert not isinstance(excinfo.value.valid_data, PartialConstructResult)


class TestPartialConstructConstructionErrors:
    def test_construction_error_is_kept_per_item(self):
        class ExplodingSchema(Schema):
            name = fields.Str(required=True)
            age = fields.Int(required=True)

            @post_load
            def make_user(self, data, **kwargs):
                if data["name"] == "bob":
                    raise ValidationError("cannot construct bob")
                return User(**data)

        schema = ExplodingSchema(many=True, partial_construct=True)
        data = [
            {"name": "alice", "age": 30},
            {"name": "bob", "age": 20},
            {"name": "carol", "age": 40},
        ]
        with pytest.raises(ValidationError) as excinfo:
            schema.load(data)
        err = excinfo.value
        assert err.messages == {1: {"_schema": ["cannot construct bob"]}}
        # Successfully constructed items are kept
        assert err.valid_data.constructed == {
            0: User("alice", 30),
            2: User("carol", 40),
        }
        # The failed item remains available as a canonical dict
        assert err.valid_data.canonical == {1: {"name": "bob", "age": 20}}

    def test_successful_items_are_not_constructed_twice(self):
        calls = []

        class CountingSchema(Schema):
            name = fields.Str(required=True)
            age = fields.Int(required=True)

            @post_load
            def make_user(self, data, **kwargs):
                calls.append(data["name"])
                if data["name"] == "bob":
                    raise ValidationError("boom")
                return User(**data)

        schema = CountingSchema(many=True, partial_construct=True)
        data = [
            {"name": "alice", "age": 30},
            {"name": "bob", "age": 20},
            {"name": "carol", "age": 40},
        ]
        with pytest.raises(ValidationError):
            schema.load(data)
        assert calls == ["alice", "bob", "carol"]

    def test_pass_original_hook_receives_original_item(self):
        seen = []

        class OriginalSchema(UserSchema):
            @post_load(pass_original=True)
            def track(self, data, original, **kwargs):
                seen.append(original)
                return data

        schema = OriginalSchema(many=True, partial_construct=True)
        data = [{"name": "alice", "age": 30}, {"name": "bob"}]
        with pytest.raises(ValidationError):
            schema.load(data)
        assert seen == [{"name": "alice", "age": 30}]


class TestPartialConstructConfiguration:
    def test_pass_collection_post_load_is_rejected(self):
        class EnvelopeSchema(UserSchema):
            @post_load(pass_collection=True)
            def envelope(self, data, **kwargs):
                return data

        with pytest.raises(ValueError, match="pass_collection"):
            EnvelopeSchema(many=True, partial_construct=True)

    def test_index_errors_disabled_is_rejected(self):
        class NoIndexSchema(UserSchema):
            class Meta:
                index_errors = False

        with pytest.raises(ValueError, match="index_errors"):
            NoIndexSchema(many=True, partial_construct=True)

    def test_default_is_disabled(self):
        schema = UserSchema(many=True)
        assert schema.partial_construct is False
        with pytest.raises(ValidationError) as excinfo:
            schema.load([{"name": "alice", "age": 30}, {"name": "bob"}])
        # Default behavior: valid_data holds unconstructed canonical dicts
        assert excinfo.value.valid_data == [
            {"name": "alice", "age": 30},
            {"name": "bob"},
        ]
