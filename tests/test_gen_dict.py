"""Tests for generated dict functions."""

from dataclasses import dataclass, field as dcfield
from math import ceil
from typing import Annotated, Dict, Literal, Type, Union

import pytest
from attrs import NOTHING, Factory, define, field, frozen
from attrs.setters import frozen as frozen_setter
from hypothesis import assume, given
from hypothesis.strategies import data, just, one_of, sampled_from

from cattrs import BaseConverter, Converter
from cattrs._compat import adapted_fields, fields
from cattrs.errors import (
    ClassValidationError,
    ForbiddenExtraKeysError,
    FrozenStructureError,
)
from cattrs.gen import (
    make_dict_structure_fn,
    make_dict_unstructure_fn,
    make_dict_unstructure_fn_from_attrs,
    override,
)

from .helpers import assert_only_unstructured
from .typed import nested_typed_classes, simple_typed_classes, simple_typed_dataclasses
from .untyped import nested_classes, simple_classes


@given(nested_classes() | simple_classes())
def test_unmodified_generated_unstructuring(cl_and_vals):
    converter = BaseConverter()
    cl, vals, kwargs = cl_and_vals
    fn = make_dict_unstructure_fn(cl, converter)

    inst = cl(*vals, **kwargs)

    res_expected = converter.unstructure(inst)

    converter.register_unstructure_hook(cl, fn)

    res_actual = converter.unstructure(inst)

    assert res_expected == res_actual


@given(nested_classes() | simple_classes())
def test_nodefs_generated_unstructuring(cl_and_vals):
    """Test omitting default values on a per-attribute basis."""
    converter = BaseConverter()
    cl, vals, kwargs = cl_and_vals

    attr_is_default = False
    for attr, val in zip([a for a in cl.__attrs_attrs__ if not a.kw_only], vals):
        if attr.default is not NOTHING:
            fn = make_dict_unstructure_fn(
                cl, converter, **{attr.name: override(omit_if_default=True)}
            )
            if attr.default == val:
                attr_is_default = True
            break
    else:
        assume(False)

    converter.register_unstructure_hook(cl, fn)

    inst = cl(*vals, **kwargs)

    res = converter.unstructure(inst)

    if attr_is_default:
        assert attr.name not in res


@given(
    one_of(just(BaseConverter), just(Converter)), nested_classes() | simple_classes()
)
def test_nodefs_generated_unstructuring_cl(
    converter_cls: Type[BaseConverter], cl_and_vals
):
    """Test omitting default values on a per-class basis."""
    converter = converter_cls()
    cl, vals, kwargs = cl_and_vals

    for attr in cl.__attrs_attrs__:
        if attr.default is not NOTHING:
            break
    else:
        assume(False)

    converter.register_unstructure_hook(
        cl, make_dict_unstructure_fn(cl, converter, _cattrs_omit_if_default=True)
    )

    inst = cl(*vals, **kwargs)

    res = converter.unstructure(inst)

    for attr, val in zip(cl.__attrs_attrs__, vals):
        if attr.default is not NOTHING:
            if not isinstance(attr.default, Factory):
                if val == attr.default:
                    assert attr.name not in res
                else:
                    assert attr.name in res
            else:
                # The default is a factory, but might take self.
                if attr.default.takes_self:
                    # Our strategies can only produce these for now.
                    assert val == attr.default.factory(cl)
                    assert attr.name not in res
                else:
                    if val == attr.default.factory():
                        assert attr.name not in res
                    else:
                        assert attr.name in res


@pytest.mark.parametrize("decorate", [define, dataclass])
@pytest.mark.parametrize("from_attrs", [False, True])
@pytest.mark.parametrize("converter_setting", [None, False, True, "base"])
@pytest.mark.parametrize("explicit", [None, False, True])
def test_omit_if_default_from_converter(
    decorate, from_attrs, converter_setting, explicit
):
    """Generated hooks inherit omission settings unless explicitly overridden."""

    @decorate
    class A:
        a: int = 1
        b: int = 2

    if converter_setting == "base":
        converter = BaseConverter()
    elif converter_setting is None:
        converter = Converter()
    else:
        converter = Converter(omit_if_default=converter_setting)

    options = {} if explicit is None else {"_cattrs_omit_if_default": explicit}
    if from_attrs:
        hook = make_dict_unstructure_fn_from_attrs(
            adapted_fields(A), A, converter, b=override(rename="renamed"), **options
        )
    else:
        hook = make_dict_unstructure_fn(
            A, converter, b=override(rename="renamed"), **options
        )
    converter.register_unstructure_hook(A, hook)

    omit = converter_setting is True if explicit is None else explicit
    assert converter.unstructure(A()) == ({} if omit else {"a": 1, "renamed": 2})
    assert converter.unstructure(A(b=3)) == (
        {"renamed": 3} if omit else {"a": 1, "renamed": 3}
    )


