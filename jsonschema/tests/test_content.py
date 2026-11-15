"""
Tests for the explicitly opt-in validation of the content keywords.

Covers :kw:`contentEncoding`, :kw:`contentMediaType` and
:kw:`contentSchema`, which are only annotations unless content validation
is enabled via `jsonschema.content.enable_content_validation`.
"""

from unittest import TestCase
import base64

from jsonschema import _content
from jsonschema.content import (
    ContentValidator,
    UnsupportedContentPolicy,
    enable_content_validation,
)
from jsonschema.exceptions import (
    ContentEncodingError,
    ContentMediaTypeError,
    ContentSchemaValidationError,
    ContentValidationError,
    ValidationError,
)
from jsonschema.validators import (
    Draft7Validator,
    Draft201909Validator,
    Draft202012Validator,
)
import jsonschema


def b64(raw: bytes) -> str:
    return base64.b64encode(raw).decode("ascii")


class TestContentValidationIsOptIn(TestCase):
    def test_content_keywords_are_ignored_by_default(self):
        schema = {
            "contentEncoding": "base64",
            "contentMediaType": "application/json",
            "contentSchema": {"type": "object", "required": ["foo"]},
        }
        for validator in (
            Draft202012Validator,
            Draft201909Validator,
            Draft7Validator,
        ):
            with self.subTest(validator=validator):
                # Garbage encoding, invalid JSON, failing schema: all pass.
                self.assertEqual(
                    list(validator(schema).iter_errors("garbage")),
                    [],
                )

    def test_enabling_does_not_change_non_content_validation(self):
        Extended = enable_content_validation(Draft202012Validator)
        schema = {"type": "integer"}
        self.assertTrue(Extended(schema).is_valid(12))
        self.assertFalse(Extended(schema).is_valid("a string"))

    def test_non_string_instances_skip_content_validation(self):
        Extended = enable_content_validation(Draft202012Validator)
        schema = {
            "type": "string",
            "contentEncoding": "base64",
            "contentMediaType": "application/json",
        }
        errors = list(Extended(schema).iter_errors(100))
        self.assertEqual(len(errors), 1)
        self.assertEqual(errors[0].validator, "type")
        self.assertNotIsInstance(errors[0], ContentValidationError)

    def test_keyword_functions_do_nothing_without_a_strategy(self):
        # The registered keywords are inert on a non-extended validator.
        validator = Draft202012Validator({"contentEncoding": "base64"})
        self.assertIsNone(validator.content_validator)
        self.assertEqual(
            list(_content.contentEncoding(validator, "base64", "garbage", {})),
            [],
        )


class TestContentEncoding(TestCase):
    def setUp(self):
        self.Validator = enable_content_validation(Draft202012Validator)

    def test_valid_base64_passes(self):
        validator = self.Validator({"contentEncoding": "base64"})
        self.assertTrue(validator.is_valid(b64(b"hello world")))

    def test_invalid_base64_reports_an_encoding_error(self):
        errors = list(
            self.Validator({"contentEncoding": "base64"}).iter_errors("a%b"),
        )
        self.assertEqual(len(errors), 1)
        self.assertIsInstance(errors[0], ContentEncodingError)
        self.assertEqual(errors[0].validator, "contentEncoding")

    def test_json_that_is_not_base64_fails_at_the_encoding_stage(self):
        # "{}" is valid JSON but not valid base64; the media type stage
        # must never run.
        errors = list(
            self.Validator(
                {
                    "contentEncoding": "base64",
                    "contentMediaType": "application/json",
                },
            ).iter_errors("{}"),
        )
        self.assertEqual(len(errors), 1)
        self.assertIsInstance(errors[0], ContentEncodingError)

    def test_decoded_bytes_that_are_not_utf8_fail(self):
        payload = b64(bytes([0xFF, 0xFE, 0xFD]))
        errors = list(
            self.Validator({"contentEncoding": "base64"}).iter_errors(payload),
        )
        self.assertIsInstance(errors[0], ContentEncodingError)
        self.assertIn("UTF-8", errors[0].message)

    def test_outer_type_is_checked_before_encoding(self):
        schema = {
            "type": "string",
            "contentEncoding": "base64",
        }
        errors = list(self.Validator(schema).iter_errors(37))
        self.assertEqual(len(errors), 1)
        self.assertEqual(errors[0].validator, "type")

    def test_unknown_encoding_is_an_error_by_default(self):
        schema = {"contentEncoding": "quoted-printable"}
        errors = list(self.Validator(schema).iter_errors("anything"))
        self.assertEqual(len(errors), 1)
        self.assertIsInstance(errors[0], ContentEncodingError)
        self.assertIn("Unsupported contentEncoding", errors[0].message)

    def test_unknown_encoding_can_be_ignored(self):
        Validator = enable_content_validation(
            Draft202012Validator,
            ContentValidator(
                unsupported_encoding=UnsupportedContentPolicy.IGNORE,
            ),
        )
        errors = list(
            Validator({"contentEncoding": "quoted-printable"}).iter_errors(
                "anything",
            ),
        )
        self.assertEqual(errors, [])

    def test_ignored_encoding_skips_the_remaining_stages(self):
        Validator = enable_content_validation(
            Draft202012Validator,
            ContentValidator(
                unsupported_encoding=UnsupportedContentPolicy.IGNORE,
            ),
        )
        schema = {
            "contentEncoding": "quoted-printable",
            "contentMediaType": "application/json",
            "contentSchema": {"type": "number"},
        }
        self.assertEqual(list(Validator(schema).iter_errors("=41=42")), [])


