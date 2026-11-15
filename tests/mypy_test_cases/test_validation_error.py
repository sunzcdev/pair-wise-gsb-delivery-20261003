from __future__ import annotations

import marshmallow as ma

# OK types for 'message'
ma.ValidationError("foo")
ma.ValidationError(["foo"])
ma.ValidationError({"foo": "bar"})

# non-OK types for 'message'
ma.ValidationError(0)  # type: ignore[arg-type]

# 'messages' is a dict|list
err = ma.ValidationError("foo")
a: dict | list = err.messages
# union type can't assign to non-union type
b: str = err.messages  # type: ignore[assignment]
c: dict = err.messages  # type: ignore[assignment]
# 'messages_dict' is a dict, so that it can assign to a dict
d: dict = err.messages_dict

# 'structured' is a list of StructuredError
from marshmallow import structured

structured_errors: list[structured.StructuredError] = err.structured
first: structured.StructuredError = structured_errors[0]
path: tuple[structured.PathSegment, ...] = first.path
segment: structured.PathSegment = path[0]
kind: str = segment.kind
jsonable: list[dict] = err.structured_jsonable()

# PathSegment constructors return PathSegment
index_segment: structured.PathSegment = structured.PathSegment.index(0)
field_segment: structured.PathSegment = structured.PathSegment.field("name")

# encode_key / to_jsonable return arbitrary JSON-compatible values
encoded_key: object = structured.encode_key((1, 2))
encoded_value: object = structured.to_jsonable({(1, 2): ["bad"]})
