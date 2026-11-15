from __future__ import annotations

from collections.abc import Callable, Iterable, Mapping, Sequence
from contextlib import suppress
from datetime import date, datetime
from enum import StrEnum
from importlib.util import find_spec
from sys import modules as _imported_modules
from urllib.parse import unquote
from uuid import UUID
import ipaddress
import re
import string
import warnings

from jsonschema.exceptions import FormatCapabilityError, FormatError

#: A format checker callable.
_FormatCheckCallable = Callable[[object], bool]
_RaisesType = type[Exception] | tuple[type[Exception], ...]

_RE_DATE = re.compile(r"^\d{4}-\d{2}-\d{2}$", re.ASCII)


class FormatCapability(StrEnum):
    """
    The availability of a format name for a particular `FormatChecker`.
    """

    AVAILABLE = "available"
    UNREGISTERED = "unregistered"
    MISSING_DEPENDENCY = "missing-dependency"


class MissingFormat(str):
    """
    A format capability which is unavailable, along with why it is missing.

    Instances compare equal to their format name (they are strings), while
    also carrying the reason, any missing optional dependencies, and the
    location in the schema at which the format was declared.
    """

    def __new__(
        cls,
        format: str,
        *,
        reason: FormatCapability,
        dependencies: Iterable[str] = (),
        resource: str | None = None,
        schema_path: Iterable[str | int] = (),
    ):
        self = super().__new__(cls, format)
        self.format = format
        self.reason = reason
        self.dependencies = tuple(dependencies)
        self.resource = resource or ""
        self.schema_path = self.path = tuple(schema_path)
        self.locations: list[tuple[str, tuple[str | int, ...]]] = [
            (self.resource, self.schema_path),
        ]
        return self

    @property
    def pointer(self) -> str:
        return _json_pointer(self.schema_path)

    @property
    def is_unregistered(self) -> bool:
        return self.reason == FormatCapability.UNREGISTERED

    @property
    def is_missing_dependency(self) -> bool:
        return self.reason == FormatCapability.MISSING_DEPENDENCY

    def __repr__(self) -> str:
        return (
            f"<MissingFormat {self.format!r} reason={str(self.reason)!r}"
            f" dependencies={self.dependencies!r}>"
        )


#: Optional dependencies which each format needs in order to be registered.
#:
#: Each value is a tuple of alternatives: at least one of the listed
#: distributions must be installed for a checker implementation to exist.
_FORMAT_DEPENDENCIES: dict[str, tuple[str, ...]] = {
    "color": ("webcolors",),
    "date-time": ("rfc3339-validator",),
    "duration": ("isoduration",),
    "host-name": ("fqdn",),
    "hostname": ("fqdn",),
    "idn-hostname": ("idna",),
    "iri": ("rfc3987", "rfc3987-syntax"),
    "iri-reference": ("rfc3987", "rfc3987-syntax"),
    "json-pointer": ("jsonpointer",),
    "relative-json-pointer": ("jsonpointer",),
    "time": ("rfc3339-validator",),
    "uri": ("rfc3987", "rfc3986-validator"),
    "uri-reference": ("rfc3987", "rfc3986-validator"),
    "uri-template": ("uri-template",),
}

_DEPENDENCY_MODULES: dict[str, str] = {
    "fqdn": "fqdn",
    "idna": "idna",
    "isoduration": "isoduration",
    "jsonpointer": "jsonpointer",
    "rfc3339-validator": "rfc3339_validator",
    "rfc3986-validator": "rfc3986_validator",
    "rfc3987": "rfc3987",
    "rfc3987-syntax": "rfc3987_syntax",
    "uri-template": "uri_template",
    "webcolors": "webcolors",
}

_FORMATS_WITHOUT_DEPENDENCIES = frozenset(
    {
        "date",
        "email",
        "idn-email",
        "ip-address",
        "ipv4",
        "ipv6",
        "regex",
        "uuid",
    },
)

_KNOWN_FORMATS = (
    frozenset(_FORMAT_DEPENDENCIES)
    | _FORMATS_WITHOUT_DEPENDENCIES
)


