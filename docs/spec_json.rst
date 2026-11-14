Saving Specs as JSON
====================

Ordinary Python glom specs can include callables, :data:`~glom.T`
method calls, and other live objects. Those specs are powerful at
runtime but they cannot be written to plain JSON for storage or
transmission across processes.

The :mod:`glom.spec_json` module defines an explicit **saveable
subset** of the spec language and provides versioned routines to turn
those specs into ordinary JSON data and load them back. Existing
Python specs are not restricted in any way; the boundary only applies
to specs that go through this format.

The saveable subset covers:

* **literals** -- :class:`~glom.Val` wrapping JSON scalars and
  containers
* **paths** -- bare strings and :class:`~glom.Path`
* **object/list projections** -- ``dict`` and ``[subspec]``
* **defaults** -- :class:`~glom.Coalesce` with a static ``default``
  (and optional static ``skip``)
* **type checks** -- :class:`~glom.Check` with ``type``,
  ``instance_of``, ``equal_to``, ``one_of``, and ``default``

Callables, method calls, tuple pipelines, and other executable objects
are intentionally excluded. Loading never uses ``eval``, dynamic
imports, or ``pickle``; types referenced by checks are resolved from an
explicit registry.

Encoding
--------

Use :func:`~glom.spec_json.encode_spec` to get JSON-compatible data,
or :func:`~glom.spec_json.dumps_spec` for a JSON string:

.. code-block:: python

    >>> from glom import glom, Val, Path, Coalesce, Check
    >>> from glom.spec_json import dumps_spec, loads_spec
    >>> spec = {
    ...     "name": "name",
    ...     "label": Val("widget"),
    ...     "owner": Coalesce("owner.email", default="unknown"),
    ...     "count": Check(type=int, spec="count"),
    ... }
    >>> text = dumps_spec(spec)
    >>> restored = loads_spec(text)
    >>> target = {"name": "gizmo", "count": 3}
    >>> glom(target, restored) == glom(target, spec)
    True

Every document is wrapped in a versioned envelope:

.. code-block:: python

    {"glom_spec_version": 1, "spec": <node>}

String paths and literal strings are never ambiguous, because paths are
always encoded as explicit ``{"op": "path", ...}`` nodes while literals
are ``{"op": "val", ...}`` nodes. Path steps also preserve whether a key
was an integer or a string, so ``Path(1)`` and ``Path("1")`` round-trip
to different lookups.

If part of a spec falls outside the subset, encoding raises
:class:`~glom.spec_json.SpecEncodeError`, whose ``spec_path`` attribute
locates the offending node inside the original spec.

Loading and Validation
----------------------

:func:`~glom.spec_json.loads_spec` reads a JSON string,
:func:`~glom.spec_json.load_spec` accepts either a string or already
parsed data, and :func:`~glom.spec_json.load_spec_file` reads a
file-like object.

Loading fails fast with :class:`~glom.spec_json.SpecDecodeError`
(again carrying a ``spec_path``) when it encounters:

* an unknown or missing :data:`~glom.spec_json.SPEC_JSON_VERSION`
* an unknown operation or field name
* a duplicate field, including duplicate JSON object keys
* a type name that is not registered

Type Checks and the Type Registry
---------------------------------

Types cannot be imported dynamically, so checks refer to them by name.
A built-in registry covers common types (``str``, ``int``, ``float``,
``bool``, ``list``, ``dict``, and more). Register additional types on
both the producing and consuming side with
:func:`~glom.spec_json.register_spec_type`.

API Reference
-------------

.. automodule:: glom.spec_json
   :members:
   :undoc-members:
