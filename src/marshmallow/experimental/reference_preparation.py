"""Explicit preparation checks for string-based :class:`Nested
<marshmallow.fields.Nested>` references (experimental).

Large code bases frequently refer to nested schemas by string instead of by
class, e.g. ``fields.Nested("AuthorSchema")``. The actual lookup against
:mod:`marshmallow.class_registry` only happens lazily, the first time the
nested schema is instantiated. That makes the following problems hard to
diagnose:

- the referenced name does not exist in the registry;
- several schema classes share the same bare class name, so the reference is
  ambiguous;
- references form long chains or cycles that are spread across modules.

This module walks a schema's declared fields *without* instantiating any
nested schema and reports, for every nested reference, the candidate
definitions found in the registry and the definition it would ultimately bind
to. Missing and ambiguous references are collected and reported together with
the declaration path of the offending field. Self references and mutually
recursive schemas are valid: when traversal reaches a schema that has already
been visited, the reference is recorded as a back edge instead.

The check is read-only. It never mutates :mod:`marshmallow.class_registry`
and never eagerly instantiates the nested schema graph, so references that
already load keep binding to exactly the same class.

Example usage:

.. code-block:: python

    from marshmallow import Schema, fields
    from marshmallow.experimental.reference_preparation import (
        prepare_references,
    )


    class BookSchema(Schema):
        title = fields.String()
        author = fields.Nested("AuthorSchema")


    report = prepare_references(BookSchema)
    for reference in report.references:
        print(reference.declaration_path, reference.ref, reference.bound)

Pass ``strict=True`` (or use :func:`check_references`) to raise a
:class:`ReferencePreparationError` listing every unresolved reference.
"""

# ruff: noqa: SLF001
from __future__ import annotations

import dataclasses
import typing

from marshmallow import class_registry
from marshmallow.exceptions import RegistryError

if typing.TYPE_CHECKING:
    from marshmallow.fields import Field, Nested
    from marshmallow.schema import Schema, SchemaMeta


class ReferencePreparationError(RegistryError):
    """Raised when one or more nested string references cannot be prepared.

    :ivar problems: The unresolved references collected during preparation.
    """

    def __init__(self, problems: list[NestedReference]) -> None:
        self.problems: list[NestedReference] = problems
        lines = ["Nested schema references could not be prepared:"]
        for problem in problems:
            candidates = ", ".join(
                _qualified_name(candidate) for candidate in problem.candidates
            ) or "<none>"
            lines.append(
                "  - "
                f"{problem.declaration_path}: {problem.ref!r} ({problem.problem}); "
                f"candidates: {candidates}"
            )
        super().__init__("\n".join(lines))


@dataclasses.dataclass(frozen=True)
class NestedReference:
    """A single nested reference discovered while preparing a schema.

    :ivar declaration_path: Dot-separated path of field names from the root
        schema to the declaring :class:`~marshmallow.fields.Nested` field.
    :ivar field_name: Name of the field on the schema that declares the
        reference.
    :ivar ref: The string reference exactly as declared on the field, or the
        string returned by a callable reference.
    :ivar candidates: All schema classes registered under ``ref`` at
        preparation time.
    :ivar bound: The schema class the reference resolves to, or ``None`` when
        the reference is missing or ambiguous.
    :ivar is_back_edge: ``True`` when ``bound`` had already been visited, so
        its fields were not traversed again. This is how self references and
        mutual recursion are represented.
    :ivar problem: ``"missing"`` when no class is registered under ``ref``,
        ``"ambiguous"`` when several classes match, otherwise ``None``.
    """

    declaration_path: str
    field_name: str | None
    ref: str
    candidates: tuple[SchemaMeta, ...]
    bound: SchemaMeta | None
    is_back_edge: bool
    problem: str | None


