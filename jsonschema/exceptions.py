"""
Validation errors, and some surrounding helpers.
"""

from __future__ import annotations

from collections import defaultdict, deque
from collections.abc import Mapping, Sequence
from pprint import pformat
from textwrap import dedent, indent
from typing import TYPE_CHECKING, Any, ClassVar
from urllib.parse import unquote, urldefrag
import re
import warnings

from attrs import define, field
from referencing.exceptions import Unresolvable as _Unresolvable

from jsonschema import _utils

if TYPE_CHECKING:
    from collections.abc import (
        Callable,
        Iterable,
        MutableMapping,
    )

    from jsonschema import _types

WEAK_MATCHES: frozenset[str] = frozenset(["anyOf", "oneOf"])
STRONG_MATCHES: frozenset[str] = frozenset()

_JSON_PATH_COMPATIBLE_PROPERTY_PATTERN = re.compile("^[a-zA-Z][a-zA-Z0-9_]*$")

_unset = _utils.Unset()


def _pretty(thing: Any, prefix: str):
    """
    Format something for an error message as prettily as we currently can.
    """
    return indent(pformat(thing, width=72, sort_dicts=False), prefix).lstrip()


def _json_pointer(path: Iterable[str | int]) -> str:
    """
    Format a sequence of path segments as a JSON Pointer.
    """
    return "".join(
        "/" + str(each).replace("~", "~0").replace("/", "~1")
        for each in path
    )


def _format_location(resource: str, path: Iterable[str | int]) -> str:
    """
    Render a resource URI and path as a single location.
    """
    return f"{resource}#{_json_pointer(path)}"


def _pointer_segments(fragment: str) -> tuple[str | int, ...]:
    """
    Split a URI fragment which is a JSON Pointer into path segments.
    """
    if not fragment.startswith("/"):
        return ()

    return tuple(
        segment.replace("~1", "/").replace("~0", "~")
        for segment in (
            unquote(each) for each in fragment[1:].split("/")
        )
    )


@define(frozen=True, eq=False)
class ReferenceStep:
    """
    One hop of a chain of ``$ref`` resolutions.

    ``source`` identifies the resource in which the reference was used,
    ``ref`` is the text of the reference as written, and ``target`` is
    the absolute location to which it resolved. ``source_path`` is the
    path of the ``$ref`` keyword within ``source`` and ``target_path`` is
    the path at which the referenced schema begins within the target
    resource.
    """

    source: str
    ref: str
    target: str
    source_path: tuple[str | int, ...] = ()
    target_path: tuple[str | int, ...] = ()
    target_resource: str = ""
    fragment: str = ""
    source_schema: Any = field(default=None, repr=False, eq=False)
    target_schema: Any = field(default=None, repr=False, eq=False)

    @property
    def source_resource(self) -> str:
        return self.source

    @property
    def source_uri(self) -> str:
        return self.source

    @property
    def from_resource(self) -> str:
        return self.source

    @property
    def reference(self) -> str:
        return self.ref

    @property
    def reference_text(self) -> str:
        return self.ref

    @property
    def target_uri(self) -> str:
        return self.target

    @property
    def resolved(self) -> str:
        return self.target

    @property
    def resolution_target(self) -> str:
        return self.target

    @property
    def resolved_target(self) -> str:
        return self.target

    @property
    def resolved_uri(self) -> str:
        return self.target

    @property
    def from_uri(self) -> str:
        return self.source

    @property
    def to_uri(self) -> str:
        return self.target

    @property
    def from_path(self) -> tuple[str | int, ...]:
        return self.source_path

    @property
    def to_path(self) -> tuple[str | int, ...]:
        return self.target_path

    @property
    def contents(self) -> Any:
        return self.target_schema

    @property
    def source_location(self) -> str:
        return _format_location(self.source, self.source_path)

    @property
    def source_pointer(self) -> str:
        return _json_pointer(self.source_path)

    @property
    def target_pointer(self) -> str:
        return _json_pointer(self.target_path)

    @property
    def target_location(self) -> str:
        resource = self.target_resource or urldefrag(self.target)[0]
        return _format_location(resource, self.target_path)

    @property
    def location(self) -> str:
        return self.source_location

    @property
    def path(self) -> tuple[str | int, ...]:
        return self.source_path

    def __getitem__(self, key):
        if isinstance(key, (int, slice)):
            return (self.source, self.ref, self.target)[key]
        if not isinstance(key, str):
            raise TypeError(
                "ReferenceStep indices must be strings, integers, or slices",
            )
        return getattr(self, key)

    def get(self, key: str, default: Any = None) -> Any:
        return getattr(self, key, default)

    def to_dict(self) -> dict[str, Any]:
        return {
            "source": self.source,
            "source_resource": self.source,
            "ref": self.ref,
            "reference": self.ref,
            "target": self.target,
            "target_uri": self.target,
            "source_path": list(self.source_path),
            "target_path": list(self.target_path),
            "source_location": self.source_location,
            "target_location": self.target_location,
        }

    def __eq__(self, other: object) -> bool:
        if isinstance(other, ReferenceStep):
            return (
                self.source,
                self.ref,
                self.target,
                self.source_path,
                self.target_path,
                self.target_resource,
                self.fragment,
            ) == (
                other.source,
                other.ref,
                other.target,
                other.source_path,
                other.target_path,
                other.target_resource,
                other.fragment,
            )
        if isinstance(other, Mapping):
            return all(
                getattr(self, key, _unset) == value
                for key, value in other.items()
            )
        return NotImplemented

    def __hash__(self) -> int:
        return hash(
            (
                self.source,
                self.ref,
                self.target,
                self.source_path,
                self.target_path,
                self.target_resource,
                self.fragment,
            ),
        )

    def __repr__(self) -> str:
        return (
            f"<ReferenceStep {self.source_location} "
            f"--{self.ref}--> {self.target_location}>"
        )

    def __str__(self) -> str:
        return (
            f"{self.source_location} "
            f"--{self.ref}--> {self.target_location}"
        )