class TestByteBudget(TestCase):
    def test_payload_exceeding_the_budget_fails(self):
        Validator = enable_content_validation(
            Draft202012Validator,
            ContentValidator(byte_budget=10),
        )
        payload = b64(b"x" * 100)
        errors = list(
            Validator({"contentEncoding": "base64"}).iter_errors(payload),
        )
        self.assertEqual(len(errors), 1)
        self.assertIsInstance(errors[0], ContentEncodingError)
        self.assertIn("byte budget", errors[0].message)

    def test_payload_at_the_budget_boundary_passes(self):
        Validator = enable_content_validation(
            Draft202012Validator,
            ContentValidator(byte_budget=100),
        )
        payload = b64(b"x" * 100)
        schema = {"contentEncoding": "base64"}
        self.assertEqual(
            list(Validator(schema).iter_errors(payload)),
            [],
        )

    def test_budget_is_independent_per_field(self):
        Validator = enable_content_validation(
            Draft202012Validator,
            ContentValidator(byte_budget=50),
        )
        schema = {
            "type": "object",
            "properties": {
                "first": {"contentEncoding": "base64"},
                "second": {"contentEncoding": "base64"},
            },
        }
        instance = {
            "first": b64(b"a" * 40),
            "second": b64(b"b" * 40),
        }
        # Each field is within budget on its own; both validate.
        self.assertEqual(
            list(Validator(schema).iter_errors(instance)),
            [],
        )

    def test_budget_is_independent_across_validations(self):
        Validator = enable_content_validation(
            Draft202012Validator,
            ContentValidator(byte_budget=50),
        )
        validator = Validator({"contentEncoding": "base64"})
        payload = b64(b"z" * 40)
        for _ in range(3):
            self.assertEqual(list(validator.iter_errors(payload)), [])

    def test_none_budget_disables_the_limit(self):
        Validator = enable_content_validation(
            Draft202012Validator,
            ContentValidator(byte_budget=None),
        )
        payload = b64(b"x" * 100_000)
        schema = {"contentEncoding": "base64"}
        self.assertEqual(
            list(Validator(schema).iter_errors(payload)),
            [],
        )

    def test_negative_budget_is_rejected(self):
        with self.assertRaises(ValueError):
            ContentValidator(byte_budget=-1)


