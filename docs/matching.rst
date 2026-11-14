Matching & Validation
=====================

.. automodule:: glom.matching

.. contents:: Contents
   :local:

Validation with Match
~~~~~~~~~~~~~~~~~~~~~

For matching whole data structures, use a :class:`~glom.Match` spec.

.. autoclass:: glom.Match
   :members:

Optional and required ``dict`` key matching
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

Note that our four :class:`~glom.Match` rules above imply that
:class:`object` is a match-anything pattern.  Because
``isinstance(val, object)`` is true for all values in Python,
``object`` is a useful stopping case. For instance, if we wanted to
extend an example above to allow additional keys and values in the
user dict above we could add :class:`object` as a generic pass through::

  >>> target = [{'id': 1, 'email': 'alice@example.com', 'extra': 'val'}]
  >>> spec = Match([{'id': int, 'email': str, object: object}])
  >>> assert glom(target, spec) == \\
      ... [{'id': 1, 'email': 'alice@example.com', 'extra': 'val'}]
  True

The fact that ``{object: object}`` will match any dictionary exposes
the subtlety in :class:`~glom.Match` dictionary evaluation.

By default, value match keys are required, and other keys are
optional.  For example, ``'id'`` and ``'email'`` above are required
because they are matched via ``==``.  If either was not present, it
would raise :class:`~glom.MatchError`.  :class:`object` however is matched
with :func:`isinstance`. Since it is not a value-match comparison,
it is not required.

This default behavior can be modified with :class:`~glom.Required`
and :class:`~glom.Optional`.

.. autoclass:: glom.Optional

.. autoclass:: glom.Required

Collecting multiple errors
~~~~~~~~~~~~~~~~~~~~~~~~~~

By default a :class:`~glom.Match` stops at the first failing location and
raises a single :class:`~glom.MatchError`. When validating a large or nested
payload it is often more useful to keep checking the independent sibling
fields and report every problem at once.

Call :meth:`~glom.Match.collect` to run a match in multi-error mode without
raising. It returns a :class:`~glom.MatchReport` holding an ordered list of
:class:`~glom.MatchProblem` objects, each with a ``path``, an ``expected``
description, and an ``actual_type``:

  >>> report = Match({'id': int, 'name': str}).collect({'id': 'x', 'name': 1})
  >>> report.ok
  False
  >>> [problem.as_dict() for problem in report.problems]
  [{'path': ['id'], 'expected': 'int', 'actual_type': 'str'}, {'path': ['name'], 'expected': 'str', 'actual_type': 'int'}]

A few rules keep the report actionable and safe:

- A parent whose type is wrong is reported once; its would-be children are
  not fabricated as missing.
- :class:`~glom.Optional`, optional defaults, and :class:`~glom.Or` keep
  their usual meaning. An :class:`~glom.Or` with any successful branch leaks
  none of its failed branches.
- ``max_problems`` caps how many problems are collected. Once reached,
  further traversal stops and the report is marked ``truncated`` (pass
  ``None`` to remove the cap).
- Actual target values are never included, only type names and safe spec
  descriptions, so sensitive payloads do not appear in the structured report.

The same mode is available as a raising spec through
``Match(spec, multi_error=True)``, which raises :class:`~glom.MultiMatchError`.

.. autoclass:: glom.MatchProblem

.. autoclass:: glom.MatchReport

.. autoclass:: glom.MultiMatchError

``M`` Expressions
~~~~~~~~~~~~~~~~~

The most concise way to express validation and guards.

.. autodata:: glom.M

Boolean operators and matching
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

While ``M`` is an easy way to construct expressions, sometimes a more
object-oriented approach can be more suitable.

.. autoclass:: glom.Or

.. autoclass:: glom.And

.. autoclass:: glom.Not

String matching
~~~~~~~~~~~~~~~

.. autoclass:: glom.Regex

Control flow with ``Switch``
~~~~~~~~~~~~~~~~~~~~~~~~~~~~

Match becomes even more powerful when combined with the ability to
branch spec execution.

.. autoclass:: glom.Switch

Exceptions
~~~~~~~~~~

.. autoclass:: glom.MatchError

.. autoclass:: glom.TypeMatchError

Validation with Check
~~~~~~~~~~~~~~~~~~~~~

.. autoclass:: glom.Check

.. autoclass:: glom.CheckError
