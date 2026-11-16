from typing import Any, Callable, Union, get_origin

from attrs import NOTHING, NothingType

from .. import BaseConverter
from .._compat import (
    adapted_fields,
    get_newtype_base,
    has,
    is_literal,
    is_subclass,
    is_union_type,
)
from ..errors import DuplicatedTagsError, TagNameCollisionError, UnknownTagError
from ..sentinels import is_sentinel
from ..typealiases import get_type_alias_base, is_type_alias

__all__ = [
    "configure_tagged_union",
    "configure_union_passthrough",
    "default_tag_generator",
]


def default_tag_generator(typ: type) -> str:
    """Return the class name."""
    return typ.__name__


def configure_tagged_union(
    union: Any,
    converter: BaseConverter,
    tag_generator: Callable[[type], str] = default_tag_generator,
    tag_name: str = "_type",
    default: Union[type, NothingType] = NOTHING,
) -> None:
    """
    Configure the converter so that `union` (which should be a union, or a type alias
    of one) is un/structured with the help of an additional piece of data in the
    unstructured payload, the tag.

    :param converter: The converter to apply the strategy to.
    :param tag_generator: A `tag_generator` function is used to map each
        member of the union to a tag, which is then included in the
        unstructured payload. The default tag generator returns the name of
        the class.
    :param tag_name: The key under which the tag will be set in the
        unstructured payload. By default, `'_type'`.
    :param default: An optional class to be used if the tag information
        is not present when structuring. Note that a tag which is present
        but unknown is never routed to the default; an
        :class:`~cattrs.errors.UnknownTagError` is raised instead.

    The tagged union strategy currently only works with the dict
    un/structuring base strategy.

    Before any hooks are registered, the configuration is validated, so a failed
    call never leaves a partially configured converter behind:

    * every union member must produce a distinct tag, otherwise a
      :class:`~cattrs.errors.DuplicatedTagsError` is raised, and
    * the tag field name must not collide with a serialized field (its name or
      alias) of any attrs class or dataclass participating in the union,
      otherwise a :class:`~cattrs.errors.TagNameCollisionError` is raised.

    .. versionadded:: 23.1.0

    ..  versionchanged:: 25.1
        Type aliases of unions are now also supported.
    ..  versionchanged:: NEXT
        Duplicate tags and tag field name collisions are now reported up front
        instead of silently overwriting the tag-to-member mapping. A default
        member now only handles payloads missing the tag; unknown tags raise
        :class:`~cattrs.errors.UnknownTagError`.
    """
    if is_type_alias(union):
        union = union.__value__
    args = union.__args__

    # All validation happens before anything is registered on the converter, so
    # a failure cannot leave a half-configured converter behind.
    cl_to_tag: dict[type, Any] = {}
    tag_to_cls: dict[Any, list[type]] = {}
    for cl in args:
        tag = tag_generator(cl)
        cl_to_tag[cl] = tag
        tag_to_cls.setdefault(tag, []).append(cl)

    # Bidirectional uniqueness: a tag must map to exactly one member and every
    # member must map to exactly one tag.
    for tag, classes in tag_to_cls.items():
        if len(classes) > 1:
            raise DuplicatedTagsError(tag, classes)

    # The tag field must not shadow a serialized field of a participating
    # attrs class or dataclass, since the tag would clobber that field in the
    # unstructured payload (and vice versa when structuring).
    checked: set[int] = set()
    members = (*args, default) if default is not NOTHING else args
    for cl in members:
        origin = get_origin(cl) or cl
        if not has(origin) or id(origin) in checked:
            continue
        checked.add(id(origin))
        for attribute in adapted_fields(origin):
            if tag_name == attribute.name:
                raise TagNameCollisionError(tag_name, origin, attribute.name)
            if tag_name == attribute.alias:
                raise TagNameCollisionError(tag_name, origin, attribute.alias)

    tag_to_hook: dict[Any, Callable] = {}
    exact_cl_unstruct_hooks: dict[type, Callable] = {}

    if default is not NOTHING:
        default_handler = converter.get_structure_hook(default)

        if getattr(converter, "forbid_extra_keys", False):

            def structure_tagged_union(
                val: dict,
                _,
                _tag_to_hook=tag_to_hook,
                _tag_name=tag_name,
                _dh=default_handler,
                _default=default,
                _union=union,
            ) -> union:
                if _tag_name not in val:
                    return _dh(val, _default)
                tag = val[_tag_name]
                hook = _tag_to_hook.get(tag)
                if hook is None:
                    raise UnknownTagError(tag, _union, _tag_name)
                val = val.copy()
                val.pop(_tag_name)
                return hook(val)

        else:

            def structure_tagged_union(
                val: dict,
                _,
                _tag_to_hook=tag_to_hook,
                _tag_name=tag_name,
                _dh=default_handler,
                _default=default,
                _union=union,
            ) -> union:
                if _tag_name not in val:
                    return _dh(val, _default)
                tag = val[_tag_name]
                hook = _tag_to_hook.get(tag)
                if hook is None:
                    raise UnknownTagError(tag, _union, _tag_name)
                return hook(val)

    elif getattr(converter, "forbid_extra_keys", False):

        def structure_tagged_union(
            val: dict,
            _,
            _tag_to_hook=tag_to_hook,
            _tag_name=tag_name,
            _union=union,
        ) -> union:
            tag = val[_tag_name]
            hook = _tag_to_hook.get(tag)
            if hook is None:
                raise UnknownTagError(tag, _union, _tag_name)
            val = val.copy()
            val.pop(_tag_name)
            return hook(val)

    else:

        def structure_tagged_union(
            val: dict,
            _,
            _tag_to_hook=tag_to_hook,
            _tag_name=tag_name,
            _union=union,
        ) -> union:
            tag = val[_tag_name]
            hook = _tag_to_hook.get(tag)
            if hook is None:
                raise UnknownTagError(tag, _union, _tag_name)
            return hook(val)

    def unstructure_tagged_union(
        val: union,
        _exact_cl_unstruct_hooks=exact_cl_unstruct_hooks,
        _cl_to_tag=cl_to_tag,
        _tag_name=tag_name,
    ) -> dict:
        res = _exact_cl_unstruct_hooks[val.__class__](val)
        res[_tag_name] = _cl_to_tag[val.__class__]
        return res

    converter.register_unstructure_hook(union, unstructure_tagged_union)
    converter.register_structure_hook(union, structure_tagged_union)

    for cl in args:
        tag = cl_to_tag[cl]
        struct_handler = converter.get_structure_hook(cl)
        unstruct_handler = converter.get_unstructure_hook(cl)

        def structure_union_member(val: dict, _cl=cl, _h=struct_handler) -> cl:
            return _h(val, _cl)

        def unstructure_union_member(val: union, _h=unstruct_handler) -> dict:
            return _h(val)

        tag_to_hook[tag] = structure_union_member
        exact_cl_unstruct_hooks[cl] = unstructure_union_member


