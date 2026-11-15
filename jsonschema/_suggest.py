"""
Advisory suggestions of :kw:`default` values for missing properties.

The :kw:`default` keyword is only an annotation, so validation never
fills anything in. This module provides an independent interface which
inspects a schema alongside an instance and reports what *could* be
filled in -- without modifying either the instance or the schema.
"""

from __future__ import annotations

from collections.abc import Hashable
from copy import deepcopy
from typing import TYPE_CHECKING, Any
import re

from attrs import define, field
import referencing
import referencing.exceptions
import referencing.jsonschema

from jsonschema._utils import equal, find_additional_properties

if TYPE_CHECKING:
    from jsonschema.protocols import Validator

#: The suggestion has a single unambiguous candidate which satisfies the
#: contract of the property it was declared for.
OK = "ok"
#: Multiple branches (e.g. of ``allOf``) declare differing candidates for
#: the same missing property.
CONFLICT = "conflict"
#: A declared candidate does not itself satisfy the property's contract.
INVALID = "invalid"

_EMPTY: dict[str, Any] = {}


@define(frozen=True)
class Candidate:
    """
    A single suggested value for a missing property.

    Attributes:

        value:

            the suggested value, deep-copied from the schema so that
            mutating it cannot affect the schema (or vice versa)

        schema_path:

            the path within the schema at which the :kw:`default` was
            declared

        branch:

            the innermost applicator branch the declaration applies
            under (e.g. ``"allOf[0]"``, ``"oneOf[1]"``, ``"then"``),
            or `None` if it applies unconditionally

        errors:

            validation errors the candidate itself has against the
            contract of the property it was declared for, if any
    """

    value: Any
    schema_path: tuple[Hashable, ...]
    branch: str | None
    errors: tuple[str, ...] = field(factory=tuple)


@define(frozen=True)
class Suggestion:
    """
    What could be filled in for one property missing from an instance.

    Attributes:

        path:

            the path within the instance of the object which is missing
            the property

        property:

            the name of the missing property

        candidates:

            the suggested values found for the property

        status:

            one of `OK`, `CONFLICT` or `INVALID`
    """

    path: tuple[Hashable, ...]
    property: Hashable
    candidates: tuple[Candidate, ...]
    status: str

    @property
    def value(self):
        """
        The suggested value, if a single unambiguous one exists.

        Returns `None` when the candidates conflict.
        """
        if self.status == CONFLICT or not self.candidates:
            return None
        return self.candidates[0].value


def suggest_defaults(validator: Validator, instance: Any) -> list[Suggestion]:
    """
    Suggest :kw:`default` values for properties missing from the instance.

    Neither the instance nor the schema is modified, and no suggestion
    is ever applied implicitly -- in particular ``oneOf`` branches which
    cannot be uniquely selected contribute no candidates.

    Arguments:

        validator:

            the validator whose schema should be inspected

        instance:

            the instance to find missing properties for

    Returns:

        a list of `Suggestion`, one per missing property which has at
        least one :kw:`default` declared for it
    """
    found: dict[tuple, list[Candidate]] = {}
    _collect(
        validator,
        instance,
        path=(),
        schema_path=(),
        branch=None,
        missing=None,
        found=found,
        seen=frozenset(),
    )
    return [
        Suggestion(
            path=path,
            property=property,
            candidates=tuple(candidates),
            status=_status_of(candidates),
        )
        for (path, property), candidates in found.items()
    ]


def _status_of(candidates: list[Candidate]) -> str:
    distinct: list[Any] = []
    for candidate in candidates:
        if not any(equal(candidate.value, each) for each in distinct):
            distinct.append(candidate.value)
    if len(distinct) > 1:
        return CONFLICT
    if any(candidate.errors for candidate in candidates):
        return INVALID
    return OK


def _subschemas(schema: dict, keyword: str):
    subschemas = schema.get(keyword)
    if isinstance(subschemas, list):
        yield from enumerate(subschemas)


def _subschema_map(schema: dict, keyword: str):
    subschemas = schema.get(keyword)
    if isinstance(subschemas, dict):
        yield from subschemas.items()


def _specification_for(validator: Validator):
    return referencing.jsonschema.specification_with(
        validator.ID_OF(validator.META_SCHEMA) or "urn:unknown-dialect",
        default=referencing.Specification.OPAQUE,
    )


def _evolve(validator: Validator, schema: Any) -> Validator:
    if validator._ref_resolver is not None:
        return validator.evolve(schema=schema)
    resolver = validator._resolver.in_subresource(
        _specification_for(validator).create_resource(schema),
    )
    return validator.evolve(schema=schema, _resolver=resolver)


def _descend(
    validator: Validator,
    instance: Any,
    schema: Any,
    path: tuple,
    schema_path: tuple,
    branch: str | None,
    missing: tuple | None,
    found: dict,
    seen: frozenset,
) -> None:
    if schema is True or schema is False:
        return
    _collect(
        _evolve(validator, schema),
        instance,
        path=path,
        schema_path=schema_path,
        branch=branch,
        missing=missing,
        found=found,
        seen=seen,
    )