def _coerce_reference_step(value) -> ReferenceStep:
    if isinstance(value, ReferenceStep):
        return value
    if not isinstance(value, Mapping):
        return value

    source = value.get(
        "source",
        value.get(
            "source_resource",
            value.get("source_uri", value.get("from_resource", "")),
        ),
    )
    ref = value.get(
        "ref",
        value.get(
            "reference",
            value.get("reference_text", value.get("ref_text", "")),
        ),
    )
    target = value.get(
        "target",
        value.get(
            "target_uri",
            value.get(
                "resolution_target",
                value.get("resolved_target", value.get("resolved", "")),
            ),
        ),
    )
    target_resource, fragment = urldefrag(target)
    return ReferenceStep(
        source=source,
        ref=ref,
        target=target,
        source_path=tuple(value.get("source_path", value.get("path", ()))),
        target_path=tuple(value.get("target_path", ())),
        target_resource=value.get("target_resource", target_resource),
        fragment=value.get("fragment", fragment),
        source_schema=value.get("source_schema"),
        target_schema=value.get("target_schema", value.get("contents")),
    )


def _reference_steps(values) -> tuple[ReferenceStep, ...]:
    if isinstance(values, ReferenceChain):
        return values.steps
    return tuple(_coerce_reference_step(each) for each in values)


