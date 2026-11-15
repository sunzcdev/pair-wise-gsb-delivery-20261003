"""Utility functions for collections."""

from __future__ import annotations

from collections import defaultdict
from collections.abc import Callable, Iterable
from functools import partial
from typing import TYPE_CHECKING, Any, DefaultDict, Literal, TypeVar

from attrs import NOTHING, Attribute, NothingType

from ._compat import (
    ANIES,
    AbcSet,
    get_args,
    get_full_type_hints,
    get_origin,
    is_annotated,
    is_bare,
    is_frozenset,
    is_generic,
    is_mapping,
    is_mutable_sequence,
    is_sequence,
    is_subclass,
)
from ._compat import is_mutable_set as is_set
from ._generics import deep_copy_with
from .dispatch import StructureHook, UnstructureHook
from .errors import IterableValidationError, IterableValidationNote
from .fns import identity
from .gen import (
    AttributeOverride,
    already_generating,
    make_dict_structure_fn_from_attrs,
    make_dict_unstructure_fn_from_attrs,
    make_hetero_tuple_unstructure_fn,
    mapping_structure_factory,
    mapping_unstructure_factory,
)
from .gen import make_iterable_unstructure_fn as iterable_unstructure_factory
from .gen._generics import generate_mapping
from .gen._lc import generate_unique_filename

if TYPE_CHECKING:
    from .converters import BaseConverter

__all__ = [
    "defaultdict_structure_factory",
    "homogenous_tuple_structure_factory",
    "is_abstract_set",
    "is_any_set",
    "is_defaultdict",
    "is_frozenset",
    "is_mapping",
    "is_mutable_sequence",
    "is_namedtuple",
    "is_sequence",
    "is_set",
    "iterable_unstructure_factory",
    "list_structure_factory",
    "mapping_structure_factory",
    "mapping_unstructure_factory",
    "namedtuple_dict_structure_factory",
    "namedtuple_dict_unstructure_factory",
    "namedtuple_structure_factory",
    "namedtuple_unstructure_factory",
]


def is_any_set(type) -> bool:
    """A predicate function for both mutable and frozensets."""
    return is_set(type) or is_frozenset(type)


def is_abstract_set(type) -> bool:
    """A predicate function for abstract (collection.abc) sets."""
    return type is AbcSet or (getattr(type, "__origin__", None) is AbcSet)


def is_namedtuple(type: Any) -> bool:
    """A predicate function for named tuples.

    Matches classes created using `typing.NamedTuple` or
    `collections.namedtuple`, including generic named tuples with actual
    type arguments applied (for example, ``Point[int]``).

    Plain tuples and objects merely quacking like named tuples (for example,
    simply having a ``_fields`` attribute) are not matched.
    """
    origin = get_origin(type) or type
    if not is_subclass(origin, tuple):
        return False
    fields = getattr(origin, "_fields", None)
    return (
        isinstance(fields, tuple)
        and all(isinstance(f, str) for f in fields)
        and isinstance(getattr(origin, "_field_defaults", None), dict)
        and callable(getattr(origin, "_make", None))
        and callable(getattr(origin, "_asdict", None))
    )


def _namedtuple_resolved_hints(cl: Any) -> dict[str, Any]:
    """Get the type hints of a namedtuple, resolving any type variables."""
    hints = get_full_type_hints(get_origin(cl) or cl)
    if is_generic(cl):
        mapping = generate_mapping(cl)
        if mapping:
            hints = {
                name: (
                    mapping.get(t.__name__, t)
                    if isinstance(t, TypeVar)
                    else (
                        deep_copy_with(t, mapping, cl)
                        if is_generic(t) and not is_bare(t) and not is_annotated(t)
                        else t
                    )
                )
                for name, t in hints.items()
            }
    return hints


def _namedtuple_fields(cl: Any) -> tuple[tuple[str, Any], ...]:
    """Get the (name, type) pairs of a namedtuple's fields, in field order.

    Fields of generic namedtuples have their actual type arguments applied.
    """
    hints = _namedtuple_resolved_hints(cl)
    return tuple(
        (name, hints.get(name, Any)) for name in (get_origin(cl) or cl)._fields
    )


def _is_passthrough(type: type[tuple], converter: BaseConverter) -> bool:
    """If all fields would be passed through, this class should not be processed
    either.
    """
    return all(
        converter.get_unstructure_hook(t) == identity
        for t in _namedtuple_resolved_hints(type).values()
    )


T = TypeVar("T")