def _collect(
    validator: Validator,
    instance: Any,
    path: tuple,
    schema_path: tuple,
    branch: str | None,
    missing: tuple | None,
    found: dict,
    seen: frozenset,
) -> None:
    schema = validator.schema
    if not isinstance(schema, dict):
        return

    key = (id(schema), id(instance))
    if key in seen:
        return
    seen = seen | {key}

    if missing is not None and "default" in schema:
        value = deepcopy(schema["default"])
        errors = tuple(
            error.message for error in validator.iter_errors(value)
        )
        found.setdefault(missing, []).append(
            Candidate(
                value=value,
                schema_path=schema_path,
                branch=branch,
                errors=errors,
            ),
        )

    ref = schema.get("$ref")
    if isinstance(ref, str):
        if validator._ref_resolver is None:
            try:
                resolved = validator._resolver.lookup(ref)
            except referencing.exceptions.Unresolvable:
                pass
            else:
                _collect(
                    validator.evolve(
                        schema=resolved.contents,
                        _resolver=resolved.resolver,
                    ),
                    instance,
                    path=path,
                    schema_path=schema_path + ("$ref",),
                    branch=branch,
                    missing=missing,
                    found=found,
                    seen=seen,
                )
        else:  # pragma: no cover -- legacy RefResolver behavior
            with validator._ref_resolver.resolving(ref) as resolved_schema:
                _collect(
                    validator.evolve(schema=resolved_schema),
                    instance,
                    path=path,
                    schema_path=schema_path + ("$ref",),
                    branch=branch,
                    missing=missing,
                    found=found,
                    seen=seen,
                )

    for index, subschema in _subschemas(schema, "allOf"):
        _descend(
            validator,
            instance,
            subschema,
            path=path,
            schema_path=schema_path + ("allOf", index),
            branch=f"allOf[{index}]",
            missing=missing,
            found=found,
            seen=seen,
        )

    matching_oneOf = [
        (index, subschema)
        for index, subschema in _subschemas(schema, "oneOf")
        if _evolve(validator, subschema).is_valid(instance)
    ]
    if len(matching_oneOf) == 1:
        ((index, subschema),) = matching_oneOf
        _descend(
            validator,
            instance,
            subschema,
            path=path,
            schema_path=schema_path + ("oneOf", index),
            branch=f"oneOf[{index}]",
            missing=missing,
            found=found,
            seen=seen,
        )

    for index, subschema in _subschemas(schema, "anyOf"):
        if _evolve(validator, subschema).is_valid(instance):
            _descend(
                validator,
                instance,
                subschema,
                path=path,
                schema_path=schema_path + ("anyOf", index),
                branch=f"anyOf[{index}]",
                missing=missing,
                found=found,
                seen=seen,
            )

    if "if" in schema:
        if _evolve(validator, schema["if"]).is_valid(instance):
            if "then" in schema:
                _descend(
                    validator,
                    instance,
                    schema["then"],
                    path=path,
                    schema_path=schema_path + ("then",),
                    branch="then",
                    missing=missing,
                    found=found,
                    seen=seen,
                )
        elif "else" in schema:
            _descend(
                validator,
                instance,
                schema["else"],
                path=path,
                schema_path=schema_path + ("else",),
                branch="else",
                missing=missing,
                found=found,
                seen=seen,
            )

    if validator.is_type(instance, "object"):
        for property, subschema in _subschema_map(schema, "properties"):
            property_schema_path = schema_path + ("properties", property)
            if property in instance:
                _descend(
                    validator,
                    instance[property],
                    subschema,
                    path=path + (property,),
                    schema_path=property_schema_path,
                    branch=branch,
                    missing=None,
                    found=found,
                    seen=seen,
                )
            else:
                _descend(
                    validator,
                    _EMPTY,
                    subschema,
                    path=path + (property,),
                    schema_path=property_schema_path,
                    branch=branch,
                    missing=(path, property),
                    found=found,
                    seen=seen,
                )

        for pattern, subschema in _subschema_map(schema, "patternProperties"):
            for property, value in instance.items():
                if re.search(pattern, property):
                    _descend(
                        validator,
                        value,
                        subschema,
                        path=path + (property,),
                        schema_path=schema_path
                        + ("patternProperties", pattern),
                        branch=branch,
                        missing=None,
                        found=found,
                        seen=seen,
                    )

        additional = schema.get("additionalProperties")
        if isinstance(additional, dict):
            for property in find_additional_properties(instance, schema):
                _descend(
                    validator,
                    instance[property],
                    additional,
                    path=path + (property,),
                    schema_path=schema_path + ("additionalProperties",),
                    branch=branch,
                    missing=None,
                    found=found,
                    seen=seen,
                )

        for property, subschema in _subschema_map(schema, "dependentSchemas"):
            if property in instance:
                _descend(
                    validator,
                    instance,
                    subschema,
                    path=path,
                    schema_path=schema_path + ("dependentSchemas", property),
                    branch=branch,
                    missing=missing,
                    found=found,
                    seen=seen,
                )

    if validator.is_type(instance, "array"):
        prefixItems = schema.get("prefixItems")
        if isinstance(prefixItems, list):
            for index, (subschema, item) in enumerate(
                zip(prefixItems, instance),
            ):
                _descend(
                    validator,
                    item,
                    subschema,
                    path=path + (index,),
                    schema_path=schema_path + ("prefixItems", index),
                    branch=branch,
                    missing=None,
                    found=found,
                    seen=seen,
                )

        items = schema.get("items")
        if isinstance(items, list):  # pragma: no cover -- legacy drafts
            for index, (subschema, item) in enumerate(zip(items, instance)):
                _descend(
                    validator,
                    item,
                    subschema,
                    path=path + (index,),
                    schema_path=schema_path + ("items", index),
                    branch=branch,
                    missing=None,
                    found=found,
                    seen=seen,
                )
        elif items is not None:
            start = len(prefixItems) if isinstance(prefixItems, list) else 0
            for index in range(start, len(instance)):
                _descend(
                    validator,
                    instance[index],
                    items,
                    path=path + (index,),
                    schema_path=schema_path + ("items",),
                    branch=branch,
                    missing=None,
                    found=found,
                    seen=seen,
                )
