import pytest

from marshmallow import structured
from marshmallow.exceptions import ValidationError


class TestValidationError:
    def test_stores_message_in_list(self):
        err = ValidationError("foo")
        assert err.messages == ["foo"]

    def test_can_pass_list_of_messages(self):
        err = ValidationError(["foo", "bar"])
        assert err.messages == ["foo", "bar"]

    def test_stores_dictionaries(self):
        messages = {"user": {"email": ["email is invalid"]}}
        err = ValidationError(messages)
        assert err.messages == messages

    def test_can_store_field_name(self):
        err = ValidationError("invalid email", field_name="email")
        assert err.field_name == "email"

    def test_str(self):
        err = ValidationError("invalid email")
        assert str(err) == "invalid email"

        err2 = ValidationError("invalid email", "email")
        assert str(err2) == "invalid email"

    def test_stores_dictionaries_in_messages_dict(self):
        messages = {"user": {"email": ["email is invalid"]}}
        err = ValidationError(messages)
        assert err.messages_dict == messages

    def test_messages_dict_type_error_on_badval(self):
        err = ValidationError("foo")
        with pytest.raises(TypeError) as excinfo:
            err.messages_dict  # noqa: B018
        assert "cannot access 'messages_dict' when 'messages' is of type list" in str(
            excinfo.value
        )


class TestStructuredErrors:
    def test_scalar_message_has_empty_path(self):
        err = ValidationError("boom")
        assert err.structured == [structured.StructuredError((), ["boom"])]

    def test_field_name_is_typed_field_segment(self):
        err = ValidationError(["bad"], field_name="email")
        (error,) = err.structured
        assert error.path == (structured.PathSegment.field("email"),)
        assert error.messages == ["bad"]

    def test_dict_messages_keep_field_and_schema_segments(self):
        messages = {"email": ["bad"], "_schema": ["global"]}
        err = ValidationError(messages)
        paths = {tuple(seg.kind for seg in e.path): e.messages for e in err.structured}
        assert paths[("field",)] == ["bad"]
        assert paths[("schema",)] == ["global"]

    def test_mapping_key_and_value_markers_are_typed(self):
        messages = {0: {"key": ["bad key"], "value": ["bad value"]}}
        err = ValidationError(messages)
        kinds = [
            tuple(seg.kind for seg in error.path) for error in err.structured
        ]
        assert ("index", "mapping_key") in kinds
        assert ("index", "mapping_key", "mapping_value") in kinds
        # The literal "key"/"value" strings never appear as field segments.
        for error in err.structured:
            assert all(
                seg.kind != "field" or seg.value not in ("key", "value")
                for seg in error.path
            )

    def test_structured_property_returns_a_copy(self):
        err = ValidationError("boom")
        err.structured.clear()
        assert err.structured  # the original records are untouched

    def test_structured_jsonable_is_json_serializable(self):
        import json

        messages = {"email": ["bad"]}
        err = ValidationError(messages)
        encoded = err.structured_jsonable()
        # Round-trips through JSON even though the raw messages cannot.
        json.dumps(encoded)
        segment = encoded[0]["path"][0]
        assert segment["kind"] == "field"
        assert segment["value"] == "email"

    def test_non_string_key_jsonable_uses_type_tagged_envelope(self):
        import json

        # A non-string key inside the message tree is encoded, not str-coerced.
        encoded = structured.to_jsonable({(1, 2): ["bad"]})
        json.dumps(encoded)
        (pair,) = encoded
        key = pair["key"]
        assert key["__marshmallow_key__"] is True
        assert key["type"] == "tuple"
        assert pair["value"] == ["bad"]


class TestEncodeKey:
    def test_json_native_keys_are_preserved(self):
        assert structured.encode_key("s") == "s"
        assert structured.encode_key(1) == 1
        assert structured.encode_key(True) is True
        assert structured.encode_key(None) is None

    def test_distinct_non_string_keys_do_not_collide(self):
        import json

        class Custom:
            def __repr__(self):
                return "1"

        keys = [1, "1", (1,), Custom()]
        encoded = {
            json.dumps(structured.encode_key(key), sort_keys=True) for key in keys
        }
        assert len(encoded) == len(keys)

    def test_envelope_records_type_name(self):
        encoded = structured.encode_key((1, 2))
        assert encoded == {
            "__marshmallow_key__": True,
            "type": "tuple",
            "value": [1, 2],
        }