def configure_union_passthrough(
    union: Any, converter: BaseConverter, accept_ints_as_floats: bool = True
) -> None:
    """
    Configure the converter to support validating and passing through unions of the
    provided types and their subsets.

    For example, all mature JSON libraries natively support producing unions of ints,
    floats, Nones, and strings. Using this strategy, a converter can be configured
    to efficiently validate and pass through unions containing these types.

    The most important point is that another library (in this example the JSON
    library) handles producing the union, and the converter is configured to just
    validate it.

    Literals of provided types are also supported, and are checked by value.

    NewTypes of provided types are also supported.

    The strategy is designed to be O(1) in execution time, and independent of the
    ordering of types in the union.

    If the union contains a class and one or more of its subclasses, the subclasses
    will also be included when validating the superclass.

    :param accept_ints_as_floats: When set (the default), if the provided union
        contains both ints and floats, actual unions containing only floats will also accept
        ints. See https://typing.python.org/en/latest/spec/special-types.html#special-cases-for-float-and-complex
        for more information.

    .. versionadded:: 23.2.0
    .. versionchanged:: 25.2.0
        Introduced the `accept_ints_as_floats` parameter.
    .. versionchanged:: 26.2.0
        PEP 695 type aliases are now supported as union members.
    .. versionchanged:: NEXT
        Unions containing Python 3.15+ sentinels defer to sentinel handling,
        matching sentinel names before passing through other values.
    """

    def get_passthrough_base(type: Any) -> Any:
        return get_type_alias_base(type) if is_type_alias(type) else type

    args = {get_passthrough_base(type) for type in union.__args__}

    def make_structure_native_union(exact_type: Any) -> Callable:
        # `exact_type` is likely to be a subset of the entire configured union (`args`).
        literal_values = {
            v for t in exact_type.__args__ if is_literal(t) for v in t.__args__
        }

        # We have no idea what the actual type of `val` will be, so we can't
        # use it blindly with an `in` check since it might not be hashable.
        # So we do an additional check when handling literals.
        # Note: do no use `literal_values` here, since {0, False} gets reduced to {0}
        literal_classes = {
            v.__class__
            for t in exact_type.__args__
            if is_literal(t)
            for v in t.__args__
        }

        non_literal_classes = {
            get_newtype_base(t) or get_passthrough_base(t)
            for t in exact_type.__args__
            if not is_literal(t)
            and ((get_newtype_base(t) or get_passthrough_base(t)) in args)
        }

        # We augment the set of allowed classes with any configured subclasses of
        # the exact subclasses.
        non_literal_classes |= {
            a for a in args if any(is_subclass(a, c) for c in non_literal_classes)
        }

        # We check for spillover - union types not handled by the strategy.
        # If spillover exists and we fail to validate our types, we call
        # further into the converter with the rest.
        spillover = {
            a
            for a in exact_type.__args__
            if (get_newtype_base(a) or get_passthrough_base(a))
            not in non_literal_classes
            and not is_literal(a)
        }

        # By default, when floats are part of the union, accept ints too.
        if (
            accept_ints_as_floats
            and int in args
            and float in args
            and float in non_literal_classes
            and int not in non_literal_classes
        ):
            non_literal_classes.add(int)

        if spillover:
            spillover_type = (
                Union[tuple(spillover)] if len(spillover) > 1 else next(iter(spillover))
            )

            def structure_native_union(
                val: Any,
                _: Any,
                classes=non_literal_classes,
                vals=literal_values,
                converter=converter,
                spillover=spillover_type,
            ) -> exact_type:
                if val.__class__ in literal_classes and val in vals:
                    return val
                if val.__class__ in classes:
                    return val
                return converter.structure(val, spillover)

        else:

            def structure_native_union(
                val: Any, _: Any, classes=non_literal_classes, vals=literal_values
            ) -> exact_type:
                if val.__class__ in literal_classes and val in vals:
                    return val
                if val.__class__ in classes:
                    return val
                raise TypeError(f"{val} ({val.__class__}) not part of {_}")

        return structure_native_union

    def contains_native_union(exact_type: Any) -> bool:
        """Can we handle this type?"""
        if is_union_type(exact_type):
            type_args = set(exact_type.__args__)
            # Sentinel names must be matched before passing through strings.
            if any(is_sentinel(t) for t in type_args):
                return False
            # We special case optionals, since they are very common
            # and are handled a little more efficiently by default.
            if len(type_args) == 2 and type(None) in type_args:
                return False

            literal_classes = {
                lit_arg.__class__
                for t in type_args
                if is_literal(t)
                for lit_arg in t.__args__
            }
            non_literal_types = {
                get_newtype_base(t) or get_passthrough_base(t)
                for t in type_args
                if not is_literal(t)
            }

            return (literal_classes | non_literal_types) & args
        return False

    converter.register_structure_hook_factory(
        contains_native_union, make_structure_native_union
    )
