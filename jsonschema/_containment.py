"""
A deliberately limited containment analysis for *scalar numeric* schemas.

The entry point, `check_scalar_numeric_containment`, answers one narrow
question: does every instance accepted by an *old* schema remain accepted by
a *new* schema?  Only a fixed subset of JSON Schema is supported:

* the ``type`` keyword restricted to ``"number"`` / ``"integer"``
  (or no ``type`` at all, treated as an unconstrained scalar),

* the numeric bounds ``minimum`` / ``maximum`` / ``exclusiveMinimum`` /
  ``exclusiveMaximum`` (including the draft 3 / draft 4 boolean form of the
  exclusive keywords),

* and ``enum`` (with numeric members).

``$ref`` is followed only when it resolves inside the same document (or via
the registry already attached to a provided validator).  Anything outside
this subset -- complex applicators (``allOf`` / ``anyOf`` / ``oneOf`` /
``not`` / ``if`` ...), non-numeric types, unresolvable references and so on
-- yields an *unknown* verdict rather than a guess.

This module is emphatically not a general-purpose decidability engine for
arbitrary schemas.  Every reported incompatibility is backed by a concrete
witness that has actually been re-validated against both validators, with all
numeric comparisons done via `decimal.Decimal` so boundary precision is
preserved.
"""

from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal, InvalidOperation
from numbers import Number
from typing import TYPE_CHECKING, Any

from referencing import Registry
from referencing.exceptions import PointerToNowhere, Unresolvable

from jsonschema.validators import (
    Draft201909Validator,
    Draft202012Validator,
    Draft3Validator,
    Draft4Validator,
    Draft6Validator,
    Draft7Validator,
    extend,
    validator_for,
)

if TYPE_CHECKING:
    from collections.abc import Mapping

    from jsonschema.protocols import Validator


COMPATIBLE = "compatible"
INCOMPATIBLE = "incompatible"
UNKNOWN = "unknown"

# Maximum length of a ``$ref`` chain we will expand before declaring a
# schema out of scope (a cyclic or unreasonably deep reference graph).
_MAX_REF_DEPTH = 32

# Annotation keywords never constrain instances, so they are ignored.
_ANNOTATION_KEYWORDS = frozenset(
    {
        "title",
        "description",
        "default",
        "examples",
        "deprecated",
        "readOnly",
        "writeOnly",
        "$comment",
    },
)

# Identifiers and reference-defining keywords are handled explicitly.
_ID_KEYWORDS = frozenset(
    {
        "$schema",
        "$id",
        "id",
        "$ref",
        "$defs",
        "definitions",
    },
)

# Numeric assertion keywords understood by this module.  ``multipleOf`` is
# intentionally absent: divisibility containment over decimal bounds is out
# of scope, so its presence forces an *unknown* verdict.
_NUMERIC_KEYWORDS = frozenset(
    {
        "minimum",
        "maximum",
        "exclusiveMinimum",
        "exclusiveMaximum",
    },
)

_SUPPORTED_KEYWORDS = (
    _ANNOTATION_KEYWORDS
    | _ID_KEYWORDS
    | _NUMERIC_KEYWORDS
    | frozenset({"type", "enum"})
)


class UnsupportedSchema(ValueError):
    """
    Raised internally when a schema falls outside the supported subset.

    The public entry point converts these into *unknown* verdicts, so this
    exception never escapes the module.  ``keyword`` records the first
    offending schema keyword when one is responsible.
    """

    def __init__(self, message, keyword=None):
        super().__init__(message)
        self.keyword = keyword


@dataclass(frozen=True)
class NumericDomain:
    """
    The accepted numeric set represented by a supported scalar schema.

    Bounds are `Decimal` values; ``lower_open`` / ``upper_open`` mark open
    (exclusive) boundaries.  ``integer_only`` restricts the set to integral
    values and ``enum`` optionally restricts it to an explicit finite set.
    """

    lower: Decimal | None = None
    lower_open: bool = False
    upper: Decimal | None = None
    upper_open: bool = False
    integer_only: bool = False
    enum: tuple[Decimal, ...] | None = None

    def contains(self, value: Decimal) -> bool:
        if self.lower is not None:
            if value < self.lower or (self.lower_open and value == self.lower):
                return False
        if self.upper is not None:
            if value > self.upper or (self.upper_open and value == self.upper):
                return False
        if self.integer_only and value != value.to_integral_value():
            return False
        if self.enum is not None and value not in self.enum:
            return False
        return True

    def is_empty(self) -> bool:
        if self.enum is not None:
            return not any(self.contains(value) for value in self.enum)
        if self.lower is None or self.upper is None:
            return False
        if self.lower > self.upper:
            return True
        if self.lower < self.upper:
            if self.integer_only:
                first = lower_bound_integer(self)
                return first is None or not self.contains(first)
            return False
        # Equal bounds: empty exactly when either side is open, or when an
        # integer domain lands on a non-integral value.
        if self.lower_open or self.upper_open:
            return True
        if self.integer_only and self.lower != self.lower.to_integral_value():
            return True
        return False