@pytest.mark.parametrize("omit_if_default", [False, True])
@pytest.mark.parametrize("from_attrs", [False, True])
def test_omit_if_default_from_converter_attribute_overrides(
    omit_if_default, from_attrs
):
    """Attribute overrides take precedence over the inherited converter setting."""

    @define
    class A:
        a: int = 1
        b: int = 2
        c: int = 3

    converter = Converter(omit_if_default=omit_if_default)
    overrides = {
        "a": override(omit_if_default=False),
        "b": override(omit_if_default=True),
    }
    if from_attrs:
        hook = make_dict_unstructure_fn_from_attrs(
            adapted_fields(A), A, converter, **overrides
        )
    else:
        hook = make_dict_unstructure_fn(A, converter, **overrides)
    converter.register_unstructure_hook(A, hook)

    assert converter.unstructure(A()) == (
        {"a": 1} if omit_if_default else {"a": 1, "c": 3}
    )


@given(
    one_of(just(BaseConverter), just(Converter)),
    nested_classes()
    | simple_classes(min_attrs=1)
    | simple_typed_dataclasses(min_attrs=1),
)
def test_individual_overrides(converter_cls, cl_and_vals):
    """
    Test omitting default values on a per-class basis, but with individual
    overrides.
    """
    converter = converter_cls()
    cl, vals, kwargs = cl_and_vals

    fields = adapted_fields(cl)

    for attr in fields:
        if attr.default is not NOTHING:
            break
    else:
        assume(False)

    chosen_name = attr.name

    converter.register_unstructure_hook(
        cl,
        make_dict_unstructure_fn(
            cl,
            converter,
            _cattrs_omit_if_default=True,
            **{attr.name: override(omit_if_default=False)},
        ),
    )

    inst = cl(*vals, **kwargs)

    res = converter.unstructure(inst)
    assert_only_unstructured(res)

    for attr, val in zip(fields, vals):
        if attr.name == chosen_name:
            assert attr.name in res
        elif attr.default is not NOTHING:
            if not isinstance(attr.default, Factory):
                if val == attr.default:
                    assert attr.name not in res
                else:
                    assert attr.name in res
            else:
                if attr.default.takes_self:
                    assert attr.name not in res
                else:
                    if val == attr.default.factory():
                        assert attr.name not in res
                    else:
                        assert attr.name in res


@given(
    cl_and_vals=nested_typed_classes(allow_nan=False)
    | simple_typed_classes(allow_nan=False)
    | simple_typed_dataclasses(allow_nan=False),
    dv=...,
)
def test_unmodified_generated_structuring(cl_and_vals, dv: bool):
    converter = Converter(detailed_validation=dv)
    cl, vals, kwargs = cl_and_vals
    fn = make_dict_structure_fn(cl, converter, _cattrs_detailed_validation=dv)
    assert fn.overrides == {}

    inst = cl(*vals, **kwargs)

    unstructured = converter.unstructure(inst)

    assert_only_unstructured(unstructured)

    converter.register_structure_hook(cl, fn)

    res = converter.structure(unstructured, cl)

    assert inst == res