class FormatChecker:
    """
    A ``format`` property checker.

    JSON Schema does not mandate that the ``format`` property actually do any
    validation. If validation is desired however, instances of this class can
    be hooked into validators to enable format validation.

    `FormatChecker` objects always return ``True`` when asked about
    formats that they do not know how to validate.

    To add a check for a custom format use the `FormatChecker.checks`
    decorator.

    Arguments:

        formats:

            The known formats to validate. This argument can be used to
            limit which formats will be used during validation.

    """

    checkers: dict[
        str,
        tuple[_FormatCheckCallable, _RaisesType],
    ] = {}  # noqa: RUF012

    def __init__(
        self,
        formats: Iterable[str] | None = None,
        strict: bool = False,
    ):
        if formats is None:
            formats = self.checkers.keys()
        self.checkers = {k: self.checkers[k] for k in formats}
        self.strict = strict

    def __repr__(self):
        return f"<FormatChecker checkers={sorted(self.checkers)}>"

    def checks[F: _FormatCheckCallable](
        self,
        format: str,
        raises: _RaisesType = (),
    ) -> Callable[[F], F]:
        """
        Register a decorated function as validating a new format.

        Arguments:

            format:

                The format that the decorated function will check.

            raises:

                The exception(s) raised by the decorated function when an
                invalid instance is found.

                The exception object will be accessible as the
                `jsonschema.exceptions.ValidationError.cause` attribute of the
                resulting validation error.

        """

        def _checks(func: F) -> F:
            self.checkers[format] = (func, raises)
            return func

        return _checks

    @classmethod
    def cls_checks[F: _FormatCheckCallable](
        cls,
        format: str,
        raises: _RaisesType = (),
    ) -> Callable[[F], F]:
        warnings.warn(
            (
                "FormatChecker.cls_checks is deprecated. Call "
                "FormatChecker.checks on a specific FormatChecker instance "
                "instead."
            ),
            DeprecationWarning,
            stacklevel=2,
        )
        return cls._cls_checks(format=format, raises=raises)

    @classmethod
    def _cls_checks[F: _FormatCheckCallable](
        cls,
        format: str,
        raises: _RaisesType = (),
    ) -> Callable[[F], F]:
        def _checks(func: F) -> F:
            cls.checkers[format] = (func, raises)
            return func

        return _checks

    def check(
        self,
        instance: object,
        format: str,
        *,
        strict: bool | None = None,
    ) -> None:
        """
        Check whether the instance conforms to the given format.

        Arguments:

            instance (*any primitive type*, i.e. str, number, bool):

                The instance to check

            format:

                The format that instance should conform to

        Raises:

            FormatError:

                if the instance does not conform to ``format``

        """
        if format not in self.checkers:
            strict = self.strict if strict is None else strict
            if strict:
                status = self.format_status(format)
                if status is FormatCapability.MISSING_DEPENDENCY:
                    dependencies = self.missing_dependencies_for(format)
                    raise FormatError(
                        f"{instance!r} cannot be checked as {format!r}: "
                        "missing optional dependency: "
                        + ", ".join(dependencies),
                    )
                raise FormatError(
                    f"{instance!r} cannot be checked as {format!r}: "
                    "the format is not registered",
                )
            return

        func, raises = self.checkers[format]
        result, cause = None, None
        try:
            result = func(instance)
        except raises as e:
            cause = e
        if not result:
            raise FormatError(f"{instance!r} is not a {format!r}", cause=cause)

    def conforms(
        self,
        instance: object,
        format: str,
        *,
        strict: bool | None = None,
    ) -> bool:
        """
        Check whether the instance conforms to the given format.

        Arguments:

            instance (*any primitive type*, i.e. str, number, bool):

                The instance to check

            format:

                The format that instance should conform to

        Returns:

            bool: whether it conformed

        """
        try:
            self.check(instance, format, strict=strict)
        except FormatError:
            return False
        else:
            return True

    @property
    def available_formats(self) -> frozenset[str]:
        """
        The format names which this checker can actually assert.
        """
        return frozenset(self.checkers)

    @property
    def known_formats(self) -> frozenset[str]:
        """
        The format names known to this implementation.

        This includes formats whose optional dependency is not installed, as
        well as formats this particular checker was not configured with.
        """
        return _KNOWN_FORMATS | frozenset(FormatChecker.checkers)

    def missing_dependencies_for(self, format: str) -> tuple[str, ...]:
        """
        The optional dependencies which are unavailable for ``format``.

        The returned names are distributions, e.g. ``"rfc3339-validator"``,
        suitable for reporting to someone installing missing extras.
        """
        return tuple(
            dependency
            for dependency in _FORMAT_DEPENDENCIES.get(format, ())
            if not _dependency_is_available(dependency)
        )

    def format_status(self, format: str) -> FormatCapability:
        """
        Whether ``format`` is available, unknown, or needs a dependency.
        """
        if format in self.checkers:
            return FormatCapability.AVAILABLE

        dependencies = self.missing_dependencies_for(format)
        if dependencies and len(dependencies) == len(
            _FORMAT_DEPENDENCIES[format],
        ):
            return FormatCapability.MISSING_DEPENDENCY
        return FormatCapability.UNREGISTERED

    def iter_missing_formats(
        self,
        schema,
        *,
        registry=None,
        resolver=None,
        follow_references: bool = True,
        validator_cls=None,
        strict=None,
        strict_formats=None,
    ):
        """
        Iterate every unavailable :kw:`format` declared by ``schema``.

        Declarations in referenced resources and in all branches of
        applicators are included, regardless of which branch an instance
        would eventually select.  No instance validation is performed.

        ``registry`` is used to resolve references.  When it is not provided,
        an empty in-memory registry is used, so this method never retrieves
        remote resources implicitly.
        """
        for name, resource, path in _iter_format_declarations(
            schema,
            registry=registry,
            resolver=resolver,
            follow_references=follow_references,
            validator_cls=validator_cls,
        ):
            status = self.format_status(name)
            if status is FormatCapability.AVAILABLE:
                continue
            dependencies = (
                self.missing_dependencies_for(name)
                if status is FormatCapability.MISSING_DEPENDENCY
                else ()
            )
            yield MissingFormat(
                name,
                reason=status,
                dependencies=dependencies,
                resource=resource,
                schema_path=path,
            )

    def missing_formats(
        self,
        schema,
        *,
        registry=None,
        resolver=None,
        follow_references: bool = True,
        validator_cls=None,
        strict=None,
        strict_formats=None,
    ) -> tuple[MissingFormat, ...]:
        """
        Return each distinct unavailable format declared by ``schema``.

        The result is sorted by format name.  Repeated declarations of the
        same format are merged, with every location available via the
        `MissingFormat.locations` attribute.
        """
        by_name: dict[str, MissingFormat] = {}
        for missing in self.iter_missing_formats(
            schema,
            registry=registry,
            resolver=resolver,
            follow_references=follow_references,
            validator_cls=validator_cls,
        ):
            existing = by_name.get(missing.format)
            if existing is None:
                by_name[missing.format] = missing
            else:
                existing.locations.append(
                    (missing.resource, missing.schema_path),
                )
        return tuple(sorted(by_name.values(), key=str))

    def check_schema(
        self,
        schema,
        *,
        registry=None,
        resolver=None,
        follow_references: bool = True,
        validator_cls=None,
        strict=None,
        strict_formats=None,
    ) -> None:
        """
        Prepare ``schema`` by asserting all declared formats are available.

        Raises:

            `jsonschema.exceptions.FormatCapabilityError`:

                if any declared format is not registered or is missing an
                optional dependency.  All unavailable capabilities are
                reported at once.

        """
        if strict is False or strict_formats is False:
            return

        missing = self.missing_formats(
            schema,
            registry=registry,
            resolver=resolver,
            follow_references=follow_references,
            validator_cls=validator_cls,
        )
        if missing:
            raise FormatCapabilityError(
                _format_capability_message(missing),
                missing=missing,
                schema=schema,
            )


draft3_format_checker = FormatChecker()
draft4_format_checker = FormatChecker()
draft6_format_checker = FormatChecker()
draft7_format_checker = FormatChecker()
draft201909_format_checker = FormatChecker()
draft202012_format_checker = FormatChecker()

_draft_checkers: dict[str, FormatChecker] = dict(
    draft3=draft3_format_checker,
    draft4=draft4_format_checker,
    draft6=draft6_format_checker,
    draft7=draft7_format_checker,
    draft201909=draft201909_format_checker,
    draft202012=draft202012_format_checker,
)


def _checks_drafts[F: _FormatCheckCallable](
    name=None,
    draft3=None,
    draft4=None,
    draft6=None,
    draft7=None,
    draft201909=None,
    draft202012=None,
    raises=(),
) -> Callable[[F], F]:
    draft3 = draft3 or name
    draft4 = draft4 or name
    draft6 = draft6 or name
    draft7 = draft7 or name
    draft201909 = draft201909 or name
    draft202012 = draft202012 or name

    def wrap(func: F) -> F:
        if draft3:
            func = _draft_checkers["draft3"].checks(draft3, raises)(func)
        if draft4:
            func = _draft_checkers["draft4"].checks(draft4, raises)(func)
        if draft6:
            func = _draft_checkers["draft6"].checks(draft6, raises)(func)
        if draft7:
            func = _draft_checkers["draft7"].checks(draft7, raises)(func)
        if draft201909:
            func = _draft_checkers["draft201909"].checks(draft201909, raises)(
                func,
            )
        if draft202012:
            func = _draft_checkers["draft202012"].checks(draft202012, raises)(
                func,
            )

        # Oy. This is bad global state, but relied upon for now, until
        # deprecation. See #519 and test_format_checkers_come_with_defaults
        FormatChecker._cls_checks(
            draft202012 or draft201909 or draft7 or draft6 or draft4 or draft3,
            raises,
        )(func)
        return func

    return wrap


