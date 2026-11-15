"""Exception classes for marshmallow-related errors."""

from __future__ import annotations

import typing

from marshmallow import structured

# Key used for schema-level validation errors
SCHEMA = "_schema"
# Keys used by ``Mapping`` fields to separate key/value sub-errors.
MAPPING_KEY_ERROR = "key"
MAPPING_VALUE_ERROR = "value"


class MarshmallowError(Exception):
    """Base class for all marshmallow-related errors."""


class ValidationError(MarshmallowError):
    """Raised when validation fails on a field or schema.

    Validators and custom fields should raise this exception.

    :param message: An error message, list of error messages, or dict of
        error messages. If a dict, the keys are subitems and the values are error messages.
    :param field_name: Field name to store the error on.
    :param data: Raw input data.
    :param valid_data: Valid (de)serialized data.
    """

    def __init__(
        self,
        message: str | list | dict,
        field_name: str = SCHEMA,
        data: typing.Mapping[str, typing.Any]
        | typing.Iterable[typing.Mapping[str, typing.Any]]
        | None = None,
        valid_data: list[typing.Any] | dict[str, typing.Any] | None = None,
        structured_errors: list[structured.StructuredError] | None = None,
        **kwargs,
    ):
        self.messages = [message] if isinstance(message, (str, bytes)) else message
        self.field_name = field_name
        self.data = data
        self.valid_data = valid_data
        self.kwargs = kwargs
        if structured_errors is not None:
            self.structured_errors = list(structured_errors)
        else:
            # A schema-level (default ``field_name``) message, or a dict that
            # already encodes its own field keys, is rooted without an extra
            # field segment. Scalar/list messages carry ``field_name``.
            root_name = (
                None
                if field_name == SCHEMA or isinstance(self.messages, dict)
                else field_name
            )
            self.structured_errors = structured_errors_from_messages(
                self.messages, root_name
            )
        super().__init__(message)

    def normalized_messages(self):
        if self.field_name == SCHEMA and isinstance(self.messages, dict):
            return self.messages
        return {self.field_name: self.messages}

    @property
    def messages_dict(self) -> dict[str, typing.Any]:
        if not isinstance(self.messages, dict):
            raise TypeError(
                "cannot access 'messages_dict' when 'messages' is of type "
                + type(self.messages).__name__
            )
        return self.messages

    @property
    def structured(self) -> list[structured.StructuredError]:
        """Typed, path-based view of the validation errors.

        Unlike :attr:`messages`, each :class:`~marshmallow.structured.StructuredError`
        keeps the distinction between list indices, tuple positions, mapping
        keys/values, field names and schema-level errors. The information cannot
        be reconstructed from the flattened string messages.
        """
        return list(self.structured_errors)

    def structured_jsonable(self) -> list[dict[str, typing.Any]]:
        """Return :attr:`structured` as JSON-serializable primitives.

        Non-string mapping keys are type-tagged rather than coerced with
        ``str()``, so they can never collide.
        """
        return [error.to_jsonable() for error in self.structured_errors]


def structured_errors_from_messages(
    messages: typing.Any,
    field_name: str | None,
) -> list[structured.StructuredError]:
    """Derive typed structured errors from a (possibly nested) message tree.

    ``field_name`` is the name to attach at the root, or ``None`` for a
    schema-level (``_schema``) message tree.

    The mapping structure produced by ``Mapping`` fields uses the literal
    strings ``"key"`` and ``"value"``; these are recognized and translated into
    ``mapping_key`` / ``mapping_value`` segments so they cannot be confused
    with ordinary field names.
    """
    prefix: tuple[structured.PathSegment, ...] = ()
    if field_name is not None:
        prefix = (structured.PathSegment.field(field_name),)
    return _collect_structured(messages, prefix)


def prepend_path(
    errors: list[structured.StructuredError],
    segments: tuple[structured.PathSegment, ...],
) -> list[structured.StructuredError]:
    """Return ``errors`` with ``segments`` prepended to every path."""
    if not segments:
        return list(errors)
    return [
        structured.StructuredError(segments + error.path, error.messages)
        for error in errors
    ]


def _collect_structured(
    node: typing.Any,
    prefix: tuple[structured.PathSegment, ...],
) -> list[structured.StructuredError]:
    if not isinstance(node, dict):
        return [structured.StructuredError(prefix, node)]

    errors: list[structured.StructuredError] = []
    for key, value in node.items():
        if key == SCHEMA:
            errors.extend(
                _collect_structured(
                    value,
                    prefix + (structured.PathSegment.schema(),),
                )
            )
        elif key == MAPPING_KEY_ERROR:
            errors.extend(
                _collect_structured(
                    value,
                    _mapping_key_prefix(prefix),
                )
            )
        elif key == MAPPING_VALUE_ERROR:
            errors.extend(
                _collect_structured(
                    value,
                    _mapping_value_prefix(prefix),
                )
            )
        else:
            segment: structured.PathSegment
            if isinstance(key, int) and not isinstance(key, bool):
                segment = structured.PathSegment.index(key)
            else:
                segment = structured.PathSegment.field(str(key))
            errors.extend(
                _collect_structured(
                    value, prefix + (segment,)
                )
            )
    return errors


def _mapping_key_prefix(
    prefix: tuple[structured.PathSegment, ...],
) -> tuple[structured.PathSegment, ...]:
    """Resolve the implicit mapping key for a ``"key"`` sub-message.

    A ``Mapping`` stores key errors as ``{key: {"key": ...}}``. The key itself
    is the trailing ``mapping_key`` segment; for a bare ``{"key": ...}`` tree
    the key is unknown, so it is left untagged.
    """
    if prefix and prefix[-1].is_mapping_key:
        return prefix
    return prefix + (structured.PathSegment.mapping_key(None),)


def _mapping_value_prefix(
    prefix: tuple[structured.PathSegment, ...],
) -> tuple[structured.PathSegment, ...]:
    """Resolve the implicit mapping key for a ``"value"`` sub-message."""
    if prefix and prefix[-1].is_mapping_key:
        return prefix + (structured.PathSegment.mapping_value(),)
    return prefix + (
        structured.PathSegment.mapping_key(None),
        structured.PathSegment.mapping_value(),
    )


class RegistryError(NameError):
    """Raised when an invalid operation is performed on the serializer
    class registry.
    """


class StringNotCollectionError(MarshmallowError, TypeError):
    """Raised when a string is passed when a list of strings is expected."""


class _FieldInstanceResolutionError(MarshmallowError, TypeError):
    """Raised when an argument is passed to a field class that cannot be resolved to a Field instance."""
