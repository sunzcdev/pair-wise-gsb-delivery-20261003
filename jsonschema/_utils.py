from collections.abc import Mapping, MutableMapping, Sequence
from urllib.parse import urlsplit
import re

# Module-level sentinels so the recursive fingerprint helpers produce
# comparable tags for nested True/False (function-default sentinels would
# also work, but this makes the intent explicit), plus markers for elements
# that cannot be fully or partially hashed.
_TRUE = object()
_FALSE = object()
_UNSUPPORTED = object()
_UNHASHABLE = object()


class URIDict(MutableMapping):
    """
    Dictionary which uses normalized URIs as keys.
    """

    def normalize(self, uri):
        return urlsplit(uri).geturl()

    def __init__(self, *args, **kwargs):
        self.store = dict()
        self.store.update(*args, **kwargs)

    def __getitem__(self, uri):
        return self.store[self.normalize(uri)]

    def __setitem__(self, uri, value):
        self.store[self.normalize(uri)] = value

    def __delitem__(self, uri):
        del self.store[self.normalize(uri)]

    def __iter__(self):
        return iter(self.store)

    def __len__(self):  # pragma: no cover -- untested, but to be removed
        return len(self.store)

    def __repr__(self):  # pragma: no cover -- untested, but to be removed
        return repr(self.store)


class Unset:
    """
    An as-of-yet unset attribute or unprovided default parameter.
    """

    def __repr__(self):  # pragma: no cover
        return "<unset>"


def format_as_index(container, indices):
    """
    Construct a single string containing indexing operations for the indices.

    For example for a container ``bar``, [1, 2, "foo"] -> bar[1][2]["foo"]

    Arguments:

        container (str):

            A word to use for the thing being indexed

        indices (sequence):

            The indices to format.

    """
    if not indices:
        return container
    return f"{container}[{']['.join(repr(index) for index in indices)}]"


def find_additional_properties(instance, schema):
    """
    Return the set of additional properties for the given ``instance``.

    Weeds out properties that should have been validated by ``properties`` and
    / or ``patternProperties``.

    Assumes ``instance`` is dict-like already.
    """
    properties = schema.get("properties", {})
    patterns = "|".join(schema.get("patternProperties", {}))
    for property in instance:
        if property not in properties:
            if patterns and re.search(patterns, property):
                continue
            yield property


def extras_msg(extras):
    """
    Create an error message for extra items or properties.
    """
    verb = "was" if len(extras) == 1 else "were"
    return ", ".join(repr(extra) for extra in extras), verb


def ensure_list(thing):
    """
    Wrap ``thing`` in a list if it's a single str.

    Otherwise, return it unchanged.
    """
    if isinstance(thing, str):
        return [thing]
    return thing


def _mapping_equal(one, two):
    """
    Check if two mappings are equal using the semantics of `equal`.
    """
    if len(one) != len(two):
        return False
    return all(
        key in two and equal(value, two[key]) for key, value in one.items()
    )


def _sequence_equal(one, two):
    """
    Check if two sequences are equal using the semantics of `equal`.
    """
    if len(one) != len(two):
        return False
    return all(equal(i, j) for i, j in zip(one, two))


def equal(one, two):
    """
    Check if two things are equal evading some Python type hierarchy semantics.

    Specifically in JSON Schema, evade `bool` inheriting from `int`,
    recursing into sequences to do the same.
    """
    if one is two:
        return True
    if isinstance(one, str) or isinstance(two, str):
        return one == two
    if isinstance(one, Sequence) and isinstance(two, Sequence):
        return _sequence_equal(one, two)
    if isinstance(one, Mapping) and isinstance(two, Mapping):
        return _mapping_equal(one, two)
    return unbool(one) == unbool(two)


def unbool(element, true=object(), false=object()):
    """
    A hack to make True and 1 and False and 0 unique for ``uniq``.
    """
    if element is True:
        return true
    elif element is False:
        return false
    return element