@given(
    simple_typed_classes(min_attrs=1, allow_nan=False)
    | simple_typed_dataclasses(min_attrs=1, allow_nan=False),
    data(),
)
def test_renaming(cl_and_vals, data):
    converter = Converter()
    cl, vals, kwargs = cl_and_vals
    attrs = fields(cl)

    to_replace = data.draw(sampled_from(attrs))

    u_fn = make_dict_unstructure_fn(
        cl, converter, **{to_replace.name: override(rename="class")}
    )
    s_fn = make_dict_structure_fn(
        cl, converter, **{to_replace.name: override(rename="class")}
    )
    assert s_fn.overrides == {to_replace.name: override(rename="class")}
    assert u_fn.overrides == {to_replace.name: override(rename="class")}

    converter.register_structure_hook(cl, s_fn)
    converter.register_unstructure_hook(cl, u_fn)

    inst = cl(*vals, **kwargs)

    raw = converter.unstructure(inst)

    assert "class" in raw

    new_inst = converter.structure(raw, cl)

    assert inst == new_inst


def test_renaming_forbid_extra_keys():
    converter = BaseConverter()

    @define
    class A:
        b: int
        c: str

    s_fn = make_dict_structure_fn(
        A, converter, c=override(rename="d"), _cattrs_forbid_extra_keys=True
    )

    converter.register_structure_hook(A, s_fn)

    new_inst = converter.structure({"b": 1, "d": "str"}, A)

    assert new_inst == A(1, "str")

    with pytest.raises(ClassValidationError) as cve:
        converter.structure({"b": 1, "c": "str"}, A)

    assert len(cve.value.exceptions) == 2
    assert isinstance(cve.value.exceptions[0], KeyError)
    assert isinstance(cve.value.exceptions[1], ForbiddenExtraKeysError)
    assert cve.value.exceptions[1].cl is A
    assert cve.value.exceptions[1].extra_fields == {"c"}


@pytest.mark.parametrize("key", ["it's", 'a"b', "a'] or 1 or ['b"])
def test_renaming_to_key_with_special_characters(key):
    """A rename target is a dict key, so it may legitimately contain quotes.

    It used to be interpolated into the generated code inside single quotes, so
    a key like ``it's`` produced invalid source and raised ``SyntaxError``.
    """

    @define
    class A:
        b: int

    converter = Converter()
    converter.register_unstructure_hook(
        A, make_dict_unstructure_fn(A, converter, b=override(rename=key))
    )
    converter.register_structure_hook(
        A, make_dict_structure_fn(A, converter, b=override(rename=key))
    )

    assert converter.unstructure(A(1)) == {key: 1}
    assert converter.structure({key: 1}, A) == A(1)


def test_omitting(converter: BaseConverter):
    """Omitting works."""

    @define
    class A:
        a: int
        b: int = field(init=False)

    converter.register_unstructure_hook(
        A, make_dict_unstructure_fn(A, converter, b=override(omit=True))
    )

    assert converter.unstructure(A(1)) == {"a": 1}


def test_omitting_none(converter: BaseConverter):
    """Omitting works properly with None."""

    @define
    class A:
        a: int
        b: int = field(init=False)

    converter.register_unstructure_hook(
        A, make_dict_unstructure_fn(A, converter, a=override(), b=override())
    )

    assert converter.unstructure(A(1)) == {"a": 1}

    converter.register_structure_hook(
        A, make_dict_structure_fn(A, converter, a=override(), b=override())
    )

    assert converter.structure({"a": 2}, A).a == 2
    assert not hasattr(converter.structure({"a": 2}, A), "b")


@pytest.mark.parametrize("detailed_validation", [True, False])
def test_omitting_structure(detailed_validation: bool):
    """Omitting fields works with generated structuring functions."""
    converter = BaseConverter()

    @define
    class A:
        a: int
        b: int = field(init=False)
        c: int = 1

    converter.register_structure_hook(
        A,
        make_dict_structure_fn(
            A,
            converter,
            b=override(omit=True),
            c=override(omit=True),
            _cattrs_detailed_validation=detailed_validation,
        ),
    )

    # The 'c' parameter is ignored and the default is used instead.
    structured = converter.structure({"a": 1, "b": 2, "c": 3}, A)
    assert structured.a == 1
    assert structured.c == 1
    assert not hasattr(structured, "b")


def test_type_names_with_quotes():
    """Types with quote characters in their reprs should work."""

    converter = Converter()

    assert converter.structure({1: 1}, Dict[Annotated[int, "'"], int]) == {1: 1}

    converter.register_structure_hook_func(
        lambda t: t == Union[Literal["a", 2, 3], Literal[4]], lambda v, _: v
    )
    assert converter.structure(
        {2: "a"}, Dict[Union[Literal["a", 2, 3], Literal[4]], str]
    ) == {2: "a"}