@define(frozen=True, eq=False)
class ReferenceChain(Sequence[ReferenceStep]):
    """
    A finite, runtime view of the references followed to an error.

    Each step preserves the source resource, the reference as written
    (or as dynamically resolved), and the resolved target. Recursion
    therefore appears only as far as it was actually traversed while
    validating a particular instance.
    """

    steps: tuple[ReferenceStep, ...] = field(
        default=(),
        converter=_reference_steps,
    )

    def __len__(self) -> int:
        return len(self.steps)

    def __getitem__(self, index):
        return self.steps[index]

    def __iter__(self):
        return iter(self.steps)

    def __bool__(self) -> bool:
        return bool(self.steps)

    def __eq__(self, other: object) -> bool:
        if isinstance(other, ReferenceChain):
            return self.steps == other.steps
        if isinstance(other, (list, tuple)):
            return len(self.steps) == len(other) and all(
                step == expected
                for step, expected in zip(self.steps, other)
            )
        return NotImplemented

    def __hash__(self) -> int:
        return hash(self.steps)

    @property
    def entry(self) -> ReferenceStep | None:
        return self.steps[0] if self.steps else None

    @property
    def first(self) -> ReferenceStep | None:
        return self.entry

    @property
    def last(self) -> ReferenceStep | None:
        return self.steps[-1] if self.steps else None

    @property
    def source(self) -> str:
        return self.entry.source if self.entry is not None else ""

    @property
    def source_resource(self) -> str:
        return self.source

    @property
    def entry_location(self) -> str:
        return self.entry.source_location if self.entry is not None else ""

    @property
    def target(self) -> str:
        return self.last.target if self.last is not None else ""

    @property
    def target_location(self) -> str:
        return self.last.target_location if self.last is not None else ""

    @property
    def refs(self) -> tuple[str, ...]:
        return tuple(step.ref for step in self.steps)

    @property
    def locations(self) -> tuple[str, ...]:
        return tuple(step.source_location for step in self.steps)

    def to_dict(self) -> dict[str, Any]:
        return {
            "entry": self.entry_location,
            "target": self.target_location,
            "steps": [step.to_dict() for step in self.steps],
        }

    def __repr__(self) -> str:
        return f"<ReferenceChain {self.steps!r}>"

    def __str__(self) -> str:
        return " -> ".join(
            f"{step.source_location} --{step.ref}--> {step.target_location}"
            for step in self.steps
        )


def __getattr__(name):
    if name == "RefResolutionError":
        warnings.warn(
            _RefResolutionError._DEPRECATION_MESSAGE,
            DeprecationWarning,
            stacklevel=2,
        )
        return _RefResolutionError
    raise AttributeError(f"module {__name__} has no attribute {name}")


