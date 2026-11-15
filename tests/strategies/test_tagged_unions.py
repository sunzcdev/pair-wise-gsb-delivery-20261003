from typing import Annotated, Optional, Sequence, Union

import attrs
import pytest
from attrs import define

from cattrs import BaseConverter, Converter
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
    # An unknown tag is reported explicitly instead of silently falling back
    # to the default member; the default only handles missing tags.
    with pytest.raises(ValueError, match="Unknown tag 'C'"):
        converter.structure({"_type": "C", "a": 1}, union)

    assert converter.structure({"_type": "A", "a": 1}, union) == A(1)
    assert converter.structure({"_type": "B", "a": 1}, union) == B("1")


def test_default_member_with_tag(converter: BaseConverter) -> None:
    """Members can access the tags, if not `forbid_extra_keys`."""

    @define
    class C:
        tag: str = ""

    union = Union[A, B, C]
    configure_tagged_union(union, converter, default=C)
    assert converter.unstructure(A(1), union) == {"_type": "A", "a": 1}
    assert converter.unstructure(B("1"), union) == {"_type": "B", "a": "1"}

    # No tag, so should structure as C.
    assert converter.structure({"a": 1}, union) == C()
    # An unknown tag must not be absorbed by the default member.
    with pytest.raises(ValueError, match="Unknown tag 'D'"):
        converter.structure({"_type": "D", "a": 1}, union)

    assert converter.structure({"_type": "A", "a": 1}, union) == A(1)
    assert converter.structure({"_type": "B", "a": 1}, union) == B("1")
    assert converter.structure({"_type": "C", "a": 1}, union) == C()


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


def test_duplicate_tags_raise(converter: BaseConverter) -> None:
    """A tag generator producing the same tag for two members raises."""

    @define
    class C:
        c: int

    union = Union[A, B, C]
    with pytest.raises(ValueError) as exc_info:
        configure_tagged_union(union, converter, tag_generator=lambda _: "same")

    msg = str(exc_info.value)
    # The error mentions the colliding tag and both participating types.
    assert "'same'" in msg
    assert "A" in msg
    assert "B" in msg


def test_duplicate_tag_error_lists_both_types(converter: BaseConverter) -> None:
    """The duplicate-tag error names both classes involved."""

    @define
    class First:
        x: int

    @define
    class Second:
        y: int

    union = Union[First, Second]
    with pytest.raises(ValueError) as exc_info:
        configure_tagged_union(
            union, converter, tag_generator={First: "t", Second: "t"}.get
        )

    msg = str(exc_info.value)
    assert "First" in msg
    assert "Second" in msg
    assert "'t'" in msg


def test_duplicate_tags_leave_converter_untouched() -> None:
    """A failed configuration does not register any hooks on the converter."""

    @define
    class C:
        c: int

    @define
    class D:
        d: int

    converter = Converter()
    union = Union[C, D]
    original_structure_hook = converter.get_structure_hook(union)
    original_unstructure_hook = converter.get_unstructure_hook(union)

    with pytest.raises(ValueError):
        configure_tagged_union(union, converter, tag_generator=lambda _: "dup")

    assert converter.get_structure_hook(union) == original_structure_hook
    assert converter.get_unstructure_hook(union) == original_unstructure_hook


def test_tag_name_field_collision_raises(converter: BaseConverter) -> None:
    """A tag name colliding with a member field name raises."""

    @define
    class Tagged:
        _type: str

    union = Union[A, Tagged]
    with pytest.raises(ValueError) as exc_info:
        configure_tagged_union(union, converter)

    msg = str(exc_info.value)
    assert "'_type'" in msg
    assert "Tagged" in msg


def test_tag_name_alias_collision_raises() -> None:
    """A tag name colliding with a renamed (aliased) field raises."""
    from typing import Annotated

    from cattrs import override

    @define
    class Renamed:
        something: Annotated[str, override(rename="_type")]

    c = Converter()

    with pytest.raises(ValueError, match="'_type'"):
        configure_tagged_union(Union[A, Renamed], c)


def test_tag_name_alias_collision_use_alias() -> None:
    """Attrs aliases are honored when the converter has use_alias enabled."""

    @define
    class WithAlias:
        something: str = attrs.field(alias="_type")

    c = Converter(use_alias=True)
    with pytest.raises(ValueError, match="'_type'"):
        configure_tagged_union(Union[A, WithAlias], c)


def test_tag_name_runtime_rename_collision_raises() -> None:
    """Renames applied through generated dict hooks are detected as well."""
    from cattrs.gen import make_dict_structure_fn, make_dict_unstructure_fn
    from cattrs.gen import override as gen_override

    @define
    class Renamed:
        something: str

    c = Converter()
    c.register_unstructure_hook(
        Renamed,
        make_dict_unstructure_fn(Renamed, c, something=gen_override(rename="_type")),
    )
    c.register_structure_hook(
        Renamed,
        make_dict_structure_fn(Renamed, c, something=gen_override(rename="_type")),
    )

    with pytest.raises(ValueError, match="'_type'"):
        configure_tagged_union(Union[A, Renamed], c)


def test_field_collision_leaves_converter_untouched() -> None:
    """A tag/field collision aborts before any hook registration."""

    @define
    class C:
        c: int

    @define
    class Tagged:
        _type: str

    converter = Converter()
    union = Union[C, Tagged]
    original_structure_hook = converter.get_structure_hook(union)
    original_unstructure_hook = converter.get_unstructure_hook(union)

    with pytest.raises(ValueError):
        configure_tagged_union(union, converter)

    assert converter.get_structure_hook(union) == original_structure_hook
    assert converter.get_unstructure_hook(union) == original_unstructure_hook


