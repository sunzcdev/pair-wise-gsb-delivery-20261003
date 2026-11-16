"""
Opt-in validation of the ``content*`` keywords.

By default, :kw:`contentEncoding`, :kw:`contentMediaType` and
:kw:`contentSchema` are purely annotations: they are collected from
schemas but never cause validation to decode or otherwise inspect the
strings they describe.

Passing a `ContentValidator` to a validator class explicitly enables
validating them::

    >>> from jsonschema import ContentValidator, Draft202012Validator
    >>> schema = {
    ...     "type": "string",
    ...     "contentEncoding": "base64",
    ...     "contentMediaType": "application/json",
    ...     "contentSchema": {"type": "object"},
    ... }
    >>> validator = Draft202012Validator(
    ...     schema, content_validator=ContentValidator(),
    ... )

The outer string is first checked to really be a string and to decode
under its declared :kw:`contentEncoding`, and only then is the decoded
content parsed (per :kw:`contentMediaType`) and validated against
:kw:`contentSchema`.

Errors produced this way refer to the *outer* string: their
`ValidationError.path` (and `json_path`) point at the field in the
original document, exactly as if no decoding had happened. Whenever a
:kw:`contentSchema` fails, the path *within the decoded content* is
available separately as ``error.content_path`` (alongside the full
inner error as ``error.content_error``), so inner instance paths are
never confused with paths into the original document.
"""

from __future__ import annotations

from collections import deque
from collections.abc import Callable, Mapping
from typing import TYPE_CHECKING, Any
import base64
import binascii
import json

from attrs import define, field
from attrs.validators import ge, in_, instance_of

from jsonschema.exceptions import ValidationError

if TYPE_CHECKING:
    from jsonschema.protocols import Validator

#: The default maximum number of bytes decoding may produce.
DEFAULT_MAX_DECODED_BYTES = 2**20  # 1 MiB

#: The policies for handling unsupported encodings or media types.
_UNSUPPORTED_POLICIES = ["error", "ignore"]


class ContentDecodingError(Exception):
    """
    Decoding or interpreting a string's declared content failed.
    """


def _decode_base64(instance: str, max_decoded_bytes: int) -> bytes:
    # 4 base64 characters decode to at most 3 bytes (and a trailing
    # partial quantum to fewer), so the budget can be enforced before
    # any decoding work happens.
    quads, remainder = divmod(len(instance.rstrip("=")), 4)
    if 3 * quads + max(remainder - 1, 0) > max_decoded_bytes:
        raise ContentDecodingError(
            f"decoding {len(instance)} base64 characters would exceed "
            f"the budget of {max_decoded_bytes} bytes",
        )
    try:
        return base64.b64decode(instance, validate=True)
    except binascii.Error as error:
        raise ContentDecodingError(
            f"{instance!r} is not valid base64",
        ) from error


_DEFAULT_DECODERS: dict[str, Callable[[str, int], bytes]] = {
    "base64": _decode_base64,
}

_DEFAULT_PARSERS: dict[str, Callable[[bytes], Any]] = {
    "application/json": json.loads,
}