@dataclasses.dataclass(frozen=True)
class ReferenceReport:
    """Result of preparing the nested references of a schema.

    :ivar root: The schema class the preparation started from.
    :ivar references: Every nested reference discovered during traversal, in
        declaration order.
    :ivar visited: Every schema class reached by the traversal.
    """

    root: SchemaMeta
    references: tuple[NestedReference, ...]
    visited: tuple[SchemaMeta, ...]

    @property
    def problems(self) -> tuple[NestedReference, ...]:
        """The references that could not be bound to a single schema class."""
        return tuple(
            reference
            for reference in self.references
            if reference.problem is not None
        )

    @property
    def back_edges(self) -> tuple[NestedReference, ...]:
        """The references pointing back at an already visited schema class."""
        return tuple(
            reference for reference in self.references if reference.is_back_edge
        )


def _qualified_name(schema_class: SchemaMeta) -> str:
    return f"{schema_class.__module__}.{schema_class.__name__}"


def _schema_class(schema: Schema | SchemaMeta) -> SchemaMeta:
    from marshmallow.schema import Schema  # noqa: PLC0415

    if isinstance(schema, Schema):
        return type(schema)
    return schema


def _iter_nested_fields(field: Field) -> typing.Iterator[tuple[str, Nested]]:
    """Yield ``(path_segment, nested_field)`` pairs contained in ``field``.

    Container fields are descended into without copying or binding them, so
    the traversal stays side-effect free.

    The segment describes how to reach the nested field from the declaring
    field: it is empty for a plain :class:`~marshmallow.fields.Nested` and
    carries container markers such as ``"[]"`` for wrapped fields.
    """
    from marshmallow.fields import List, Mapping, Nested, Tuple  # noqa: PLC0415

    if isinstance(field, Nested):
        yield "", field
        return
    if isinstance(field, List):
        yield from (
            (f"[].{segment}" if segment else "[]", nested)
            for segment, nested in _iter_nested_fields(field.inner)
        )
        return
    if isinstance(field, Tuple):
        for index, inner in enumerate(field.tuple_fields):
            for segment, nested in _iter_nested_fields(inner):
                prefix = f"[{index}]"
                yield (
                    f"{prefix}.{segment}" if segment else prefix, nested
                )
        return
    if isinstance(field, Mapping):
        if field.value_field is not None:
            for segment, nested in _iter_nested_fields(field.value_field):
                yield (
                    f"{{}}.{segment}" if segment else "{}",
                    nested,
                )
        if field.key_field is not None:
            for segment, nested in _iter_nested_fields(field.key_field):
                yield (
                    f"{{key}}.{segment}" if segment else "{key}",
                    nested,
                )


def _resolve_ref(nested: Nested) -> tuple[str | None, SchemaMeta | None]:
    """Resolve the raw ``nested`` argument without instantiating schemas.

    Returns ``(string_ref, schema_class)``. Exactly one value is set: string
    references return the string, class/instance/dict/callable references
    return the schema class they point at. Only user supplied callables are
    invoked, and their return value is never instantiated recursively.
    """
    from marshmallow.schema import Schema, SchemaMeta  # noqa: PLC0415

    raw = nested.nested
    if isinstance(raw, str):
        return raw, None
    if isinstance(raw, bytes):
        return raw.decode("utf-8"), None
    if isinstance(raw, SchemaMeta):
        return None, raw
    if isinstance(raw, Schema):
        return None, type(raw)
    if isinstance(raw, dict):
        return None, Schema.from_dict(raw)
    if callable(raw):
        resolved = raw()
        if isinstance(resolved, str):
            return resolved, None
        if isinstance(resolved, bytes):
            return resolved.decode("utf-8"), None
        if isinstance(resolved, SchemaMeta):
            return None, resolved
        if isinstance(resolved, Schema):
            return None, type(resolved)
        if isinstance(resolved, dict):
            return None, Schema.from_dict(resolved)
        raise ValueError(
            "`Nested` callable must return a Schema, schema class, string "
            f"reference, or dictionary; got {resolved!r}."
        )
    # Nested itself rejects anything else when its schema is first accessed.
    raise ValueError(
        "`Nested` fields must be passed a `Schema`, schema class, string "
        f"reference, dictionary, or callable; got {raw!r}."
    )


