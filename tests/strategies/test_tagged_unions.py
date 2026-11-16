from typing import Optional, Sequence, Union

import pytest
from attrs import define, field

from cattrs import BaseConverter, Converter
from cattrs.errors import (
    DuplicatedTagsError,
    TagNameCollisionError,
    TaggedUnionError,
    UnknownTagError,
)
from cattrs.strategies import configure_tagged_union


@define
class A:
    a: int


@define
class B:
    a: str


def test_defaults(converter: BaseConverter) -> None:
    """Defaults should work."""
    union = Union[A, B]
    configure_tagged_union(union, converter)

    assert converter.unstructure(A(1), union) == {"_type": "A", "a": 1}
    assert converter.unstructure(B("1"), union) == {"_type": "B", "a": "1"}

    assert converter.structure({"_type": "A", "a": 1}, union) == A(1)
    assert converter.structure({"_type": "B", "a": 1}, union) == B("1")


def test_tag_name(converter: BaseConverter) -> None:
    """Tag names are customizable."""
    union = Union[A, B]
    tag_name = "t"
    configure_tagged_union(union, converter, tag_name=tag_name)

    assert converter.unstructure(A(1), union) == {tag_name: "A", "a": 1}
    assert converter.unstructure(B("1"), union) == {tag_name: "B", "a": "1"}

    assert converter.structure({tag_name: "A", "a": 1}, union) == A(1)
    assert converter.structure({tag_name: "B", "a": 1}, union) == B("1")


def test_tag_generator(converter: BaseConverter) -> None:
    """Tag values are customizable using a callable."""
    union = Union[A, B]
    configure_tagged_union(
        union, converter, tag_generator=lambda t: f"{t.__module__}.{t.__name__}"
    )

    assert converter.unstructure(A(1), union) == {
        "_type": "tests.strategies.test_tagged_unions.A",
        "a": 1,
    }
    assert converter.unstructure(B("1"), union) == {
        "_type": "tests.strategies.test_tagged_unions.B",
        "a": "1",
    }

    assert converter.structure(
        {"_type": "tests.strategies.test_tagged_unions.A", "a": 1}, union
    ) == A(1)
    assert converter.structure(
        {"_type": "tests.strategies.test_tagged_unions.B", "a": 1}, union
    ) == B("1")


def test_tag_generator_dict(converter: BaseConverter) -> None:
    """Tag values are customizable using a dict."""
    union = Union[A, B]
    configure_tagged_union(
        union,
        converter,
        tag_generator={cl: f"type:{cl.__name__}" for cl in union.__args__}.get,
    )

    assert converter.unstructure(A(1), union) == {"_type": "type:A", "a": 1}
    assert converter.unstructure(B("1"), union) == {"_type": "type:B", "a": "1"}

    assert converter.structure({"_type": "type:A", "a": 1}, union) == A(1)
    assert converter.structure({"_type": "type:B", "a": 1}, union) == B("1")


def test_default_member(converter: BaseConverter) -> None:
    """Tagged unions can have default members."""
    union = Union[A, B]
    configure_tagged_union(union, converter, default=A)
    assert converter.unstructure(A(1), union) == {"_type": "A", "a": 1}
    assert converter.unstructure(B("1"), union) == {"_type": "B", "a": "1"}

    # No tag, so should structure as A.
    assert converter.structure({"a": 1}, union) == A(1)

    # An unknown tag must not silently fall through to the default member.
    with pytest.raises(UnknownTagError) as exc_info:
        converter.structure({"_type": "C", "a": 1}, union)
    assert exc_info.value.tag == "C"
    assert exc_info.value.tag_name == "_type"

    assert converter.structure({"_type": "A", "a": 1}, union) == A(1)
    assert converter.structure({"_type": "B", "a": 1}, union) == B("1")