@define
class ContentValidator:
    """
    A policy describing how to validate the ``content*`` keywords.

    Content validation is opt-in: validator classes only decode and
    validate string contents when constructed with one of these (via
    their ``content_validator`` argument). Without one, the keywords
    remain annotations.

    Arguments:

        max_decoded_bytes (int):

            an independent budget, in bytes, for how large decoded
            content may be. Decoding which would produce more than
            this many bytes fails validation before the content is
            interpreted further.

        on_unsupported (str):

            what to do when a schema declares a :kw:`contentEncoding`
            (or a :kw:`contentMediaType` needed to apply
            :kw:`contentSchema`) which this policy does not support:
            ``"error"`` (the default) reports the declaration as
            unsupported via a validation error, while ``"ignore"``
            treats it as a mere annotation.

        decoders (collections.abc.Mapping):

            a mapping from :kw:`contentEncoding` values to callables
            taking the instance and ``max_decoded_bytes`` and
            returning the decoded ``bytes``, raising
            `ContentDecodingError` if the content is malformed.
            Defaults to supporting ``base64``.

        parsers (collections.abc.Mapping):

            a mapping from :kw:`contentMediaType` values to callables
            taking the decoded ``bytes`` and returning the Python
            object they represent. Defaults to supporting
            ``application/json``.

    """

    max_decoded_bytes: int = field(
        default=DEFAULT_MAX_DECODED_BYTES,
        validator=[instance_of(int), ge(0)],
    )
    on_unsupported: str = field(
        default="error",
        validator=in_(_UNSUPPORTED_POLICIES),
    )
    decoders: Mapping[str, Callable[[str, int], bytes]] = field(
        factory=lambda: dict(_DEFAULT_DECODERS),
    )
    parsers: Mapping[str, Callable[[bytes], Any]] = field(
        factory=lambda: dict(_DEFAULT_PARSERS),
    )

    def _validate(
        self,
        validator: Validator,
        instance: Any,
        schema: Mapping[str, Any],
    ):
        # The content keywords only apply to strings; anything else is
        # none of their business.
        if not validator.is_type(instance, "string"):
            return

        encoding = schema.get("contentEncoding")
        if encoding is None:
            decoded = instance.encode("utf-8")
        else:
            decoder = self.decoders.get(encoding)
            if decoder is None:
                yield from self._unsupported(
                    "contentEncoding",
                    encoding,
                    f"contentEncoding {encoding!r} is not supported",
                )
                return
            try:
                decoded = decoder(instance, self.max_decoded_bytes)
            except ContentDecodingError as error:
                yield ValidationError(
                    str(error),
                    validator="contentEncoding",
                    validator_value=encoding,
                    cause=error,
                )
                return
            # The budget is enforced independently of the decoder, so
            # that even custom decoders cannot exceed it.
            if len(decoded) > self.max_decoded_bytes:
                yield ValidationError(
                    f"decoded content is {len(decoded)} bytes, exceeding "
                    f"the budget of {self.max_decoded_bytes} bytes",
                    validator="contentEncoding",
                    validator_value=encoding,
                )
                return

        media_type = schema.get("contentMediaType")
        if media_type is None:
            # Without a media type, contentSchema is defined to be
            # ignored, and there is nothing further to check.
            return

        content_schema = schema.get("contentSchema")
        parser = self.parsers.get(media_type)
        if parser is None:
            if content_schema is not None:
                yield from self._unsupported(
                    "contentMediaType",
                    media_type,
                    f"contentMediaType {media_type!r} is not supported, "
                    "so contentSchema cannot be applied",
                )
            return

        try:
            content = parser(decoded)
        except Exception as error:  # noqa: BLE001
            yield ValidationError(
                f"decoded content is not valid {media_type}: {error}",
                validator="contentMediaType",
                validator_value=media_type,
                cause=error,
            )
            return

        if content_schema is None:
            return

        evolved = validator.evolve(schema=content_schema)
        for error in evolved.iter_errors(content):
            outer = ValidationError(
                f"decoded content does not conform to contentSchema "
                f"(at {error.json_path} within the decoded content): "
                f"{error.message}",
                validator="contentSchema",
                validator_value=content_schema,
            )
            # The inner path describes the *decoded* document, so it
            # is attached separately rather than merged into `path`,
            # which continues to locate the outer string.
            outer.content_path = deque(error.relative_path)
            outer.content_error = error
            yield outer

    def _unsupported(self, keyword: str, value: Any, message: str):
        if self.on_unsupported == "error":
            yield ValidationError(
                message,
                validator=keyword,
                validator_value=value,
            )
        # Otherwise the declaration is treated as a pure annotation.


def contentEncoding(validator, encoding, instance, schema):
    if validator.content_validator is not None:
        yield from validator.content_validator._validate(
            validator,
            instance,
            schema,
        )


def contentMediaType(validator, media_type, instance, schema):
    if (
        validator.content_validator is not None
        and "contentEncoding" not in schema
    ):
        yield from validator.content_validator._validate(
            validator,
            instance,
            schema,
        )


def contentSchema(validator, content_schema, instance, schema):
    if (
        validator.content_validator is not None
        and "contentEncoding" not in schema
        and "contentMediaType" not in schema
    ):
        yield from validator.content_validator._validate(
            validator,
            instance,
            schema,
        )