def _lookup_candidates(ref: str) -> tuple[SchemaMeta, ...]:
    return tuple(class_registry._registry.get(ref, ()))


def prepare_references(
    schema: Schema | SchemaMeta, *, strict: bool = False
) -> ReferenceReport:
    """Prepare and inspect every nested reference reachable from ``schema``.

    The schema's declared fields are traversed statically. String references
    are looked up in :mod:`marshmallow.class_registry`; each result records
    the candidate definitions and the single class the reference binds to.
    Already visited schemas are not traversed again, and the reference that
    leads back to them is flagged as a back edge, so self references and
    mutual recursion terminate naturally.

    :param schema: The root schema class or schema instance to prepare.
    :param strict: When ``True``, raise :class:`ReferencePreparationError`
        if any reference is missing or ambiguous instead of returning a
        report containing the problems.
    :raises ReferencePreparationError: If ``strict`` is ``True`` and at least
        one reference cannot be bound.
    """
    root = _schema_class(schema)
    references: list[NestedReference] = []
    visited: set[SchemaMeta] = set()

    # Explicit depth-first traversal. Each frame keeps an iterator over the
    # nested references of one schema class and the declaration path at which
    # it was reached. Resolving a reference before descending means a target
    # already fully visited (a self reference, a mutual recursion edge, or the
    # second branch of a diamond) is detected at the moment the reference is
    # read and recorded as a back edge. An explicit stack also avoids
    # recursion limits on deep, acyclic schema graphs.

    def _reference_iter(
        current_schema: SchemaMeta, base_path: str
    ) -> typing.Iterator[NestedReference]:
        for field_name, field in current_schema._declared_fields.items():
            for segment, nested_field in _iter_nested_fields(field):
                local_path = f"{field_name}{segment}"
                declaration_path = (
                    f"{base_path}.{local_path}" if base_path else local_path
                )
                ref, direct_class = _resolve_ref(nested_field)

                if direct_class is not None:
                    candidates = (direct_class,)
                    bound: SchemaMeta | None = direct_class
                    problem: str | None = None
                else:
                    assert ref is not None
                    candidates = _lookup_candidates(ref)
                    if not candidates:
                        bound = None
                        problem = "missing"
                    elif len(candidates) > 1:
                        bound = None
                        problem = "ambiguous"
                    else:
                        bound = candidates[0]
                        problem = None

                yield NestedReference(
                    declaration_path=declaration_path,
                    field_name=field_name,
                    ref=ref if ref is not None else _qualified_name(bound),
                    candidates=candidates,
                    bound=bound,
                    is_back_edge=bound is not None and bound in visited,
                    problem=problem,
                )

    stack: list[typing.Iterator[NestedReference]] = [_reference_iter(root, "")]
    visited.add(root)

    while stack:
        reference_iter = stack[-1]
        try:
            reference = next(reference_iter)
        except StopIteration:
            stack.pop()
            continue

        references.append(reference)
        bound = reference.bound
        if bound is not None and bound not in visited:
            visited.add(bound)
            stack.append(_reference_iter(bound, reference.declaration_path))

    report = ReferenceReport(
        root=root,
        references=tuple(references),
        visited=tuple(visited),
    )
    if strict and report.problems:
        raise ReferencePreparationError(list(report.problems))
    return report


def check_references(schema: Schema | SchemaMeta) -> ReferenceReport:
    """Prepare nested references and raise if any cannot be bound.

    This is a thin wrapper around :func:`prepare_references` with
    ``strict=True`` for callers that only want the validation behavior.
    """
    return prepare_references(schema, strict=True)