def test_unknown_tag_raises_without_default(converter: BaseConverter) -> None:
    """An unknown tag raises instead of producing a KeyError."""
    union = Union[A, B]
    configure_tagged_union(union, converter)
    with pytest.raises(ValueError, match="Unknown tag 'C'"):
        converter.structure({"_type": "C", "a": 1}, union)


def test_unknown_tag_raises_forbid_extra_keys() -> None:
    """Unknown tags are reported when extra keys are forbidden."""

    @define
    class NoFields:
        pass

    @define
    class Other:
        pass

    c = Converter(forbid_extra_keys=True)
    union = Union[NoFields, Other]
    configure_tagged_union(union, c)
    with pytest.raises(ValueError, match="Unknown tag 'nope'"):
        c.structure({"_type": "nope"}, union)


def test_input_not_mutated_forbid_extra_keys() -> None:
    """Structuring pops the tag from a copy, leaving the input intact."""

    @define
    class NoFields:
        pass

    @define
    class Other:
        pass

    c = Converter(forbid_extra_keys=True)
    union = Union[NoFields, Other]
    configure_tagged_union(union, c)

    payload = {"_type": "NoFields"}
    c.structure(payload, union)
    assert payload == {"_type": "NoFields"}


def test_input_not_mutated_default_forbid_extra_keys() -> None:
    """The default branch never mutates the input with forbid_extra_keys."""

    @define
    class NoFields:
        pass

    @define
    class Other:
        pass

    c = Converter(forbid_extra_keys=True)
    union = Union[NoFields, Other]
    configure_tagged_union(union, c, default=NoFields)

    payload_without_tag = {}
    assert c.structure(payload_without_tag, union) == NoFields()
    assert payload_without_tag == {}

    payload = {"_type": "NoFields"}
    assert c.structure(payload, union) == NoFields()
    assert payload == {"_type": "NoFields"}


def test_roundtrip_preserved(converter: BaseConverter) -> None:
    """Normal un/structuring round trips keep working."""
    union = Union[A, B]
    configure_tagged_union(union, converter)

    for instance in (A(1), B("1")):
        unstructured = converter.unstructure(instance, union)
        assert converter.structure(unstructured, union) == instance


def test_roundtrip_preserved_forbid_extra_keys() -> None:
    """Round trips work with forbid_extra_keys and don't mutate payloads."""

    @define
    class X:
        x: int

    @define
    class Y:
        y: str

    c = Converter(forbid_extra_keys=True)
    union = Union[X, Y]
    configure_tagged_union(union, c)

    payload = {"_type": "X", "x": 1}
    assert c.structure(payload, union) == X(1)
    assert payload == {"_type": "X", "x": 1}

    payload = {"_type": "Y", "y": "s"}
    assert c.structure(payload, union) == Y("s")
    assert payload == {"_type": "Y", "y": "s"}


def test_default_only_handles_missing_tag_forbid_extra_keys() -> None:
    """With a default member, only a missing tag uses the default."""

    @define
    class X:
        x: int

    @define
    class Y:
        y: int

    c = Converter(forbid_extra_keys=True)
    union = Union[X, Y]
    configure_tagged_union(union, c, default=X)

    assert c.structure({"x": 1}, union) == X(1)
    with pytest.raises(ValueError, match="Unknown tag 'Z'"):
        c.structure({"_type": "Z", "x": 1}, union)


def test_dataclass_field_collision_raises() -> None:
    """Dataclass field names are also checked against the tag name."""
    from dataclasses import dataclass

    @dataclass
    class TaggedDC:
        _type: str

    c = Converter()
    with pytest.raises(ValueError, match="'_type'"):
        configure_tagged_union(Union[A, TaggedDC], c)


def test_non_attrs_member_skipped_in_collision_check() -> None:
    """Members without attrs/dataclass fields don't trigger false collisions."""

    class Plain:
        def __init__(self, a):
            self.a = a

        def __eq__(self, other):
            return isinstance(other, Plain) and other.a == self.a

    c = Converter()
    c.register_structure_hook(Plain, lambda val, _: Plain(val["a"]))
    c.register_unstructure_hook(Plain, lambda val: {"a": val.a})

    configure_tagged_union(Union[A, Plain], c)
    assert c.unstructure(Plain(1), Union[A, Plain]) == {"a": 1, "_type": "Plain"}
    assert c.structure({"a": 1, "_type": "Plain"}, Union[A, Plain]) == Plain(1)


def test_runtime_rename_failure_leaves_tag_mappings_consistent() -> None:
    """A late collision check leaves no half-installed member tag mapping."""
    from cattrs.gen import make_dict_structure_fn, make_dict_unstructure_fn
    from cattrs.gen import override as gen_override

    @define
    class Good:
        g: int

    @define
    class Bad:
        something: str

    c = Converter()
    c.register_unstructure_hook(
        Bad,
        make_dict_unstructure_fn(Bad, c, something=gen_override(rename="_type")),
    )
    c.register_structure_hook(
        Bad,
        make_dict_structure_fn(Bad, c, something=gen_override(rename="_type")),
    )

    union = Union[Good, Bad]
    with pytest.raises(ValueError, match="'_type'"):
        configure_tagged_union(union, c)

    # The valid member's tag must not resolve to a placeholder class either;
    # structuring any tagged payload must raise the unknown-tag error.
    with pytest.raises(ValueError, match="Unknown tag 'Good'"):
        c.structure({"_type": "Good", "g": 1}, union)