def _structural_fingerprint(element):
    """
    Build a hashable *fingerprint* for an element.

    The fingerprint is only a candidate-grouping hint: two elements with the
    same fingerprint are not assumed equal, and two elements with different
    fingerprints can never be `equal`. Every fingerprint collision is still
    adjudicated with `equal`, so a hash collision never produces a wrong
    result.

    JSON equivalence is preserved while building it:

    * mapping key order is ignored (items are collected into a frozenset),
    * sequence order is significant (items are kept in order),
    * booleans are tagged separately from numbers via `unbool`,

    """
    # NaN never equals itself, so it can't be deduplicated via hashing.
    # Custom objects may raise arbitrary errors from their comparisons, in
    # which case we give up on fingerprinting and fall back to brute force.
    try:
        if element != element:  # noqa: PLR0124 -- NaN detection
            return _UNSUPPORTED
    except Exception:  # noqa: BLE001 -- any failure means: use the fallback
        return _UNSUPPORTED

    element = unbool(element, true=_TRUE, false=_FALSE)

    # Tagged tuples keep values of different shapes in separate buckets.
    if isinstance(element, Sequence) and not isinstance(element, str):
        values = []
        for each in element:
            fingerprint = _structural_fingerprint(each)
            if fingerprint is _UNSUPPORTED:
                return _UNSUPPORTED
            values.append(fingerprint)
        return "sequence", tuple(values)

    if isinstance(element, Mapping):
        items = []
        for key, value in element.items():
            value_fingerprint = _structural_fingerprint(value)
            if value_fingerprint is _UNSUPPORTED:
                return _UNSUPPORTED
            items.append((key, value_fingerprint))
        try:
            return "mapping", frozenset(items)
        except Exception:  # noqa: BLE001 -- e.g. an unhashable mapping key
            return _UNSUPPORTED

    try:
        hash(element)
    except Exception:  # noqa: BLE001 -- custom __hash__ may raise anything
        return _UNSUPPORTED

    return "scalar", element


def _fallback_signature(element):
    """
    Cheap structural grouping hint for elements without a full fingerprint.

    Unlike `_structural_fingerprint`, an unhashable or NaN leaf does not
    invalidate the whole element: it is replaced by a shared sentinel so the
    surrounding structure still contributes to grouping. This keeps elements
    with different shapes in separate buckets instead of lumping every
    fallback element together. The signature is only a hint -- equality is
    always decided by `equal`.
    """
    return type(element), _partial_fingerprint(element)


def _partial_fingerprint(element):
    try:
        if element != element:  # noqa: PLR0124 -- NaN detection
            return _UNHASHABLE
    except Exception:  # noqa: BLE001 -- uncooperative custom comparison
        return _UNHASHABLE

    element = unbool(element, true=_TRUE, false=_FALSE)

    if isinstance(element, Sequence) and not isinstance(element, str):
        return "sequence", tuple(
            _partial_fingerprint(each) for each in element
        )

    if isinstance(element, Mapping):
        try:
            return "mapping", frozenset(
                (key, _partial_fingerprint(value))
                for key, value in element.items()
            )
        except Exception:  # noqa: BLE001 -- e.g. an unhashable mapping key
            return _UNHASHABLE

    return "scalar", _safe_hash(element)


def _safe_hash(element):
    try:
        return hash(element)
    except Exception:  # noqa: BLE001 -- unhashable or a failing __hash__
        return _UNHASHABLE


def uniq(container):
    """
    Check if all of a container's elements are unique.

    Elements are first split into candidate groups by a structural
    fingerprint. Deep comparisons via `equal` only happen *within* a group,
    so the common case stays linear while hash collisions are still
    adjudicated correctly. Elements without a usable fingerprint (NaN,
    unhashable values, or custom objects whose comparisons fail) are grouped
    separately and checked with the same equality semantics as a fallback.
    """
    fingerprinted = {}
    fallback = {}

    for element in container:
        fingerprint = _structural_fingerprint(element)

        if fingerprint is _UNSUPPORTED:
            bucket = fallback.setdefault(_fallback_signature(element), [])
        else:
            bucket = fingerprinted.setdefault(fingerprint, [])

        for candidate in bucket:
            if equal(candidate, element):
                return False
        bucket.append(element)

    return True


def find_evaluated_item_indexes_by_schema(validator, instance, schema):
    """
    Get all indexes of items that get evaluated under the current schema.

    Covers all keywords related to unevaluatedItems: items, prefixItems, if,
    then, else, contains, unevaluatedItems, allOf, oneOf, anyOf
    """
    if validator.is_type(schema, "boolean"):
        return []
    evaluated_indexes = []

    if "items" in schema:
        return list(range(len(instance)))

    ref = schema.get("$ref")
    if ref is not None:
        resolved = validator._resolver.lookup(ref)
        evaluated_indexes.extend(
            find_evaluated_item_indexes_by_schema(
                validator.evolve(
                    schema=resolved.contents,
                    _resolver=resolved.resolver,
                ),
                instance,
                resolved.contents,
            ),
        )

    dynamicRef = schema.get("$dynamicRef")
    if dynamicRef is not None:
        resolved = validator._resolver.lookup(dynamicRef)
        evaluated_indexes.extend(
            find_evaluated_item_indexes_by_schema(
                validator.evolve(
                    schema=resolved.contents,
                    _resolver=resolved.resolver,
                ),
                instance,
                resolved.contents,
            ),
        )

    if "prefixItems" in schema:
        evaluated_indexes += list(range(len(schema["prefixItems"])))

    if "if" in schema:
        if validator.evolve(schema=schema["if"]).is_valid(instance):
            evaluated_indexes += find_evaluated_item_indexes_by_schema(
                validator,
                instance,
                schema["if"],
            )
            if "then" in schema:
                evaluated_indexes += find_evaluated_item_indexes_by_schema(
                    validator,
                    instance,
                    schema["then"],
                )
        elif "else" in schema:
            evaluated_indexes += find_evaluated_item_indexes_by_schema(
                validator,
                instance,
                schema["else"],
            )

    for keyword in ["contains", "unevaluatedItems"]:
        if keyword in schema:
            for k, v in enumerate(instance):
                if validator.evolve(schema=schema[keyword]).is_valid(v):
                    evaluated_indexes.append(k)

    for keyword in ["allOf", "oneOf", "anyOf"]:
        if keyword in schema:
            for subschema in schema[keyword]:
                errs = next(validator.descend(instance, subschema), None)
                if errs is None:
                    evaluated_indexes += find_evaluated_item_indexes_by_schema(
                        validator,
                        instance,
                        subschema,
                    )

    return evaluated_indexes


def find_evaluated_property_keys_by_schema(validator, instance, schema):
    """
    Get all keys of items that get evaluated under the current schema.

    Covers all keywords related to unevaluatedProperties: properties,
    additionalProperties, unevaluatedProperties, patternProperties,
    dependentSchemas, allOf, oneOf, anyOf, if, then, else
    """
    if validator.is_type(schema, "boolean"):
        return []
    evaluated_keys = []

    ref = schema.get("$ref")
    if ref is not None:
        resolved = validator._resolver.lookup(ref)
        evaluated_keys.extend(
            find_evaluated_property_keys_by_schema(
                validator.evolve(
                    schema=resolved.contents,
                    _resolver=resolved.resolver,
                ),
                instance,
                resolved.contents,
            ),
        )

    dynamicRef = schema.get("$dynamicRef")
    if dynamicRef is not None:
        resolved = validator._resolver.lookup(dynamicRef)
        evaluated_keys.extend(
            find_evaluated_property_keys_by_schema(
                validator.evolve(
                    schema=resolved.contents,
                    _resolver=resolved.resolver,
                ),
                instance,
                resolved.contents,
            ),
        )

    properties = schema.get("properties")
    if validator.is_type(properties, "object"):
        evaluated_keys += properties.keys() & instance.keys()

    for keyword in ["additionalProperties", "unevaluatedProperties"]:
        if (subschema := schema.get(keyword)) is None:
            continue
        evaluated_keys += (
            key
            for key, value in instance.items()
            if is_valid(validator.descend(value, subschema))
        )

    if "patternProperties" in schema:
        for property in instance:
            for pattern in schema["patternProperties"]:
                if re.search(pattern, property):
                    evaluated_keys.append(property)

    if "dependentSchemas" in schema:
        for property, subschema in schema["dependentSchemas"].items():
            if property not in instance:
                continue
            evaluated_keys += find_evaluated_property_keys_by_schema(
                validator,
                instance,
                subschema,
            )

    for keyword in ["allOf", "oneOf", "anyOf"]:
        for subschema in schema.get(keyword, []):
            if not is_valid(validator.descend(instance, subschema)):
                continue
            evaluated_keys += find_evaluated_property_keys_by_schema(
                validator,
                instance,
                subschema,
            )

    if "if" in schema:
        if validator.evolve(schema=schema["if"]).is_valid(instance):
            evaluated_keys += find_evaluated_property_keys_by_schema(
                validator,
                instance,
                schema["if"],
            )
            if "then" in schema:
                evaluated_keys += find_evaluated_property_keys_by_schema(
                    validator,
                    instance,
                    schema["then"],
                )
        elif "else" in schema:
            evaluated_keys += find_evaluated_property_keys_by_schema(
                validator,
                instance,
                schema["else"],
            )

    return evaluated_keys


def is_valid(errs_it):
    """Whether there are no errors in the given iterator."""
    return next(errs_it, None) is None
