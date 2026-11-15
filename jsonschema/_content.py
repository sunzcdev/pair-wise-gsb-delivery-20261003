"""
Explicitly opt-in validation of the content-related keywords.

By default :kw:`contentEncoding`, :kw:`contentMediaType` and
:kw:`contentSchema` are purely *annotational*: normal JSON Schema validation
never decodes a string field on their behalf. This module provides a
`ContentValidator` strategy which, once enabled, actually performs that
content validation.

The first supported combination is a (possibly base64 encoded) JSON string,
optionally validated against a :kw:`contentSchema`.

Validation always proceeds from the outside in: the outer type and the
declared encoding are checked first, and only successfully decoded content is
ever parsed or schema-validated. Errors therefore keep their outer field
location in :attr:`~jsonschema.exceptions.ValidationError.path`, while any
location inside the decoded content is reported separately on
:attr:`~jsonschema.exceptions.ContentValidationError.inner_path`.
"""

from __future__ import annotations

from enum import StrEnum
from typing import TYPE_CHECKING, Any
import base64
import binascii
import json

from jsonschema.exceptions import (
    ContentEncodingError,
    ContentMediaTypeError,
    ContentSchemaValidationError,
    ContentValidationError,
)

if TYPE_CHECKING:
    from collections.abc import Iterable
    from typing import Protocol

    from jsonschema.protocols import Validator

    class _ContentCachingValidator(Validator, Protocol):
        content_validator: ContentValidator | None
        _content_cache: dict[
            tuple[int, int],
            list[ContentValidationError],
        ]


#: The only encoding supported in this first round of content validation.
BASE64 = "base64"

#: The only media type supported in this first round of content validation.
APPLICATION_JSON = "application/json"

#: Default maximum number of decoded bytes allowed for a single string field.
DEFAULT_BYTE_BUDGET = 1024 * 1024


class UnsupportedContentPolicy(StrEnum):
    """
    What to do when a schema declares an unsupported encoding or media type.

    A `ContentValidator` does not know how to validate the declared value.
    """

    #: Report a `ContentValidationError` for the unsupported declaration.
    ERROR = "error"
    #: Skip content validation silently for the affected field.
    IGNORE = "ignore"


