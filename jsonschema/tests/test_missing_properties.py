from collections import deque
from unittest import TestCase

from jsonschema import (
    Draft7Validator,
    Draft202012Validator,
    _missing_properties,
    exceptions,
)
from jsonschema.exceptions import (
    MissingProperty,
    best_match,
    escape_json_pointer,
    iter_missing_properties,
    project_missing_properties,
    to_json_pointer,
)


class TestJsonPointerEscaping(TestCase):
    def test_escape_tilde_before_slash(self):
        # The tilde must be escaped first per RFC 6901.
        self.assertEqual(escape_json_pointer("a~b/c"), "a~0b~1c")
        self.assertEqual(escape_json_pointer("~"), "~0")
        self.assertEqual(escape_json_pointer("/"), "~1")
        self.assertEqual(escape_json_pointer("~/"), "~0~1")
        self.assertEqual(escape_json_pointer("/~"), "~1~0")
        self.assertEqual(escape_json_pointer("~0"), "~00")
        self.assertEqual(escape_json_pointer(""), "")
        self.assertEqual(escape_json_pointer("plain"), "plain")

    def test_to_json_pointer_root(self):
        self.assertEqual(to_json_pointer([]), "")
        self.assertEqual(to_json_pointer(()), "")

    def test_to_json_pointer_mixed_path(self):
        pointer = to_json_pointer(["a", 0, "b/c", "d~e"])
        self.assertEqual(pointer, "/a/0/b~1c/d~0e")

    def test_string_key_that_looks_like_an_index_is_not_confused(self):
        # Both render the same pointer segment, but the original path keeps
        # the int and the string distinct.
        self.assertEqual(to_json_pointer([0]), "/0")
        self.assertEqual(to_json_pointer(["0"]), "/0")

        int_error = exceptions.ValidationError(
            "'x' is a required property",
            validator="required",
            path=deque([0]),
            instance=[{}],
        )
        str_error = exceptions.ValidationError(
            "'x' is a required property",
            validator="required",
            path=deque(["0"]),
            instance={"0": {}},
        )
        _missing_properties.mark_missing_property(int_error, name="x")
        _missing_properties.mark_missing_property(str_error, name="x")

        (int_path,) = iter_missing_properties([int_error])
        (str_path,) = iter_missing_properties([str_error])
        self.assertEqual(int_path.parent_path, (0,))
        self.assertEqual(str_path.parent_path, ("0",))
        self.assertIsInstance(int_path.parent_path[0], int)
        self.assertIsInstance(str_path.parent_path[0], str)


