"""
Opt-in validation of the content-related keywords.

The keywords are :kw:`contentEncoding`, :kw:`contentMediaType` and
:kw:`contentSchema`. By default, these three keywords are only annotations:
normal JSON Schema validation never decodes string fields on their behalf.
The entry point here explicitly turns that validation on.

Example:

    >>> import base64
    >>> import jsonschema
    >>> from jsonschema.content import enable_content_validation
    >>>
    >>> Validator = enable_content_validation(jsonschema.Draft202012Validator)
    >>> schema = {
    ...     "type": "string",
    ...     "contentEncoding": "base64",
    ...     "contentMediaType": "application/json",
    ...     "contentSchema": {
    ...         "type": "object",
    ...         "required": ["foo"],
    ...     },
    ... }
    >>> payload = base64.b64encode(b'{"foo": "bar"}').decode("ascii")
    >>> Validator(schema).validate(payload)

The first supported combination is base64-encoded JSON, optionally checked
against a :kw:`contentSchema`. Validation runs from the outside in: the outer
type and the encoding are verified before the decoded content is parsed or
schema-checked.

"""

from __future__ import annotations

from jsonschema._content import (
    APPLICATION_JSON,
    BASE64,
    DEFAULT_BYTE_BUDGET,
    ContentValidator,
    UnsupportedContentPolicy,
    enable_content_validation,
)
from jsonschema.exceptions import (
    ContentEncodingError,
    ContentMediaTypeError,
    ContentSchemaValidationError,
    ContentValidationError,
)

__all__ = [
    "APPLICATION_JSON",
    "BASE64",
    "DEFAULT_BYTE_BUDGET",
    "ContentEncodingError",
    "ContentMediaTypeError",
    "ContentSchemaValidationError",
    "ContentValidationError",
    "ContentValidator",
    "UnsupportedContentPolicy",
    "enable_content_validation",
]