@dataclass(frozen=True)
class ContainmentResult:
    """
    The outcome of a containment check.

    ``status`` is one of `COMPATIBLE`, `INCOMPATIBLE` or `UNKNOWN`.
    ``reason`` is a human-readable explanation and ``witness`` is the
    `Decimal` value backing an incompatibility.  ``old_accepts_witness`` /
    ``new_accepts_witness`` record the actual re-validation outcomes against
    both validators and are only populated for incompatibilities.
    """

    status: str
    reason: str
    witness: Decimal | None = None
    old_accepts_witness: bool | None = None
    new_accepts_witness: bool | None = None
    unsupported_keyword: str | None = None


@dataclass(frozen=True)
class _Side:
    schema: Mapping[str, Any] | bool
    validator: Validator
    registry: Registry
    decimal_validator: Validator
    dialect: str


# ---------------------------------------------------------------------------
# Decimal-aware validator construction (witness re-validation)
# ---------------------------------------------------------------------------


def _is_integer(checker, instance):
    """Type check accepting both real ints and integral `Decimal` values."""

    # bool inherits from int in Python; mirror the library's guard.
    if isinstance(instance, bool):
        return False
    if isinstance(instance, int):
        return True
    if isinstance(instance, Decimal):
        return instance == instance.to_integral_value()
    return False


def _is_number(checker, instance):
    """Type check accepting numeric values while excluding booleans."""

    if isinstance(instance, bool):
        return False
    return isinstance(instance, (Number, Decimal))


def _decimal_validator_for(validator):
    """
    Build a validator identical to ``validator`` but Decimal-aware.

    Witnesses are `Decimal` values, and float/Decimal comparisons raise
    `TypeError`, so both the *type* checker and the numeric literals inside
    the schema are made decimal.  Everything else (keywords, registry,
    dialect) is carried over unchanged.
    """

    type_checker = validator.TYPE_CHECKER.redefine_many(
        {"integer": _is_integer, "number": _is_number},
    )
    cls = extend(type(validator), type_checker=type_checker)
    return cls(
        _decimalize(validator.schema),
        registry=getattr(validator, "_registry", Registry()),
        format_checker=validator.format_checker,
    )


_NUMERIC_LITERAL_KEYWORDS = frozenset(
    {
        "minimum",
        "maximum",
        "exclusiveMinimum",
        "exclusiveMaximum",
        "multipleOf",
        "divisibleBy",
    },
)


def _decimalize(schema, parent_key=None):
    """
    Recursively replace numeric literals in a schema with `Decimal`.

    Numeric bounds serialized as strings (to preserve decimal precision
    through JSON) are converted only under the known numeric keywords; all
    other strings are left untouched.
    """

    if isinstance(schema, bool):
        return schema
    if isinstance(schema, dict):
        return {
            key: _decimalize(value, parent_key=key)
            for key, value in schema.items()
        }
    if isinstance(schema, list):
        if parent_key == "enum":
            decimalized: list = []
            for each in schema:
                if isinstance(each, str):
                    try:
                        decimalized.append(Decimal(each))
                        continue
                    except InvalidOperation:
                        pass
                decimalized.append(_decimalize(each))
            return decimalized
        return [_decimalize(each) for each in schema]
    if isinstance(schema, int):
        return Decimal(schema)
    if isinstance(schema, float):
        return Decimal(str(schema))
    if isinstance(schema, str) and parent_key in _NUMERIC_LITERAL_KEYWORDS:
        try:
            return Decimal(schema)
        except InvalidOperation:
            return schema
    return schema


# ---------------------------------------------------------------------------
# $ref expansion (local / registry-resolvable references only)
# ---------------------------------------------------------------------------