@_checks_drafts(name="idn-email")
@_checks_drafts(name="email")
def is_email(instance: object) -> bool:
    if not isinstance(instance, str):
        return True
    return "@" in instance


@_checks_drafts(
    draft3="ip-address",
    draft4="ipv4",
    draft6="ipv4",
    draft7="ipv4",
    draft201909="ipv4",
    draft202012="ipv4",
    raises=ipaddress.AddressValueError,
)
def is_ipv4(instance: object) -> bool:
    if not isinstance(instance, str):
        return True
    return bool(ipaddress.IPv4Address(instance))


@_checks_drafts(name="ipv6", raises=ipaddress.AddressValueError)
def is_ipv6(instance: object) -> bool:
    if not isinstance(instance, str):
        return True
    address = ipaddress.IPv6Address(instance)
    return not getattr(address, "scope_id", "")


with suppress(ImportError):
    from fqdn import FQDN

    @_checks_drafts(
        draft3="host-name",
        draft4="hostname",
        draft6="hostname",
        draft7="hostname",
        draft201909="hostname",
        draft202012="hostname",
        # fqdn.FQDN("") raises a ValueError due to a bug
        # however, it's not clear when or if that will be fixed, so catch it
        # here for now
        raises=ValueError,
    )
    def is_host_name(instance: object) -> bool:
        if not isinstance(instance, str):
            return True
        return FQDN(instance, min_labels=1).is_valid


with suppress(ImportError):
    # The built-in `idna` codec only implements RFC 3890, so we go elsewhere.
    import idna

    @_checks_drafts(
        draft7="idn-hostname",
        draft201909="idn-hostname",
        draft202012="idn-hostname",
        raises=(idna.IDNAError, UnicodeError),
    )
    def is_idn_host_name(instance: object) -> bool:
        if not isinstance(instance, str):
            return True
        idna.encode(instance)
        return True


try:
    import rfc3987
except ImportError:
    with suppress(ImportError):
        from rfc3986_validator import validate_rfc3986

        @_checks_drafts(name="uri")
        def is_uri(instance: object) -> bool:
            if not isinstance(instance, str):
                return True
            return validate_rfc3986(instance, rule="URI")

        @_checks_drafts(
            draft6="uri-reference",
            draft7="uri-reference",
            draft201909="uri-reference",
            draft202012="uri-reference",
            raises=ValueError,
        )
        def is_uri_reference(instance: object) -> bool:
            if not isinstance(instance, str):
                return True
            return validate_rfc3986(instance, rule="URI_reference")

    with suppress(ImportError):
        from rfc3987_syntax import is_valid_syntax as _rfc3987_is_valid_syntax

        @_checks_drafts(
            draft7="iri",
            draft201909="iri",
            draft202012="iri",
            raises=ValueError,
        )
        def is_iri(instance: object) -> bool:
            if not isinstance(instance, str):
                return True
            return _rfc3987_is_valid_syntax("iri", instance)

        @_checks_drafts(
            draft7="iri-reference",
            draft201909="iri-reference",
            draft202012="iri-reference",
            raises=ValueError,
        )
        def is_iri_reference(instance: object) -> bool:
            if not isinstance(instance, str):
                return True
            return _rfc3987_is_valid_syntax("iri_reference", instance)