class TestRequiredProjection(TestCase):
    def test_single_required_property(self):
        validator = Draft202012Validator({"required": ["foo"]})
        (error,) = validator.iter_errors({})

        (branch,) = project_missing_properties([error])
        self.assertEqual(branch.branch, ())
        self.assertIsNone(branch.keyword)
        self.assertIsNone(branch.index)
        (missing,) = branch.properties

        self.assertIsInstance(missing, MissingProperty)
        self.assertEqual(missing.name, "foo")
        self.assertIsNone(missing.trigger)
        self.assertEqual(missing.validator, "required")
        self.assertEqual(missing.parent_path, ())
        self.assertEqual(missing.parent_pointer, "")
        self.assertEqual(missing.pointer, "/foo")
        self.assertIs(missing.error, error)

    def test_multiple_required_properties_are_individually_addressable(self):
        validator = Draft202012Validator({"required": ["foo", "bar", "baz"]})
        errors = list(validator.iter_errors({}))

        missing = list(iter_missing_properties(errors))
        self.assertEqual(
            [each.name for each in missing],
            ["foo", "bar", "baz"],
        )
        self.assertEqual(
            [each.pointer for each in missing],
            ["/foo", "/bar", "/baz"],
        )
        # one entry per original error, preserving the correspondence
        self.assertEqual(
            [each.error for each in missing],
            errors,
        )

    def test_nested_required_uses_the_parent_object_location(self):
        schema = {
            "properties": {
                "outer": {
                    "properties": {
                        "inner": {"required": ["deep"]},
                    },
                },
            },
        }
        validator = Draft202012Validator(schema)
        errors = list(
            validator.iter_errors({"outer": {"inner": {}}}),
        )

        (missing,) = iter_missing_properties(errors)
        self.assertEqual(missing.name, "deep")
        self.assertEqual(
            missing.parent_path,
            ("outer", "inner"),
        )
        self.assertEqual(missing.parent_pointer, "/outer/inner")
        self.assertEqual(missing.pointer, "/outer/inner/deep")

    def test_required_inside_array_keeps_integer_index(self):
        schema = {"items": {"required": ["foo"]}}
        validator = Draft202012Validator(schema)
        errors = list(validator.iter_errors([{}, {}]))

        missing = list(iter_missing_properties(errors))
        self.assertEqual([each.parent_path for each in missing], [(0,), (1,)])
        self.assertTrue(
            all(isinstance(each.parent_path[0], int) for each in missing),
        )
        self.assertEqual(
            [each.pointer for each in missing],
            ["/0/foo", "/1/foo"],
        )

    def test_escaped_property_names(self):
        validator = Draft202012Validator(
            {"required": ["a/b", "m~n", "~1", "x/y~z"]},
        )
        errors = list(validator.iter_errors({}))

        missing = list(iter_missing_properties(errors))
        self.assertEqual(
            [each.name for each in missing],
            ["a/b", "m~n", "~1", "x/y~z"],
        )
        self.assertEqual(
            [each.pointer for each in missing],
            ["/a~1b", "/m~0n", "/~01", "/x~1y~0z"],
        )

    def test_does_not_fabricate_a_path_to_an_existing_member(self):
        error = exceptions.ValidationError(
            "'present' is a required property",
            validator="required",
            instance={"present": 1},
        )
        _missing_properties.mark_missing_property(error, name="present")
        self.assertEqual(project_missing_properties([error]), [])
        self.assertEqual(list(iter_missing_properties([error])), [])


class TestDependentRequiredProjection(TestCase):
    def test_triggering_dependency_is_recorded(self):
        schema = {"dependentRequired": {"foo": ["bar"]}}
        validator = Draft202012Validator(schema)
        errors = list(validator.iter_errors({"foo": 1}))

        (missing,) = iter_missing_properties(errors)
        self.assertEqual(missing.name, "bar")
        self.assertEqual(missing.trigger, "foo")
        self.assertEqual(missing.validator, "dependentRequired")
        self.assertEqual(missing.pointer, "/bar")

    def test_multiple_dependencies(self):
        schema = {"dependentRequired": {"foo": ["bar", "baz"]}}
        validator = Draft202012Validator(schema)
        errors = list(validator.iter_errors({"foo": 1}))

        missing = list(iter_missing_properties(errors))
        self.assertEqual(
            [(each.name, each.trigger) for each in missing],
            [("bar", "foo"), ("baz", "foo")],
        )

    def test_no_error_when_trigger_is_absent(self):
        schema = {"dependentRequired": {"foo": ["bar"]}}
        validator = Draft202012Validator(schema)
        self.assertEqual(list(validator.iter_errors({})), [])

    def test_dependency_under_nested_path(self):
        schema = {
            "properties": {
                "thing": {
                    "dependentRequired": {"foo": ["bar"]},
                },
            },
        }
        validator = Draft202012Validator(schema)
        errors = list(
            validator.iter_errors({"thing": {"foo": 1}}),
        )
        (missing,) = iter_missing_properties(errors)
        self.assertEqual(missing.parent_path, ("thing",))
        self.assertEqual(missing.parent_pointer, "/thing")
        self.assertEqual(missing.name, "bar")
        self.assertEqual(missing.trigger, "foo")
        self.assertEqual(missing.pointer, "/thing/bar")