def _resolve_refs(schema, registry, specification):
    """
    Expand ``$ref`` chains into plain, inlined object schemas.

    Only references resolvable through the root document itself or an
    explicitly supplied registry are followed.  Remote, unresolvable,
    cyclic or excessively deep references raise `UnsupportedSchema`.
    """

    seen: set[str] = set()
    current = schema
    depth = 0
    while isinstance(current, dict) and "$ref" in current:
        ref = current["$ref"]
        if not isinstance(ref, str):
            raise UnsupportedSchema("a non-string $ref was encountered")
        if ref in seen:
            raise UnsupportedSchema(f"a cyclic $ref chain was found: {ref!r}")
        seen.add(ref)
        depth += 1
        if depth > _MAX_REF_DEPTH:
            raise UnsupportedSchema("the $ref chain is too deep to expand")

        resource = specification.create_resource(schema)
        resolver = registry.resolver_with_root(resource)
        try:
            resolved = resolver.lookup(ref)
        except (Unresolvable, PointerToNowhere) as error:
            raise UnsupportedSchema(
                f"the reference {ref!r} could not be resolved",
            ) from error

        target = resolved.contents
        if not isinstance(target, dict):
            # A reference ending at a boolean schema is unsupported.
            raise UnsupportedSchema(
                f"the reference {ref!r} resolves outside the object subset",
            )

        # Drafts 3 and 4 ignore sibling keywords of ``$ref``; later drafts
        # evaluate them alongside the referenced subschema.
        if specification.name in {"DRAFT3", "DRAFT4"}:
            current = target
        else:
            merged = dict(target)
            for key, value in current.items():
                if key != "$ref":
                    merged.setdefault(key, value)
            current = merged
    return current


# ---------------------------------------------------------------------------
# Domain extraction
# ---------------------------------------------------------------------------


def _as_decimal(value, keyword):
    """Coerce a numeric bound (or numeric string) into `Decimal` exactly."""

    if isinstance(value, bool):
        raise UnsupportedSchema(
            f"{keyword!r} must be a numeric bound, found {value!r}",
        )
    if isinstance(value, Decimal):
        return value
    if isinstance(value, (int, float)):
        # Stringifying floats avoids binary float artifacts at the boundary.
        return Decimal(str(value))
    if isinstance(value, str):
        # Numeric literals are sometimes carried as strings so that full
        # decimal precision survives JSON transport.
        try:
            return Decimal(value)
        except Exception as error:
            raise UnsupportedSchema(
                f"{keyword!r} has a non-numeric string bound {value!r}",
            ) from error
    raise UnsupportedSchema(
        f"{keyword!r} must be a numeric bound, found {value!r}",
    )


def _merge_lower_bounds(inclusive, exclusive, boolean_open):
    """
    Merge ``minimum`` / ``exclusiveMinimum`` into one effective lower bound.

    The tighter of the two numeric bounds wins; on a tie an exclusive bound
    keeps the boundary open.  Draft 3 / 4's boolean flag only re-marks an
    existing inclusive bound as open.
    """

    if inclusive is None and exclusive is None:
        return None, False
    if inclusive is None:
        return exclusive, True
    if exclusive is None:
        return inclusive, boolean_open
    if exclusive > inclusive:
        return exclusive, True
    if exclusive < inclusive:
        return inclusive, boolean_open
    return inclusive, True


def _merge_upper_bounds(inclusive, exclusive, boolean_open):
    """Merge ``maximum`` / ``exclusiveMaximum`` into one upper bound."""

    if inclusive is None and exclusive is None:
        return None, False
    if inclusive is None:
        return exclusive, True
    if exclusive is None:
        return inclusive, boolean_open
    if exclusive < inclusive:
        return exclusive, True
    if exclusive > inclusive:
        return inclusive, boolean_open
    return inclusive, True