class TestContentMediaType(TestCase):
    def setUp(self):
        self.Validator = enable_content_validation(Draft202012Validator)

    def test_valid_json_without_encoding_passes(self):
        schema = {"contentMediaType": "application/json"}
        self.assertTrue(self.Validator(schema).is_valid('{"foo": "bar"}'))

    def test_invalid_json_reports_a_media_type_error(self):
        errors = list(
            self.Validator(
                {"contentMediaType": "application/json"},
            ).iter_errors("{:}"),
        )
        self.assertEqual(len(errors), 1)
        self.assertIsInstance(errors[0], ContentMediaTypeError)
        self.assertEqual(errors[0].validator, "contentMediaType")

    def test_base64_json_is_decoded_before_parsing(self):
        schema = {
            "contentEncoding": "base64",
            "contentMediaType": "application/json",
        }
        validator = self.Validator(schema)
        self.assertTrue(validator.is_valid(b64(b'{"foo": "bar"}')))

        errors = list(validator.iter_errors(b64(b"{:}")))
        self.assertEqual(len(errors), 1)
        self.assertIsInstance(errors[0], ContentMediaTypeError)

    def test_unknown_media_type_is_an_error_by_default(self):
        errors = list(
            self.Validator(
                {"contentMediaType": "application/xml"},
            ).iter_errors("<a></a>"),
        )
        self.assertEqual(len(errors), 1)
        self.assertIsInstance(errors[0], ContentMediaTypeError)
        self.assertIn("Unsupported contentMediaType", errors[0].message)

    def test_unknown_media_type_can_be_ignored(self):
        Validator = enable_content_validation(
            Draft202012Validator,
            ContentValidator(
                unsupported_media_type=UnsupportedContentPolicy.IGNORE,
            ),
        )
        self.assertEqual(
            list(
                Validator({"contentMediaType": "application/xml"}).iter_errors(
                    "<a></a>",
                ),
            ),
            [],
        )

    def test_invalid_policy_values_are_rejected(self):
        with self.assertRaises(ValueError):
            ContentValidator(unsupported_encoding="bogus")
        with self.assertRaises(ValueError):
            ContentValidator(unsupported_media_type="bogus")


class TestContentSchema(TestCase):
    SCHEMA = {
        "contentEncoding": "base64",
        "contentMediaType": "application/json",
        "contentSchema": {
            "type": "object",
            "required": ["foo"],
            "properties": {"foo": {"type": "string"}},
        },
    }

    def setUp(self):
        self.Validator = enable_content_validation(Draft202012Validator)

    def test_matching_content_passes(self):
        validator = self.Validator(self.SCHEMA)
        self.assertTrue(validator.is_valid(b64(b'{"foo": "bar"}')))

    def test_content_failing_the_schema_reports_an_error(self):
        errors = list(
            self.Validator(self.SCHEMA).iter_errors(b64(b'{"boo": 20}')),
        )
        self.assertEqual(len(errors), 1)
        self.assertIsInstance(errors[0], ContentSchemaValidationError)
        self.assertEqual(errors[0].validator, "contentSchema")

    def test_inner_path_is_kept_separate_from_the_outer_path(self):
        schema = {
            "type": "object",
            "properties": {
                "payload": {
                    "contentMediaType": "application/json",
                    "contentSchema": {
                        "type": "object",
                        "properties": {"foo": {"type": "integer"}},
                    },
                },
            },
        }
        instance = {"payload": '{"foo": "not an integer"}'}
        errors = list(self.Validator(schema).iter_errors(instance))
        self.assertEqual(len(errors), 1)
        error = errors[0]
        self.assertIsInstance(error, ContentSchemaValidationError)
        # The error points at the outer string field...
        self.assertEqual(list(error.path), ["payload"])
        self.assertEqual(list(error.absolute_path), ["payload"])
        # ...and the failure within the decoded content is reported apart.
        self.assertEqual(list(error.inner_path), ["foo"])
        self.assertIsNotNone(error.inner_error)
        self.assertEqual(list(error.inner_error.path), ["foo"])

    def test_inner_path_inside_arrays(self):
        schema = {
            "contentMediaType": "application/json",
            "contentSchema": {
                "type": "array",
                "items": {"type": "integer"},
            },
        }
        errors = list(self.Validator(schema).iter_errors('[1, "two", 3]'))
        self.assertEqual(len(errors), 1)
        self.assertEqual(list(errors[0].inner_path), [1])
        self.assertEqual(list(errors[0].path), [])

    def test_outer_path_includes_array_indices(self):
        schema = {
            "type": "array",
            "items": {
                "contentEncoding": "base64",
                "contentMediaType": "application/json",
                "contentSchema": {"type": "integer"},
            },
        }
        instance = [b64(b"7"), b64(b'"seven"')]
        errors = list(self.Validator(schema).iter_errors(instance))
        self.assertEqual(len(errors), 1)
        self.assertEqual(list(errors[0].path), [1])
        self.assertEqual(list(errors[0].inner_path), [])

    def test_inner_paths_are_not_treated_as_original_document_paths(self):
        schema = {
            "type": "object",
            "properties": {
                "payload": {
                    "contentMediaType": "application/json",
                    "contentSchema": {
                        "type": "object",
                        "properties": {"deep": {"type": "integer"}},
                    },
                },
            },
        }
        error = next(
            iter(
                self.Validator(schema).iter_errors(
                    {"payload": '{"deep": "x"}'},
                ),
            ),
        )
        # Nothing from the inner path leaks into the original document path.
        self.assertEqual(list(error.path), ["payload"])
        self.assertNotIn("deep", error.path)
        self.assertEqual(list(error.inner_path), ["deep"])

    def test_content_schema_without_media_type_is_not_applied(self):
        # There is no decoded/parsed representation to validate.
        schema = {"contentSchema": {"type": "number"}}
        self.assertEqual(
            list(self.Validator(schema).iter_errors("a plain string")),
            [],
        )

    def test_content_schema_resolves_refs_through_the_validator(self):
        schema = {
            "$defs": {"integer": {"type": "integer"}},
            "contentMediaType": "application/json",
            "contentSchema": {"$ref": "#/$defs/integer"},
        }
        self.assertTrue(self.Validator(schema).is_valid("12"))
        errors = list(self.Validator(schema).iter_errors('"string"'))
        self.assertIsInstance(errors[0], ContentSchemaValidationError)

    def test_inner_error_is_a_validation_error(self):
        schema = {
            "contentMediaType": "application/json",
            "contentSchema": {"const": 4},
        }
        error = next(iter(self.Validator(schema).iter_errors("5")))
        self.assertIsInstance(error, ContentSchemaValidationError)
        self.assertIsInstance(error.inner_error, ValidationError)
        self.assertEqual(error.inner_error.validator, "const")


