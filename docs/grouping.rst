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

Ending a fold early with FoldStop
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

Sometimes a fold's *op* function reaches its final result before the
whole iterable has been consumed. Returning a :class:`~glom.FoldStop`
wrapping that result ends the fold immediately, without reading any
remaining records, and the wrapped value is returned as the fold's
result. Because termination is decided by type, an ordinary
accumulator value that merely looks like a ``FoldStop`` does not stop
anything. ``FoldStop`` is distinct from :data:`~glom.STOP` and
:data:`~glom.SKIP`, which keep their usual meanings.

.. autoclass:: glom.FoldStop

Exceptions
----------

.. autoclass:: glom.FoldError