def _extract_domain(schema, side):
    """Build the `NumericDomain` of one side, rejecting out-of-scope forms."""

    if schema is True:
        return NumericDomain()
    if schema is False:
        # An empty domain: represent it with an impossible open interval.
        return NumericDomain(
            lower=Decimal(0),
            upper=Decimal(0),
            lower_open=True,
        )
    if not isinstance(schema, dict):
        raise UnsupportedSchema("a schema must be an object or a boolean")

    expanded = _resolve_refs(
        schema,
        side.registry,
        _specification_for(side.dialect),
    )

    unsupported = next(
        (
            keyword
            for keyword in expanded
            if keyword not in _SUPPORTED_KEYWORDS
        ),
        None,
    )
    if unsupported is not None:
        raise UnsupportedSchema(
            f"keyword {unsupported!r} is outside the supported scalar subset",
            keyword=unsupported,
        )

    integer_only = False
    if "type" in expanded:
        type_value = expanded["type"]
        if isinstance(type_value, str):
            if type_value == "integer":
                integer_only = True
            elif type_value != "number":
                raise UnsupportedSchema(
                    f"type {type_value!r} is not a numeric scalar type",
                )
        elif isinstance(type_value, list):
            normalized = set(type_value)
            if not normalized <= {"integer", "number"} or not normalized:
                raise UnsupportedSchema(
                    "multi-type schemas with non-numeric types are "
                    "unsupported",
                )
            integer_only = normalized == {"integer"}
        else:
            raise UnsupportedSchema("the type keyword has an unsupported form")

    # Collect inclusive and exclusive bounds separately, then merge them.
    # Draft 6+ allows ``minimum`` and ``exclusiveMinimum`` to coexist as
    # independent keywords; the effective lower bound is the tighter of the
    # two, so overwriting one with the other would silently lose a bound.
    inclusive_lower = inclusive_upper = None
    exclusive_lower = exclusive_upper = None
    boolean_lower_open = boolean_upper_open = False
    bound_keywords = (
        "minimum",
        "maximum",
        "exclusiveMinimum",
        "exclusiveMaximum",
    )
    for keyword in bound_keywords:
        if keyword not in expanded:
            continue
        raw_value = expanded[keyword]
        if keyword in {"exclusiveMinimum", "exclusiveMaximum"} and isinstance(
            raw_value,
            bool,
        ):
            # Draft 3 / 4 boolean form: it only flips openness of the
            # accompanying minimum / maximum.
            if raw_value:
                if keyword == "exclusiveMinimum" and "minimum" in expanded:
                    boolean_lower_open = True
                if keyword == "exclusiveMaximum" and "maximum" in expanded:
                    boolean_upper_open = True
            continue
        value = _as_decimal(raw_value, keyword)
        if keyword == "minimum":
            inclusive_lower = value
        elif keyword == "maximum":
            inclusive_upper = value
        elif keyword == "exclusiveMinimum":
            exclusive_lower = value
        elif keyword == "exclusiveMaximum":
            exclusive_upper = value

    lower, lower_open = _merge_lower_bounds(
        inclusive_lower,
        exclusive_lower,
        boolean_lower_open,
    )
    upper, upper_open = _merge_upper_bounds(
        inclusive_upper,
        exclusive_upper,
        boolean_upper_open,
    )

    enum = None
    if "enum" in expanded:
        raw_enum = expanded["enum"]
        if not isinstance(raw_enum, list) or not raw_enum:
            raise UnsupportedSchema("enum must be a non-empty array")
        members: list[Decimal] = []
        for member in raw_enum:
            if isinstance(member, bool):
                raise UnsupportedSchema(
                    f"enum member {member!r} is not numeric",
                )
            if isinstance(member, Decimal):
                members.append(member)
            elif isinstance(member, (int, float)):
                members.append(Decimal(str(member)))
            elif isinstance(member, str):
                # Numeric literals may be carried as strings to preserve
                # decimal precision through JSON transport.
                try:
                    members.append(Decimal(member))
                except InvalidOperation:
                    raise UnsupportedSchema(
                        f"enum member {member!r} is not numeric",
                    ) from None
            else:
                raise UnsupportedSchema(
                    f"enum member {member!r} is not numeric",
                )
        enum = tuple(members)

    return NumericDomain(
        lower=lower,
        lower_open=bool(lower_open),
        upper=upper,
        upper_open=bool(upper_open),
        integer_only=integer_only,
        enum=enum,
    )


def _specification_for(dialect):
    from referencing import jsonschema as referencing_jsonschema

    mapping = {
        "draft3": referencing_jsonschema.DRAFT3,
        "draft4": referencing_jsonschema.DRAFT4,
        "draft6": referencing_jsonschema.DRAFT6,
        "draft7": referencing_jsonschema.DRAFT7,
        "draft201909": referencing_jsonschema.DRAFT201909,
        "draft202012": referencing_jsonschema.DRAFT202012,
    }
    try:
        return mapping[dialect]
    except KeyError as error:
        raise UnsupportedSchema(
            f"unsupported JSON Schema dialect: {dialect!r}",
        ) from error


# ---------------------------------------------------------------------------
# Witness construction
# ---------------------------------------------------------------------------


def _integer_at_or_above(value: Decimal) -> Decimal:
    return value.to_integral_value(rounding="ROUND_CEILING")


