Schema linting
==============

JSON Schema permits unknown keywords, so ordinary validation silently ignores
them.  This is usually the right runtime behavior, but it also means that a
mistyped keyword such as ``minLenght`` can sit in a contract without any
effect.


Opt-in inspection
-----------------

:func:`jsonschema.lint` inspects a schema document without validating an
instance and without changing the behavior of any validator::

    >>> from jsonschema import lint
    >>> schema = {"type": "string", "minLenght": 3}
    >>> issues = lint(schema)
    >>> issues[0].keyword
    'minLenght'
    >>> issues[0].suggestion
    'minLength'

A suggestion is only a hint.  Linting never rewrites the schema or applies the
suggested spelling.

The set of recognized keywords comes from the schema's dialect, selected from
``$schema`` or from an explicitly provided validator class::

    >>> from jsonschema import Draft7Validator, lint
    >>> lint({"prefixItems": []}, cls=Draft7Validator)[0].keyword
    'prefixItems'


Extension vocabularies
----------------------

Callers can declare extension keywords which are not part of the selected
dialect.  Exact keywords and namespace prefixes are both supported::

    >>> from jsonschema import ExtensionVocabulary, lint
    >>> vocabulary = ExtensionVocabulary(
    ...     keywords={"acme:retention"},
    ...     prefixes=("x-",),
    ... )
    >>> lint(
    ...     {"x-trace-id": 1, "acme:retention": 2},
    ...     extensions=vocabulary,
    ... )
    []

An iterable of exact keyword names or a mapping of namespace prefixes to local
keyword names may also be used.  Mapping keys which are vocabulary identifiers
instead of prefixes declare the mapped names as exact keywords.


Property names and annotations
------------------------------

Member names below ``properties``, ``patternProperties``, ``$defs``,
``definitions``, and ``dependentSchemas`` are instance names, not keywords.
Their values are still inspected as subschemas::

    >>> lint({"properties": {"minLenght": {"type": "string"}}})
    []
    >>> lint({"properties": {"name": {"minLenght": 1}}})[0].pointer
    '/properties/name/minLenght'

Values of annotation keywords such as ``default``, ``examples``, ``const``,
``enum``, ``title``, and ``description`` are treated as data, not as nested
schemas, and are not recursively inspected.


Referenced resources
--------------------

When a `referencing.Registry` is provided, registered documents reached through
``$ref`` are linted too.  Each resource is visited once, and recursive
references terminate::

    >>> from referencing import Registry
    >>> from referencing.jsonschema import DRAFT202012
    >>> other = {
    ...     "$id": "https://example.com/other",
    ...     "minLenght": 1,
    ... }
    >>> registry = Registry().with_resource(
    ...     "https://example.com/other",
    ...     DRAFT202012.create_resource(other),
    ... )
    >>> schema = {
    ...     "$ref": "https://example.com/other",
    ... }
    >>> lint(schema, registry=registry)[0].resource
    'https://example.com/other'


Diagnostics
-----------

Each `jsonschema.LintIssue` reports:

* ``resource`` -- the resource URI after resolving ``$id`` or ``id``
* ``pointer`` -- a JSON Pointer within that resource
* ``keyword`` -- the unrecognized keyword
* ``suggestion`` -- an optional near-spelling hint

The returned list is sorted stably by resource URI, JSON Pointer, and keyword.
Issues discovered in referenced resources are included in the same report.
