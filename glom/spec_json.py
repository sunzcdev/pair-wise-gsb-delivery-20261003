"""A versioned, JSON-serializable subset of glom specs.

Ordinary Python glom specs may contain callables, :class:`~glom.T`
method calls, and other live objects, so they cannot be persisted as
plain JSON. This module defines an explicit *saveable subset* of the
spec language and provides versioned routines to encode those specs
into ordinary JSON data and load them back into glom spec objects.

The supported subset covers:

* **literals** -- :class:`~glom.Val` around JSON scalars and containers
* **paths** -- bare strings and :class:`~glom.Path` (including integer
  vs. string key distinction)
* **object/list projections** -- ``dict`` and ``[subspec]``
* **defaults** -- :class:`~glom.Coalesce` with a static ``default``
* **type checks** -- :class:`~glom.Check` with ``type``,
  ``instance_of``, ``equal_to``, ``one_of`` and ``default``

No callable, method call, or other executable object is part of the
subset. Loading never uses ``eval``, dynamic imports, or ``pickle``;
types referenced by checks are resolved from an explicit registry.

Wire format (current version is :data:`SPEC_JSON_VERSION` = 1)::

    {"glom_spec_version": 1, "spec": <node>}

Each node is an object tagged with an ``"op"``. A string *path* and a
string *literal* are never ambiguous, because paths are always encoded
as an explicit ``{"op": "path", ...}`` node while literals are
``{"op": "val", ...}`` nodes. Likewise, path steps preserve whether a
key was an integer or a string.
"""

import json
import math

from .core import (T, TType, Path, Val, Spec, Coalesce, GlomError,
                   _t_child, _MISSING as _CORE_MISSING)
from .matching import Check, RAISE


SPEC_JSON_VERSION = 1
"""The only wire-format version this module can encode and load."""

_VERSION_KEY = "glom_spec_version"
_PAIRS = "__pairs__"


class SpecJSONError(GlomError, ValueError):
    """Base class for errors raised while encoding or loading spec JSON.

    Every error carries the *path* within the spec (or encoded
    document) where the problem was found, available as
    :attr:`spec_path`. An empty tuple means the error applies to the
    top level.
    """

    def __init__(self, message, path=()):
        self.spec_path = tuple(path)
        rendered = render_spec_path(self.spec_path)
        full_message = ("at spec path %s: %s" % (rendered, message)
                        if self.spec_path else message)
        super(SpecJSONError, self).__init__(full_message)


class SpecEncodeError(SpecJSONError):
    """Raised when a Python spec is not part of the saveable subset."""


class SpecDecodeError(SpecJSONError):
    """Raised when an encoded document cannot be loaded."""


def render_spec_path(path):
    """Render an in-spec *path* (a tuple of segments) as a stable string.

    Identifier-like string segments use dot notation; everything else
    (including integer indexes) uses bracket notation. Encoding and
    loading share this rendering so error locations match.
    """
    parts = []
    for seg in path:
        if type(seg) is str and seg.isidentifier():
            parts.append(seg)
        else:
            parts.append("[%s]" % json.dumps(seg))
    return ".".join(parts) if parts else "<root>"


def _err(exc_type, path, message):
    raise exc_type(message, path)


# ---------------------------------------------------------------------------
# type registry
# ---------------------------------------------------------------------------

def _default_type_registry():
    # Resolved by name only -- no dynamic import ever takes place.
    return {
        "bool": bool,
        "int": int,
        "float": float,
        "str": str,
        "bytes": bytes,
        "list": list,
        "dict": dict,
        "tuple": tuple,
        "set": set,
        "frozenset": frozenset,
        "none": type(None),
    }


TYPE_REGISTRY = _default_type_registry()
"""Mapping of JSON type names to the concrete types usable in checks.

Extend it with :func:`register_spec_type` rather than mutating it
directly. Only registered types can be referenced by encoded checks.
"""