def lower_bound_integer(domain: NumericDomain) -> Decimal | None:
    if domain.lower is None:
        return Decimal(0)
    candidate = _integer_at_or_above(domain.lower)
    if domain.lower_open and candidate == domain.lower:
        candidate += Decimal(1)
    if domain.upper is not None and candidate > domain.upper:
        return None
    if (
        domain.upper is not None
        and candidate == domain.upper
        and domain.upper_open
    ):
        return None
    return candidate


def _witness_below(new: NumericDomain, old: NumericDomain) -> Decimal | None:
    """
    A value inside ``old`` but below the new lower bound.

    Candidates are derived from the *old* domain's lower edge (or, for an
    integer domain, from the first integer the new bound admits minus one),
    then accepted only if the old domain actually contains them.
    """

    candidates: list[Decimal] = []
    if old.integer_only or new.integer_only:
        candidates.append(
            _integer_at_or_above(new.lower) - Decimal(1),
        )
    else:
        candidates.append(new.lower)
        if not new.lower_open:
            candidates.append(new.lower - _unit(new.lower))
    if old.lower is not None:
        candidates.append(old.lower)
        candidates.append(old.lower + _unit(old.lower))
    for candidate in candidates:
        if old.contains(candidate) and not new.contains(candidate):
            return candidate
    return None


def _unit(value: Decimal) -> Decimal:
    """A decimal unit matching the last significant place of ``value``."""

    exponent = value.as_tuple().exponent
    if isinstance(exponent, int):
        return Decimal(1).scaleb(exponent)
    return Decimal(1)


def _witness_above(new: NumericDomain, old: NumericDomain) -> Decimal | None:
    """A value inside ``old`` but above the new upper bound."""

    candidates: list[Decimal] = []
    if old.integer_only or new.integer_only:
        candidates.append(
            new.upper.to_integral_value(rounding="ROUND_FLOOR") + Decimal(1),
        )
    else:
        candidates.append(new.upper)
        if not new.upper_open:
            candidates.append(new.upper + _unit(new.upper))
    if old.upper is not None:
        candidates.append(old.upper)
        candidates.append(old.upper - _unit(old.upper))
    for candidate in candidates:
        if old.contains(candidate) and not new.contains(candidate):
            return candidate
    return None


def _witness_non_integer(old: NumericDomain) -> Decimal | None:
    """A fractional value inside a number domain excluded by integer-only."""

    candidates: list[Decimal] = []
    if old.lower is not None:
        base = old.lower
        candidate = base + Decimal("0.5") * _unit(base)
        if old.lower_open and candidate <= base:
            candidate = base + _unit(base)
        candidates.append(candidate)
    if old.upper is not None:
        base = old.upper
        candidate = base - Decimal("0.5") * _unit(base)
        if old.upper_open and candidate >= base:
            candidate = base - _unit(base)
        candidates.append(candidate)
    if not candidates:
        candidates.append(Decimal("0.5"))
    for candidate in candidates:
        if old.contains(candidate):
            return candidate
    return None


def _is_point_domain(domain: NumericDomain) -> bool:
    """Whether a non-integer domain has collapsed to one closed point."""

    if domain.integer_only or domain.enum is not None:
        return False
    if domain.lower is None or domain.upper is None:
        return False
    if domain.lower != domain.upper:
        return False
    return not domain.lower_open and not domain.upper_open


_FINITE_ENUMERATION_LIMIT = 1000


def _finite_values(domain: NumericDomain) -> tuple[Decimal, ...] | None:
    """
    Enumerate every value of a finite supported domain, else return ``None``.

    Explicit enums and bounded integer-only intervals are finite; bounded
    number intervals are continuous and therefore not enumerable.  An
    unexpectedly large interval also yields ``None`` to stay conservative.
    """

    if domain.enum is not None:
        return tuple(value for value in domain.enum if domain.contains(value))
    if not domain.integer_only or domain.lower is None or domain.upper is None:
        return None
    first = lower_bound_integer(domain)
    if first is None:
        return ()
    last = domain.upper.to_integral_value(rounding="ROUND_FLOOR")
    if domain.upper_open and last == domain.upper:
        last -= Decimal(1)
    if last < first:
        return ()
    count = int(last - first) + 1
    if count > _FINITE_ENUMERATION_LIMIT:
        return None
    return tuple(first + Decimal(index) for index in range(count))