class _Error(Exception):
    _word_for_schema_in_error_message: ClassVar[str]
    _word_for_instance_in_error_message: ClassVar[str]

    def __init__(
        self,
        message: str,
        validator: str = _unset,  # type: ignore[assignment]
        path: Iterable[str | int] = (),
        cause: Exception | None = None,
        context=(),
        validator_value: Any = _unset,
        instance: Any = _unset,
        schema: Mapping[str, Any] | bool = _unset,  # type: ignore[assignment]
        schema_path: Iterable[str | int] = (),
        parent: _Error | None = None,
        type_checker: _types.TypeChecker = _unset,  # type: ignore[assignment]
        schema_uri: str | None = _unset,  # type: ignore[assignment]
        declaration_path: Iterable[str | int] | None = None,
        ref_chain: Iterable[ReferenceStep] = (),
    ) -> None:
        super().__init__(
            message,
            validator,
            path,
            cause,
            context,
            validator_value,
            instance,
            schema,
            schema_path,
            parent,
        )
        self.message = message
        self.path = self.relative_path = deque(path)
        self.schema_path = self.relative_schema_path = deque(schema_path)
        self.context = list(context)
        self.cause = self.__cause__ = cause
        self.validator = validator
        self.validator_value = validator_value
        self.instance = instance
        self.schema = schema
        self.parent = parent
        self._type_checker = type_checker
        self.schema_uri = schema_uri
        self._declaration_path = (
            None if declaration_path is None else tuple(declaration_path)
        )
        self._ref_chain = ReferenceChain(ref_chain).steps

        for error in context:
            error.parent = self

    def __repr__(self) -> str:
        return f"<{self.__class__.__name__}: {self.message!r}>"

    def __str__(self) -> str:
        essential_for_verbose = (
            self.validator,
            self.validator_value,
            self.instance,
            self.schema,
        )
        if any(m is _unset for m in essential_for_verbose):
            return self.message

        schema_path = _utils.format_as_index(
            container=self._word_for_schema_in_error_message,
            indices=list(self.relative_schema_path)[:-1],
        )
        instance_path = _utils.format_as_index(
            container=self._word_for_instance_in_error_message,
            indices=self.relative_path,
        )
        prefix = 16 * " "

        rendered = dedent(
            f"""\
            {self.message}

            Failed validating {self.validator!r} in {schema_path}:
                {_pretty(self.schema, prefix=prefix)}

            On {instance_path}:
                {_pretty(self.instance, prefix=prefix)}
            """.rstrip(),
        )
        chains = self.ref_chains
        if chains:
            if len(chains) == 1:
                heading = "Reference chain:"
            else:
                heading = "Reference chains:"
            lines = ["", heading]
            for chain in chains:
                lines.append(f"  entry: {chain.entry_location}")
                lines.extend(f"    {step}" for step in chain.steps)
                declaration = None
                for error in self.context:
                    if (
                        error.ref_chain == chain
                        and error._declaration_path is not None
                    ):
                        declaration = error.declaration_location
                        break
                if chain == self.ref_chain and self._ref_chain:
                    declaration = self.declaration_location
                elif declaration is None and self._declaration_path is not None:
                    declaration = self.declaration_location
                if declaration is not None:
                    lines.append(
                        f"  declaration: {declaration}",
                    )
                elif chain.last is not None:
                    lines.append(
                        f"  declaration: {chain.last.target_location}",
                    )
            rendered += "\n" + "\n".join(lines)
        return rendered

    @classmethod
    def create_from(cls, other: _Error):
        return cls(**other._contents())

    @property
    def absolute_path(self) -> Sequence[str | int]:
        parent = self.parent
        if parent is None:
            return self.relative_path

        path = deque(self.relative_path)
        path.extendleft(reversed(parent.absolute_path))
        return path

    @property
    def absolute_schema_path(self) -> Sequence[str | int]:
        parent = self.parent
        if parent is None:
            return self.relative_schema_path

        path = deque(self.relative_schema_path)
        path.extendleft(reversed(parent.absolute_schema_path))
        return path

    @property
    def json_path(self) -> str:
        path = "$"
        for elem in self.absolute_path:
            if isinstance(elem, int):
                path += "[" + str(elem) + "]"
            elif _JSON_PATH_COMPATIBLE_PROPERTY_PATTERN.match(elem):
                path += "." + elem
            else:
                escaped_elem = elem.replace("\\", "\\\\").replace("'", r"\'")
                path += "['" + escaped_elem + "']"
        return path

    @property
    def schema_location(self) -> str:
        """
        The location of the failed constraint, including its resource.
        """
        return self.declaration_location

    @property
    def declaration_path(self) -> tuple[str | int, ...]:
        """
        The path of the failed constraint within its declaring resource.
        """
        if self._declaration_path is not None:
            return self._declaration_path
        return tuple(self.absolute_schema_path)

    @property
    def constraint_path(self) -> tuple[str | int, ...]:
        return self.declaration_path

    @property
    def declaration_location(self) -> str:
        """
        The resource and path at which the failed constraint was declared.
        """
        resource = "" if self.schema_uri is _unset else (self.schema_uri or "")
        return _format_location(resource, self.declaration_path)

    @property
    def constraint_location(self) -> str:
        return self.declaration_location

    @property
    def final_location(self) -> str:
        return self.declaration_location

    @property
    def schema_resource(self) -> str:
        return "" if self.schema_uri is _unset else (self.schema_uri or "")

    @property
    def ref_chain(self) -> ReferenceChain:
        """
        The references traversed to reach this error.

        When the error itself was produced at the end of a chain, this is
        that chain. For an applicator error, it is the first of the
        distinct chains found in its context (see `ref_chains`).
        """
        if self._ref_chain:
            return ReferenceChain(self._ref_chain)
        chains = self.ref_chains
        return chains[0] if chains else ReferenceChain(())

    @property
    def reference_chain(self) -> ReferenceChain:
        return self.ref_chain

    @property
    def ref_chains(self) -> tuple[ReferenceChain, ...]:
        """
        All distinct reference chains which contributed to this error.
        """
        chains: list[ReferenceChain] = []
        if self._ref_chain:
            chains.append(ReferenceChain(self._ref_chain))
        for error in self.context:
            chains.extend(error.ref_chains)

        distinct: list[ReferenceChain] = []
        for chain in chains:
            if chain not in distinct:
                distinct.append(chain)
        return tuple(distinct)

    @property
    def ref_steps(self) -> tuple[ReferenceStep, ...]:
        return self.ref_chain.steps

    @property
    def entry_location(self) -> str:
        """
        The location at which the entry reference was used.
        """
        chains = self.ref_chains
        if not chains:
            return ""
        return chains[0].entry_location

    @property
    def entry_usage_location(self) -> str:
        return self.entry_location

    @property
    def entry_locations(self) -> tuple[str, ...]:
        return tuple(chain.entry_location for chain in self.ref_chains)

    @property
    def entry(self) -> ReferenceStep | None:
        return self.ref_chain.entry

    @property
    def entry_step(self) -> ReferenceStep | None:
        return self.entry

    @property
    def final_step(self) -> ReferenceStep | None:
        return self.ref_chain.last

    @property
    def target_location(self) -> str:
        return self.ref_chain.target_location

    @property
    def usage_location(self) -> str:
        return self.entry_location

    @property
    def entry_schema_location(self) -> str:
        return self.entry_location

    @property
    def entry_uri(self) -> str:
        return self.entry_resource

    @property
    def entry_path(self) -> tuple[str | int, ...]:
        return self.entry_schema_path

    @property
    def entry_pointer(self) -> str:
        return _json_pointer(self.entry_schema_path)

    @property
    def entry_resource(self) -> str:
        chains = self.ref_chains
        return chains[0].source if chains else ""

    @property
    def entry_schema_path(self) -> tuple[str | int, ...]:
        chains = self.ref_chains
        if not chains or chains[0].entry is None:
            return ()
        return chains[0].entry.source_path

    @property
    def declaration_schema_path(self) -> tuple[str | int, ...]:
        return self.declaration_path

    @property
    def constraint_schema_path(self) -> tuple[str | int, ...]:
        return self.declaration_path

    @property
    def declaration_pointer(self) -> str:
        return _json_pointer(self.declaration_path)

    @property
    def declaration_uri(self) -> str:
        return self.schema_resource

    @property
    def declaration_resource(self) -> str:
        return self.schema_resource

    @property
    def chain(self) -> ReferenceChain:
        return self.ref_chain

    @property
    def chains(self) -> tuple[ReferenceChain, ...]:
        return self.ref_chains

    @property
    def refs(self) -> tuple[ReferenceStep, ...]:
        return self.ref_steps

    def _prepend_ref_step(self, step: ReferenceStep) -> None:
        self._ref_chain = (step, *self._ref_chain)

    def _set(
        self,
        type_checker: _types.TypeChecker | None = None,
        **kwargs: Any,
    ) -> None:
        if type_checker is not None and self._type_checker is _unset:
            self._type_checker = type_checker

        for k, v in kwargs.items():
            if getattr(self, k) is _unset:
                setattr(self, k, v)

    def _contents(self):
        attrs = (
            "message",
            "cause",
            "context",
            "validator",
            "validator_value",
            "path",
            "schema_path",
            "instance",
            "schema",
            "parent",
        )
        contents = {attr: getattr(self, attr) for attr in attrs}
        if self._ref_chain or (
            self.schema_uri is not _unset and self.schema_uri
        ):
            contents["schema_uri"] = self.schema_uri
            contents["declaration_path"] = self._declaration_path
        if self._ref_chain:
            contents["ref_chain"] = self._ref_chain
        return contents

    def _matches_type(self) -> bool:
        try:
            # We ignore this as we want to simply crash if this happens
            expected = self.schema["type"]  # type: ignore[index]
        except (KeyError, TypeError):
            return False

        if isinstance(expected, str):
            return self._type_checker.is_type(self.instance, expected)

        return any(
            self._type_checker.is_type(self.instance, expected_type)
            for expected_type in expected
        )