def register_spec_type(name, type_obj):
    """Register *type_obj* under *name* so checks can be round-tripped.

    The same registration must be in place on the side that encodes
    and the side that loads. *name* must be a string and *type_obj* an
    actual type.
    """
    if type(name) is not str or not name:
        raise ValueError("expected non-empty string type name, not: %r" % (name,))
    if not isinstance(type_obj, type):
        raise ValueError("expected a type, not: %r" % (type_obj,))
    if name in TYPE_REGISTRY and TYPE_REGISTRY[name] is not type_obj:
        raise ValueError("type name %r is already registered to %r"
                         % (name, TYPE_REGISTRY[name]))
    TYPE_REGISTRY[name] = type_obj


def _type_name(type_obj, path):
    for name, registered in TYPE_REGISTRY.items():
        if registered is type_obj:
            return name
    _err(SpecEncodeError, path,
         "type %r is not in the spec JSON type registry; register it with "
         "register_spec_type()" % (type_obj,))


# Coalesce uses core's private _MISSING sentinel for unset options.
_COALESCE_MISSING = _CORE_MISSING


# ---------------------------------------------------------------------------
# JSON literal handling
# ---------------------------------------------------------------------------

_LITERAL_TYPES = (str, int, float, bool, type(None), list, dict)


def _ensure_json_literal(value, path):
    """Validate *value* is representable as strict JSON (no NaN/Inf)."""
    if isinstance(value, bool) or value is None or isinstance(value, (str, int)):
        return value
    if isinstance(value, float):
        if math.isnan(value) or math.isinf(value):
            _err(SpecEncodeError, path,
                 "non-finite float is not JSON-serializable: %r" % (value,))
        return value
    if isinstance(value, list):
        for idx, item in enumerate(value):
            _ensure_json_literal(item, path + (idx,))
        return value
    if isinstance(value, dict):
        for key, item in value.items():
            if type(key) is not str:
                _err(SpecEncodeError, path + (key,),
                     "literal objects may only have string keys; use an object "
                     "projection for typed keys")
            _ensure_json_literal(item, path + (key,))
        return value
    _err(SpecEncodeError, path,
         "unsupported literal value (only JSON data is allowed): %r" % (value,))


_PATH_OPS = {".": "attr", "[": "item", "P": "key"}
_PATH_OP_NAMES = {value: key for key, value in _PATH_OPS.items()}


# ---------------------------------------------------------------------------
# encoding: Python spec -> JSON data
# ---------------------------------------------------------------------------

def _encode_path(path_t, path):
    root = path_t.__ops__[0]
    if root is not T:
        _err(SpecEncodeError, path,
             "only target (T-based) paths can be encoded, not scope paths")
    steps = []
    ops = path_t.__ops__
    idx = 1
    while idx < len(ops):
        op, arg = ops[idx], ops[idx + 1]
        idx += 2
        if op not in _PATH_OPS:
            _err(SpecEncodeError, path + (len(steps),),
                 "unsupported path operation %r (method calls and operators "
                 "are not saveable)" % (op,))
        if type(arg) not in (str, int) or isinstance(arg, bool):
            _err(SpecEncodeError, path + (len(steps),),
                 "path keys must be strings or integers, not: %r" % (arg,))
        steps.append({"op": _PATH_OPS[op], "value": arg})
    return {"op": "path", "steps": steps}


def _encode_projection_key(key, path):
    if type(key) not in (str, int) or isinstance(key, bool):
        _err(SpecEncodeError, path,
             "object projection keys must be strings or integers, not: %r"
             % (key,))
    return {"type": type(key).__name__, "value": key}