def test_overriding_struct_hook(converter: BaseConverter) -> None:
    """Overriding structure hooks works."""

    @define
    class A:
        a: int
        b: str
        c: int = 0

    converter.register_structure_hook(
        A,
        make_dict_structure_fn(
            A,
            converter,
            a=override(struct_hook=lambda v, _: ceil(v)),
            c=override(struct_hook=lambda v, _: ceil(v)),
            _cattrs_detailed_validation=converter.detailed_validation,
        ),
    )

    assert converter.structure({"a": 0.5, "b": 1, "c": 0.5}, A) == A(1, "1", 1)


def test_overriding_unstruct_hook(converter: BaseConverter) -> None:
    """Overriding unstructure hooks works."""

    @define
    class A:
        a: int
        b: str

    converter.register_unstructure_hook(
        A,
        make_dict_unstructure_fn(
            A, converter, a=override(unstruct_hook=lambda v: v + 1)
        ),
    )

    assert converter.unstructure(A(1, "")) == {"a": 2, "b": ""}


def test_alias_keys(converter: BaseConverter) -> None:
    """Alias keys work."""

    @define
    class A:
        _a: int
        b: int = field(alias="aliased")
        c: int = field(alias="also_aliased", default=3)
        d: int = field(alias="d_aliased", default=5)

    converter.register_unstructure_hook(
        A,
        make_dict_unstructure_fn(
            A,
            converter,
            _cattrs_use_alias=True,
            c=override(omit=True),
            d=override(rename="d_renamed"),
        ),
    )

    assert converter.unstructure(A(1, 2, 3, 4)) == {
        "a": 1,
        "aliased": 2,
        "d_renamed": 4,
    }

    converter.register_structure_hook(
        A,
        make_dict_structure_fn(
            A,
            converter,
            _cattrs_use_alias=True,
            _cattrs_detailed_validation=converter.detailed_validation,
            c=override(omit=True),
            d=override(rename="d_renamed"),
        ),
    )

    assert converter.structure({"a": 1, "aliased": 2, "d_renamed": 4}, A) == A(
        1, 2, 3, 4
    )


def test_alias_implicit_rename(converter: BaseConverter) -> None:
    """Attributes with aliases generate rename metadata."""

    @define
    class A:
        a: int = field(alias="b")
        c: int = field(alias="d", default=1)

    u_fn = make_dict_unstructure_fn(A, converter, _cattrs_use_alias=True)
    s_fn = make_dict_structure_fn(A, converter, _cattrs_use_alias=True)

    assert u_fn.overrides["a"].rename == "b"
    assert s_fn.overrides["a"].rename == "b"
    assert u_fn.overrides["c"].rename == "d"
    assert s_fn.overrides["c"].rename == "d"

    converter.register_unstructure_hook(A, u_fn)
    converter.register_structure_hook(A, s_fn)

    assert converter.unstructure(A(1)) == {"b": 1, "d": 1}
    assert converter.structure({"b": 1}, A) == A(1)
    assert converter.structure({"b": 1, "d": 2}, A) == A(1, 2)


def test_init_false(converter: BaseConverter) -> None:
    """By default init=False keys are ignored."""

    @define
    class A:
        a: int
        b: int = field(init=False)
        _c: int = field(init=False)
        d: int = field(init=False, default=4)

    converter.register_unstructure_hook(A, make_dict_unstructure_fn(A, converter))

    a = A(1)
    a.b = 2
    a._c = 3

    assert converter.unstructure(a) == {"a": 1}

    converter.register_structure_hook(
        A,
        make_dict_structure_fn(
            A, converter, _cattrs_detailed_validation=converter.detailed_validation
        ),
    )

    structured = converter.structure({"a": 1}, A)

    assert not hasattr(structured, "b")
    assert not hasattr(structured, "_c")
    assert structured.d == 4
    assert structured.a == 1


