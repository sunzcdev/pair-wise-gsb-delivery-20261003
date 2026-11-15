"""Tests for tuples of all kinds."""

from collections import namedtuple
from typing import Annotated, Generic, List, Literal, NamedTuple, Tuple, TypeVar

from attrs import Factory, define
from pytest import raises

from cattrs import Converter
from cattrs.cols import (
    is_namedtuple,
    namedtuple_dict_structure_factory,
    namedtuple_dict_unstructure_factory,
    namedtuple_structure_factory,
    namedtuple_unstructure_factory,
)
from cattrs.converters import BaseConverter
from cattrs.errors import (
    ClassValidationError,
    ForbiddenExtraKeysError,
    IterableValidationError,
)

T = TypeVar("T")


def test_structuring_invalid_tuples(converter: BaseConverter):
    """Structuring (hetero) tuples raises properly."""

    if converter.detailed_validation:
        with raises(IterableValidationError) as exc_info:
            converter.structure(["1", 2, "c"], tuple[int, int, int])
        assert isinstance(exc_info.value.exceptions[0], ValueError)
    else:
        # `int("c")` raises a ValueError
        with raises(ValueError):
            converter.structure(["1", 2, "c"], tuple[int, int, int])


def test_type_names_with_quotes():
    """Types with quote characters in their reprs should work.

    The index note is baked into the generated source, and it used to be
    interpolated inside single quotes, so `Literal["a"]` produced invalid
    source and raised `SyntaxError` before anything was structured.
    """

    class NT(NamedTuple):
        a: Literal["a"]
        b: int

    c = Converter()

    assert c.structure(["a", 1], Tuple[Literal["a"], int]) == ("a", 1)
    assert c.structure([["a"], 1], Tuple[List[Literal["a"]], int]) == (["a"], 1)
    assert c.structure([1, 2], Tuple[Annotated[int, "it's"], int]) == (1, 2)
    assert c.structure(["a", 1], NT) == NT("a", 1)

    with raises(IterableValidationError) as exc_info:
        c.structure(["b", 1], Tuple[Literal["a"], int])

    assert exc_info.value.exceptions[0].__notes__ == [
        f"Structuring {Tuple[Literal['a'], int]} @ index 0"
    ]


def test_simple_hetero_tuples(genconverter: Converter):
    """Simple heterogenous tuples work.

    Only supported for the Converter (not the BaseConverter).
    """

    genconverter.register_unstructure_hook(int, lambda v: v + 1)

    assert genconverter.unstructure((1, "2"), unstructure_as=Tuple[int, str]) == (
        2,
        "2",
    )

    genconverter.register_structure_hook(int, lambda v, _: v - 1)

    assert genconverter.structure([2, "2"], Tuple[int, str]) == (1, "2")


def test_named_tuple_predicate():
    """The NamedTuple predicate works."""

    assert not is_namedtuple(tuple)
    assert not is_namedtuple(Tuple[int, ...])
    assert not is_namedtuple(Tuple[int])

    class Test(NamedTuple):
        a: int

    assert is_namedtuple(Test)

    class Test2(Tuple[int, int]):
        pass

    assert not is_namedtuple(Test2)

    # `collections.namedtuple` classes are namedtuples.
    assert is_namedtuple(namedtuple("Test3", "a b"))

    # Generic namedtuples, bare and with actual arguments.
    class Test4(NamedTuple, Generic[T]):
        a: T

    assert is_namedtuple(Test4)
    assert is_namedtuple(Test4[int])

    # Objects merely quacking like namedtuples are not namedtuples.
    class Test5:
        _fields = ("a", "b")

    assert not is_namedtuple(Test5)

    class Test6(tuple):
        _fields = ("a", "b")

    assert not is_namedtuple(Test6)


def test_simple_typed_namedtuples(genconverter: Converter):
    """Simple typed namedtuples work."""

    class Test(NamedTuple):
        a: int

    assert genconverter.unstructure(Test(1)) == Test(1)
    assert genconverter.structure([1], Test) == Test(1)

    genconverter.register_unstructure_hook(int, lambda v: v + 1)
    genconverter.register_structure_hook(int, lambda v, _: v - 1)

    assert genconverter.unstructure(Test(1)) == (2,)
    assert genconverter.structure([2], Test) == Test(1)