class TestStagesAndCaching(TestCase):
    def setUp(self):
        self.Validator = enable_content_validation(Draft202012Validator)

    def test_only_one_error_is_yielded_per_keyword_for_a_failure(self):
        # All three keywords are present; a broken encoding must produce
        # exactly one error, attributed to contentEncoding.
        schema = {
            "contentEncoding": "base64",
            "contentMediaType": "application/json",
            "contentSchema": {"type": "object"},
        }
        errors = list(self.Validator(schema).iter_errors("%%%"))
        self.assertEqual(len(errors), 1)
        self.assertIsInstance(errors[0], ContentEncodingError)

    def test_a_single_field_is_decoded_only_once(self):
        strategy = ContentValidator()
        calls = []
        original = strategy.iter_errors

        def counting(validator, schema, instance):
            calls.append(instance)
            yield from original(validator, schema, instance)

        strategy.iter_errors = counting  # type: ignore[method-assign]
        Validator = enable_content_validation(Draft202012Validator, strategy)
        schema = {
            "contentEncoding": "base64",
            "contentMediaType": "application/json",
            "contentSchema": {"type": "object"},
        }
        list(Validator(schema).iter_errors(b64(b"{}")))
        # The three sibling keywords share a single cached result.
        self.assertEqual(calls, [b64(b"{}")])

    def test_distinct_fields_are_each_decoded_separately(self):
        strategy = ContentValidator()
        calls = []
        original = strategy.iter_errors

        def counting(validator, schema, instance):
            calls.append(instance)
            yield from original(validator, schema, instance)

        strategy.iter_errors = counting  # type: ignore[method-assign]
        Validator = enable_content_validation(Draft202012Validator, strategy)
        schema = {
            "type": "object",
            "properties": {
                "a": {
                    "contentEncoding": "base64",
                    "contentMediaType": "application/json",
                },
                "b": {
                    "contentEncoding": "base64",
                    "contentMediaType": "application/json",
                },
            },
        }
        first, second = b64(b"{}"), b64(b"[]")
        list(Validator(schema).iter_errors({"a": first, "b": second}))
        self.assertEqual(sorted(calls), sorted([first, second]))

    def test_all_three_error_types_are_validation_errors(self):
        for error_type, schema, instance in (
            (ContentEncodingError, {"contentEncoding": "base64"}, "%%%"),
            (
                ContentMediaTypeError,
                {"contentMediaType": "application/json"},
                "{:}",
            ),
            (
                ContentSchemaValidationError,
                {
                    "contentMediaType": "application/json",
                    "contentSchema": {"type": "number"},
                },
                "false",
            ),
        ):
            with self.subTest(error_type=error_type):
                error = next(
                    iter(self.Validator(schema).iter_errors(instance)),
                )
                self.assertIsInstance(error, error_type)
                self.assertIsInstance(error, ContentValidationError)
                self.assertIsInstance(error, ValidationError)