def list_structure_factory(type: type, converter: BaseConverter) -> StructureHook:
    """A hook factory for structuring lists.

    Converts any given iterable into a list.
    """

    if is_bare(type) or type.__args__[0] in ANIES:

        def structure_list(obj: Iterable[T], _: type = type) -> list[T]:
            return list(obj)

        return structure_list

    elem_type = type.__args__[0]

    try:
        handler = converter.get_structure_hook(elem_type)
    except RecursionError:
        # Break the cycle by using late binding.
        handler = converter.structure

    if converter.detailed_validation:

        def structure_list(
            obj: Iterable[T], _: type = type, _handler=handler, _elem_type=elem_type
        ) -> list[T]:
            errors = []
            res = []
            ix = 0  # Avoid `enumerate` for performance.
            for e in obj:
                try:
                    res.append(handler(e, _elem_type))
                except Exception as e:
                    msg = IterableValidationNote(
                        f"Structuring {type} @ index {ix}", ix, elem_type
                    )
                    e.__notes__ = [*getattr(e, "__notes__", []), msg]
                    errors.append(e)
                finally:
                    ix += 1
            if errors:
                raise IterableValidationError(
                    f"While structuring {type!r}", errors, type
                )

            return res

    else:

        def structure_list(
            obj: Iterable[T], _: type = type, _handler=handler, _elem_type=elem_type
        ) -> list[T]:
            return [_handler(e, _elem_type) for e in obj]

    return structure_list


def homogenous_tuple_structure_factory(
    type: type, converter: BaseConverter
) -> StructureHook:
    """A hook factory for homogeneous (all elements the same, indeterminate length) tuples.

    Converts any given iterable into a tuple.
    """

    if is_bare(type) or type.__args__[0] in ANIES:

        def structure_tuple(obj: Iterable[T], _: type = type) -> tuple[T, ...]:
            return tuple(obj)

        return structure_tuple

    elem_type = type.__args__[0]

    try:
        handler = converter.get_structure_hook(elem_type)
    except RecursionError:
        # Break the cycle by using late binding.
        handler = converter.structure

    if converter.detailed_validation:
        # We have to structure into a list first anyway.
        list_structure = list_structure_factory(type, converter)

        def structure_tuple(obj: Iterable[T], _: type = type) -> tuple[T, ...]:
            return tuple(list_structure(obj, _))

    else:

        def structure_tuple(
            obj: Iterable[T], _: type = type, _handler=handler, _elem_type=elem_type
        ) -> tuple[T, ...]:
            return tuple([_handler(e, _elem_type) for e in obj])

    return structure_tuple


def namedtuple_unstructure_factory(
    cl: type[tuple],
    converter: BaseConverter,
    unstructure_to: Any = None,
    use_linecache: bool = True,
    **kwargs: AttributeOverride,
) -> UnstructureHook:
    """A hook factory for unstructuring namedtuples.

    :param unstructure_to: Force unstructuring to this type, if provided.
        Pass `dict` to unstructure into dictionaries of field names to values.
    :param use_linecache: Whether to store the source code in the Python linecache.
    :param kwargs: Attribute overrides, applied when unstructuring to dictionaries.

    ..  versionchanged:: NEXT
        `dict` is now supported for *unstructure_to*, and generic namedtuples
        have their actual type arguments applied.
    """
    if unstructure_to is dict:
        return namedtuple_dict_unstructure_factory(
            cl, converter, use_linecache=use_linecache, **kwargs
        )

    if unstructure_to is None and _is_passthrough(cl, converter):
        return identity

    return make_hetero_tuple_unstructure_fn(
        cl,
        converter,
        unstructure_to=tuple if unstructure_to is None else unstructure_to,
        type_args=tuple(t for _, t in _namedtuple_fields(cl)),
    )


def namedtuple_structure_factory(
    cl: type[tuple],
    converter: BaseConverter,
    structure_from: Literal["sequence", "mapping"] = "sequence",
    detailed_validation: bool | Literal["from_converter"] = "from_converter",
    forbid_extra_keys: bool = False,
    use_linecache: bool = True,
    **kwargs: AttributeOverride,
) -> StructureHook:
    """A hook factory for structuring namedtuples.

    :param structure_from: The expected shape of the input; ``"sequence"`` (the
        default) for positional sequences, ``"mapping"`` for mappings of field
        names to values.
    :param detailed_validation: Whether to use a slower mode that produces more
        detailed errors.
    :param forbid_extra_keys: When structuring from mappings, raise a
        `ForbiddenExtraKeysError` if unknown keys are encountered.
    :param use_linecache: Whether to store the source code in the Python linecache.
    :param kwargs: Attribute overrides, applied when structuring from mappings.

    When structuring from sequences, trailing fields with defaults may be
    omitted from the input; sequences longer than the number of fields are
    an error.

    ..  versionchanged:: NEXT
        The *structure_from*, *detailed_validation*, *forbid_extra_keys* and
        *use_linecache* parameters were added; trailing fields with defaults
        may now be omitted, and generic namedtuples have their actual type
        arguments applied.
    """
    if structure_from == "mapping":
        return namedtuple_dict_structure_factory(
            cl,
            converter,
            detailed_validation,
            forbid_extra_keys,
            use_linecache,
            **kwargs,
        )
    if structure_from != "sequence":
        raise ValueError(f"Invalid `structure_from` value: {structure_from!r}")
    if forbid_extra_keys or kwargs:
        raise ValueError(
            "`forbid_extra_keys` and attribute overrides are only supported "
            "when structuring namedtuples from mappings"
        )

    if detailed_validation == "from_converter":
        detailed_validation = converter.detailed_validation

    fields = _namedtuple_fields(cl)
    field_defaults = (get_origin(cl) or cl)._field_defaults
    defaults = tuple(
        field_defaults[name] for name, _ in fields if name in field_defaults
    )
    return _make_namedtuple_structure_fn(
        cl, converter, fields, defaults, detailed_validation, use_linecache
    )