def _fractional_neighbor(
    members: list[Decimal],
    old: NumericDomain,
) -> Decimal | None:
    """
    Find a fractional neighbor of an enum member that stays in the old
    continuous domain but is absent from the enum.
    """

    forbidden = set(members)
    for member in members:
        unit = _unit(member)
        for delta in (
            Decimal("0.5") * unit,
            unit,
            -Decimal("0.5") * unit,
            -unit,
        ):
            candidate = member + delta
            if (
                old.contains(candidate)
                and candidate not in forbidden
                and candidate != candidate.to_integral_value()
            ):
                return candidate
    return None


def _any_escape(old: NumericDomain, new: NumericDomain) -> Decimal | None:
    """
    Find any value accepted by ``old`` but rejected by ``new``.

    This is the safety net for geometric arrangements that the directed
    witness helpers do not anticipate (e.g. an old domain sitting entirely
    beyond the new bound).  It samples both edges of the old domain in
    integer and fractional granularity and verifies each candidate against
    both domains before returning it.
    """

    candidates: list[Decimal] = []
    if old.lower is not None:
        candidates.extend(
            [
                old.lower,
                old.lower + _unit(old.lower),
                old.lower + Decimal("0.5") * _unit(old.lower),
            ],
        )
    if old.upper is not None:
        candidates.extend(
            [
                old.upper,
                old.upper - _unit(old.upper),
                old.upper - Decimal("0.5") * _unit(old.upper),
            ],
        )
    first = lower_bound_integer(old)
    if first is not None:
        candidates.extend([first, first + 1, first + 2])
    candidates.extend([Decimal(0), Decimal("0.5"), Decimal(1), Decimal(-1)])
    for candidate in candidates:
        if old.contains(candidate) and not new.contains(candidate):
            return candidate
    return None


def _witness_outside_enum(
    new_enum: tuple[Decimal, ...],
    old: NumericDomain,
) -> Decimal | None:
    """A value accepted by ``old`` but absent from the new enum."""

    forbidden = set(new_enum)
    candidates: list[Decimal] = []
    if old.enum is not None:
        candidates.extend(old.enum)
    if old.integer_only:
        first = lower_bound_integer(old)
        if first is not None:
            candidates.extend([first, first + 1, first + 2])
    else:
        if old.lower is not None:
            candidates.extend(
                [
                    old.lower,
                    old.lower + Decimal("0.5") * _unit(old.lower),
                    old.lower + _unit(old.lower),
                ],
            )
        elif old.upper is not None:
            candidates.extend(
                [
                    old.upper,
                    old.upper - Decimal("0.5") * _unit(old.upper),
                    old.upper - _unit(old.upper),
                ],
            )
        else:
            candidates.extend([Decimal(0), Decimal("0.5"), Decimal(1)])
    for candidate in candidates:
        if old.contains(candidate) and candidate not in forbidden:
            return candidate
    return None


# ---------------------------------------------------------------------------
# Public entry point
# ---------------------------------------------------------------------------


_DIALECTS = frozenset(
    {"draft3", "draft4", "draft6", "draft7", "draft201909", "draft202012"},
)

# Meta-schema URIs of the drafts this module understands; anything else
# declared via ``$schema`` is out of scope.
_META_SCHEMA_IDS = frozenset(
    validator.ID_OF(validator.META_SCHEMA)
    for validator in (
        Draft3Validator,
        Draft4Validator,
        Draft6Validator,
        Draft7Validator,
        Draft201909Validator,
        Draft202012Validator,
    )
)


def _dialect_of(validator):
    """Return the supported draft name backing a validator instance."""

    dialect = type(validator).__name__.replace("Validator", "").lower()
    if dialect not in _DIALECTS:
        raise UnsupportedSchema(
            f"the validator {type(validator).__name__!r} does not correspond "
            "to a supported JSON Schema draft",
        )
    return dialect


def _build_side(schema, validator, label):
    """Assemble everything needed to analyze and re-check one contract."""

    if validator is not None:
        dialect = _dialect_of(validator)
        registry = getattr(validator, "_registry", Registry())
        return _Side(
            schema=validator.schema,
            validator=validator,
            registry=registry,
            decimal_validator=_decimal_validator_for(validator),
            dialect=dialect,
        )

    if schema is None:
        raise UnsupportedSchema(
            f"either a {label} schema or a {label} validator must be supplied",
        )
    if isinstance(schema, dict) and "$schema" in schema:
        declared = schema["$schema"]
        if declared not in _META_SCHEMA_IDS:
            raise UnsupportedSchema(
                f"the {label} schema declares an unsupported dialect "
                f"{declared!r}",
            )
    cls = validator_for(schema)
    dialect = cls.__name__.replace("Validator", "").lower()
    if dialect not in _DIALECTS:
        raise UnsupportedSchema(
            f"the {label} schema does not use a supported JSON Schema draft",
        )
    registry = Registry()
    built = cls(schema, registry=registry)
    return _Side(
        schema=schema,
        validator=built,
        registry=registry,
        decimal_validator=_decimal_validator_for(built),
        dialect=dialect,
    )