class TestEnableAndIntegration(TestCase):
    def test_enable_returns_a_new_class(self):
        Extended = enable_content_validation(Draft202012Validator)
        self.assertIsNot(Extended, Draft202012Validator)
        # The original class stays annotation-only.
        self.assertNotIn(
            "contentEncoding",
            Draft202012Validator.VALIDATORS,
        )
        self.assertIn("contentEncoding", Extended.VALIDATORS)
        self.assertIn("contentMediaType", Extended.VALIDATORS)
        self.assertIn("contentSchema", Extended.VALIDATORS)

    def test_custom_default_strategy(self):
        strategy = ContentValidator(byte_budget=1)
        Validator = enable_content_validation(Draft202012Validator, strategy)
        self.assertIs(Validator.CONTENT_VALIDATOR, strategy)
        errors = list(
            Validator({"contentEncoding": "base64"}).iter_errors(
                b64(b"too long"),
            ),
        )
        self.assertIsInstance(errors[0], ContentEncodingError)

    def test_per_instance_strategy_overrides_the_default(self):
        strict = ContentValidator(byte_budget=1)
        relaxed = ContentValidator(byte_budget=10_000)
        Validator = enable_content_validation(Draft202012Validator, strict)

        payload = b64(b"hello world")
        schema = {"contentEncoding": "base64"}
        self.assertIsInstance(
            next(iter(Validator(schema).iter_errors(payload))),
            ContentEncodingError,
        )
        self.assertEqual(
            list(
                Validator(
                    {"contentEncoding": "base64"},
                    content_validator=relaxed,
                ).iter_errors(payload),
            ),
            [],
        )

    def test_evolve_preserves_the_strategy(self):
        strategy = ContentValidator(byte_budget=1)
        Validator = enable_content_validation(Draft202012Validator, strategy)
        validator = Validator({"contentEncoding": "base64"})
        evolved = validator.evolve(
            schema={"contentMediaType": "application/json"},
        )
        self.assertIs(evolved.content_validator, strategy)

    def test_works_with_the_validate_function(self):
        Validator = enable_content_validation(Draft202012Validator)
        schema = {
            "contentEncoding": "base64",
            "contentMediaType": "application/json",
            "contentSchema": {"type": "object", "required": ["foo"]},
        }
        good = {"payload": b64(b'{"foo": "bar"}')}
        jsonschema.validate(
            good,
            {"type": "object", "properties": {"payload": schema}},
            cls=Validator,
        )
        with self.assertRaises(jsonschema.ValidationError) as caught:
            jsonschema.validate(
                {"payload": b64(b"[]")},
                {"type": "object", "properties": {"payload": schema}},
                cls=Validator,
            )
        self.assertIsInstance(
            caught.exception,
            ContentSchemaValidationError,
        )
        self.assertEqual(list(caught.exception.path), ["payload"])

    def test_draft7_and_2019_09_are_supported(self):
        for base in (Draft7Validator, Draft201909Validator):
            with self.subTest(base=base):
                Validator = enable_content_validation(base)
                errors = list(
                    Validator(
                        {
                            "contentEncoding": "base64",
                            "contentMediaType": "application/json",
                        },
                    ).iter_errors("not base64!"),
                )
                self.assertIsInstance(errors[0], ContentEncodingError)

    def test_repr_matches_the_standard_validator_shape(self):
        Validator = enable_content_validation(Draft202012Validator)
        # `extend` produces an unnamed class, but the repr stays free of
        # any content-validation internals.
        self.assertEqual(
            repr(Validator({})),
            "Validator(schema={}, format_checker=None)",
        )

    def test_content_validator_repr(self):
        validator = ContentValidator()
        rendered = repr(validator)
        self.assertIn("ContentValidator", rendered)
        self.assertIn("byte_budget", rendered)