def _make_namedtuple_structure_fn(
    cl: Any,
    converter: BaseConverter,
    fields: tuple[tuple[str, Any], ...],
    defaults: tuple[Any, ...],
    detailed_validation: bool,
    use_linecache: bool,
) -> StructureHook:
    """Generate a specialized hook structuring a namedtuple from a sequence.

    Only trailing fields with defaults may be omitted from the input.
    """
    fn_name = "structure_namedtuple"
    total = len(fields)
    required = total - len(defaults)
    internal_arg_parts = {"__c_cl": cl}
    lines = []

    if detailed_validation:
        internal_arg_parts["__c_ive"] = IterableValidationError
        internal_arg_parts["__c_ivn"] = IterableValidationNote
        internal_arg_parts["__c_types"] = tuple(t for _, t in fields)
        lines.extend(["  errors = []", "  res = []"])

        for ix, (_, t) in enumerate(fields):
            handler = converter.get_structure_hook(t)
            struct_handler_name = f"__c_structure_{ix}"
            type_name = f"__c_type_{ix}"
            internal_arg_parts[struct_handler_name] = handler
            internal_arg_parts[type_name] = t
            if handler == converter._structure_call:
                internal_arg_parts[struct_handler_name] = t
                invocation = f"{struct_handler_name}(o[{ix}])"
            else:
                invocation = f"{struct_handler_name}(o[{ix}], {type_name})"
            note = f"Structuring {cl} @ index {ix}"
            lines.extend(
                [
                    f"  if len(o) > {ix}:",
                    "    try:",
                    f"      res.append({invocation})",
                    "    except Exception as e:",
                    (
                        f"      e.__notes__ = [*getattr(e, '__notes__', []), "
                        f"__c_ivn({note!r}, {ix}, {type_name})]"
                    ),
                    "      errors.append(e)",
                ]
            )
            if ix >= required:
                default_name = f"__c_default_{ix}"
                internal_arg_parts[default_name] = defaults[ix - required]
                lines.append(f"  else: res.append({default_name})")

        lines.extend(
            [
                f"  if len(o) < {required}:",
                (
                    '    exc = ValueError(f"Not enough values in {o!r} '
                    'to structure as {__c_cl!r}")'
                ),
                (
                    "    exc.__notes__ = [__c_ivn(f'Structuring {__c_cl} @ index "
                    "{len(o)}', len(o), __c_types[len(o)])]"
                ),
                "    errors.append(exc)",
                f"  elif len(o) > {total}:",
                (
                    '    exc = ValueError(f"Too many values in {o!r} '
                    'to structure as {__c_cl!r}")'
                ),
                (
                    f"    exc.__notes__ = [__c_ivn(f'Structuring {{__c_cl}} @ index "
                    f"{total}', {total}, o[{total}].__class__)]"
                ),
                "    errors.append(exc)",
                "  if errors:",
                "    raise __c_ive(f'While structuring {__c_cl!r}', errors, __c_cl)",
                "  return __c_cl(*res)",
            ]
        )
    else:
        arg_lines = []
        for ix, (_, t) in enumerate(fields):
            handler = converter.get_structure_hook(t)
            struct_handler_name = f"__c_structure_{ix}"
            type_name = f"__c_type_{ix}"
            internal_arg_parts[struct_handler_name] = handler
            internal_arg_parts[type_name] = t
            if handler == converter._structure_call:
                internal_arg_parts[struct_handler_name] = t
                invocation = f"{struct_handler_name}(o[{ix}])"
            else:
                invocation = f"{struct_handler_name}(o[{ix}], {type_name})"
            if ix >= required:
                default_name = f"__c_default_{ix}"
                internal_arg_parts[default_name] = defaults[ix - required]
                invocation = f"{invocation} if len(o) > {ix} else {default_name}"
            arg_lines.append(f"    {invocation},")

        lines = [
            f"  if len(o) > {total}:",
            (
                '    raise ValueError(f"Too many values in {o!r} '
                'to structure as {__c_cl!r}")'
            ),
            f"  if len(o) < {required}:",
            (
                '    raise ValueError(f"Not enough values in {o!r} '
                'to structure as {__c_cl!r}")'
            ),
            "  return __c_cl(",
            *arg_lines,
            "  )",
        ]

    internal_arg_line = ", ".join([f"{i}={i}" for i in internal_arg_parts])
    globs = dict(internal_arg_parts)
    script = "\n".join([f"def {fn_name}(o, _=__c_cl, {internal_arg_line}):", *lines])

    fname = generate_unique_filename(
        get_origin(cl) or cl,
        "structure",
        lines=script.splitlines() if use_linecache else [],
    )
    eval(compile(script, fname, "exec"), globs)
    return globs[fn_name]


