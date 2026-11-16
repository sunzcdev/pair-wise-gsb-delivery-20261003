"""
Tests for opt-in validation of the ``content*`` keywords.
"""

from unittest import TestCase
import base64
import json

from jsonschema import (
    ContentValidator,
    Draft7Validator,
    Draft202012Validator,
    ValidationError,
)

SCHEMA = {
    "type": "object",
    "properties": {
        "payload": {
            "type": "string",
            "contentEncoding": "base64",
            "contentMediaType": "application/json",
            "contentSchema": {
                "type": "object",
                "properties": {"n": {"type": "integer"}},
                "required": ["n"],
            },
        },
    },
}


def encoded(thing):
    return base64.b64encode(json.dumps(thing).encode("utf-8")).decode("ascii")


class TestContentValidationIsOptIn(TestCase):
    def test_content_keywords_are_annotations_by_default(self):
        validator = Draft202012Validator(SCHEMA)
        self.assertTrue(validator.is_valid({"payload": "!!! not base64 !!!"}))
        self.assertTrue(validator.is_valid({"payload": encoded({"n": "no"})}))

    def test_jsonschema_validate_does_not_decode_by_default(self):
        validator = Draft202012Validator(
            SCHEMA["properties"]["payload"],
        )
        self.assertEqual(
            list(validator.iter_errors(encoded({"n": "no"}))),
            [],
        )

    def test_non_strings_are_not_the_content_keywords_business(self):
        validator = Draft202012Validator(
            {
                "contentEncoding": "base64",
                "contentMediaType": "application/json",
                "contentSchema": {"type": "integer"},
            },
            content_validator=ContentValidator(),
        )
        self.assertTrue(validator.is_valid(12))
        self.assertTrue(validator.is_valid({"not": "a string"}))


class TestContentEncoding(TestCase):
    def validator(self, schema, **kwargs):
        return Draft202012Validator(
            schema,
            content_validator=ContentValidator(**kwargs),
        )

    def test_invalid_base64_is_an_error(self):
        validator = self.validator(SCHEMA)
        (error,) = validator.iter_errors({"payload": "!!! not base64 !!!"})
        self.assertEqual(error.validator, "contentEncoding")
        self.assertEqual(error.validator_value, "base64")
        self.assertEqual(list(error.absolute_path), ["payload"])
        self.assertIn("base64", error.message)

    def test_unknown_encoding_is_reported_unsupported_by_default(self):
        validator = self.validator({"contentEncoding": "rot13"})
        (error,) = validator.iter_errors("hello")
        self.assertEqual(error.validator, "contentEncoding")
        self.assertEqual(error.validator_value, "rot13")
        self.assertIn("not supported", error.message)

    def test_unknown_encoding_can_be_declared_to_be_ignored(self):
        validator = self.validator(
            {"contentEncoding": "rot13"},
            on_unsupported="ignore",
        )
        self.assertTrue(validator.is_valid("hello"))

    def test_an_unknown_policy_is_rejected(self):
        with self.assertRaises(ValueError):
            ContentValidator(on_unsupported="explode")

    def test_the_decoding_budget_is_enforced(self):
        validator = self.validator(SCHEMA, max_decoded_bytes=4)
        (error,) = validator.iter_errors({"payload": encoded({"n": 3})})
        self.assertEqual(error.validator, "contentEncoding")
        self.assertIn("budget", error.message)
        self.assertEqual(list(error.absolute_path), ["payload"])

    def test_the_budget_is_independent_of_the_encoded_length(self):
        # 8 base64 characters decode to 5 bytes, which fits a budget of
        # 5 even though the encoded string itself is longer.
        payload = base64.b64encode(b"hello").decode("ascii")
        self.assertGreater(len(payload), 5)
        validator = self.validator(
            {"contentEncoding": "base64"},
            max_decoded_bytes=5,
        )
        self.assertTrue(validator.is_valid(payload))

    def test_the_budget_applies_to_custom_decoders_too(self):
        def decoder(instance, max_decoded_bytes):
            return b"x" * (max_decoded_bytes + 1)

        validator = self.validator(
            {"contentEncoding": "explode"},
            decoders={"explode": decoder},
        )
        (error,) = validator.iter_errors("anything")
        self.assertEqual(error.validator, "contentEncoding")
        self.assertIn("budget", error.message)