else:

    @_checks_drafts(
        draft7="iri",
        draft201909="iri",
        draft202012="iri",
        raises=ValueError,
    )
    def is_iri(instance: object) -> bool:
        if not isinstance(instance, str):
            return True
        return rfc3987.parse(instance, rule="IRI")

    @_checks_drafts(
        draft7="iri-reference",
        draft201909="iri-reference",
        draft202012="iri-reference",
        raises=ValueError,
    )
    def is_iri_reference(instance: object) -> bool:
        if not isinstance(instance, str):
            return True
        return rfc3987.parse(instance, rule="IRI_reference")

    @_checks_drafts(name="uri", raises=ValueError)
    def is_uri(instance: object) -> bool:
        if not isinstance(instance, str):
            return True
        return rfc3987.parse(instance, rule="URI")

    @_checks_drafts(
        draft6="uri-reference",
        draft7="uri-reference",
        draft201909="uri-reference",
        draft202012="uri-reference",
        raises=ValueError,
    )
    def is_uri_reference(instance: object) -> bool:
        if not isinstance(instance, str):
            return True
        return rfc3987.parse(instance, rule="URI_reference")


with suppress(ImportError):
    from rfc3339_validator import validate_rfc3339

    @_checks_drafts(name="date-time")
    def is_datetime(instance: object) -> bool:
        if not isinstance(instance, str):
            return True
        return validate_rfc3339(instance.upper())

    @_checks_drafts(
        draft7="time",
        draft201909="time",
        draft202012="time",
    )
    def is_time(instance: object) -> bool:
        if not isinstance(instance, str):
            return True
        return is_datetime("1970-01-01T" + instance)


@_checks_drafts(name="regex", raises=re.error)
def is_regex(instance: object) -> bool:
    if not isinstance(instance, str):
        return True
    return bool(re.compile(instance))


@_checks_drafts(
    draft3="date",
    draft7="date",
    draft201909="date",
    draft202012="date",
    raises=ValueError,
)
def is_date(instance: object) -> bool:
    if not isinstance(instance, str):
        return True
    return bool(_RE_DATE.fullmatch(instance) and date.fromisoformat(instance))


@_checks_drafts(draft3="time", raises=ValueError)
def is_draft3_time(instance: object) -> bool:
    if not isinstance(instance, str):
        return True
    return bool(datetime.strptime(instance, "%H:%M:%S"))  # noqa: DTZ007


with suppress(ImportError):
    import webcolors

    @_checks_drafts(draft3="color", raises=(ValueError, TypeError))
    def is_css21_color(instance: object) -> bool:
        if isinstance(instance, str):
            try:
                webcolors.name_to_hex(instance)
            except ValueError:
                webcolors.normalize_hex(instance.lower())
        return True


with suppress(ImportError):
    import jsonpointer

    @_checks_drafts(
        draft6="json-pointer",
        draft7="json-pointer",
        draft201909="json-pointer",
        draft202012="json-pointer",
        raises=jsonpointer.JsonPointerException,
    )
    def is_json_pointer(instance: object) -> bool:
        if not isinstance(instance, str):
            return True
        return bool(jsonpointer.JsonPointer(instance))

    # TODO: I don't want to maintain this, so it
    #       needs to go either into jsonpointer (pending
    #       https://github.com/stefankoegl/python-json-pointer/issues/34) or
    #       into a new external library.
    @_checks_drafts(
        draft7="relative-json-pointer",
        draft201909="relative-json-pointer",
        draft202012="relative-json-pointer",
        raises=jsonpointer.JsonPointerException,
    )
    def is_relative_json_pointer(instance: object) -> bool:
        # Definition taken from:
        # https://tools.ietf.org/html/draft-handrews-relative-json-pointer-01#section-3
        if not isinstance(instance, str):
            return True
        if not instance:
            return False

        # A non-negative-integer prefix of ASCII digits (%x30-39), which is
        # either "0" or has no leading "0".
        rest = instance.lstrip(string.digits)
        prefix = instance[: len(instance) - len(rest)]
        if not prefix:
            return False
        if len(prefix) > 1 and prefix[0] == "0":
            return False
        return (rest == "#") or bool(jsonpointer.JsonPointer(rest))


with suppress(ImportError):
    import uri_template

    @_checks_drafts(
        draft6="uri-template",
        draft7="uri-template",
        draft201909="uri-template",
        draft202012="uri-template",
    )
    def is_uri_template(instance: object) -> bool:
        if not isinstance(instance, str):
            return True
        return uri_template.validate(instance)


with suppress(ImportError):
    import isoduration

    @_checks_drafts(
        draft201909="duration",
        draft202012="duration",
        raises=isoduration.DurationParsingException,
    )
    def is_duration(instance: object) -> bool:
        if not isinstance(instance, str):
            return True
        isoduration.parse_duration(instance)
        # FIXME: See bolsote/isoduration#25 and bolsote/isoduration#21
        return instance.endswith(tuple("DMYWHMS"))


@_checks_drafts(
    draft201909="uuid",
    draft202012="uuid",
    raises=ValueError,
)
def is_uuid(instance: object) -> bool:
    if not isinstance(instance, str):
        return True
    return str(UUID(instance)) == instance.lower()


_SINGLE_SUBSCHEMA_KEYWORDS = frozenset(
    {
        "additionalItems",
        "additionalProperties",
        "contains",
        "contentSchema",
        "else",
        "if",
        "not",
        "propertyNames",
        "then",
        "unevaluatedItems",
        "unevaluatedProperties",
    },
)

_SEQUENCE_SUBSCHEMA_KEYWORDS = frozenset(
    {
        "allOf",
        "anyOf",
        "oneOf",
        "prefixItems",
    },
)

_MAPPING_SUBSCHEMA_KEYWORDS = frozenset(
    {
        "$defs",
        "definitions",
        "dependentSchemas",
        "patternProperties",
        "properties",
    },
)


def _dependency_is_available(distribution: str) -> bool:
    module = _DEPENDENCY_MODULES.get(
        distribution,
        distribution.replace("-", "_"),
    )
    if module in _imported_modules:
        return _imported_modules[module] is not None
    try:
        return find_spec(module) is not None
    except (ImportError, ModuleNotFoundError, ValueError):
        return False


def _json_pointer(path: Iterable[str | int]) -> str:
    pointer = ""
    for part in path:
        escaped = str(part).replace("~", "~0").replace("/", "~1")
        pointer += "/" + escaped
    return "#" + pointer


def _path_from_reference(reference: str) -> tuple[str | int, ...]:
    _, _, fragment = reference.partition("#")
    if not fragment.startswith("/"):
        return ()

    path: list[str | int] = []
    for part in unquote(fragment[1:]).split("/"):
        path.append(int(part) if part.isdigit() else part)
    return tuple(path)


def _specification_for(validator_cls):
    from referencing import Specification
    from referencing.jsonschema import specification_with

    try:
        dialect_id = validator_cls.ID_OF(validator_cls.META_SCHEMA)
    except Exception:  # noqa: BLE001
        dialect_id = None
    return specification_with(
        dialect_id=dialect_id or "urn:unknown-dialect",
        default=Specification.OPAQUE,
    )


def _resource_for(contents, specification):
    from referencing import Resource

    try:
        return Resource.from_contents(
            contents,
            default_specification=specification,
        )
    except Exception:  # noqa: BLE001
        return specification.create_resource(contents)