def _encode(spec, path):
    if type(spec) is Spec:
        if spec.scope:
            _err(SpecEncodeError, path,
                 "Spec objects carrying scope data are not saveable")
        return _encode(spec.spec, path)

    if type(spec) is TType:
        return _encode_path(spec, path)

    if type(spec) is Path:
        return _encode_path(spec.path_t, path)

    if type(spec) is Val:
        _ensure_json_literal(spec.value, path + ("value",))
        return {"op": "val", "value": spec.value}

    if type(spec) is dict:
        fields = []
        for key, subspec in spec.items():
            encoded_key = _encode_projection_key(key, path + (len(fields),))
            fields.append([encoded_key, _encode(subspec, path + (key,))])
        return {"op": "object", "fields": fields}

    if type(spec) is list:
        if len(spec) != 1:
            _err(SpecEncodeError, path,
                 "list projections must contain exactly one subspec")
        return {"op": "list", "spec": _encode(spec[0], path + (0,))}

    if type(spec) is Coalesce:
        if spec.default_factory is not _COALESCE_MISSING:
            _err(SpecEncodeError, path,
                 "Coalesce.default_factory is callable and not saveable")
        if callable(spec.skip) or isinstance(spec.skip, tuple):
            _err(SpecEncodeError, path + ("skip",),
                 "Coalesce.skip callables and tuples are not saveable")
        if type(spec.skip_exc) is not type or not issubclass(spec.skip_exc, GlomError):
            _err(SpecEncodeError, path + ("skip_exc",),
                 "custom Coalesce.skip_exc is not saveable")
        node = {"op": "coalesce",
                "subspecs": [_encode(sub, path + (idx,))
                             for idx, sub in enumerate(spec.subspecs)]}
        if spec.default is not _COALESCE_MISSING:
            _ensure_json_literal(spec.default, path + ("default",))
            node["default"] = spec.default
        if spec.skip is not _COALESCE_MISSING:
            _ensure_json_literal(spec.skip, path + ("skip",))
            node["skip"] = spec.skip
        return node

    if type(spec) is Check:
        if spec.validators:
            _err(SpecEncodeError, path + ("validate",),
                 "Check.validate callables are not saveable")
        node = {"op": "check"}
        if spec.spec is not T:
            node["spec"] = _encode(spec.spec, path + ("spec",))
        node["types"] = [_type_name(t, path + ("type",)) for t in spec.types]
        node["instance_of"] = [_type_name(t, path + ("instance_of",))
                               for t in spec.instance_of]
        node["values"] = list(spec.vals)
        for idx, value in enumerate(node["values"]):
            _ensure_json_literal(value, path + ("one_of", idx))
        if spec.default is not RAISE:
            _ensure_json_literal(spec.default, path + ("default",))
            node["default"] = spec.default
        return node

    if type(spec) is str:
        # A bare string is a path, not a literal: expand exactly like
        # Path.from_text so dotted/star semantics stay faithful.
        return _encode(Path.from_text(spec), path)

    if isinstance(spec, _LITERAL_TYPES) and not callable(spec):
        # A bare constant behaves like Val(constant).
        _ensure_json_literal(spec, path)
        return {"op": "val", "value": spec}

    _err(SpecEncodeError, path,
         "unsupported spec type (callables and other executable objects are "
         "not saveable): %r" % (spec,))


# ---------------------------------------------------------------------------
# public encoding interface
# ---------------------------------------------------------------------------

def encode_spec(spec):
    """Encode a saveable glom *spec* into JSON-compatible data.

    Returns a plain ``dict`` in the versioned envelope, suitable for
    :func:`json.dumps`. Raises :class:`SpecEncodeError` (with an
    in-spec path) if any part of *spec* falls outside the saveable
    subset.
    """
    return {_VERSION_KEY: SPEC_JSON_VERSION, "spec": _encode(spec, ())}


def dumps_spec(spec):
    """Encode *spec* and return a JSON string.

    Equivalent to ``json.dumps(encode_spec(spec))`` using strict JSON
    (no NaN/Infinity) with sorted, stable key order.
    """
    return json.dumps(encode_spec(spec), allow_nan=False, sort_keys=True)


def dump_spec(spec, fp):
    """Encode *spec* and write the JSON string to file-like *fp*."""
    fp.write(dumps_spec(spec))


# ---------------------------------------------------------------------------
# loading: JSON data -> Python spec
# ---------------------------------------------------------------------------

def _pairs_hook(pairs):
    # Preserve every key/value pair so duplicate keys can be rejected
    # with their location, instead of silently taking the last one.
    return {_PAIRS: pairs}


def _duplicate_key(pairs):
    seen = []
    for key, _ in pairs:
        for existing in seen:
            if type(existing) is type(key) and existing == key:
                return key
        seen.append(key)
    return None


def _to_plain(value, path):
    """Convert pair-holder trees into plain lists/dicts and validate
    that every leaf is strict JSON data."""
    if isinstance(value, dict) and _PAIRS in value:
        dup = _duplicate_key(value[_PAIRS])
        if dup is not None:
            _err(SpecDecodeError, path + (dup,),
                 "duplicate field %r" % (dup,))
        result = {}
        for key, item in value[_PAIRS]:
            if type(key) is not str:
                _err(SpecDecodeError, path + (key,),
                     "object keys must be strings in encoded JSON")
            result[key] = _to_plain(item, path + (key,))
        return result
    if isinstance(value, list):
        return [_to_plain(item, path + (idx,)) for idx, item in enumerate(value)]
    if isinstance(value, bool) or value is None or isinstance(value, (str, int, float)):
        if isinstance(value, float) and (math.isnan(value) or math.isinf(value)):
            _err(SpecDecodeError, path,
                 "non-finite float is not valid JSON: %r" % (value,))
        return value
    _err(SpecDecodeError, path, "unsupported JSON value: %r" % (value,))


def _resolve_type(name, path):
    if type(name) is not str:
        _err(SpecDecodeError, path, "type names must be strings, not: %r" % (name,))
    try:
        return TYPE_REGISTRY[name]
    except KeyError:
        _err(SpecDecodeError, path,
             "unknown type name %r; register it with register_spec_type()"
             % (name,))


def _decode_literal(value, path):
    """Validate an embedded literal (Val payload, Coalesce default, ...)."""
    if isinstance(value, bool) or value is None or isinstance(value, (str, int, float)):
        if isinstance(value, float) and (math.isnan(value) or math.isinf(value)):
            _err(SpecDecodeError, path, "non-finite float is not valid JSON")
        return value
    if isinstance(value, list):
        return [_decode_literal(item, path + (idx,))
                for idx, item in enumerate(value)]
    if isinstance(value, dict):
        plain = _to_plain(value, path)
        return plain
    _err(SpecDecodeError, path, "unsupported literal value: %r" % (value,))


def _check_fields(node_pairs, allowed, path):
    """Reject duplicate and unknown fields; return the parsed mapping."""
    dup = _duplicate_key(node_pairs)
    if dup is not None:
        _err(SpecDecodeError, path + (dup,), "duplicate field %r" % (dup,))
    fields = dict(node_pairs)
    unknown = sorted(set(fields) - set(allowed))
    if unknown:
        _err(SpecDecodeError, path + (unknown[0],),
             "unknown field %r" % (unknown[0],))
    return fields