def test_default_member_with_tag(converter: BaseConverter) -> None:
    """Members whose serialized payload keeps the tag still work without
    `forbid_extra_keys`, as long as their fields don't collide with the tag."""

    @define
    class C:
        marker: str = ""

    union = Union[A, B, C]
    configure_tagged_union(union, converter, default=C)
    assert converter.unstructure(A(1), union) == {"_type": "A", "a": 1}
    assert converter.unstructure(B("1"), union) == {"_type": "B", "a": "1"}

    # No tag, so should structure as C.
    assert converter.structure({"a": 1}, union) == C()

    # An unknown tag is reported instead of being swallowed by the default.
    with pytest.raises(UnknownTagError) as exc_info:
        converter.structure({"_type": "D", "a": 1}, union)
    assert exc_info.value.tag == "D"

    assert converter.structure({"_type": "A", "a": 1}, union) == A(1)
    assert converter.structure({"_type": "B", "a": 1}, union) == B("1")
    assert converter.structure({"_type": "C", "marker": "x"}, union) == C("x")


def test_tag_field_name_collision(converter: BaseConverter) -> None:
    """A field named like the tag field is detected up front."""

    @define
    class C:
        _type: str = ""

    with pytest.raises(TagNameCollisionError) as exc_info:
        configure_tagged_union(Union[A, B, C], converter)
    assert exc_info.value.tag_name == "_type"
    assert exc_info.value.cl is C
    assert exc_info.value.field == "_type"
    assert isinstance(exc_info.value, TaggedUnionError)


def test_tag_field_alias_collision(converter: BaseConverter) -> None:
    """A field aliased to the tag name is detected too."""

    @define
    class C:
        internal_name: str = field(default="", alias="_type")

    with pytest.raises(TagNameCollisionError) as exc_info:
        configure_tagged_union(Union[A, C], converter)
    assert exc_info.value.tag_name == "_type"
    assert exc_info.value.cl is C
    assert exc_info.value.field == "_type"


def test_tag_field_name_collision_custom_tag_name(converter: BaseConverter) -> None:
    """Collision detection honors a custom tag field name."""

    @define
    class C:
        t: str = ""

    with pytest.raises(TagNameCollisionError) as exc_info:
        configure_tagged_union(Union[A, C], converter, tag_name="t")
    assert exc_info.value.tag_name == "t"
    assert exc_info.value.cl is C


def test_tag_field_name_collision_default_member(converter: BaseConverter) -> None:
    """The default member participates in the collision check."""

    @define
    class C:
        _type: str = ""

    with pytest.raises(TagNameCollisionError):
        configure_tagged_union(Union[A, B], converter, default=C)


def test_duplicated_tags(converter: BaseConverter) -> None:
    """A tag produced by two members is reported with both participants."""
    union = Union[A, B]
    with pytest.raises(DuplicatedTagsError) as exc_info:
        configure_tagged_union(union, converter, tag_generator=lambda cl: "same")
    err = exc_info.value
    assert err.tag == "same"
    assert set(err.classes) == {A, B}
    assert "A" in str(err) and "B" in str(err) and "same" in str(err)
    assert isinstance(err, TaggedUnionError)


def test_duplicated_tags_leave_no_partial_configuration(
    converter: BaseConverter,
) -> None:
    """A failed configuration must not register any hooks.

    A second, valid configuration afterwards must behave exactly like a fresh
    one, proving nothing got half-registered by the failed call.
    """
    union = Union[A, B]

    with pytest.raises(DuplicatedTagsError):
        configure_tagged_union(union, converter, tag_generator=lambda cl: "same")

    configure_tagged_union(union, converter)
    assert converter.structure({"_type": "A", "a": 1}, union) == A(1)
    assert converter.unstructure(B("1"), union) == {"_type": "B", "a": "1"}
    with pytest.raises(UnknownTagError):
        converter.structure({"_type": "same", "a": 1}, union)


def test_tag_collision_leaves_no_partial_configuration(
    converter: BaseConverter,
) -> None:
    """A tag field collision must not register any hooks either."""

    @define
    class C:
        _type: str = ""

    union = Union[A, B, C]

    with pytest.raises(TagNameCollisionError):
        configure_tagged_union(union, converter)

    # Reconfiguring the same converter with a non-colliding tag name works,
    # which would not be the case if the failed call had left hooks behind.
    configure_tagged_union(union, converter, tag_name="tag")
    assert converter.structure({"tag": "A", "a": 1}, union) == A(1)
    assert converter.unstructure(C("x"), union) == {"_type": "x", "tag": "C"}


def test_unknown_tag_without_default(converter: BaseConverter) -> None:
    """Unknown tags raise a descriptive error even without a default member."""
    union = Union[A, B]
    configure_tagged_union(union, converter)

    with pytest.raises(UnknownTagError) as exc_info:
        converter.structure({"_type": "nope", "a": 1}, union)
    assert exc_info.value.tag == "nope"
    assert isinstance(exc_info.value, TaggedUnionError)


def test_unknown_tag_forbid_extra_keys() -> None:
    """Unknown tags raise instead of silently defaulting with extra keys forbidden."""

    @define
    class X:
        pass

    @define
    class Y:
        pass

    c = Converter(forbid_extra_keys=True)
    union = Union[X, Y]
    configure_tagged_union(union, c, default=X)

    # Missing tag still goes to the default.
    assert c.structure({}, union) == X()
    # Unknown tag does not.
    with pytest.raises(UnknownTagError):
        c.structure({"_type": "mystery"}, union)


def test_input_payload_is_not_mutated(converter: BaseConverter) -> None:
    """Structuring must leave the original payload (including its tag) intact."""
    union = Union[A, B]
    configure_tagged_union(union, converter)

    payload = {"_type": "A", "a": 1}
    assert converter.structure(payload, union) == A(1)
    assert payload == {"_type": "A", "a": 1}


def test_input_payload_is_not_mutated_forbid_extra_keys() -> None:
    """The copy-before-pop behavior must not mutate the caller's payload."""

    @define
    class X:
        value: int

    @define
    class Y:
        value: int

    c = Converter(forbid_extra_keys=True)
    union = Union[X, Y]
    configure_tagged_union(union, c)

    payload = {"_type": "X", "value": 7}
    assert c.structure(payload, union) == X(7)
    assert payload == {"_type": "X", "value": 7}


def test_round_trip_preserved(converter: BaseConverter) -> None:
    """Normal un/structure round trips keep working after the validation changes."""
    union = Union[A, B]
    configure_tagged_union(union, converter)

    for instance in (A(1), B("1")):
        payload = converter.unstructure(instance, union)
        assert converter.structure(payload, union) == instance


def test_default_member_validation(converter: BaseConverter) -> None:
    """Default members are structured properly.."""
    union = Union[A, B]
    configure_tagged_union(union, converter, default=A)

    # A.a should be coerced to an int.
    assert converter.structure({"_type": "A", "a": "1"}, union) == A(1)


def test_forbid_extra_keys():
    """The strategy works when converters forbid extra keys."""

    @define
    class A:
        pass

    @define
    class B:
        pass

    c = Converter(forbid_extra_keys=True)
    configure_tagged_union(Union[A, B], c)

    data = c.unstructure(A(), Union[A, B])
    c.structure(data, Union[A, B])


def test_forbid_extra_keys_default():
    """The strategy works when converters forbid extra keys."""

    @define
    class A:
        pass

    @define
    class B:
        pass

    c = Converter(forbid_extra_keys=True)
    configure_tagged_union(Union[A, B], c, default=A)

    data = c.unstructure(A(), Union[A, B])
    assert c.structure(data, Union[A, B]) == A()

    data.pop("_type")
    assert c.structure(data, Union[A, B]) == A()


def test_nested_sequence_union():
    @define
    class Top:
        u: Optional[Sequence[Union[A, B]]]

    c = Converter()
    configure_tagged_union(Union[A, B], c)

    data = c.unstructure(Top(u=[B(a="")]), Top)
    c.structure(data, Top)
