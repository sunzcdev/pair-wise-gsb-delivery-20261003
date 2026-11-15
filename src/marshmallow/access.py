"""Attribute access policies for safe serialization of ORM objects.

When a schema dumps an ORM object, simply reading an attribute may execute a
``property``/descriptor or trigger a lazy database query. The classes in this
module let a schema declare *how* attributes may be accessed during a dump.

Two modes are provided:

- ``"instance"``: only values already stored on the instance (``__dict__``) or
  in a mapping are read. Descriptors are never invoked just to fetch a value.
- ``"allowlist"``: descriptor access is permitted only for the declared field
  names passed to the policy. Everything else behaves like ``"instance"``.

A policy never uses :func:`hasattr` or otherwise executes a getter to find out
whether an attribute exists. A forbidden access is represented by
:data:`forbidden_access`, a sentinel distinct from
:data:`marshmallow.missing`, so callers can always tell "access denied" apart
from "attribute not set".

Example usage:

.. code-block:: python

    from marshmallow import Schema, fields
    from marshmallow.experimental.access import AccessPolicy


    class OrderSchema(Schema):
        id = fields.Int()
        items = fields.Nested(ItemSchema, many=True)


    with AccessPolicy("instance") as policy:
        data = OrderSchema().dump(order)
        # fields whose descriptors were skipped are reported here
        policy.read_plan.unevaluated
"""

from __future__ import annotations

import contextvars
import types
import typing
from collections.abc import Mapping

from marshmallow.constants import missing

if typing.TYPE_CHECKING:
    from marshmallow.schema import Schema

__all__ = [
    "ACCESS_MODES",
    "AccessPolicy",
    "AccessPolicyError",
    "ReadPlan",
    "forbidden_access",
    "get_active_policy",
]

#: Attribute access only reads data already present on the instance.
INSTANCE_MODE: typing.Final = "instance"
#: Attribute access additionally permits descriptors for declared field names.
ALLOWLIST_MODE: typing.Final = "allowlist"

ACCESS_MODES: typing.Final[frozenset[str]] = frozenset({INSTANCE_MODE, ALLOWLIST_MODE})
AccessMode = typing.Literal["instance", "allowlist"]


class _ForbiddenAccess:
    """Sentinel returned when a policy forbids an attribute access.

    Distinct from :data:`marshmallow.missing`: ``missing`` means the attribute
    was legitimately absent, while this sentinel means the access was blocked
    before any getter could run.
    """

    def __bool__(self) -> bool:
        return False

    def __copy__(self) -> _ForbiddenAccess:
        return self

    def __deepcopy__(self, _memo: typing.Any) -> _ForbiddenAccess:
        return self

    def __repr__(self) -> str:
        return "<marshmallow.forbidden_access>"


forbidden_access: typing.Final = _ForbiddenAccess()


class AccessPolicyError(ValueError):
    """Raised when an :class:`AccessPolicy` is configured incorrectly."""


_ACTIVE_POLICY: contextvars.ContextVar[AccessPolicy | None] = contextvars.ContextVar(
    "marshmallow_access_policy", default=None
)


def get_active_policy() -> AccessPolicy | None:
    """Return the policy governing the current dump, or ``None``."""
    return _ACTIVE_POLICY.get()


def _find_descriptor(obj: typing.Any, name: str) -> typing.Any:
    """Return the descriptor stored for ``name`` on ``obj``'s class.

    The lookup walks the MRO and reads class dictionaries directly, so no
    getter is ever executed. Returns ``None`` when no descriptor exists.
    """
    klass = type(obj)
    for base in klass.__mro__:
        descriptor = base.__dict__.get(name)
        if descriptor is not None:
            return descriptor
    return None


class _NoInstanceData:
    """Marker meaning a name is not stored as plain data on an instance."""

    def __repr__(self) -> str:
        return "<no instance data>"


_NO_INSTANCE_DATA = _NoInstanceData()


class _UnassignedSlot:
    """Marker meaning a name is an unassigned ``__slots__`` member."""

    def __repr__(self) -> str:
        return "<unassigned slot>"


_UNASSIGNED_SLOT = _UnassignedSlot()