def _decode(node, path):
    if not isinstance(node, dict) or _PAIRS not in node:
        _err(SpecDecodeError, path, "each spec node must be an object with an 'op'")

    pairs = node[_PAIRS]
    op_pairs = [(key, value) for key, value in pairs if key == "op"]
    if not op_pairs:
        _err(SpecDecodeError, path, "missing required field 'op'")
    dup = _duplicate_key(pairs)
    if dup is not None:
        _err(SpecDecodeError, path + (dup,), "duplicate field %r" % (dup,))
    op = op_pairs[0][1]
    if type(op) is not str:
        _err(SpecDecodeError, path + ("op",),
             "'op' must be a string, not: %r" % (op,))

    if op == "val":
        fields = _check_fields(pairs, ("op", "value"), path)
        if "value" not in fields:
            _err(SpecDecodeError, path + ("value",), "missing required field 'value'")
        return Val(_decode_literal(fields["value"], path + ("value",)))

    if op == "path":
        fields = _check_fields(pairs, ("op", "steps"), path)
        steps = fields.get("steps")
        if type(steps) is not list:
            _err(SpecDecodeError, path + ("steps",),
                 "'steps' must be a list")
        path_t = T
        for idx, step in enumerate(steps):
            step_path = path + ("steps", idx)
            if not isinstance(step, dict) or _PAIRS not in step:
                _err(SpecDecodeError, step_path, "each path step must be an object")
            step_fields = _check_fields(step[_PAIRS], ("op", "value"), step_path)
            step_op = step_fields.get("op")
            if step_op not in _PATH_OP_NAMES:
                _err(SpecDecodeError, step_path + ("op",),
                     "unknown path operation %r" % (step_op,))
            value = step_fields.get("value")
            if type(value) not in (str, int) or isinstance(value, bool):
                _err(SpecDecodeError, step_path + ("value",),
                     "path keys must be strings or integers, not: %r" % (value,))
            path_t = _t_child(path_t, _PATH_OP_NAMES[step_op], value)
        return Path(path_t)

    if op == "object":
        fields = _check_fields(pairs, ("op", "fields"), path)
        raw_fields = fields.get("fields")
        if type(raw_fields) is not list:
            _err(SpecDecodeError, path + ("fields",), "'fields' must be a list")
        result = {}
        seen = []
        for idx, entry in enumerate(raw_fields):
            entry_path = path + ("fields", idx)
            if type(entry) is not list or len(entry) != 2:
                _err(SpecDecodeError, entry_path,
                     "each field must be a [key, spec] pair")
            key_node, sub_node = entry
            if not isinstance(key_node, dict) or _PAIRS not in key_node:
                _err(SpecDecodeError, entry_path + (0,), "field key must be an object")
            key_fields = _check_fields(key_node[_PAIRS], ("type", "value"),
                                       entry_path + (0,))
            key_type = key_fields.get("type")
            if key_type not in ("str", "int"):
                _err(SpecDecodeError, entry_path + (0, "type"),
                     "field key type must be 'str' or 'int', not: %r" % (key_type,))
            key_value = key_fields.get("value")
            if type(key_value).__name__ != key_type or isinstance(key_value, bool):
                _err(SpecDecodeError, entry_path + (0, "value"),
                     "field key value does not match declared type %r" % (key_type,))
            for existing in seen:
                if type(existing) is type(key_value) and existing == key_value:
                    _err(SpecDecodeError, entry_path + (0,),
                         "duplicate field %r" % (key_value,))
            seen.append(key_value)
            result[key_value] = _decode(sub_node, entry_path + (1,))
        return result

    if op == "list":
        fields = _check_fields(pairs, ("op", "spec"), path)
        if "spec" not in fields:
            _err(SpecDecodeError, path + ("spec",), "missing required field 'spec'")
        return [_decode(fields["spec"], path + ("spec",))]

    if op == "coalesce":
        fields = _check_fields(pairs, ("op", "subspecs", "default", "skip"), path)
        subspecs = fields.get("subspecs")
        if type(subspecs) is not list:
            _err(SpecDecodeError, path + ("subspecs",),
                 "'subspecs' must be a list")
        kwargs = {}
        if "default" in fields:
            kwargs["default"] = _decode_literal(fields["default"],
                                                path + ("default",))
        if "skip" in fields:
            kwargs["skip"] = _decode_literal(fields["skip"], path + ("skip",))
        decoded = [_decode(sub, path + ("subspecs", idx))
                   for idx, sub in enumerate(subspecs)]
        return Coalesce(*decoded, **kwargs)

    if op == "check":
        fields = _check_fields(
            pairs,
            ("op", "spec", "types", "instance_of", "values", "default"),
            path)
        kwargs = {}
        if "spec" in fields:
            kwargs["spec"] = _decode(fields["spec"], path + ("spec",))

        types = fields.get("types", [])
        if type(types) is not list:
            _err(SpecDecodeError, path + ("types",), "'types' must be a list")
        if types:
            kwargs["type"] = tuple(_resolve_type(name, path + ("types", idx))
                                   for idx, name in enumerate(types))

        instance_of = fields.get("instance_of", [])
        if type(instance_of) is not list:
            _err(SpecDecodeError, path + ("instance_of",),
                 "'instance_of' must be a list")
        if instance_of:
            kwargs["instance_of"] = tuple(
                _resolve_type(name, path + ("instance_of", idx))
                for idx, name in enumerate(instance_of))

        values = fields.get("values", [])
        if type(values) is not list:
            _err(SpecDecodeError, path + ("values",), "'values' must be a list")
        decoded_values = [_decode_literal(value, path + ("values", idx))
                          for idx, value in enumerate(values)]
        if len(decoded_values) == 1:
            kwargs["equal_to"] = decoded_values[0]
        elif len(decoded_values) > 1:
            kwargs["one_of"] = tuple(decoded_values)

        if "default" in fields:
            kwargs["default"] = _decode_literal(fields["default"],
                                                path + ("default",))
        return Check(**kwargs)

    _err(SpecDecodeError, path + ("op",), "unknown operation %r" % (op,))