class ValidationError(_Error):
    """
    An instance was invalid under a provided schema.
    """

    _word_for_schema_in_error_message = "schema"
    _word_for_instance_in_error_message = "instance"


class SchemaError(_Error):
    """
    A schema was invalid under its corresponding metaschema.
    """

    _word_for_schema_in_error_message = "metaschema"
    _word_for_instance_in_error_message = "schema"


@define(slots=False)
class _RefResolutionError(Exception):  # noqa: PLW1641
    """
    A ref could not be resolved.
    """

    _DEPRECATION_MESSAGE = (
        "jsonschema.exceptions.RefResolutionError is deprecated as of version "
        "4.18.0. If you wish to catch potential reference resolution errors, "
        "directly catch referencing.exceptions.Unresolvable."
    )

    _cause: Exception

    def __eq__(self, other):
        if self.__class__ is not other.__class__:
            return (
                NotImplemented  # pragma: no cover -- uncovered but deprecated
            )
        return self._cause == other._cause

    def __str__(self) -> str:
        return str(self._cause)


class _WrappedReferencingError(
    _RefResolutionError,
    _Unresolvable,
):  # pragma: no cover -- partially uncovered but to be removed
    def __init__(self, cause: _Unresolvable):
        object.__setattr__(self, "_wrapped", cause)

    def __eq__(self, other):
        if other.__class__ is self.__class__:
            return self._wrapped == other._wrapped
        elif other.__class__ is self._wrapped.__class__:
            return self._wrapped == other
        return NotImplemented

    def __getattr__(self, attr):
        return getattr(self._wrapped, attr)

    def __hash__(self):
        return hash(self._wrapped)

    def __repr__(self):
        return f"<WrappedReferencingError {self._wrapped!r}>"

    def __str__(self):
        return f"{self._wrapped.__class__.__name__}: {self._wrapped}"