class ContentValidator:
    """
    A strategy for validating the content-related keywords.

    It validates :kw:`contentEncoding`, :kw:`contentMediaType` and
    :kw:`contentSchema` once content validation has explicitly been enabled.

    Instances are stateless aside from their configuration and are safe to
    reuse across validator objects and threads.

    Arguments:

        unsupported_encoding:
            the `UnsupportedContentPolicy` applied when a schema declares a
            :kw:`contentEncoding` which this object cannot decode. Only
            ``"base64"`` is currently supported.

        unsupported_media_type:
            the `UnsupportedContentPolicy` applied when a schema declares a
            :kw:`contentMediaType` which this object cannot parse. Only
            ``"application/json"`` is currently supported.

        byte_budget:
            the maximum number of decoded bytes a single string field is
            allowed to produce. The budget is independent for each field
            (and each validation), so large documents elsewhere never
            exhaust it. ``None`` disables the limit.

    """

    def __init__(
        self,
        unsupported_encoding: UnsupportedContentPolicy | str = (
            UnsupportedContentPolicy.ERROR
        ),
        unsupported_media_type: UnsupportedContentPolicy | str = (
            UnsupportedContentPolicy.ERROR
        ),
        byte_budget: int | None = DEFAULT_BYTE_BUDGET,
    ) -> None:
        self.unsupported_encoding = UnsupportedContentPolicy(
            unsupported_encoding,
        )
        self.unsupported_media_type = UnsupportedContentPolicy(
            unsupported_media_type,
        )
        if byte_budget is not None and byte_budget < 0:
            raise ValueError("byte_budget must be non-negative or None")
        self.byte_budget = byte_budget

    def __repr__(self) -> str:
        return (
            "<ContentValidator "
            f"unsupported_encoding={self.unsupported_encoding!r}, "
            f"unsupported_media_type={self.unsupported_media_type!r}, "
            f"byte_budget={self.byte_budget!r}>"
        )

    def iter_errors(
        self,
        validator: Validator,
        schema: dict[str, Any],
        instance: Any,
    ) -> Iterable[ContentValidationError]:
        """
        Validate the content keywords declared in ``schema``.

        The keywords are checked against ``instance``.

        Only string instances are considered, matching the scope of the
        content keywords in the specifications.
        """
        if not isinstance(instance, str):
            return ()

        encoding = schema.get("contentEncoding")
        decoded: str | None = instance
        if encoding is not None:
            error, decoded = self._decode(encoding, instance)
            if error is not None:
                return (error,)
            if decoded is None:
                # An unsupported encoding was explicitly ignored, so there
                # is nothing left for the later stages to work with.
                return ()

        media_type = schema.get("contentMediaType")
        if media_type is None:
            # Without a parsed representation there is nothing a
            # contentSchema can meaningfully be applied to.
            return ()

        media_error, parsed = self._parse(media_type, decoded)
        if media_error is not None:
            return (media_error,)
        if parsed is None:
            # An unsupported media type was explicitly ignored.
            return ()

        if "contentSchema" not in schema:
            return ()

        return self._validate_content_schema(
            validator=validator,
            content_schema=schema["contentSchema"],
            parsed=parsed,
        )

    def _decode(
        self,
        encoding: str,
        instance: str,
    ) -> tuple[ContentEncodingError | None, str | None]:
        if encoding != BASE64:
            if self.unsupported_encoding is UnsupportedContentPolicy.IGNORE:
                return None, None
            error = ContentEncodingError(
                f"Unsupported contentEncoding {encoding!r}; "
                f"only {BASE64!r} content can be validated",
            )
            return error, None

        budget = self.byte_budget
        if budget is not None:
            # The exact decoded length, checked *before* allocating any
            # decoded bytes so the budget is still effective against
            # maliciously large inputs.
            padding = instance[-2:].count("=") if instance else 0
            estimated = (len(instance) * 3 // 4) - padding
            if estimated > budget:
                return (
                    ContentEncodingError(
                        f"Decoding {instance!r} would produce {estimated} "
                        f"bytes, exceeding the byte budget of {budget}",
                    ),
                    None,
                )

        try:
            raw = base64.b64decode(instance, validate=True)
        except (binascii.Error, ValueError) as error:
            return (
                ContentEncodingError(
                    f"{instance!r} is not valid base64-encoded content: "
                    f"{error}",
                ),
                None,
            )

        if budget is not None and len(raw) > budget:
            return (
                ContentEncodingError(
                    f"Decoding {instance!r} produced {len(raw)} bytes, "
                    f"exceeding the byte budget of {budget}",
                ),
                None,
            )

        try:
            return None, raw.decode("utf-8")
        except UnicodeDecodeError as error:
            return (
                ContentEncodingError(
                    f"The base64-decoded content of {instance!r} is not "
                    f"valid UTF-8: {error}",
                ),
                None,
            )

    def _parse(
        self,
        media_type: str,
        decoded: str | None,
    ) -> tuple[ContentMediaTypeError | None, Any]:
        if decoded is None:
            # An earlier (ignored or absent) encoding step produced nothing.
            return None, None
        if media_type != APPLICATION_JSON:
            if self.unsupported_media_type is UnsupportedContentPolicy.IGNORE:
                return None, None
            return (
                ContentMediaTypeError(
                    f"Unsupported contentMediaType {media_type!r}; "
                    f"only {APPLICATION_JSON!r} content can be validated",
                ),
                None,
            )

        try:
            return None, json.loads(decoded)
        except json.JSONDecodeError as error:
            return (
                ContentMediaTypeError(
                    f"{decoded!r} is not valid {media_type} content: "
                    f"{error.msg} (at line {error.lineno}, "
                    f"column {error.colno})",
                ),
                None,
            )

    def _validate_content_schema(
        self,
        validator: Validator,
        content_schema: Any,
        parsed: Any,
    ) -> Iterable[ContentSchemaValidationError]:
        # `descend` runs the full schema against the decoded instance.
        for inner in validator.descend(instance=parsed, schema=content_schema):
            inner_path = list(inner.path)
            yield ContentSchemaValidationError(
                "The decoded content does not validate against "
                f"contentSchema: {inner.message}",
                inner_path=inner_path,
                inner_error=inner,
            )


def _content_errors(
    validator: _ContentCachingValidator,
    schema: dict[str, Any],
    instance: Any,
    error_type: type[ContentValidationError],
) -> Iterable[ContentValidationError]:
    strategy = validator.content_validator
    if strategy is None:
        # Content validation remains strictly opt-in.
        return ()

    cache = validator._content_cache
    cache_key = (id(schema), id(instance))
    if cache_key not in cache:
        cache[cache_key] = list(
            strategy.iter_errors(validator, schema, instance),
        )

    return (
        error for error in cache[cache_key] if isinstance(error, error_type)
    )


def contentEncoding(
    validator: _ContentCachingValidator,
    encoding: str,
    instance: Any,
    schema: dict[str, Any],
) -> Iterable[ContentValidationError]:
    yield from _content_errors(
        validator,
        schema,
        instance,
        ContentEncodingError,
    )


def contentMediaType(
    validator: _ContentCachingValidator,
    media_type: str,
    instance: Any,
    schema: dict[str, Any],
) -> Iterable[ContentValidationError]:
    yield from _content_errors(
        validator,
        schema,
        instance,
        ContentMediaTypeError,
    )


def contentSchema(
    validator: _ContentCachingValidator,
    content_schema: Any,
    instance: Any,
    schema: dict[str, Any],
) -> Iterable[ContentValidationError]:
    yield from _content_errors(
        validator,
        schema,
        instance,
        ContentSchemaValidationError,
    )


def enable_content_validation(
    validator: type[Validator],
    content_validator: ContentValidator | None = None,
) -> type[Validator]:
    """
    Return a new validator class which actively validates content keywords.

    The returned class behaves identically to ``validator`` except that
    :kw:`contentEncoding`, :kw:`contentMediaType` and :kw:`contentSchema`
    are no longer treated as mere annotations.

    Arguments:

        validator:
            an existing validator class (e.g.
            `jsonschema.Draft202012Validator`)

        content_validator:
            the `ContentValidator` strategy used by default for instances
            which do not receive one explicitly. If unprovided, a
            `ContentValidator` with default settings is used. Per-instance
            strategies can still be passed as the ``content_validator``
            argument when constructing the returned class.

    Returns:

        a new validator class, as produced by
        `jsonschema.validators.extend`

    """
    from jsonschema import validators

    strategy = content_validator or ContentValidator()

    extended = validators.extend(
        validator,
        validators={
            "contentEncoding": contentEncoding,
            "contentMediaType": contentMediaType,
            "contentSchema": contentSchema,
        },
    )
    extended.CONTENT_VALIDATOR = strategy  # type: ignore[attr-defined]
    return extended