def _iter_subschemas(schema: Mapping):
    for keyword in _SINGLE_SUBSCHEMA_KEYWORDS:
        if keyword in schema:
            yield schema[keyword], (keyword,)

    for keyword in _SEQUENCE_SUBSCHEMA_KEYWORDS:
        value = schema.get(keyword)
        if isinstance(value, Sequence) and not isinstance(
            value,
            (str, bytes, bytearray, Mapping),
        ):
            for index, subschema in enumerate(value):
                yield subschema, (keyword, index)

    for keyword in _MAPPING_SUBSCHEMA_KEYWORDS:
        value = schema.get(keyword)
        if isinstance(value, Mapping):
            for name, subschema in value.items():
                yield subschema, (keyword, name)

    items = schema.get("items")
    if isinstance(items, (Mapping, bool)):
        yield items, ("items",)
    elif isinstance(items, Sequence) and not isinstance(
        items,
        (str, bytes, bytearray),
    ):
        for index, subschema in enumerate(items):
            yield subschema, ("items", index)

    dependencies = schema.get("dependencies")
    if isinstance(dependencies, Mapping):
        for name, dependency in dependencies.items():
            if isinstance(dependency, (Mapping, bool)):
                yield dependency, ("dependencies", name)

    extends = schema.get("extends")
    if isinstance(extends, (Mapping, bool)):
        yield extends, ("extends",)
    elif isinstance(extends, Sequence) and not isinstance(
        extends,
        (str, bytes, bytearray),
    ):
        for index, subschema in enumerate(extends):
            yield subschema, ("extends", index)

    # Draft 3 allowed schemas as members of these unions.
    for keyword in ("type", "disallow"):
        value = schema.get(keyword)
        if isinstance(value, Mapping):
            yield value, (keyword,)
        elif isinstance(value, Sequence) and not isinstance(
            value,
            (str, bytes, bytearray),
        ):
            for index, member in enumerate(value):
                if isinstance(member, Mapping):
                    yield member, (keyword, index)


def _walk_schema(
    schema,
    resolver,
    specification,
    *,
    path=(),
    seen=None,
    follow_references=True,
    apply_id=True,
):
    if not isinstance(schema, Mapping):
        return

    if seen is None:
        seen = set()
    identity = (id(schema), getattr(resolver, "_base_uri", ""))
    if identity in seen:
        return
    seen.add(identity)

    if apply_id:
        resource = _resource_for(schema, specification)
        if resource.id() is not None:
            resolver = resolver.in_subresource(resource)

    format = schema.get("format")
    if isinstance(format, str):
        yield format, getattr(resolver, "_base_uri", "") or None, path

    if follow_references:
        for keyword in ("$ref", "$dynamicRef", "$recursiveRef"):
            reference = schema.get(keyword)
            if not isinstance(reference, str):
                continue
            try:
                resolved = resolver.lookup(reference)
            except Exception:  # noqa: BLE001
                continue
            yield from _walk_schema(
                resolved.contents,
                resolved.resolver,
                specification,
                path=_path_from_reference(reference),
                seen=seen,
                follow_references=follow_references,
                apply_id=False,
            )

    for subschema, suffix in _iter_subschemas(schema):
        yield from _walk_schema(
            subschema,
            resolver,
            specification,
            path=(*path, *suffix),
            seen=seen,
            follow_references=follow_references,
        )


def _iter_format_declarations(
    schema,
    *,
    registry=None,
    resolver=None,
    follow_references=True,
    validator_cls=None,
):
    if not isinstance(schema, Mapping):
        return

    from jsonschema.validators import Draft202012Validator, validator_for

    if validator_cls is None:
        validator_cls = validator_for(schema, default=Draft202012Validator)

    if resolver is None:
        if registry is None:
            from referencing import Registry

            registry = Registry()
        validator = validator_cls(schema, registry=registry)
        resolver = validator._resolver

    specification = _specification_for(validator_cls)
    yield from _walk_schema(
        schema,
        resolver,
        specification,
        follow_references=follow_references,
    )


def _format_capability_message(
    missing: Iterable[MissingFormat],
) -> str:
    descriptions = []
    for each in missing:
        if each.reason is FormatCapability.MISSING_DEPENDENCY:
            reason = "missing dependency (not installed): " + " or ".join(
                each.dependencies,
            )
        else:
            reason = "not registered; unknown format"

        locations = []
        for resource, path in each.locations:
            pointer = _json_pointer(path)
            if resource:
                locations.append(f"{resource}{pointer}")
            else:
                locations.append(pointer)
        location = ""
        if locations:
            location = " at " + ", ".join(sorted(set(locations)))

        descriptions.append(f"{each.format!r} ({reason}){location}")

    return "Unavailable format capabilities: " + "; ".join(descriptions)