class ReferenceResolutionError(_WrappedReferencingError):
    """
    A reference could not be resolved while validating an instance.

    Unlike a `ValidationError`, this indicates that validation could not
    continue because a referenced resource (or a pointer or anchor within
    it) does not exist. It is therefore never swallowed by applicators
    such as :kw:`anyOf`, and is not a statement about the instance's
    validity.

    It retains the reference chain which led to the failure, along with
    the entry location at which the chain was entered.
    """

    def __init__(
        self,
        cause: _Unresolvable,
        *,
        ref: str | None = None,
        source: str = "",
        target: str = "",
        chain: Iterable[ReferenceStep] = (),
    ) -> None:
        super().__init__(cause)
        object.__setattr__(
            self,
            "ref",
            cause.ref if ref is None else ref,
        )
        object.__setattr__(self, "source", source)
        object.__setattr__(self, "target", target)
        object.__setattr__(self, "cause", cause)
        object.__setattr__(self, "__cause__", cause)
        object.__setattr__(
            self,
            "_ref_chain",
            ReferenceChain(chain).steps,
        )

    @property
    def ref_chain(self) -> ReferenceChain:
        return ReferenceChain(self._ref_chain)

    @property
    def reference_chain(self) -> ReferenceChain:
        return self.ref_chain

    @property
    def chain(self) -> ReferenceChain:
        return self.ref_chain

    @property
    def steps(self) -> tuple[ReferenceStep, ...]:
        return self._ref_chain

    @property
    def entry_location(self) -> str:
        entry = self.ref_chain.entry
        return "" if entry is None else entry.source_location

    @property
    def source_resource(self) -> str:
        return self.source

    @property
    def target_resource(self) -> str:
        return self.target

    @property
    def target_location(self) -> str:
        return self.ref_chain.target_location

    @property
    def unresolved_location(self) -> str:
        return self.target_location

    def _prepend_ref_step(self, step: ReferenceStep) -> None:
        object.__setattr__(self, "_ref_chain", (step, *self._ref_chain))
        object.__setattr__(self, "source", step.source)

    def __str__(self) -> str:
        rendered = super().__str__()
        if len(self._ref_chain) <= 1:
            return rendered

        lines = [rendered, "", "Reference chain:"]
        lines.append(f"  entry: {self.entry_location}")
        lines.extend(f"    {step}" for step in self._ref_chain)
        lines.append(f"  unresolved: {self._ref_chain[-1].target_location}")
        return "\n".join(lines)

    def __repr__(self) -> str:
        return (
            f"<{self.__class__.__name__}: {self.ref!r}; "
            f"chain={self.ref_chain!r}>"
        )


