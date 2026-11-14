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

Deep-merging documents
----------------------

:func:`~glom.deep_merge` and :class:`~glom.DeepMerge` recursively
combine heterogeneous mapping documents. Nested mappings merge by
key, while scalar conflicts and every list merge have explicit
policies. Inputs are copied at every level, and circular references
are rejected with the full path.

.. autofunction:: glom.deep_merge

.. autoclass:: glom.DeepMerge

.. autoclass:: glom.DeepMergeError

.. autoclass:: glom.DeepMergeConflictError

.. autoclass:: glom.DeepMergeTypeError

.. autoclass:: glom.DeepMergeCycleError

.. autoclass:: glom.DeepMergeKeyError

Exceptions
----------

.. autoclass:: glom.FoldError