class TestLegacyDrafts(TestCase):
    def test_draft7_dependencies_array_form(self):
        schema = {"dependencies": {"foo": ["bar", "baz"]}}
        validator = Draft7Validator(schema)
        errors = list(validator.iter_errors({"foo": 1}))

        missing = list(iter_missing_properties(errors))
        self.assertEqual(
            [(each.name, each.trigger) for each in missing],
            [("bar", "foo"), ("baz", "foo")],
        )
        self.assertEqual(
            [each.validator for each in missing],
            ["dependencies", "dependencies"],
        )

    def test_draft7_required(self):
        validator = Draft7Validator({"required": ["foo", "a/b"]})
        errors = list(validator.iter_errors({}))
        missing = list(iter_missing_properties(errors))
        self.assertEqual(
            [each.pointer for each in missing],
            ["/foo", "/a~1b"],
        )

    def test_allOf_errors_form_a_single_root_group(self):
        # allOf does not wrap its sub-errors in a context, so the errors it
        # produces are projected into a single root-level group.
        schema = {"allOf": [{"required": ["a"]}, {"required": ["b"]}]}
        validator = Draft202012Validator(schema)
        errors = list(validator.iter_errors({}))

        (group,) = project_missing_properties(errors)
        self.assertEqual(group.branch, ())
        self.assertEqual(
            [each.name for each in group.properties],
            ["a", "b"],
        )


class TestBranchGrouping(TestCase):
    def test_anyOf_groups_per_subschema(self):
        schema = {
            "anyOf": [
                {"required": ["a"]},
                {
                    "required": ["b"],
                    "dependentRequired": {"present": ["c"]},
                },
            ],
        }
        validator = Draft202012Validator(schema)
        errors = list(validator.iter_errors({"present": 1}))

        branches = project_missing_properties(errors)
        self.assertEqual(len(branches), 2)
        self.assertEqual(
            [branch.branch for branch in branches],
            [(("anyOf", 0),), (("anyOf", 1),)],
        )

        first, second = branches
        self.assertEqual([p.name for p in first.properties], ["a"])
        self.assertEqual(
            [(p.name, p.trigger) for p in second.properties],
            [("b", None), ("c", "present")],
        )
        self.assertEqual(first.keyword, "anyOf")
        self.assertEqual(first.index, 0)
        self.assertEqual(second.index, 1)

    def test_oneOf_groups_per_subschema(self):
        schema = {
            "oneOf": [
                {"required": ["a"]},
                {"required": ["b"]},
            ],
        }
        validator = Draft202012Validator(schema)
        errors = list(validator.iter_errors({}))

        branches = project_missing_properties(errors)
        self.assertEqual(
            [branch.branch for branch in branches],
            [(("oneOf", 0),), (("oneOf", 1),)],
        )

    def test_nested_applicators_produce_full_branch_paths(self):
        schema = {
            "anyOf": [
                {
                    "oneOf": [
                        {"required": ["deep"]},
                    ],
                },
            ],
        }
        validator = Draft202012Validator(schema)
        errors = list(validator.iter_errors({}))

        (branch,) = project_missing_properties(errors)
        self.assertEqual(
            branch.branch,
            (("anyOf", 0), ("oneOf", 0)),
        )
        (missing,) = branch.properties
        self.assertEqual(missing.name, "deep")

    def test_root_and_branch_errors_are_separate_groups(self):
        schema = {
            "required": ["top"],
            "anyOf": [{"required": ["inner"]}],
        }
        validator = Draft202012Validator(schema)
        errors = list(validator.iter_errors({}))

        branches = project_missing_properties(errors)
        self.assertEqual(len(branches), 2)
        root = next(b for b in branches if b.branch == ())
        nested = next(b for b in branches if b.branch != ())
        self.assertEqual([p.name for p in root.properties], ["top"])
        self.assertEqual(
            [p.name for p in nested.properties],
            ["inner"],
        )

    def test_branch_is_a_frozen_value_object(self):
        from attrs.exceptions import FrozenInstanceError

        schema = {"anyOf": [{"required": ["a"]}]}
        validator = Draft202012Validator(schema)
        (branch,) = project_missing_properties(
            list(validator.iter_errors({})),
        )
        with self.assertRaises(FrozenInstanceError):
            branch.branch = ()  # type: ignore[misc]
        with self.assertRaises(FrozenInstanceError):
            branch.properties = ()  # type: ignore[misc]