def test_simple_dict_nametuples(genconverter: Converter):
    """Namedtuples can be un/structured to/from dicts."""

    class TestInner(NamedTuple):
        a: int

    class Test(NamedTuple):
        a: int
        b: str = "test"
        c: TestInner = TestInner(1)

    genconverter.register_unstructure_hook_factory(
        lambda t: t in (Test, TestInner), namedtuple_dict_unstructure_factory
    )
    genconverter.register_structure_hook_factory(
        lambda t: t in (Test, TestInner), namedtuple_dict_structure_factory
    )

    assert genconverter.unstructure(Test(1)) == {"a": 1, "b": "test", "c": {"a": 1}}
    assert genconverter.structure({"a": 1, "b": "2"}, Test) == Test(
        1, "2", TestInner(1)
    )

    # Defaults work.
    assert genconverter.structure({"a": 1}, Test) == Test(1, "test")


@define
class RecursiveAttrs:
    b: "List[RecursiveNamedtuple]" = Factory(list)


class RecursiveNamedtuple(NamedTuple):
    a: RecursiveAttrs


def test_recursive_dict_nametuples(genconverter: Converter):
    """Recursive namedtuples can be un/structured to/from dicts."""

    genconverter.register_unstructure_hook_factory(
        lambda t: t is RecursiveNamedtuple, namedtuple_dict_unstructure_factory
    )
    genconverter.register_structure_hook_factory(
        lambda t: t is RecursiveNamedtuple, namedtuple_dict_structure_factory
    )

    assert genconverter.unstructure(RecursiveNamedtuple(RecursiveAttrs())) == {
        "a": {"b": []}
    }
    assert genconverter.structure(
        {"a": {}}, RecursiveNamedtuple
    ) == RecursiveNamedtuple(RecursiveAttrs())


def test_dict_nametuples_forbid_extra_keys(genconverter: Converter):
    """Forbidding extra keys works for structuring namedtuples from dicts."""

    class Test(NamedTuple):
        a: int

    genconverter.register_structure_hook_factory(
        lambda t: t is Test,
        lambda t, c: namedtuple_dict_structure_factory(t, c, "from_converter", True),
    )

    with raises(Exception) as exc_info:
        genconverter.structure({"a": 1, "b": "2"}, Test)

    if genconverter.detailed_validation:
        exc = exc_info.value.exceptions[0]
    else:
        exc = exc_info.value

    assert isinstance(exc, ForbiddenExtraKeysError)
    assert exc.extra_fields == {"b"}


def test_dict_namedtuples_detailed_validation():
    """Passing detailed_validation to namedtuple_dict_structure_factory works.

    Regression test for the parameter being passed under the wrong name.
    """

    class Test(NamedTuple):
        a: int

    # Create a converter that does NOT use detailed validation by default.
    c = Converter(detailed_validation=False)

    # But explicitly enable it in the factory.
    c.register_structure_hook_factory(
        lambda t: t is Test,
        lambda t, conv: namedtuple_dict_structure_factory(t, conv, True),
    )

    # With detailed validation, structuring errors should be wrapped
    # in a ClassValidationError instead of being raised directly.

    with raises(ClassValidationError):
        c.structure({"a": "not_an_int"}, Test)


def test_namedtuple_sequence_defaults(converter: BaseConverter):
    """Only trailing fields with defaults may be omitted from sequences."""

    class Test(NamedTuple):
        a: int
        b: str = "test"
        c: float = 1.5

    assert converter.structure([1], Test) == Test(1, "test", 1.5)
    assert converter.structure([1, "a"], Test) == Test(1, "a", 1.5)
    assert converter.structure([1, "a", 2.5], Test) == Test(1, "a", 2.5)

    # Fields without defaults may not be omitted.
    with raises(Exception) as exc_info:
        converter.structure([], Test)

    if converter.detailed_validation:
        assert isinstance(exc_info.value, IterableValidationError)
        exc = exc_info.value.exceptions[0]
        assert isinstance(exc, ValueError)
        # The note points at the first missing position.
        assert exc.__notes__ == [f"Structuring {Test} @ index 0"]
    else:
        assert isinstance(exc_info.value, ValueError)