def test_init_false_overridden(converter: BaseConverter) -> None:
    """init=False handling can be overriden."""

    @frozen
    class Inner:
        a: int

    @define
    class A:
        a: int
        b: int = field(init=False)
        _c: int = field(init=False)
        d: Inner = field(init=False)
        e: int = field(init=False, default=4)
        f: Inner = field(init=False, default=Inner(1))

    converter.register_unstructure_hook(
        A, make_dict_unstructure_fn(A, converter, _cattrs_include_init_false=True)
    )

    a = A(1)
    a.b = 2
    a._c = 3
    a.d = Inner(4)

    assert converter.unstructure(a) == {
        "a": 1,
        "b": 2,
        "_c": 3,
        "d": {"a": 4},
        "e": 4,
        "f": {"a": 1},
    }

    converter.register_structure_hook(
        A, make_dict_structure_fn(A, converter, _cattrs_include_init_false=True)
    )

    structured = converter.structure({"a": 1, "b": 2, "_c": 3, "d": {"a": 1}}, A)
    assert structured.b == 2
    assert structured._c == 3
    assert structured.d == Inner(1)
    assert structured.e == 4
    assert structured.f == Inner(1)

    structured = converter.structure(
        {"a": 1, "b": 2, "_c": 3, "d": {"a": 5}, "e": -4, "f": {"a": 2}}, A
    )
    assert structured.b == 2
    assert structured._c == 3
    assert structured.d == Inner(5)
    assert structured.e == -4
    assert structured.f == Inner(2)


def test_init_false_field_override(converter: BaseConverter) -> None:
    """init=False handling can be overriden on a per-field basis."""

    @define
    class A:
        a: int
        b: int = field(init=False)
        _c: int = field(init=False)
        d: int = field(init=False, default=4)

    converter.register_unstructure_hook(
        A,
        make_dict_unstructure_fn(
            A,
            converter,
            b=override(omit=False),
            _c=override(omit=False),
            d=override(omit=False),
        ),
    )

    a = A(1)
    a.b = 2
    a._c = 3

    assert converter.unstructure(a) == {"a": 1, "b": 2, "_c": 3, "d": 4}

    converter.register_structure_hook(
        A,
        make_dict_structure_fn(
            A,
            converter,
            b=override(omit=False),
            _c=override(omit=False),
            d=override(omit=False),
            _cattrs_detailed_validation=converter.detailed_validation,
        ),
    )

    structured = converter.structure({"a": 1, "b": 2, "_c": 3}, A)
    assert structured.b == 2
    assert structured._c == 3
    assert structured.d == 4

    structured = converter.structure({"a": 1, "b": 2, "_c": 3, "d": -4}, A)
    assert structured.b == 2
    assert structured._c == 3
    assert structured.d == -4


def test_init_false_no_structure_hook(converter: BaseConverter):
    """init=False attributes with converters and `prefer_attrs_converters` work."""

    @define
    class A:
        a: int = field(converter=int, init=False)
        b: int = field(converter=int, init=False, default=5)

    converter.register_structure_hook(
        A,
        make_dict_structure_fn(
            A,
            converter,
            _cattrs_prefer_attrib_converters=True,
            _cattrs_include_init_false=True,
        ),
    )

    res = A()
    res.a = 5

    assert converter.structure({"a": "5"}, A) == res


def test_init_false_frozen_class_rejected() -> None:
    """Frozen attrs classes cannot restore init=False fields after construction."""

    @frozen
    class FrozenClass:
        a: int
        b: int = field(init=False, default=1)

    @define
    class ReadOnlyField:
        a: int
        b: int = field(init=False, default=1, on_setattr=frozen_setter)

    c = Converter()

    # The configuration is refused while generating the hook, before any
    # instance is constructed.
    with pytest.raises(FrozenStructureError) as exc_info:
        make_dict_structure_fn(FrozenClass, c, _cattrs_include_init_false=True)
    assert exc_info.value.cl is FrozenClass
    assert exc_info.value.field_names == ("b",)

    # The same applies to a per-field include override.
    with pytest.raises(FrozenStructureError):
        make_dict_structure_fn(FrozenClass, c, b=override(omit=False))

    # A field-level frozen setter on an otherwise mutable class is refused too.
    with pytest.raises(FrozenStructureError) as exc_info:
        make_dict_structure_fn(ReadOnlyField, c, _cattrs_include_init_false=True)
    assert exc_info.value.field_names == ("b",)

    # Skipping the field (the default) keeps working.
    hook = make_dict_structure_fn(FrozenClass, c)
    c.register_structure_hook(FrozenClass, hook)
    assert c.structure({"a": 1}, FrozenClass) == FrozenClass(1)