def _get_instance_data(obj: typing.Any, name: str) -> typing.Any:
    """Return data already stored on ``obj`` without invoking any descriptor.

    Handles both regular instances (``__dict__``) and slotted instances. A
    name that is only reachable through a descriptor or a custom
    ``__getattr__`` returns the internal marker instead. An unassigned slot
    returns :data:`_UNASSIGNED_SLOT`.
    """
    instance_dict = getattr(obj, "__dict__", None)
    if isinstance(instance_dict, Mapping) and name in instance_dict:
        return instance_dict[name]

    # A slot stores its value on the instance while keeping its descriptor on
    # the class. Reading the member descriptor directly from the declaring
    # class bypasses any overridden property and cannot run a database query.
    for base in type(obj).__mro__:
        slot_descriptor = base.__dict__.get(name)
        if isinstance(slot_descriptor, types.MemberDescriptorType):
            try:
                return slot_descriptor.__get__(obj)
            except AttributeError:
                # Slot declared but never assigned: genuinely missing data.
                return _UNASSIGNED_SLOT
    return _NO_INSTANCE_DATA


def access_value(
    obj: typing.Any,
    key: str,
    default: typing.Any = missing,
    policy: AccessPolicy | None = None,
) -> typing.Any:
    """Read ``key`` from ``obj`` according to ``policy``.

    Mappings are always read through their existing data. For regular objects,
    the policy decides whether a descriptor may be invoked:

    - an instance-dict entry is always considered already-loaded data;
    - descriptors are only invoked when the policy allows them;
    - otherwise :data:`forbidden_access` is returned without touching a getter;
    - if the attribute is genuinely absent, ``default`` is returned.

    Dot-delimited ``key`` values are traversed segment by segment, applying the
    same decision at every hop.
    """
    if policy is None:
        # No policy in scope: defer to the standard, unrestricted accessor.
        from marshmallow.utils import get_value  # noqa: PLC0415

        return get_value(obj, key, default)

    if "." not in key:
        return _access_segment(obj, key, default, policy, key)

    current = obj
    for segment in key.split("."):
        # Record against the full attribute path so the read plan keeps the
        # name the schema actually declared.
        current = _access_segment(current, segment, missing, policy, key)
        if current is forbidden_access:
            return forbidden_access
        if current is missing:
            return default
    return current


def _access_segment(  # noqa: PLR0911
    obj: typing.Any,
    name: str,
    default: typing.Any,
    policy: AccessPolicy,
    record_key: str,
) -> typing.Any:
    # Mappings expose already-loaded data directly; no descriptors are involved.
    if isinstance(obj, Mapping):
        if name in obj:
            return obj[name]
        policy.record_missing(record_key)
        return default

    # Data already stored on the instance is always considered loaded and is
    # therefore readable in every mode; the policy only governs descriptor
    # access that may run a getter or a lazy query.
    instance_value = _get_instance_data(obj, name)
    if instance_value is _UNASSIGNED_SLOT:
        policy.record_missing(record_key)
        return default
    if instance_value is not _NO_INSTANCE_DATA:
        return instance_value

    descriptor = _find_descriptor(obj, name)
    if descriptor is not None and hasattr(descriptor, "__get__"):
        # The attribute only exists behind a descriptor: ask the policy before
        # ever invoking the getter.
        if not policy.allows(name, obj):
            policy.record_forbidden(record_key)
            return forbidden_access
        return getattr(obj, name, default)

    # No descriptor and nothing on the instance: the attribute is genuinely
    # absent. Custom __getattr__ hooks are deliberately not invoked, as they
    # may trigger lazy loading.
    policy.record_missing(record_key)
    return default


