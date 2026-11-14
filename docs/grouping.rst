Reduction & Grouping
====================

This document contains glom techniques for transforming a collection
of data to a smaller set, otherwise known as "grouping" or
"reduction".

Combining iterables with Flatten and Merge
------------------------------------------

.. versionadded:: 19.1.0

Got lists of lists? Sets of tuples? A sequence of dicts (but only want
one)? Do you find yourself reaching for Python's builtin :func:`sum`
and :func:`reduce`? To handle these situations and more, glom has five
specifier types and two convenience functions:

.. autofunction:: glom.flatten

.. autoclass:: glom.Flatten

.. autofunction:: glom.merge

.. autoclass:: glom.Merge

.. autoclass:: glom.Sum

.. autoclass:: glom.Fold

Exceptions
----------

.. autoclass:: glom.FoldError

Saving and restoring Group aggregation state
--------------------------------------------

Long-running collection jobs are often split into batches. A
:class:`glom.grouping.Group` keeps its running aggregation state on the
instance, so the same Group can be fed data across multiple calls. For
state that has to cross a process or program boundary, Group can export a
portable snapshot and restore it later, with results identical to
processing every input in a single pass.

Only the built-in :class:`glom.Count`, :class:`glom.Sum` (with the
default ``int`` or a ``float`` init) and :class:`glom.grouping.Avg`
aggregations, plus nested dict groupings, are portable; arbitrary
callables and other aggregations cannot be restored and raise
:class:`glom.grouping.GroupStateError` up front, before processing.

.. code-block:: python

    >>> from glom import glom, T
    >>> from glom.grouping import Group
    >>> from glom.reduction import Count
    >>> spec = {T % 2: Count()}
    >>> group = Group(spec)
    >>> glom(range(4), group)            # first batch
    {0: 2, 1: 2}
    >>> state = group.export_state()    # JSON-safe snapshot
    >>> # ...persist state, start a new process, then later...
    >>> group = Group({T % 2: Count()})
    >>> group.restore_state(state)      # doctest: +ELLIPSIS
    <...Group...>
    >>> glom(range(4, 10), group)       # next batch
    {0: 5, 1: 5}

The state document carries a format version and a fingerprint of the
grouping definition. Restoring it into a Group built from a different
spec raises :class:`glom.grouping.GroupStateError`. Group keys (including
tuples) and numeric types round-trip losslessly, and an exported snapshot
is never mutated by continuing to feed the original Group.

.. autoclass:: glom.grouping.Group
   :members: export_state, restore_state

.. autoclass:: glom.grouping.GroupStateError