def _decode_document(document):
    if not isinstance(document, dict) or _PAIRS not in document:
        _err(SpecDecodeError, (),
             "top-level document must be an object with a version field")
    pairs = document[_PAIRS]
    dup = _duplicate_key(pairs)
    if dup is not None:
        _err(SpecDecodeError, (dup,), "duplicate field %r" % (dup,))
    top = dict(pairs)

    if _VERSION_KEY not in top:
        _err(SpecDecodeError, (_VERSION_KEY,),
             "missing required field %r" % _VERSION_KEY)
    version = top[_VERSION_KEY]
    if version != SPEC_JSON_VERSION:
        _err(SpecDecodeError, (_VERSION_KEY,),
             "unknown spec JSON version %r (supported version: %d)"
             % (version, SPEC_JSON_VERSION))
    if "spec" not in top:
        _err(SpecDecodeError, ("spec",), "missing required field 'spec'")
    unknown = sorted(set(top) - {_VERSION_KEY, "spec"})
    if unknown:
        _err(SpecDecodeError, (unknown[0],), "unknown field %r" % (unknown[0],))
    return _decode(top["spec"], ("spec",))


def load_spec(document):
    """Load a spec from already-parsed JSON data *document*.

    *document* may be the envelope ``dict`` returned by
    :func:`encode_spec` or a JSON string. Raises
    :class:`SpecDecodeError` (with an in-spec path) on unknown
    versions, unknown operations, or duplicate fields.
    """
    if isinstance(document, str):
        return loads_spec(document)
    # Re-serialize so duplicate-key detection and strict parsing apply
    # uniformly even when callers hand in a plain Python dict.
    try:
        text = json.dumps(document, allow_nan=False)
    except (TypeError, ValueError) as exc:
        _err(SpecDecodeError, (), "document is not strict JSON data: %s" % exc)
    return loads_spec(text)


def loads_spec(text):
    """Load a spec from a JSON string produced by :func:`dumps_spec`."""
    if type(text) is not str:
        _err(SpecDecodeError, (), "expected a JSON string, not: %r" % (text,))
    try:
        document = json.loads(text, object_pairs_hook=_pairs_hook)
    except ValueError as exc:
        _err(SpecDecodeError, (), "invalid JSON: %s" % exc)
    return _decode_document(document)


def load_spec_file(fp):
    """Read JSON from file-like *fp* and load it into a glom spec."""
    return loads_spec(fp.read())