def test_init_false_frozen_dataclass_rejected() -> None:
    """Frozen dataclasses are rejected the same way as frozen attrs classes."""

    @dataclass(frozen=True)
    class FrozenDataclass:
        a: int
        b: int = dcfield(init=False, default=1)

    c = Converter()
    with pytest.raises(FrozenStructureError) as exc_info:
        make_dict_structure_fn(FrozenDataclass, c, _cattrs_include_init_false=True)
    assert exc_info.value.cl is FrozenDataclass
    assert exc_info.value.field_names == ("b",)


def test_init_false_frozen_inherited_rejected() -> None:
    """A mutable class inheriting from a frozen base cannot be assigned to."""

    @frozen
    class FrozenBase:
        a: int

    @define
    class Child(FrozenBase):
        b: int = field(init=False, default=1)

    c = Converter()
    with pytest.raises(FrozenStructureError):
        make_dict_structure_fn(Child, c, _cattrs_include_init_false=True)


def test_init_false_not_passed_to_constructor() -> None:
    """init=False values are assigned after construction, never via __init__."""

    constructor_args = []

    @define
    class A:
        a: int
        b: int = field(init=False, default=1)

        def __attrs_post_init__(self) -> None:
            constructor_args.append(("b", self.b))

    c = Converter(detailed_validation=False)
    hook = make_dict_structure_fn(A, c, _cattrs_include_init_false=True)
    c.register_structure_hook(A, hook)

    result = c.structure({"a": 1, "b": 2}, A)
    assert result.a == 1
    assert result.b == 2
    # The constructor only ever saw the default for the init=False field.
    assert constructor_args == [("b", 1)]


def test_init_false_converter_and_validator_run_once(converter: BaseConverter) -> None:
    """Restoration runs each field's converter and validator on the restored value only.

    The constructor is still free to apply the definitions to the field
    default, but a post-construction restoration must not run them a second
    time on that default: each restored value is converted and validated
    exactly once.
    """
    converter_inputs = []
    validator_inputs = []

    def tracking_converter(value):
        converter_inputs.append(value)
        return f"converted:{value}"

    def tracking_validator(instance, attribute, value):
        validator_inputs.append((attribute.name, value))

    @define
    class A:
        a: int
        plain: str = field(
            init=False, default="default-plain",
            converter=tracking_converter, validator=tracking_validator,
        )
        other: str = field(
            init=False, default=Factory(lambda: "default-other"),
            converter=tracking_converter,
        )

    converter.register_structure_hook(
        A, make_dict_structure_fn(A, converter, _cattrs_include_init_false=True)
    )

    result = converter.structure(
        {"a": 1, "plain": "restore-plain", "other": "restore-other"}, A
    )

    assert result.plain == "converted:restore-plain"
    assert result.other == "converted:restore-other"

    # Each restored value reaches the converter exactly once during the
    # post-construction assignment; the converter never sees a restored value
    # more than once.
    assert converter_inputs.count("restore-plain") == 1
    assert converter_inputs.count("restore-other") == 1
    # The validator runs once for the restored value, on its converted form.
    assert validator_inputs.count(
        ("plain", "converted:restore-plain")
    ) == 1


def test_init_false_missing_key_keeps_default(converter: BaseConverter) -> None:
    """When no value is supplied, the constructor default is left intact."""

    @define
    class A:
        a: int
        b: int = field(init=False, default=5)

    converter.register_structure_hook(
        A, make_dict_structure_fn(A, converter, _cattrs_include_init_false=True)
    )

    result = converter.structure({"a": 1}, A)
    assert result.a == 1
    assert result.b == 5