class TestErrorMessageRendering(TestCase):
    def test_inner_path_appears_in_the_rendered_message(self):
        Validator = enable_content_validation(Draft202012Validator)
        schema = {
            "contentMediaType": "application/json",
            "contentSchema": {
                "type": "object",
                "properties": {"foo": {"type": "integer"}},
            },
        }
        error = next(iter(Validator(schema).iter_errors('{"foo": "x"}')))
        rendered = str(error)
        self.assertIn("contentSchema", rendered)
        self.assertIn("Within the decoded content at: /foo", rendered)

    def test_encoding_error_has_no_inner_path_section(self):
        Validator = enable_content_validation(Draft202012Validator)
        error = next(
            iter(
                Validator({"contentEncoding": "base64"}).iter_errors("%%%"),
            ),
        )
        self.assertNotIn("Within the decoded content", str(error))
        self.assertEqual(list(error.inner_path), [])


class TestEdgeCases(TestCase):
    def setUp(self):
        self.Validator = enable_content_validation(Draft202012Validator)

    def test_empty_string_passes_encoding_but_fails_json_parsing(self):
        self.assertEqual(
            list(
                self.Validator(
                    {"contentEncoding": "base64"},
                ).iter_errors(""),
            ),
            [],
        )
        errors = list(
            self.Validator(
                {
                    "contentEncoding": "base64",
                    "contentMediaType": "application/json",
                },
            ).iter_errors(""),
        )
        self.assertIsInstance(errors[0], ContentMediaTypeError)

    def test_strict_base64_rejects_whitespace_and_urlsafe_alphabets(self):
        schema = {"contentEncoding": "base64"}
        for payload in ("eyJmb28iOiAiYmFyIn0K\n", "a-b-"):
            with self.subTest(payload=payload):
                errors = list(self.Validator(schema).iter_errors(payload))
                self.assertIsInstance(errors[0], ContentEncodingError)

    def test_encoded_json_null_and_numbers_are_supported(self):
        for raw, content_schema in (
            (b"null", {"type": "null"}),
            (b"42", {"type": "integer"}),
        ):
            with self.subTest(raw=raw):
                schema = {
                    "contentEncoding": "base64",
                    "contentMediaType": "application/json",
                    "contentSchema": content_schema,
                }
                payload = base64.b64encode(raw).decode("ascii")
                self.assertEqual(
                    list(self.Validator(schema).iter_errors(payload)),
                    [],
                )

    def test_boolean_schemas_are_unaffected(self):
        self.assertEqual(
            list(self.Validator(True).iter_errors("anything")),
            [],
        )
        errors = list(self.Validator(False).iter_errors("anything"))
        self.assertEqual(len(errors), 1)
        self.assertNotIsInstance(errors[0], ContentValidationError)

    def test_error_tree_uses_the_outer_path_only(self):
        from jsonschema.exceptions import ErrorTree

        schema = {
            "type": "object",
            "properties": {
                "payload": {
                    "contentMediaType": "application/json",
                    "contentSchema": {
                        "type": "object",
                        "properties": {"foo": {"type": "integer"}},
                    },
                },
            },
        }
        tree = ErrorTree(
            self.Validator(schema).iter_errors(
                {"payload": '{"foo": "x"}'},
            ),
        )
        self.assertEqual(list(tree), ["payload"])
        self.assertEqual(
            list(tree["payload"].errors),
            ["contentSchema"],
        )

    def test_pipeline_short_circuits_before_the_inner_stages(self):
        # A broken encoding must never surface a media type or schema error.
        schema = {
            "contentEncoding": "base64",
            "contentMediaType": "application/json",
            "contentSchema": {"type": "object"},
        }
        errors = list(self.Validator(schema).iter_errors("not base64!"))
        self.assertEqual(
            [type(error) for error in errors],
            [ContentEncodingError],
        )

        # A valid encoding with unparseable content must never reach the
        # content schema stage.
        errors = list(
            self.Validator(schema).iter_errors(
                base64.b64encode(b"{:}").decode(),
            ),
        )
        self.assertEqual(
            [type(error) for error in errors],
            [ContentMediaTypeError],
        )
