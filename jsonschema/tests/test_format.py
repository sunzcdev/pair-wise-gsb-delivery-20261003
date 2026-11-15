"""
Tests for the parts of jsonschema related to the :kw:`format` keyword.
"""

from unittest import mock
from unittest import TestCase

import referencing.jsonschema
from referencing import Registry, Resource

from jsonschema import (
    Draft202012Validator,
    FormatChecker,
    SchemaError,
    ValidationError,
)
from jsonschema import _format
from jsonschema.exceptions import FormatCapabilityError, FormatError
from jsonschema.validators import Draft4Validator

BOOM = ValueError("Boom!")
BANG = ZeroDivisionError("Bang!")


def boom(thing):
    if thing == "bang":
        raise BANG
    raise BOOM


class TestFormatChecker(TestCase):
    def test_it_can_validate_no_formats(self):
        checker = FormatChecker(formats=())
        self.assertFalse(checker.checkers)

    def test_it_raises_a_key_error_for_unknown_formats(self):
        with self.assertRaises(KeyError):
            FormatChecker(formats=["o noes"])

    def test_it_can_register_cls_checkers(self):
        original = dict(FormatChecker.checkers)
        self.addCleanup(FormatChecker.checkers.pop, "boom")
        with self.assertWarns(DeprecationWarning):
            FormatChecker.cls_checks("boom")(boom)
        self.assertEqual(
            FormatChecker.checkers,
            dict(original, boom=(boom, ())),
        )

    def test_it_can_register_checkers(self):
        checker = FormatChecker()
        checker.checks("boom")(boom)
        self.assertEqual(
            checker.checkers,
            dict(FormatChecker.checkers, boom=(boom, ())),
        )

    def test_it_catches_registered_errors(self):
        checker = FormatChecker()
        checker.checks("boom", raises=type(BOOM))(boom)

        with self.assertRaises(FormatError) as cm:
            checker.check(instance=12, format="boom")

        self.assertIs(cm.exception.cause, BOOM)
        self.assertIs(cm.exception.__cause__, BOOM)
        self.assertEqual(str(cm.exception), "12 is not a 'boom'")

        # Unregistered errors should not be caught
        with self.assertRaises(type(BANG)):
            checker.check(instance="bang", format="boom")

    def test_format_error_causes_become_validation_error_causes(self):
        checker = FormatChecker()
        checker.checks("boom", raises=ValueError)(boom)
        validator = Draft4Validator({"format": "boom"}, format_checker=checker)

        with self.assertRaises(ValidationError) as cm:
            validator.validate("BOOM")

        self.assertIs(cm.exception.cause, BOOM)
        self.assertIs(cm.exception.__cause__, BOOM)

    def test_format_checkers_come_with_defaults(self):
        # This is bad :/ but relied upon.
        # The docs for quite awhile recommended people do things like
        # validate(..., format_checker=FormatChecker())
        # We should change that, but we can't without deprecation...
        checker = FormatChecker()
        with self.assertRaises(FormatError):
            checker.check(instance="not-an-ipv4", format="ipv4")

    def test_repr(self):
        checker = FormatChecker(formats=())
        checker.checks("foo")(lambda thing: True)  # pragma: no cover
        checker.checks("bar")(lambda thing: True)  # pragma: no cover
        checker.checks("baz")(lambda thing: True)  # pragma: no cover
        self.assertEqual(
            repr(checker),
            "<FormatChecker checkers=['bar', 'baz', 'foo']>",
        )

    def test_strict_formats_report_unknown_and_dependencies(self):
        checker = FormatChecker(formats=())
        schema = {
            "if": {"format": "emial"},
            "then": {"format": "duration"},
        }
        with mock.patch.object(
            _format,
            "_dependency_is_available",
            return_value=False,
        ):
            with self.assertRaises(FormatCapabilityError) as cm:
                checker.check_schema(schema)

        error = cm.exception
        self.assertIsInstance(error, SchemaError)
        self.assertEqual(
            set(error.unknown_formats),
            {"emial"},
        )
        self.assertEqual(
            error.missing_dependencies,
            {"duration": ("isoduration",)},
        )
        self.assertIn("'emial' (not registered", str(error))
        self.assertIn("'duration' (missing dependency", str(error))
        self.assertIn("#/if", str(error))
        self.assertIn("#/then", str(error))

    def test_annotation_only_mode_allows_unknown_formats(self):
        Draft202012Validator.check_schema(
            {"format": "emial"},
            format_checker=None,
            strict_formats=True,
        )

    def test_strict_formats_do_not_register_custom_formats_globally(self):
        original = dict(FormatChecker.checkers)
        checker = FormatChecker()
        checker.checks("custom")(lambda instance: True)

        Draft202012Validator.check_schema(
            {"format": "custom"},
            format_checker=checker,
            strict_formats=True,
        )

        self.assertEqual(FormatChecker.checkers, original)
        self.assertNotIn("custom", FormatChecker.checkers)

    def test_strict_formats_follow_referenced_resources(self):
        schema = {
            "properties": {
                "value": {"$ref": "https://example.com/remote"},
            },
        }
        registry = Registry().with_resource(
            "https://example.com/remote",
            Resource.from_contents(
                {"format": "emial"},
                default_specification=referencing.jsonschema.DRAFT202012,
            ),
        )

        with self.assertRaises(FormatCapabilityError) as cm:
            Draft202012Validator.check_schema(
                schema,
                strict_formats=True,
                registry=registry,
            )

        self.assertIn("emial", str(cm.exception))
        self.assertEqual(
            cm.exception.missing[0].resource,
            "https://example.com/remote",
        )