def _recheck(witness, old_side, new_side):
    """Validate one witness against both Decimal-aware validators."""

    old_ok = old_side.decimal_validator.is_valid(witness)
    new_ok = new_side.decimal_validator.is_valid(witness)
    return old_ok, new_ok


def _incompatible(
    witness,
    reason,
    old_side,
    new_side,
):
    """Build a verdict only after the witness survives both validators."""

    old_ok, new_ok = _recheck(witness, old_side, new_side)
    if not old_ok or new_ok:
        # The static analysis and the actual validators disagree, so no
        # counterexample has been proven.  Stay conservative.
        return ContainmentResult(
            status=UNKNOWN,
            reason=(
                "the constructed witness could not be confirmed by both "
                f"validators (old accepts it: {old_ok}, new accepts it: "
                f"{new_ok}); refusing to guess"
            ),
            witness=witness,
            old_accepts_witness=old_ok,
            new_accepts_witness=new_ok,
        )
    return ContainmentResult(
        status=INCOMPATIBLE,
        reason=reason,
        witness=witness,
        old_accepts_witness=True,
        new_accepts_witness=False,
    )


def _lower_bound_violated(old, new):
    """Whether the new lower bound excludes some value of the old domain."""

    if new.lower is None:
        return False
    if old.lower is None:
        return True
    if new.lower > old.lower:
        return True
    return (
        new.lower == old.lower
        and new.lower_open
        and not old.lower_open
    )


def _upper_bound_violated(old, new):
    """Whether the new upper bound excludes some value of the old domain."""

    if new.upper is None:
        return False
    if old.upper is None:
        return True
    if new.upper < old.upper:
        return True
    return (
        new.upper == old.upper
        and new.upper_open
        and not old.upper_open
    )