def test_namedtuple_sequence_too_many(converter: BaseConverter):
    """Sequences longer than the number of fields are an error, with position."""

    class Test(NamedTuple):
        a: int
        b: str = "test"

    with raises(Exception) as exc_info:
        converter.structure([1, "a", 3], Test)

    if converter.detailed_validation:
        assert isinstance(exc_info.value, IterableValidationError)
        exc = exc_info.value.exceptions[0]
        assert isinstance(exc, ValueError)
        # The note points at the first extra position.
        assert exc.__notes__ == [f"Structuring {Test} @ index 2"]
    else:
        assert isinstance(exc_info.value, ValueError)


def test_namedtuple_sequence_element_errors(converter: BaseConverter):
    """Positional element errors are reported with their indices."""

    class Test(NamedTuple):
        a: int
        b: int

    if converter.detailed_validation:
        with raises(IterableValidationError) as exc_info:
            converter.structure([1, "not-an-int"], Test)

        exc = exc_info.value.exceptions[0]
        assert exc.__notes__ == [f"Structuring {Test} @ index 1"]
    else:
        with raises(ValueError):
            converter.structure([1, "not-an-int"], Test)


def test_generic_namedtuples(genconverter: Converter):
    """Generic namedtuples keep their actual type arguments."""

    class Test(NamedTuple, Generic[T]):
        a: T
        b: List[T] = Factory(list)

    genconverter.register_structure_hook(int, lambda v, _: v + 1)
    genconverter.register_unstructure_hook(int, lambda v: v - 1)

    assert genconverter.structure([1, [2]], Test[int]) == Test(2, [3])
    assert genconverter.unstructure(Test(2, [3]), unstructure_as=Test[int]) == (
        1,
        [2],
    )


def test_generic_namedtuples_dict_factories(genconverter: Converter):
    """Generic namedtuples keep their actual type arguments with dict factories."""

    class Test(NamedTuple, Generic[T]):
        a: T
        b: str = "test"

    genconverter.register_structure_hook(int, lambda v, _: v + 1)
    genconverter.register_unstructure_hook(int, lambda v: v - 1)

    genconverter.register_structure_hook_factory(
        lambda t: t is Test[int],
        lambda t, c: namedtuple_dict_structure_factory(t, c),
    )
    genconverter.register_unstructure_hook_factory(
        lambda t: t is Test[int],
        lambda t, c: namedtuple_dict_unstructure_factory(t, c),
    )

    assert genconverter.structure({"a": 1}, Test[int]) == Test(2, "test")
    assert genconverter.unstructure(Test(2, "a"), unstructure_as=Test[int]) == {
        "a": 1,
        "b": "a",
    }


def test_namedtuple_structure_from_mapping(genconverter: Converter):
    """`namedtuple_structure_factory` can be configured to structure from mappings."""

    class Test(NamedTuple):
        a: int
        b: str = "test"

    genconverter.register_structure_hook_factory(
        lambda t: t is Test,
        lambda t, c: namedtuple_structure_factory(
            t, c, "mapping", forbid_extra_keys=True
        ),
    )

    assert genconverter.structure({"a": 1}, Test) == Test(1, "test")

    with raises(Exception) as exc_info:
        genconverter.structure({"a": 1, "b": "2", "c": 3}, Test)

    if genconverter.detailed_validation:
        exc = exc_info.value.exceptions[0]
    else:
        exc = exc_info.value

    assert isinstance(exc, ForbiddenExtraKeysError)
    assert exc.extra_fields == {"c"}


def test_namedtuple_structure_from_invalid(genconverter: Converter):
    """An invalid `structure_from` value raises."""

    class Test(NamedTuple):
        a: int

    with raises(ValueError, match="structure_from"):
        namedtuple_structure_factory(Test, genconverter, "set")


def test_namedtuple_unstructure_to_dict(genconverter: Converter):
    """`namedtuple_unstructure_factory` can unstructure to dicts."""

    class TestInner(NamedTuple):
        a: int

    class Test(NamedTuple):
        a: int
        b: TestInner = TestInner(1)

    genconverter.register_unstructure_hook_factory(
        lambda t: t in (Test, TestInner),
        lambda t, c: namedtuple_unstructure_factory(t, c, dict),
    )

    assert genconverter.unstructure(Test(1, TestInner(2))) == {
        "a": 1,
        "b": {"a": 2},
    }


def test_collections_namedtuples(converter: BaseConverter):
    """`collections.namedtuple` classes are supported."""

    Test = namedtuple("Test", "a b")

    assert converter.structure([1, "2"], Test) == Test(1, "2")
    assert converter.unstructure(Test(1, "2")) == Test(1, "2")