class TestContentMediaType(TestCase):
    def validator(self, schema, **kwargs):
        return Draft202012Validator(
            schema,
            content_validator=ContentValidator(**kwargs),
        )

    def test_content_which_is_not_json_is_an_error(self):
        validator = self.validator(SCHEMA)
        payload = base64.b64encode(b"not json").decode("ascii")
        (error,) = validator.iter_errors({"payload": payload})
        self.assertEqual(error.validator, "contentMediaType")
        self.assertEqual(error.validator_value, "application/json")
        self.assertEqual(list(error.absolute_path), ["payload"])

    def test_media_types_are_checked_without_a_content_schema(self):
        validator = self.validator(
            {
                "contentEncoding": "base64",
                "contentMediaType": "application/json",
            },
        )
        payload = base64.b64encode(b"not json").decode("ascii")
        (error,) = validator.iter_errors(payload)
        self.assertEqual(error.validator, "contentMediaType")

    def test_json_strings_without_an_encoding_are_checked(self):
        schema = {
            "contentMediaType": "application/json",
            "contentSchema": {"type": "object"},
        }
        validator = self.validator(schema)
        self.assertTrue(validator.is_valid('{"a": 1}'))
        self.assertFalse(validator.is_valid("[1, 2]"))

    def test_unknown_media_type_with_content_schema_is_unsupported(self):
        validator = self.validator(
            {
                "contentMediaType": "application/x-unknown",
                "contentSchema": True,
            },
        )
        (error,) = validator.iter_errors("whatever")
        self.assertEqual(error.validator, "contentMediaType")
        self.assertIn("not supported", error.message)

    def test_unknown_media_type_with_content_schema_can_be_ignored(self):
        validator = self.validator(
            {
                "contentMediaType": "application/x-unknown",
                "contentSchema": True,
            },
            on_unsupported="ignore",
        )
        self.assertTrue(validator.is_valid("whatever"))

    def test_unknown_media_type_without_content_schema_is_annotation(self):
        validator = self.validator(
            {"contentMediaType": "application/x-unknown"},
        )
        self.assertTrue(validator.is_valid("whatever"))


class TestContentSchema(TestCase):
    def validator(self, schema, **kwargs):
        return Draft202012Validator(
            schema,
            content_validator=ContentValidator(**kwargs),
        )

    def test_valid_content_passes(self):
        validator = self.validator(SCHEMA)
        self.assertTrue(validator.is_valid({"payload": encoded({"n": 3})}))

    def test_errors_point_at_the_outer_field(self):
        validator = self.validator(SCHEMA)
        (error,) = validator.iter_errors({"payload": encoded({"n": "no"})})

        # The error is located at the outer string in the original
        # document...
        self.assertEqual(error.validator, "contentSchema")
        self.assertEqual(list(error.absolute_path), ["payload"])
        self.assertEqual(error.json_path, "$.payload")
        self.assertEqual(error.instance, encoded({"n": "no"}))

        # ... while the inner path within the decoded content is kept
        # separately, not mistaken for a path into the original document.
        self.assertEqual(list(error.content_path), ["n"])
        self.assertNotIn("n", error.absolute_path)
        self.assertEqual(error.content_error.json_path, "$.n")

    def test_inner_paths_can_be_deeper(self):
        schema = {
            "properties": {
                "payload": {
                    "contentMediaType": "application/json",
                    "contentSchema": {
                        "properties": {
                            "a": {"items": {"type": "integer"}},
                        },
                    },
                },
            },
        }
        validator = self.validator(schema)
        instance = {"payload": json.dumps({"a": [1, "two", 3]})}
        (error,) = validator.iter_errors(instance)
        self.assertEqual(list(error.absolute_path), ["payload"])
        self.assertEqual(list(error.content_path), ["a", 1])

    def test_each_inner_error_is_reported(self):
        validator = self.validator(SCHEMA)
        errors = validator.iter_errors({"payload": encoded({})})
        self.assertEqual(len(list(errors)), 1)  # missing required "n"

    def test_content_schema_is_ignored_without_a_media_type(self):
        validator = self.validator(
            {
                "contentEncoding": "base64",
                "contentSchema": {"type": "integer"},
            },
        )
        self.assertTrue(validator.is_valid(encoded({"not": "an integer"})))

    def test_boolean_content_schemas(self):
        schema = {
            "contentMediaType": "application/json",
            "contentSchema": False,
        }
        validator = self.validator(schema)
        (error,) = validator.iter_errors("12")
        self.assertEqual(error.validator, "contentSchema")

    def test_content_schema_may_use_refs(self):
        schema = {
            "$defs": {"positive": {"type": "integer", "minimum": 0}},
            "contentMediaType": "application/json",
            "contentSchema": {"$ref": "#/$defs/positive"},
        }
        validator = self.validator(schema)
        self.assertTrue(validator.is_valid("12"))
        (error,) = validator.iter_errors("-12")
        self.assertEqual(list(error.content_path), [])


class TestContentValidationAcrossDrafts(TestCase):
    def test_draft7_supports_encoding_and_media_type(self):
        schema = {
            "contentEncoding": "base64",
            "contentMediaType": "application/json",
        }
        validator = Draft7Validator(
            schema,
            content_validator=ContentValidator(),
        )
        payload = base64.b64encode(b"not json").decode("ascii")
        (error,) = validator.iter_errors(payload)
        self.assertEqual(error.validator, "contentMediaType")
        self.assertTrue(
            validator.is_valid(encoded({"any": "thing"})),
        )

    def test_the_policy_is_carried_along_when_evolving(self):
        validator = Draft202012Validator(
            {},
            content_validator=ContentValidator(),
        )
        evolved = validator.evolve(schema={"contentEncoding": "base64"})
        (error,) = evolved.iter_errors("not base64!")
        self.assertEqual(error.validator, "contentEncoding")

    def test_errors_are_validation_errors(self):
        validator = Draft202012Validator(
            SCHEMA,
            content_validator=ContentValidator(),
        )
        with self.assertRaises(ValidationError):
            validator.validate({"payload": encoded({"n": "no"})})