def test_init_false_failure_not_swallowed() -> None:
    """A failed post-construction assignment surfaces instead of a partial object."""

    @define
    class A:
        a: int
        b: int = field(init=False, default=1)

    c = Converter()  # detailed validation enabled

    def failing_hook(value, type):
        raise ValueError("boom")

    c.register_structure_hook(int, failing_hook)
    c.register_structure_hook(
        A, make_dict_structure_fn(A, c, _cattrs_include_init_false=True)
    )

    with pytest.raises(ClassValidationError) as exc_info:
        c.structure({"a": 1, "b": 2}, A)
    assert any(
        isinstance(exc, ValueError) and str(exc) == "boom"
        for exc in exc_info.value.exceptions
    )


def test_init_false_aliases_slots_and_inheritance() -> None:
    """Aliases, slots and inherited init=False fields are all restored."""

    @define(slots=True)
    class Base:
        a: int
        slotted: int = field(init=False, default=0, alias="slotted_alias")

    @define(slots=True)
    class Child(Base):
        b: int
        own: int = field(init=False, default=0)

    c = Converter()
    hook = make_dict_structure_fn(
        Child,
        c,
        _cattrs_include_init_false=True,
        _cattrs_use_alias=True,
    )
    c.register_structure_hook(Child, hook)

    result = c.structure(
        {"a": 1, "b": 2, "slotted_alias": 3, "own": 4}, Child
    )
    assert result.a == 1
    assert result.b == 2
    assert result.slotted == 3
    assert result.own == 4

    unstructure_hook = make_dict_unstructure_fn(
        Child,
        c,
        _cattrs_include_init_false=True,
        _cattrs_use_alias=True,
    )
    c.register_unstructure_hook(Child, unstructure_hook)
    assert c.unstructure(result) == {
        "a": 1,
        "b": 2,
        "slotted_alias": 3,
        "own": 4,
    }


@given(forbid_extra_keys=..., detailed_validation=...)
def test_forbid_extra_keys_from_converter(
    forbid_extra_keys: bool, detailed_validation: bool
):
    """
    `forbid_extra_keys` is taken from the converter by default.
    """
    c = Converter(
        forbid_extra_keys=forbid_extra_keys, detailed_validation=detailed_validation
    )

    @define
    class A:
        a: int

    c.register_structure_hook(A, make_dict_structure_fn(A, c))

    if forbid_extra_keys:
        with pytest.raises((ForbiddenExtraKeysError, ClassValidationError)):
            c.structure({"a": 1, "b": 2}, A)
    else:
        c.structure({"a": 1, "b": 2}, A)


@given(detailed_validation=...)
def test_forbid_extra_keys_from_baseconverter(detailed_validation: bool):
    """
    `forbid_extra_keys` is taken from the converter by default.

    BaseConverter should default to False.
    """
    c = BaseConverter(detailed_validation=detailed_validation)

    @define
    class A:
        a: int

    c.register_structure_hook(A, make_dict_structure_fn(A, c))

    c.structure({"a": 1, "b": 2}, A)


def test_detailed_validation_from_converter(converter: BaseConverter):
    """
    `detailed_validation` is taken from the converter by default.
    """

    @define
    class A:
        a: int

    converter.register_structure_hook(A, make_dict_structure_fn(A, converter))

    if converter.detailed_validation:
        with pytest.raises(ClassValidationError):
            converter.structure({"a": "a"}, A)
    else:
        with pytest.raises(ValueError):
            converter.structure({"a": "a"}, A)


@given(prefer=..., dv=...)
def test_prefer_converters_from_converter(prefer: bool, dv: bool):
    """
    `prefer_attrs_converters` is taken from the converter by default.
    """

    @define
    class A:
        a: int = field(converter=lambda x: x + 1)
        b: int = field(converter=lambda x: x + 1, default=5)

    converter = BaseConverter(prefer_attrib_converters=prefer)
    converter.register_structure_hook(int, lambda x, _: x + 1)
    converter.register_structure_hook(
        A, make_dict_structure_fn(A, converter, _cattrs_detailed_validation=dv)
    )

    if prefer:
        assert converter.structure({"a": 1, "b": 2}, A).a == 2
        assert converter.structure({"a": 1, "b": 2}, A).b == 3
    else:
        assert converter.structure({"a": 1}, A).a == 3


def test_fields_exception():
    """fields() raises on a non-attrs, non-dataclass class."""
    with pytest.raises(Exception):  # noqa: B017
        fields(int)