class AccessPolicy:
    """Declare which attribute accesses a dump may perform.

    :param mode: ``"instance"`` reads only data already present on each
        instance. ``"allowlist"`` additionally permits descriptor access for
        the names given in ``allowed_fields``.
    :param allowed_fields: Field/attribute names whose descriptors may be
        invoked. Ignored in ``"instance"`` mode. When ``None`` in
        ``"allowlist"`` mode, every declared field is allowed (useful when the
        schema field list itself is the trusted declaration scope).
    """

    def __init__(
        self,
        mode: AccessMode = INSTANCE_MODE,
        *,
        allowed_fields: typing.Collection[str] | None = None,
    ) -> None:
        if mode not in ACCESS_MODES:
            raise AccessPolicyError(
                f"Invalid access mode {mode!r}. Valid modes: {sorted(ACCESS_MODES)}."
            )
        self.mode: AccessMode = mode
        self.allowed_fields: frozenset[str] | None = (
            None if allowed_fields is None else frozenset(allowed_fields)
        )
        self._forbidden: set[str] = set()
        self._missing: set[str] = set()
        self._read_plan: ReadPlan | None = None
        self._token: contextvars.Token[AccessPolicy | None] | None = None

    def __repr__(self) -> str:
        return (
            f"<AccessPolicy(mode={self.mode!r}, "
            f"allowed_fields={set(self.allowed_fields) if self.allowed_fields else None})>"
        )

    def __enter__(self) -> typing.Self:
        self._token = _ACTIVE_POLICY.set(self)
        return self

    def __exit__(self, *exc: object) -> None:
        token = self._token
        if token is not None:
            _ACTIVE_POLICY.reset(token)
            self._token = None

    def allows(self, name: str, obj: typing.Any) -> bool:
        """Whether the policy permits invoking a descriptor named ``name``.

        Instance data and mappings are not gated by this check; only
        descriptor/getter access is.
        """
        if self.mode == INSTANCE_MODE:
            return False
        if self.allowed_fields is None:
            return True
        return name in self.allowed_fields

    def record_forbidden(self, name: str) -> None:
        self._forbidden.add(name)

    def record_missing(self, name: str) -> None:
        self._missing.add(name)

    @property
    def forbidden(self) -> frozenset[str]:
        """Attribute names whose access was blocked."""
        return frozenset(self._forbidden)

    @property
    def missing(self) -> frozenset[str]:
        """Attribute names that were legitimately absent."""
        return frozenset(self._missing)

    def build_read_plan(
        self,
        dump_fields: typing.Collection[str],
        attribute_to_field: typing.Mapping[str, str] | None = None,
    ) -> ReadPlan:
        """Build the :class:`ReadPlan` for a finished dump.

        :param dump_fields: Names of the fields the schema attempted to dump.
        :param attribute_to_field: Optional mapping from the attribute key a
            field reads (including dot paths and ``attribute=`` values) back
            to the declared field name, so the plan always speaks in field
            names.
        """
        mapping = attribute_to_field or {}
        forbidden = {mapping.get(name, name) for name in self._forbidden}
        missing_names = {mapping.get(name, name) for name in self._missing}
        unevaluated = forbidden | missing_names
        evaluated = frozenset(dump_fields) - unevaluated
        return ReadPlan(
            evaluated_fields=frozenset(evaluated),
            forbidden_fields=frozenset(forbidden),
            missing_fields=frozenset(missing_names),
        )

    @property
    def read_plan(self) -> ReadPlan | None:
        """The :class:`ReadPlan` attached by the most recent dump, if any."""
        return self._read_plan

    def dump(self, schema: Schema, obj: typing.Any, *, many: bool | None = None):
        """Run ``schema.dump`` with this policy active and attach the plan."""
        with self:
            return schema.dump(obj, many=many)


class ReadPlan:
    """Description of which fields a dump evaluated and which it skipped.

    Instances are immutable views returned by
    :attr:`AccessPolicy.read_plan`.
    """

    def __init__(
        self,
        *,
        evaluated_fields: typing.Collection[str] = (),
        forbidden_fields: typing.Collection[str] = (),
        missing_fields: typing.Collection[str] = (),
    ) -> None:
        self.evaluated_fields: frozenset[str] = frozenset(evaluated_fields)
        self.forbidden_fields: frozenset[str] = frozenset(forbidden_fields)
        self.missing_fields: frozenset[str] = frozenset(missing_fields)

    @property
    def unevaluated(self) -> frozenset[str]:
        """All fields that produced no value, with the reason recorded in
        :attr:`forbidden_fields` and :attr:`missing_fields`."""
        return self.forbidden_fields | self.missing_fields

    def __repr__(self) -> str:
        return (
            "<ReadPlan "
            f"evaluated={set(self.evaluated_fields)} "
            f"forbidden={set(self.forbidden_fields)} "
            f"missing={set(self.missing_fields)}>"
        )

    def __eq__(self, other: object) -> bool:
        if not isinstance(other, ReadPlan):
            return NotImplemented
        return (
            self.evaluated_fields == other.evaluated_fields
            and self.forbidden_fields == other.forbidden_fields
            and self.missing_fields == other.missing_fields
        )

    def __hash__(self) -> int:
        return hash((self.evaluated_fields, self.forbidden_fields, self.missing_fields))
