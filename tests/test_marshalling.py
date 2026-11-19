from __future__ import annotations

import json
import sys
from datetime import UTC, datetime, timedelta
from functools import partial
from types import ModuleType
from typing import Any, NoReturn

import pytest

from apscheduler import SerializationError
from apscheduler._marshalling import (
    _NOT_FOUND,
    callable_from_ref,
    callable_to_ref,
    find_unserializable,
    marshal_field,
    marshal_metadata,
)
from apscheduler.serializers.json import JSONSerializer
from apscheduler.serializers.pickle import PickleSerializer


class DummyClass:
    def meth(self):
        pass

    @staticmethod
    def staticmeth():
        pass

    @classmethod
    def classmeth(cls):
        pass

    def __call__(self):
        pass

    class InnerDummyClass:
        @classmethod
        def innerclassmeth(cls):
            pass


class InheritedDummyClass(DummyClass):
    pass


class TestCallableToRef:
    @pytest.mark.parametrize(
        "obj, error",
        [
            (partial(DummyClass.meth), "Cannot create a reference to a partial()"),
            (lambda: None, "Cannot create a reference to a lambda"),
        ],
        ids=["partial", "lambda"],
    )
    def test_errors(self, obj, error):
        exc = pytest.raises(SerializationError, callable_to_ref, obj)
        assert str(exc.value) == error

    def test_nested_function_error(self):
        def nested():
            pass

        exc = pytest.raises(SerializationError, callable_to_ref, nested)
        assert str(exc.value) == "Cannot create a reference to a nested function"

    @pytest.mark.parametrize(
        "input,expected",
        [
            (DummyClass.meth, "test_marshalling:DummyClass.meth"),
            (DummyClass.classmeth, "test_marshalling:DummyClass.classmeth"),
            (
                DummyClass.InnerDummyClass.innerclassmeth,
                "test_marshalling:DummyClass.InnerDummyClass.innerclassmeth",
            ),
            (DummyClass.staticmeth, "test_marshalling:DummyClass.staticmeth"),
            (
                InheritedDummyClass.classmeth,
                "test_marshalling:InheritedDummyClass.classmeth",
            ),
            (timedelta, "datetime:timedelta"),
        ],
        ids=[
            "unbound method",
            "class method",
            "inner class method",
            "static method",
            "inherited class method",
            "timedelta",
        ],
    )
    def test_valid_refs(self, input, expected):
        assert callable_to_ref(input) == expected


class TestCallableFromRef:
    def test_valid_ref(self):
        from logging.handlers import RotatingFileHandler

        assert (
            callable_from_ref("logging.handlers:RotatingFileHandler")
            is RotatingFileHandler
        )

    def test_complex_path(self):
        pkg1 = ModuleType("pkg1")
        pkg1.pkg2 = "blah"
        pkg2 = ModuleType("pkg1.pkg2")
        pkg2.varname = lambda: None
        sys.modules["pkg1"] = pkg1
        sys.modules["pkg1.pkg2"] = pkg2
        assert callable_from_ref("pkg1.pkg2:varname") == pkg2.varname

    @pytest.mark.parametrize(
        "input,error",
        [(object(), TypeError), ("module", ValueError), ("module:blah", LookupError)],
        ids=["raw object", "module", "module attribute"],
    )
    def test_lookup_error(self, input, error):
        pytest.raises(error, callable_from_ref, input)


class Unserializable:
    def __getstate__(self) -> NoReturn:
        raise ValueError("cannot be serialized")

    def __repr__(self) -> str:
        return "<TOP SECRET VALUE>"


class Stubborn:
    """Fails serialization, but has no individually offending value."""

    def __getstate__(self) -> NoReturn:
        raise ValueError("stubborn")

    def __repr__(self) -> str:
        return "<TOP SECRET VALUE>"


class NoState:
    def __repr__(self) -> str:
        return "<TOP SECRET VALUE>"


class TestFindUnserializable:
    def test_accepted_value(self) -> None:
        assert find_unserializable(json.dumps, {"a": [1, 2]}) is _NOT_FOUND

    def test_nested_dict(self) -> None:
        offender = NoState()
        value: dict[str, Any] = {"a": {"b": [1, offender]}}
        assert find_unserializable(json.dumps, value) is offender

    def test_nested_list(self) -> None:
        offender = NoState()
        value = [1, [2, [offender]]]
        assert find_unserializable(json.dumps, value) is offender

    def test_non_string_key(self) -> None:
        key = frozenset({1})
        assert find_unserializable(json.dumps, {key: "foo"}) is key

    def test_offender_is_value_itself(self) -> None:
        offender = NoState()
        assert find_unserializable(json.dumps, offender) is offender

    def test_reference_cycle(self) -> None:
        value: list[Any] = []
        value.append(value)
        container: dict[str, Any] = {"key": value}
        # The cycle itself is the problem, so the innermost problematic container is
        # returned
        assert find_unserializable(json.dumps, container) is value


class TestMarshalField:
    @pytest.mark.parametrize("serializer_class", [JSONSerializer, PickleSerializer])
    def test_success(self, serializer_class: type) -> None:
        serializer = serializer_class()
        value = {"a": [1, "two"]}
        assert marshal_field(serializer, "job", "kwargs", value) == (
            serializer.serialize(value)
        )

    @pytest.mark.parametrize("serializer_class", [JSONSerializer, PickleSerializer])
    def test_failure(self, serializer_class: type) -> None:
        serializer = serializer_class()
        with pytest.raises(SerializationError) as exc_info:
            marshal_field(serializer, "job", "kwargs", {"a": Unserializable()})

        assert str(exc_info.value) == (
            "Failed to serialize the job's kwargs: value of type "
            "test_marshalling.Unserializable is not serializable"
        )
        assert exc_info.value.__cause__ is not None
        assert "TOP SECRET" not in str(exc_info.value)

    def test_failure_without_offending_member(self) -> None:
        with pytest.raises(SerializationError) as exc_info:
            marshal_field(PickleSerializer(), "job", "args", (Stubborn(),))

        assert "test_marshalling.Stubborn" in str(exc_info.value)

    def test_field_names_are_reported_independently(self) -> None:
        for field in ("args", "kwargs"):
            with pytest.raises(SerializationError) as exc_info:
                marshal_field(JSONSerializer(), "schedule", field, (Unserializable(),))

            assert f"schedule's {field}" in str(exc_info.value)


class TestMarshalMetadata:
    def test_success(self) -> None:
        metadata = {"a": [1, "two", None, True, {"b": 2.5}]}
        assert marshal_metadata("job", metadata) is metadata

    def test_failure(self) -> None:
        with pytest.raises(SerializationError) as exc_info:
            marshal_metadata("job", {"a": NoState()})

        assert str(exc_info.value) == (
            "Failed to serialize the job's metadata: value of type "
            "test_marshalling.NoState is not JSON serializable"
        )
        assert exc_info.value.__cause__ is not None
        assert "TOP SECRET" not in str(exc_info.value)

    def test_non_json_type(self) -> None:
        with pytest.raises(SerializationError) as exc_info:
            marshal_metadata("task", {"a": datetime.now(UTC)})

        assert "task's metadata" in str(exc_info.value)
        assert "datetime.datetime" in str(exc_info.value)