def _compare_domains(old, new, old_side, new_side):
    """Decide containment between two extracted numeric domains."""

    # An empty old domain accepts nothing: containment holds vacuously.
    if old.is_empty():
        return ContainmentResult(
            status=COMPATIBLE,
            reason=(
                "the old schema accepts no instance in its numeric domain, "
                "so containment holds vacuously"
            ),
        )

    # Finite-to-finite: every surviving old enum member must survive new.
    if old.enum is not None:
        for member in old.enum:
            if not old.contains(member):
                continue
            if not new.contains(member):
                return _incompatible(
                    member,
                    "an enum member accepted by the old schema is rejected "
                    "by the new schema",
                    old_side,
                    new_side,
                )
        return ContainmentResult(
            status=COMPATIBLE,
            reason=(
                "every numeric enum member accepted by the old schema is "
                "also accepted by the new schema"
            ),
        )

    # Any new enum turns the effective new domain into a finite set (inter
    # sected with its bounds); reason about concrete values rather than
    # interval shapes.
    if new.enum is not None:
        # A continuous old domain reduced to a single point can be decided
        # directly, regardless of what other bounds the new schema adds.
        if _is_point_domain(old):
            point = old.lower
            if new.contains(point):
                return ContainmentResult(
                    status=COMPATIBLE,
                    reason=(
                        "the old domain is a single point covered by the new "
                        "schema"
                    ),
                )
            return _incompatible(
                point,
                "the single point accepted by the old schema is rejected "
                "by the new enum and bounds",
                old_side,
                new_side,
            )

        # When the new schema declares no bounds, its domain is exactly the
        # enum set, so any old value outside it disproves containment.
        new_bounds_present = (
            new.lower is not None
            or new.upper is not None
            or new.integer_only
        )
        if not new_bounds_present:
            witness = _witness_outside_enum(new.enum, old)
            if witness is None:
                return ContainmentResult(
                    status=UNKNOWN,
                    reason=(
                        "no witness outside the new enum could be "
                        "constructed within the old domain"
                    ),
                )
            return _incompatible(
                witness,
                "the new enum does not cover every value accepted by the "
                "old schema",
                old_side,
                new_side,
            )

        # If the old domain is itself finite (bounded integer interval or a
        # finite enum), enumerate every value it can actually accept.
        old_values = _finite_values(old)
        if old_values is not None:
            for value in old_values:
                if old.contains(value) and not new.contains(value):
                    return _incompatible(
                        value,
                        "a value accepted by the old finite domain is "
                        "missing from the new enum and bounds",
                        old_side,
                        new_side,
                    )
            return ContainmentResult(
                status=COMPATIBLE,
                reason=(
                    "every value of the finite old domain is accepted by "
                    "the new enum and bounds"
                ),
            )
        # A continuous old domain intersected with a finite new enum: the
        # only old values the new schema can accept are the enum members
        # lying inside the old domain.  Containment holds only when the old
        # domain itself reduces to those points -- which, for a genuinely
        # continuous interval, can never happen.  Prove it with a witness
        # adjacent to one of the surviving enum members.
        surviving = [member for member in new.enum if old.contains(member)]
        if not surviving:
            witness = _any_escape(old, new)
        else:
            witness = _fractional_neighbor(surviving, old)
            if witness is None:
                witness = _any_escape(old, new)
        if witness is not None:
            return _incompatible(
                witness,
                "the new finite enum cannot cover the continuous old "
                "numeric domain",
                old_side,
                new_side,
            )
        return ContainmentResult(
            status=UNKNOWN,
            reason=(
                "the bounded enum intersection could not be decided without "
                "a confirmed witness"
            ),
        )

    violations: list[tuple[str, Decimal | None]] = []

    # Integer-only narrowing: a fractional value accepted by the old number
    # domain is rejected by the new integer-only domain.
    if new.integer_only and not old.integer_only:
        violations.append(
            (
                "the new schema requires integers but the old schema allows "
                "non-integer numbers",
                _witness_non_integer(old),
            ),
        )

    if _lower_bound_violated(old, new):
        violations.append(
            (
                "the new lower bound excludes values accepted by the old "
                "schema",
                _witness_below(new, old),
            ),
        )

    if _upper_bound_violated(old, new):
        violations.append(
            (
                "the new upper bound excludes values accepted by the old "
                "schema",
                _witness_above(new, old),
            ),
        )

    if violations:
        for reason, witness in violations:
            if witness is None:
                # The directed witness did not fit; exhaust the old domain's
                # edges before giving up on a proven counterexample.
                witness = _any_escape(old, new)
            if witness is not None:
                return _incompatible(
                    witness,
                    reason,
                    old_side,
                    new_side,
                )
        # A geometric violation was detected but no concrete witness could
        # be produced and confirmed.  Never guess compatibility.
        return ContainmentResult(
            status=UNKNOWN,
            reason=(
                "a bound or type narrowing was detected but no witness "
                "could be confirmed against both validators"
            ),
        )

    return ContainmentResult(
        status=COMPATIBLE,
        reason=(
            "the new numeric domain contains the old numeric domain over "
            "the supported bounds, integer type and enum subset"
        ),
    )


def check_scalar_numeric_containment(
    old_schema=None,
    new_schema=None,
    *,
    old_validator=None,
    new_validator=None,
):
    """
    Check whether the new schema accepts every scalar the old schema accepts.

    Only the documented scalar-numeric subset is supported:

    * ``type`` limited to ``"number"`` / ``"integer"`` (or absent),
    * ``minimum`` / ``maximum`` / ``exclusiveMinimum`` /
      ``exclusiveMaximum``, including the draft 3 / 4 boolean form,
    * numeric ``enum``,
    * and locally or registry-resolvable ``$ref`` chains.

    Returns a `ContainmentResult` whose ``status`` is:

    * ``"compatible"`` when containment has been proven,
    * ``"incompatible"`` when a concrete `Decimal` witness was found and
      re-validated as accepted by the old validator but rejected by the new
      one,
    * ``"unknown"`` whenever the schemas leave the supported subset, a
      reference cannot be resolved or a combination cannot be decided.

    Either raw schemas or ready-made validator objects (or one of each) may
    be supplied.  Validators are used directly for the witness re-check, so
    any custom registry is honored.
    """

    try:
        old_side = _build_side(old_schema, old_validator, "old")
        new_side = _build_side(new_schema, new_validator, "new")
        old_domain = _extract_domain(old_side.schema, old_side)
        new_domain = _extract_domain(new_side.schema, new_side)
    except UnsupportedSchema as error:
        return ContainmentResult(
            status=UNKNOWN,
            reason=f"outside the supported subset: {error}",
            unsupported_keyword=getattr(error, "keyword", None),
        )

    return _compare_domains(old_domain, new_domain, old_side, new_side)