def _namedtuple_to_attrs(cl: type[tuple]) -> list[Attribute]:
    """Generate pseudo attributes for a namedtuple."""
    origin = get_origin(cl) or cl
    hints = _namedtuple_resolved_hints(cl)
    return [
        Attribute(
            name,
            origin._field_defaults.get(name, NOTHING),
            None,
            False,
            False,
            False,
            True,
            False,
            type=hints.get(name, Any),
            alias=name,
        )
        for name in origin._fields
    ]


def namedtuple_dict_structure_factory(
    cl: type[tuple],
    converter: BaseConverter,
    detailed_validation: bool | Literal["from_converter"] = "from_converter",
    forbid_extra_keys: bool = False,
    use_linecache: bool = True,
    /,
    **kwargs: AttributeOverride,
) -> StructureHook:
    """A hook factory for hooks structuring namedtuples from dictionaries.

    :param forbid_extra_keys: Whether the hook should raise a `ForbiddenExtraKeysError`
        if unknown keys are encountered.
    :param use_linecache: Whether to store the source code in the Python linecache.

    .. versionadded:: 24.1.0
    .. versionchanged:: NEXT
        Generic namedtuples have their actual type arguments applied.
    """
    try:
        working_set = already_generating.working_set
    except AttributeError:
        working_set = set()
        already_generating.working_set = working_set
    else:
        if cl in working_set:
            raise RecursionError()

    working_set.add(cl)

    try:
        return make_dict_structure_fn_from_attrs(
            _namedtuple_to_attrs(cl),
            get_origin(cl) or cl,
            converter,
            typevar_map=generate_mapping(cl) if is_generic(cl) else {},
            _cattrs_forbid_extra_keys=forbid_extra_keys,
            _cattrs_detailed_validation=detailed_validation,
            _cattrs_use_linecache=use_linecache,
            **kwargs,
        )
    finally:
        working_set.remove(cl)
        if not working_set:
            del already_generating.working_set


def namedtuple_dict_unstructure_factory(
    cl: type[tuple],
    converter: BaseConverter,
    omit_if_default: bool = False,
    use_linecache: bool = True,
    /,
    **kwargs: AttributeOverride,
) -> UnstructureHook:
    """A hook factory for hooks unstructuring namedtuples to dictionaries.

    :param omit_if_default: When true, attributes equal to their default values
        will be omitted in the result dictionary.
    :param use_linecache: Whether to store the source code in the Python linecache.

    .. versionadded:: 24.1.0
    .. versionchanged:: NEXT
        Generic namedtuples have their actual type arguments applied.
    """
    try:
        working_set = already_generating.working_set
    except AttributeError:
        working_set = set()
        already_generating.working_set = working_set
    if cl in working_set:
        raise RecursionError()

    working_set.add(cl)

    try:
        return make_dict_unstructure_fn_from_attrs(
            _namedtuple_to_attrs(cl),
            get_origin(cl) or cl,
            converter,
            typevar_map=generate_mapping(cl) if is_generic(cl) else {},
            _cattrs_omit_if_default=omit_if_default,
            _cattrs_use_linecache=use_linecache,
            **kwargs,
        )
    finally:
        working_set.remove(cl)
        if not working_set:
            del already_generating.working_set


def is_defaultdict(type: Any) -> bool:
    """Is this type a defaultdict?

    Bare defaultdicts (defaultdicts with no type arguments) are not supported
    since there's no way to discover their _default_factory_.
    """
    return is_subclass(get_origin(type), (defaultdict, DefaultDict))


def defaultdict_structure_factory(
    type: type[defaultdict],
    converter: BaseConverter,
    default_factory: Callable[[], Any] | NothingType = NOTHING,
) -> StructureHook:
    """A structure hook factory for defaultdicts.

    The value type parameter will be used as the *default factory*.
    """
    if default_factory is NOTHING:
        default_factory = get_args(type)[1]
    return mapping_structure_factory(
        type, converter, partial(defaultdict, default_factory)
    )
