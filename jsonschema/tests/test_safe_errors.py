from unittest import TestCase
from typing import Any
import json

from jsonschema import Draft202012Validator, exceptions


def assert_no_leak(test, value, *secrets):
    text = json.dumps(value, ensure_ascii=False, default=repr)
    for secret in secrets:
        test.assertNotIn(secret, text)


class TestSafeErrorSerialization(TestCase):
    def password_error(self, instance, schema=None):
        if schema is None:
            schema = {
                "type": "object",
                "properties": {
                    "password": {"type": "integer"},
                    "name": {"type": "string"},
                },
            }
        errors = list(Draft202012Validator(schema).iter_errors(instance))
        return errors[0]

    def test_field_name_redacts_message_instance_and_keyword_value(self):
        error = self.password_error({"password": "s3cr3t", "name": 3})
        result = exceptions.serialize_error(
            error, sensitive_fields={"password"}
        )

        self.assertEqual(result["validator"], "type")
        self.assertEqual(result["keyword"], "type")
        self.assertEqual(result["json_path"], "$.password")
        self.assertEqual(result["instance"], "***")
        self.assertEqual(result["validator_value"], "***")
        self.assertEqual(result["reason_category"], "type")
        self.assertTrue(result["redacted"])
        self.assertNotIn("s3cr3t", result["message"])
        self.assertIn("integer", result["message"])
        assert_no_leak(self, result, "s3cr3t")

    def test_schema_path_policy_redacts_nested_fields(self):
        schema = {
            "type": "object",
            "properties": {
                "users": {
                    "type": "array",
                    "items": {
                        "type": "object",
                        "properties": {"password": {"type": "integer"}},
                    },
                },
            },
        }
        error = self.password_error(
            {"users": [{"password": "nested-secret"}]}, schema
        )
        result = exceptions.serialize_error(
            error,
            sensitive_schema_paths=[
                ("properties", "users", "items", "properties", "password"),
            ],
        )

        self.assertEqual(result["json_path"], "$.users[0].password")
        self.assertEqual(result["instance"], "***")
        assert_no_leak(self, result, "nested-secret")

    def test_json_pointer_and_json_path_policies(self):
        error = self.password_error({"password": "pointer-secret"})
        pointer = exceptions.serialize_error(
            error, sensitive_schema_paths="/properties/password"
        )
        path = exceptions.serialize_error(
            error, sensitive_paths="$.password"
        )
        for result in (pointer, path):
            assert_no_leak(self, result, "pointer-secret")
            self.assertEqual(result["json_path"], "$.password")

    def test_field_policy_forms(self):
        schema = {
            "type": "object",
            "properties": {
                "account": {
                    "type": "object",
                    "properties": {"api_token": {"type": "integer"}},
                },
                "password": {"type": "integer"},
            },
        }
        errors = list(
            Draft202012Validator(schema).iter_errors(
                {
                    "account": {"api_token": "token-secret"},
                    "password": "pw-secret",
                },
            ),
        )
        account_error = next(
            e for e in errors if list(e.path) == ["account", "api_token"]
        )
        password_error = next(
            e for e in errors if list(e.path) == ["password"]
        )

        callable_result = exceptions.serialize_error(
            account_error,
            field_policy=lambda name, path, value: name.endswith("_token"),
        )
        mapping_result = exceptions.serialize_error(
            password_error, field_policy={"password": True}
        )
        policy_result = exceptions.serialize_error(
            password_error,
            policy=exceptions.RedactionPolicy(
                sensitive_fields={"password"}
            ),
        )
        for result in (callable_result, mapping_result, policy_result):
            self.assertEqual(result["instance"], "***")
        assert_no_leak(
            self, callable_result, "token-secret", "pw-secret"
        )
        assert_no_leak(
            self, mapping_result, "token-secret", "pw-secret"
        )

    def test_nested_mapping_field_policy(self):
        schema = {
            "type": "object",
            "properties": {
                "account": {
                    "type": "object",
                    "properties": {"password": {"type": "integer"}},
                },
            },
        }
        error = next(
            iter(
                Draft202012Validator(schema).iter_errors(
                    {"account": {"password": "nested-policy-secret"}},
                ),
            ),
        )
        result = exceptions.serialize_error(
            error, field_policy={"account": {"password": True}}
        )
        self.assertEqual(result["instance"], "***")
        assert_no_leak(self, result, "nested-policy-secret")

    def test_parent_message_and_repr_do_not_carry_child_secret(self):
        schema = {
            "anyOf": [
                {
                    "type": "object",
                    "properties": {
                        "password": {"type": "integer"},
                        "name": {"type": "string"},
                    },
                },
                {
                    "type": "object",
                    "properties": {
                        "password": {"type": "boolean"},
                        "name": {"type": "string"},
                    },
                },
            ],
        }
        error = self.password_error(
            {"password": "parent-secret", "name": 3}, schema
        )
        result = exceptions.serialize_error(
            error, sensitive_fields={"password"}
        )

        self.assertIn("password", result["instance"])
        self.assertEqual(result["instance"]["password"], "***")
        self.assertIn("is not valid under", result["message"])
        assert_no_leak(self, result, "parent-secret")
        self.assertEqual(result["context"][0]["parent"]["json_path"], "$")
        self.assertNotIn("message", result["context"][0]["parent"])

    def test_original_error_is_not_modified(self):
        error = self.password_error({"password": "original-secret"})
        original_message = error.message
        original_instance = error.instance
        original_context = list(error.context)
        best = exceptions.best_match([error])

        result = exceptions.serialize_error(
            error, sensitive_fields={"password"}
        )

        self.assertEqual(error.message, original_message)
        self.assertIs(error.instance, original_instance)
        self.assertEqual(error.context, original_context)
        self.assertIn("original-secret", error.message)
        self.assertIn("original-secret", str(error))
        self.assertIn("original-secret", repr(error))
        self.assertEqual(best.message, original_message)
        self.assertNotIn("original-secret", json.dumps(result))

    def test_schema_annotations_are_redacted(self):
        error = exceptions.ValidationError(
            "a secret default",
            validator="type",
            validator_value="string",
            instance="schema-secret",
            schema={
                "type": "string",
                "default": "schema-secret",
                "examples": ["schema-secret"],
            },
            path=["password"],
            schema_path=["properties", "password", "type"],
        )
        result = exceptions.serialize_error(
            error,
            sensitive_schema_paths=[("properties", "password")],
        )

        self.assertEqual(result["schema"]["type"], "string")
        self.assertEqual(result["schema"]["default"], "***")
        self.assertEqual(result["schema"]["examples"], "***")
        assert_no_leak(self, result, "schema-secret")

    def test_const_and_enum_values_are_redacted(self):
        schema = {
            "type": "object",
            "properties": {
                "password": {"const": "const-secret"},
                "token": {"enum": ["enum-secret", "ok"]},
            },
        }
        errors = list(
            Draft202012Validator(schema).iter_errors(
                {"password": "wrong-password", "token": "wrong-token"}
            ),
        )
        self.assertEqual(len(errors), 2)
        for error in errors:
            result = exceptions.serialize_error(
                error, sensitive_fields={"password", "token"}
            )
            self.assertEqual(result["instance"], "***")
            self.assertEqual(result["validator_value"], "***")
            assert_no_leak(
                self,
                result,
                "const-secret",
                "enum-secret",
                "wrong-password",
                "wrong-token",
            )

    def test_unknown_custom_exception_uses_explicit_summary(self):
        class Unknown(Exception):
            def __str__(self):
                raise AssertionError("must not stringify unknown errors")

            def __repr__(self):
                raise AssertionError("must not repr unknown errors")

        result = exceptions.serialize_error(Unknown())
        self.assertEqual(result["category"], "unknown")
        self.assertEqual(result["reason"], "unknown")
        self.assertIn("Unrecognized exception type", result["summary"])
        self.assertIn("Unknown", result["summary"])
        self.assertEqual(result["context"], [])

    def test_unknown_custom_exception_in_context_is_summarized(self):
        class Unknown(Exception):
            def __str__(self):
                return "context-secret"

        error = exceptions.ValidationError("parent", context=[Unknown()])
        result = exceptions.serialize_error(error)
        self.assertEqual(result["context"][0]["category"], "unknown")
        self.assertIn(
            "Unrecognized exception type", result["context"][0]["summary"]
        )
        assert_no_leak(self, result, "context-secret")

    def test_cause_is_summarized_not_expanded(self):
        class Cause(Exception):
            def __str__(self):
                return "cause-secret"

            def __repr__(self):
                return "cause-secret-repr"

        error = exceptions.ValidationError(
            "failed",
            validator="type",
            validator_value="integer",
            instance=1,
            schema={"type": "integer"},
            cause=Cause(),
        )
        result = exceptions.serialize_error(
            error, sensitive_fields={"password"}
        )
        cause = result["cause"]
        self.assertEqual(cause["category"], "unknown")
        self.assertIn("Unrecognized exception type", cause["summary"])
        self.assertIn("Cause", cause["summary"])
        assert_no_leak(self, result, "cause-secret")

    def test_cause_can_be_omitted(self):
        error = exceptions.ValidationError("failed", cause=ValueError("x"))
        result = exceptions.serialize_error(error, include_cause=False)
        self.assertIsNone(result["cause"])

    def test_raised_from_cause_is_summarized(self):
        try:
            try:
                raise ValueError("raised-from-secret")
            except ValueError as cause:
                raise exceptions.ValidationError("failed") from cause
        except exceptions.ValidationError as error:
            result = exceptions.serialize_error(error)
        self.assertEqual(result["cause"]["category"], "python")
        self.assertIn("ValueError", result["cause"]["summary"])
        assert_no_leak(self, result, "raised-from-secret")

    def test_limits_mark_truncation(self):
        schema = {
            "anyOf": [
                {"type": "string"},
                {"type": "array", "items": {"type": "string"}},
            ],
        }
        error = self.password_error({"nested": [1, 2, 3, 4, 5, 6]}, schema)
        result = exceptions.serialize_error(
            error,
            max_depth=1,
            max_length=24,
            max_items=2,
            max_context=1,
        )

        self.assertTrue(result["truncated"])
        for reason in ("depth", "items", "length", "context"):
            self.assertIn(reason, result["truncation"])
        self.assertIn("truncated", json.dumps(result))

    def test_size_limit_is_explicit(self):
        error = self.password_error({"password": "size-secret"})
        result = exceptions.serialize_error(
            error, sensitive_fields={"password"}, max_bytes=160
        )
        dumped = json.dumps(
            result, ensure_ascii=False, separators=(",", ":")
        )
        self.assertTrue(result["truncated"])
        self.assertIn("size", result["truncation"])
        self.assertLessEqual(len(dumped.encode("utf-8")), 160)
        assert_no_leak(self, result, "size-secret")

    def test_impossible_size_limit_still_yields_valid_json(self):
        error = self.password_error({"password": "tiny-secret"})
        text = exceptions.dump_error(
            error, sensitive_fields={"password"}, max_bytes=8
        )
        self.assertIsInstance(json.loads(text), dict)
        assert_no_leak(self, json.loads(text), "tiny-secret")

    def test_json_dump_and_aliases(self):
        error = self.password_error({"password": "dump-secret"})
        text = exceptions.dump_error(error, sensitive_fields={"password"})
        loaded = json.loads(text)
        self.assertEqual(loaded["validator"], "type")
        assert_no_leak(self, loaded, "dump-secret")

        for alias in (
            exceptions.safe_error,
            exceptions.safe_error_dict,
            exceptions.safe_serialize,
            exceptions.redact_error,
        ):
            result = alias(error, sensitive_fields={"password"})
            assert_no_leak(self, result, "dump-secret")

        threaded = exceptions.safe_error_json(
            error, sensitive_fields={"password"}
        )
        assert_no_leak(self, json.loads(threaded), "dump-secret")

    def test_error_methods_do_not_mutate(self):
        error = self.password_error({"password": "method-secret"})
        result = error.to_safe_dict(sensitive_fields={"password"})
        self.assertEqual(result["instance"], "***")
        assert_no_leak(self, result, "method-secret")
        self.assertIn("method-secret", error.message)

    def test_cyclic_instance_is_bounded(self):
        instance = {"self": None}
        instance["self"] = instance
        error = exceptions.ValidationError(
            "a cycle",
            validator="type",
            validator_value="object",
            instance=instance,
            schema={"type": "object"},
        )
        result = exceptions.serialize_error(error, max_depth=4, max_items=2)
        self.assertTrue(result["truncated"])
        self.assertIn("cycle", result["truncation"])

    def test_unknown_values_use_type_summaries(self):
        class SensitiveObject:
            def __repr__(self):
                return "unknown-object-secret"

        error = exceptions.ValidationError(
            "bad object",
            validator="type",
            validator_value="object",
            instance=SensitiveObject(),
            schema={"type": "object"},
        )
        result = exceptions.serialize_error(error)
        self.assertEqual(result["instance"], "<SensitiveObject object>")
        assert_no_leak(self, result, "unknown-object-secret")

    def test_output_is_stable_json(self):
        error = exceptions.ValidationError(
            "odd keys",
            validator="type",
            validator_value="integer",
            instance={
                1: "int-key",
                1.0: "float-key",
                None: "none-key",
                (1, 2): "tuple-key",
                b"k": "bytes-key",
                float("nan"): "nan-key",
            },
            schema={"type": "integer"},
        )
        result = exceptions.serialize_error(error)
        self.assertEqual(json.loads(json.dumps(result)), result)
        self.assertNotIn("NaN", json.dumps(result))
        self.assertNotIn("Infinity", json.dumps(result))
        self.assertIn("<tuple key>", result["instance"])
        self.assertIn("<nan>", result["instance"])

    def test_nan_and_infinity_are_not_written_as_bare_literals(self):
        error = exceptions.ValidationError(
            "numbers",
            validator="type",
            validator_value="integer",
            instance={"nan": float("nan"), "inf": float("inf")},
            schema={"type": "integer"},
        )
        text = exceptions.dump_error(error)
        self.assertNotIn("NaN", text)
        self.assertIn("<nan>", text)
        json.loads(text)


    def test_recursive_error_context_is_bounded(self):
        error = exceptions.ValidationError("outer")
        error.context.append(error)
        result = exceptions.serialize_error(error, max_depth=2)
        self.assertTrue(result["truncated"])
        self.assertIn("cycle", result["truncation"])

    def test_deep_inputs_do_not_raise_recursion_errors(self):
        outer = exceptions.ValidationError(
            "outer",
            validator="type",
            validator_value="string",
            instance="outer-secret",
            schema={"type": "string"},
            path=["password"],
        )
        current = outer
        for index in range(1000):
            child = exceptions.ValidationError(
                f"child {index}",
                validator="type",
                validator_value="string",
                instance=f"child-secret-{index}",
                schema={"type": "string"},
                path=["password"],
            )
            current.context = [child]
            current = child
        result = exceptions.serialize_error(
            outer,
            sensitive_fields={"password"},
            max_depth=None,
            max_context=None,
        )
        self.assertTrue(result["truncated"])
        self.assertIn("depth", result["truncation"])
        assert_no_leak(self, result, "outer-secret", "child-secret")

        nested = {}
        current_value = nested
        for index in range(1000):
            child_value: dict[str, Any] = {}
            current_value[str(index)] = child_value
            current_value = child_value
        error = exceptions.ValidationError(
            "deep",
            validator="type",
            validator_value="object",
            instance=nested,
            schema={"type": "object"},
        )
        deep_result = exceptions.serialize_error(
            error, max_depth=None, max_items=None
        )
        self.assertTrue(deep_result["truncated"])
        self.assertIn("depth", deep_result["truncation"])

    def test_unknown_alias_is_rejected(self):
        error = exceptions.ValidationError("a message")
        with self.assertRaises(TypeError):
            exceptions.serialize_error(error, nonsense=1)

    def test_aliases_are_honored(self):
        error = exceptions.ValidationError(
            "secret here", path=["password"], instance="a-secret"
        )
        result = exceptions.serialize_error(
            error, fields={"password"}, depth=1
        )
        self.assertEqual(result["instance"], "***")
        self.assertNotIn("a-secret", json.dumps(result))
        self.assertFalse(result["truncated"])

    def test_field_names_can_be_masked_with_locations_kept(self):
        error = exceptions.ValidationError(
            "an error",
            validator="type",
            validator_value="object",
            instance={"password": "name-secret", "name": "kept"},
            schema={"type": "object"},
        )
        result = exceptions.serialize_error(
            error,
            sensitive_fields={"password"},
            redact_field_names=True,
        )
        self.assertEqual(result["instance"], {"***": "***", "name": "kept"})
        self.assertEqual(result["json_path"], "$")
        assert_no_leak(self, result, "name-secret")

    def test_nested_context_errors_keep_keyword_and_location(self):
        schema = {
            "anyOf": [
                {"type": "object", "properties": {"a": {"type": "string"}}},
                {"type": "object", "properties": {"b": {"type": "string"}}},
            ],
        }
        error = self.password_error({"a": 1, "b": 2}, schema)
        result = exceptions.serialize_error(error, max_items=None)
        self.assertEqual(result["keyword"], "anyOf")
        self.assertEqual(result["reason_category"], "composition")
        for child in result["context"]:
            self.assertIsNotNone(child["keyword"])
            self.assertTrue(child["json_path"].startswith("$"))
            self.assertIsNotNone(child["reason_category"])

    def test_values_nested_inside_a_sensitive_field_are_masked(self):
        schema = {
            "type": "object",
            "properties": {
                "password": {
                    "type": "array",
                    "items": {"type": "integer"},
                },
                "deep": {
                    "type": "object",
                    "properties": {"password": {"type": "integer"}},
                },
            },
        }
        nested_array = next(
            iter(
                Draft202012Validator(schema).iter_errors(
                    {"password": ["array-secret"]}
                ),
            ),
        )
        nested_object = next(
            iter(
                Draft202012Validator(schema).iter_errors(
                    {"deep": {"password": "object-secret"}}
                ),
            ),
        )
        for error, secret in (
            (nested_array, "array-secret"),
            (nested_object, "object-secret"),
        ):
            result = exceptions.serialize_error(
                error, sensitive_fields={"password"}
            )
            self.assertEqual(result["instance"], "***")
            # The keyword and the location survive the masking.
            self.assertEqual(result["keyword"], "type")
            self.assertIn("password", result["json_path"])
            assert_no_leak(self, result, secret)

    def test_schema_annotations_do_not_leak_through_messages(self):
        schema = {
            "type": "object",
            "properties": {
                "password": {"const": "annotation-secret"},
            },
        }
        error = next(
            iter(
                Draft202012Validator(schema).iter_errors(
                    {"password": "wrong"}
                ),
            ),
        )
        result = exceptions.serialize_error(
            error, sensitive_fields={"password"}
        )
        assert_no_leak(self, result, "annotation-secret", "wrong")

    def test_schema_error_is_distinguished(self):
        error = exceptions.SchemaError(
            "a schema error", validator="type", validator_value="integer"
        )
        result = exceptions.serialize_error(error)
        self.assertEqual(result["category"], "schema")
        self.assertEqual(result["reason_category"], "type")

        bare = exceptions.SchemaError("a bare schema error")
        bare_result = exceptions.serialize_error(bare)
        self.assertEqual(bare_result["category"], "schema")
        self.assertEqual(bare_result["reason_category"], "schema")

    def test_policy_can_be_extended_per_call(self):
        policy = exceptions.RedactionPolicy(
            sensitive_fields={"password"}, mask="[hidden]"
        )
        extended = policy.with_(sensitive_paths="$.token")
        self.assertEqual(extended.mask, "[hidden]")
        self.assertTrue(extended.is_sensitive_field("password", (), None))
        self.assertTrue(extended.is_sensitive_path(("a", "token")))

        error = exceptions.ValidationError(
            "an error",
            validator="type",
            validator_value="integer",
            instance={"password": "pw-secret", "token": "tok-secret"},
            schema={"type": "integer"},
        )
        result = exceptions.serialize_error(error, policy=policy)
        self.assertEqual(
            result["instance"],
            {"password": "[hidden]", "token": "tok-secret"},
        )
        assert_no_leak(self, result, "pw-secret")

        combined = exceptions.serialize_error(
            error, policy=policy, sensitive_fields={"token"}
        )
        self.assertEqual(
            combined["instance"],
            {"password": "[hidden]", "token": "[hidden]"},
        )
        assert_no_leak(self, combined, "pw-secret", "tok-secret")

    def test_context_can_be_omitted(self):
        error = exceptions.ValidationError(
            "parent",
            context=[exceptions.ValidationError("child-secret")],
        )
        result = exceptions.serialize_error(error, include_context=False)
        self.assertEqual(result["context"], [])
        assert_no_leak(self, result, "child-secret")

    def test_other_jsonschema_exceptions_are_handled(self):
        unknown = exceptions.UnknownType(
            "custom", instance={"a": 1}, schema={"type": "custom"}
        )
        result = exceptions.serialize_error(unknown, max_items=1)
        self.assertEqual(result["reason_category"], "type")
        self.assertEqual(result["validator"], "type")
        self.assertEqual(result["json_path"], "$")

        undefined = exceptions.UndefinedTypeCheck("widget")
        undefined_result = exceptions.serialize_error(undefined)
        self.assertEqual(undefined_result["reason_category"], "type")
        self.assertIn("widget", undefined_result["message"])

        format_error = exceptions.FormatError("a format failed")
        format_result = exceptions.serialize_error(format_error)
        self.assertEqual(format_result["reason_category"], "format")
        self.assertEqual(format_result["message"], "a format failed")

    def test_json_dump_has_no_arbitrary_reprs(self):
        class Cause(Exception):
            def __repr__(self):  # pragma: no cover
                raise AssertionError("must not repr causes")

        error = exceptions.ValidationError(
            "failed",
            instance={"a": object()},
            cause=Cause(),
        )
        text = exceptions.dump_error(error, sensitive_fields={"a"})
        self.assertIn("Unrecognized exception type", text)
        self.assertNotIn("object at 0x", text)
