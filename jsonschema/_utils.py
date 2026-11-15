from collections.abc import Mapping, MutableMapping, Sequence
from urllib.parse import urlsplit
import re

from referencing.jsonschema import lookup_recursive_ref

# Module-level sentinels so recursive `_uniq_key` calls produce comparable
# keys for nested True/False (function-default sentinels would also work, but
# this makes the intent explicit).
_TRUE = object()
_FALSE = object()
_UNSUPPORTED = object()


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


def _uniq_key(element):
    """
    Convert an element into a hashable key compatible with `equal`.

    Returns `_UNSUPPORTED` when an element cannot be canonically hashed.
    """
    # NaN never equals itself, so it can't be deduplicated via hashing.
    # Some custom container equality implementations can also raise here
    # (e.g. mappings containing unhashable keys), in which case we fall back
    # to brute force as well.
    try:
        if element != element:  # noqa: PLR0124 -- NaN detection
            return _UNSUPPORTED
    except TypeError:
        return _UNSUPPORTED

    element = unbool(element, true=_TRUE, false=_FALSE)

    # Tagged tuples ("scalar"/"sequence"/"mapping", ...) prevent hash
    # collisions between values of different shapes.
    if isinstance(element, Sequence) and not isinstance(element, str):
        values = []
        for each in element:
            key = _uniq_key(each)
            if key is _UNSUPPORTED:
                return _UNSUPPORTED
            values.append(key)
        return "sequence", tuple(values)

    if isinstance(element, Mapping):
        items = []
        for key, value in element.items():
            value_key = _uniq_key(value)
            if value_key is _UNSUPPORTED:
                return _UNSUPPORTED
            items.append((key, value_key))
        try:
            return "mapping", frozenset(items)
        except TypeError:
            # Unhashable mapping key — fall back to brute force.
            return _UNSUPPORTED

    try:
        hash(element)
    except TypeError:
        return _UNSUPPORTED

    return "scalar", element


def uniq(container):
    """
    Check if all of a container's elements are unique.

    Tries to use a structural hash compatible with `equal`, falling back to
    brute force when necessary.
    """
    seen_keys = set()
    unsupported = []

    for element in container:
        key = _uniq_key(element)

        if key is _UNSUPPORTED:
            for previous in unsupported:
                if equal(previous, element):
                    return False
            unsupported.append(element)
            continue

        if key in seen_keys:
            return False

        seen_keys.add(key)

    return True


def _uses_tuple_items(validator):
    """
    Whether the given validator uses pre-2020-12 tuple ``items`` semantics.

    2020-12 moved tuples to ``prefixItems`` and made ``items`` apply to
    every remaining item.  Older dialects (and extensions of them) still
    treat an array-valued ``items`` as a tuple of subschemas.
    """
    return "prefixItems" not in validator.VALIDATORS


def find_contains_item_indexes(validator, instance, contains_schema):
    """
    Return the indexes of items which match a ``contains`` subschema.

    This is the annotation value of ``contains``, before its count
    constraints are applied.
    """
    contains_validator = validator.evolve(schema=contains_schema)
    return [
        index
        for index, item in enumerate(instance)
        if contains_validator.is_valid(item)
    ]


def contains_count_satisfied(schema, instance, matches):
    """
    Whether the number of ``contains`` matches satisfies its constraints.
    """
    min_contains = schema.get("minContains", 1)
    max_contains = schema.get("maxContains", len(instance))
    return min_contains <= matches <= max_contains


def _is_valid_subschema(validator, instance, schema):
    return next(validator.descend(instance, schema), None) is None