#: Convenient aliases for the reference-chain view.
RefStep = ReferenceStep
ReferenceHop = ReferenceStep
RefHop = ReferenceStep
RefChain = ReferenceChain
ReferenceTrace = ReferenceChain
ReferenceChainError = ReferenceResolutionError
UnresolvableReferenceError = ReferenceResolutionError
ReferenceError = ReferenceResolutionError
RefError = ReferenceResolutionError
ReferenceResolutionFailure = ReferenceResolutionError


def reference_chain(error: _Error) -> ReferenceChain:
    """
    Return the reference chain attached to a validation error.
    """
    return error.ref_chain


def reference_chains(
    errors: Iterable[_Error],
) -> tuple[ReferenceChain, ...]:
    """
    Return every distinct reference chain found amongst the given errors.
    """
    chains: list[ReferenceChain] = []
    for error in errors:
        for chain in error.ref_chains:
            if chain not in chains:
                chains.append(chain)
    return tuple(chains)


class UndefinedTypeCheck(Exception):
    """
    A type checker was asked to check a type it did not have registered.
    """

    def __init__(self, type: str) -> None:
        self.type = type

    def __str__(self) -> str:
        return f"Type {self.type!r} is unknown to this type checker"


class UnknownType(Exception):
    """
    A validator was asked to validate an instance against an unknown type.
    """

    def __init__(self, type, instance, schema):
        self.type = type
        self.instance = instance
        self.schema = schema

    def __str__(self):
        prefix = 16 * " "

        return dedent(
            f"""\
            Unknown type {self.type!r} for validator with schema:
                {_pretty(self.schema, prefix=prefix)}

            While checking instance:
                {_pretty(self.instance, prefix=prefix)}
            """.rstrip(),
        )


class FormatError(Exception):
    """
    Validating a format failed.
    """

    def __init__(self, message, cause=None):
        super().__init__(message, cause)
        self.message = message
        self.cause = self.__cause__ = cause

    def __str__(self):
        return self.message


class ErrorTree:
    """
    ErrorTrees make it easier to check which validations failed.

    Arguments:

        errors:

            the errors to populate the tree with

        instance:

            the instance the tree corresponds to, if known, which
            enables indexing the tree at indices not present in it to
            raise the same error that indexing the instance itself would

    """

    def __init__(
        self,
        errors: Iterable[ValidationError] = (),
        *,
        instance: Any = _unset,
    ):
        self.errors: MutableMapping[str, ValidationError] = {}
        self._contents: MutableMapping[str | int, ErrorTree] = {}
        self._instance = instance

        for error in errors:
            container = self
            for element in error.path:
                container = container._contents.setdefault(
                    element,
                    self.__class__(),
                )
            container.errors[error.validator] = error

            container._instance = error.instance

    def __contains__(self, index: str | int):
        """
        Check whether ``instance[index]`` has any errors.
        """
        return index in self._contents

    def __getitem__(self, index):
        """
        Retrieve the child tree one level down at the given ``index``.

        If the index is not in the instance that this tree corresponds
        to and is not known by this tree, whatever error would be raised
        by ``instance.__getitem__`` will be propagated (usually this is
        some subclass of `LookupError`.
        """
        if index in self:
            return self._contents[index]
        if self._instance is _unset:
            return self.__class__()
        return self.__class__(instance=self._instance[index])

    def __setitem__(self, index: str | int, value: ErrorTree):
        """
        Add an error to the tree at the given ``index``.

        .. deprecated:: v4.20.0

            Setting items on an `ErrorTree` is deprecated without replacement.
            To populate a tree, provide all of its sub-errors when you
            construct the tree.
        """
        warnings.warn(
            "ErrorTree.__setitem__ is deprecated without replacement.",
            DeprecationWarning,
            stacklevel=2,
        )
        self._contents[index] = value

    def __iter__(self):
        """
        Iterate (non-recursively) over the indices in the instance with errors.
        """
        return iter(self._contents)

    def __len__(self):
        """
        Return the `total_errors`.
        """
        return self.total_errors

    def __repr__(self):
        total = len(self)
        errors = "error" if total == 1 else "errors"
        return f"<{self.__class__.__name__} ({total} total {errors})>"

    @property
    def total_errors(self):
        """
        The total number of errors in the entire tree, including children.
        """
        child_errors = sum(len(tree) for _, tree in self._contents.items())
        return len(self.errors) + child_errors