class TestOriginalErrorsAreUnchanged(TestCase):
    def setUp(self):
        self.schema = {
            "required": ["foo", "bar"],
            "dependentRequired": {"foo": ["baz"]},
            "anyOf": [{"required": ["quux"]}],
        }
        self.instance = {"foo": 1}

    def test_iteration_messages_and_paths_are_unchanged(self):
        validator = Draft202012Validator(self.schema)
        before = list(validator.iter_errors(self.instance))
        expected = [(e.message, list(e.path), e.validator) for e in before]

        # Projecting must not mutate anything.
        project_missing_properties(before)
        list(iter_missing_properties(before))

        after = list(validator.iter_errors(self.instance))
        actual = [(e.message, list(e.path), e.validator) for e in after]
        self.assertEqual(actual, expected)

        # The original error's path still points at the parent object.
        required_error = next(e for e in after if e.validator == "required")
        self.assertEqual(list(required_error.path), [])

    def test_best_match_is_unchanged(self):
        validator = Draft202012Validator(self.schema)
        errors = list(validator.iter_errors(self.instance))
        expected = best_match(errors)

        projected = project_missing_properties(errors)
        flat = list(iter_missing_properties(errors))
        self.assertTrue(projected)
        self.assertTrue(flat)

        again = best_match(list(validator.iter_errors(self.instance)))
        self.assertEqual(again._contents(), expected._contents())

    def test_projection_does_not_add_context(self):
        validator = Draft202012Validator(self.schema)
        errors = list(validator.iter_errors(self.instance))
        contexts = [len(e.context) for e in errors]
        project_missing_properties(errors)
        self.assertEqual([len(e.context) for e in errors], contexts)


class TestHandConstructedErrors(TestCase):
    def test_required_message_fallback(self):
        error = exceptions.ValidationError(
            "'foo' is a required property",
            validator="required",
            instance={},
        )
        (missing,) = iter_missing_properties([error])
        self.assertEqual(missing.name, "foo")
        self.assertIsNone(missing.trigger)
        self.assertIs(missing.error, error)

    def test_dependent_required_message_fallback(self):
        error = exceptions.ValidationError(
            "'bar' is a dependency of 'foo'",
            validator="dependentRequired",
            instance={"foo": 1},
        )
        (missing,) = iter_missing_properties([error])
        self.assertEqual(missing.name, "bar")
        self.assertEqual(missing.trigger, "foo")

    def test_unrelated_errors_are_ignored(self):
        error = exceptions.ValidationError(
            "12 is not of type 'string'",
            validator="type",
            instance=12,
        )
        self.assertEqual(project_missing_properties([error]), [])

    def test_unparseable_required_message_is_ignored(self):
        error = exceptions.ValidationError(
            "something else entirely",
            validator="required",
            instance={},
        )
        self.assertEqual(list(iter_missing_properties([error])), [])

    def test_message_with_special_characters_round_trips(self):
        for name in ["a/b", "m~n", "it's", 'he said "hi"', "uni∂ode"]:
            message = f"{name!r} is a required property"
            error = exceptions.ValidationError(
                message,
                validator="required",
                instance={},
            )
            (missing,) = iter_missing_properties([error])
            self.assertEqual(missing.name, name)
            self.assertEqual(
                missing.pointer,
                "/" + escape_json_pointer(name),
            )

    def test_missing_instance_attribute_does_not_crash(self):
        error = exceptions.ValidationError(
            "'foo' is a required property",
            validator="required",
        )
        (missing,) = iter_missing_properties([error])
        self.assertEqual(missing.name, "foo")

    def test_mark_and_reproject(self):
        error = exceptions.ValidationError(
            "'foo' is a required property",
            validator="required",
            instance={},
        )
        result = _missing_properties.mark_missing_property(
            error,
            name="foo",
        )
        self.assertIs(result, error)
        (missing,) = iter_missing_properties([error])
        self.assertEqual(missing.name, "foo")


class TestEmptyInput(TestCase):
    def test_no_errors(self):
        self.assertEqual(project_missing_properties([]), [])
        self.assertEqual(list(iter_missing_properties(iter([]))), [])

    def test_valid_instance(self):
        validator = Draft202012Validator({"required": ["foo"]})
        errors = list(validator.iter_errors({"foo": 1}))
        self.assertEqual(errors, [])
        self.assertEqual(project_missing_properties(errors), [])