def _find_evaluated_item_indexes_by_schema(
    validator,
    instance,
    schema,
    *,
    include_unevaluated_items=True,
):
    if validator.is_type(schema, "boolean"):
        return set()

    evaluated_indexes = set()
    tuple_items = _uses_tuple_items(validator)

    ref = schema.get("$ref")
    if ref is not None:
        resolved = validator._resolver.lookup(ref)
        evaluated_indexes.update(
            _find_evaluated_item_indexes_by_schema(
                validator.evolve(
                    schema=resolved.contents,
                    _resolver=resolved.resolver,
                ),
                instance,
                resolved.contents,
            ),
        )

    if tuple_items:
        if "$recursiveRef" in schema:
            resolved = lookup_recursive_ref(validator._resolver)
            evaluated_indexes.update(
                _find_evaluated_item_indexes_by_schema(
                    validator.evolve(
                        schema=resolved.contents,
                        _resolver=resolved.resolver,
                    ),
                    instance,
                    resolved.contents,
                ),
            )
    elif "$dynamicRef" in schema:
        resolved = validator._resolver.lookup(schema["$dynamicRef"])
        evaluated_indexes.update(
            _find_evaluated_item_indexes_by_schema(
                validator.evolve(
                    schema=resolved.contents,
                    _resolver=resolved.resolver,
                ),
                instance,
                resolved.contents,
            ),
        )

    if "items" in schema:
        if not tuple_items:
            # ``items`` applies to every item after ``prefixItems``,
            # so together they cover the entire instance.
            evaluated_indexes.update(range(len(instance)))
        elif "additionalItems" in schema:
            evaluated_indexes.update(range(len(instance)))
        elif validator.is_type(schema["items"], "array"):
            evaluated_indexes.update(
                range(min(len(schema["items"]), len(instance))),
            )
        else:
            evaluated_indexes.update(range(len(instance)))

    if not tuple_items and "prefixItems" in schema:
        evaluated_indexes.update(
            range(min(len(schema["prefixItems"]), len(instance))),
        )

    if len(evaluated_indexes) == len(instance):
        # Every item already has a coverage source, so evaluating later
        # applicators again cannot change the result.
        return evaluated_indexes

    if "if" in schema:
        if validator.evolve(schema=schema["if"]).is_valid(instance):
            evaluated_indexes.update(
                _find_evaluated_item_indexes_by_schema(
                    validator,
                    instance,
                    schema["if"],
                ),
            )
            if "then" in schema:
                evaluated_indexes.update(
                    _find_evaluated_item_indexes_by_schema(
                        validator,
                        instance,
                        schema["then"],
                    ),
                )
        elif "else" in schema:
            evaluated_indexes.update(
                _find_evaluated_item_indexes_by_schema(
                    validator,
                    instance,
                    schema["else"],
                ),
            )

    if "contains" in schema:
        matches = find_contains_item_indexes(
            validator,
            instance,
            schema["contains"],
        )
        if contains_count_satisfied(schema, instance, len(matches)):
            evaluated_indexes.update(matches)

    for keyword in ("allOf", "oneOf", "anyOf"):
        subschemas = schema.get(keyword)
        if not subschemas:
            continue

        valid = [
            subschema
            for subschema in subschemas
            if _is_valid_subschema(validator, instance, subschema)
        ]
        if keyword == "allOf":
            # If any branch failed, allOf's annotations are discarded.
            valid_subschemas = valid if len(valid) == len(subschemas) else []
        elif keyword == "oneOf":
            # An ambiguous oneOf is a failure, so it annotates nothing.
            valid_subschemas = valid if len(valid) == 1 else []
        else:
            # anyOf collects annotations from every successful branch.
            valid_subschemas = valid

        for subschema in valid_subschemas:
            evaluated_indexes.update(
                _find_evaluated_item_indexes_by_schema(
                    validator,
                    instance,
                    subschema,
                ),
            )

    if include_unevaluated_items and "unevaluatedItems" in schema:
        # Once ``unevaluatedItems`` is applied, every remaining item has
        # been evaluated for any outer scope.
        evaluated_indexes.update(range(len(instance)))

    return evaluated_indexes


def find_evaluated_item_indexes_by_schema(
    validator,
    instance,
    schema,
    *,
    include_unevaluated_items=True,
):
    """
    Get all indexes of items that get evaluated under the current schema.

    Covers all keywords related to unevaluatedItems: items, prefixItems,
    if, then, else, contains, minContains, maxContains, unevaluatedItems,
    allOf, oneOf, anyOf, and references.

    ``contains`` only contributes its matching indexes when its count
    constraints are satisfied.  A failed ``contains`` is therefore not
    allowed to make otherwise-uncovered items appear evaluated.

    The same helper is used by both draft 2019-09 and draft 2020-12.
    Array-valued ``items`` retains its tuple meaning for dialects which
    lack ``prefixItems``.
    """
    return sorted(
        _find_evaluated_item_indexes_by_schema(
            validator,
            instance,
            schema,
            include_unevaluated_items=include_unevaluated_items,
        ),
    )


def find_unevaluated_item_indexes_by_schema(validator, instance, schema):
    """
    Get the indexes of items which no adjacent keyword evaluated.

    ``unevaluatedItems`` itself is deliberately excluded: it consumes
    coverage, rather than contributing to it.
    """
    evaluated = set(
        find_evaluated_item_indexes_by_schema(
            validator,
            instance,
            schema,
            include_unevaluated_items=False,
        ),
    )
    return [
        index for index in range(len(instance)) if index not in evaluated
    ]


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