def by_relevance(weak=WEAK_MATCHES, strong=STRONG_MATCHES):
    """
    Create a key function that can be used to sort errors by relevance.

    Arguments:
        weak (set):
            a collection of validation keywords to consider to be
            "weak".  If there are two errors at the same level of the
            instance and one is in the set of weak validation keywords,
            the other error will take priority. By default, :kw:`anyOf`
            and :kw:`oneOf` are considered weak keywords and will be
            superseded by other same-level validation errors.

        strong (set):
            a collection of validation keywords to consider to be
            "strong"

    """

    def relevance(error):
        validator = error.validator
        return (  # prefer errors which are ...
            -len(error.path),  # shorter path thereby more general
            validator not in weak,  # for a non-low-priority keyword
            validator in strong,  # for a high priority keyword
            not error._matches_type(),  # at least match the instance's type
        )  # otherwise we'll treat them the same

    return relevance


relevance = by_relevance()
"""
A key function (e.g. to use with `sorted`) which sorts errors by relevance.

Example:

.. code:: python

    sorted(validator.iter_errors(12), key=jsonschema.exceptions.relevance)
"""


def best_match(errors, key=relevance):
    """
    Try to find an error that appears to be the best match among given errors.

    In general, errors that are higher up in the instance (i.e. for which
    `ValidationError.path` is shorter) are considered better matches,
    since they indicate "more" is wrong with the instance.

    If the resulting match is either :kw:`oneOf` or :kw:`anyOf`, the
    *opposite* assumption is made -- i.e. the deepest error is picked
    among the most relevant errors in each separate subschema (preferring
    subschemas which produced fewer errors when tied), since these
    keywords only need to match once, and any other errors may not be
    relevant.

    Arguments:
        errors (collections.abc.Iterable):

            the errors to select from. Do not provide a mixture of
            errors from different validation attempts (i.e. from
            different instances or schemas), since it won't produce
            sensical output.

        key (collections.abc.Callable):

            the key to use when sorting errors. See `relevance` and
            transitively `by_relevance` for more details (the default is
            to sort with the defaults of that function). Changing the
            default is only useful if you want to change the function
            that rates errors but still want the error context descent
            done by this function.

    Returns:
        the best matching error, or ``None`` if the iterable was empty

    .. note::

        This function is a heuristic. Its return value may change for a given
        set of inputs from version to version if better heuristics are added.

    """
    _, best = _most_relevant(errors, key=key)
    if best is None:
        return

    while best.context:
        # Group the errors by the subschema which produced them.
        by_subschema: dict[Any, list[_Error]] = defaultdict(list)
        for error in best.context:
            index = error.schema_path[0] if error.schema_path else None
            by_subschema[index].append(error)

        # Rank each subschema by how deep its most relevant error is,
        # and amongst those equally deep, by how few errors it produced
        # (i.e. how close it was to being valid). Lower ranks are better.
        best_rank, best_in_subschema, tied = None, None, False
        for errors_in_subschema in by_subschema.values():
            error_key, error = _most_relevant(errors_in_subschema, key=key)
            rank = error_key, len(errors_in_subschema)
            if best_rank is None or rank < best_rank:
                best_rank, best_in_subschema, tied = rank, error, False
            elif rank == best_rank:
                tied = True

        # If multiple subschemas rank equally we can't tell which was
        # intended, so we stop here rather than descend into one of them.
        if tied:
            break
        best = best_in_subschema
    return best


def _most_relevant[E: _Error](
    errors: Iterable[E],
    key: Callable[[E], Any],
) -> tuple[Any, E | None]:
    """
    Find the most relevant error along with its key, computing each key once.

    Equally relevant errors are settled by picking the one which appears
    earlier in the instance, which makes the choice independent of the
    order in which the errors happened to be produced.

    Returns ``(None, None)`` if there were no errors.
    """
    best_key, best = None, None
    for error in errors:
        error_key = key(error)
        if (
            best is None
            or error_key > best_key
            or (error_key == best_key and error.path < best.path)
        ):
            best_key, best = error_key, error
    return best_key, best
