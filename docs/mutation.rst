Assignment & Mutation
=====================

.. automodule:: glom.mutation

.. contents:: Contents
   :local:

Assignment
----------

Deeply assign within an existing structure, given a path and a value.

.. autofunction:: glom.assign

.. autoclass:: glom.Assign

Deletion
--------

Delete attributes from objects and keys from containers.

.. autofunction:: glom.delete

.. autoclass:: glom.Delete


Atomic mutation
---------------

``Atomic`` batches several assignments and deletions and applies them
all-or-nothing. It is limited to path assignment and deletion on
built-in :class:`dict` and :class:`list` objects; unsupported target
types are rejected during preflight, before the target is changed.

Rollback restores touched containers in place rather than replacing the
root with a deep copy. Existing references to the root and to child
objects therefore survive a failed batch unchanged.

.. autofunction:: glom.atomic

.. autoclass:: glom.Atomic

.. autoclass:: glom.AtomicMutationError

.. autoclass:: glom.UnsupportedAtomicMutation


Exceptions
----------

.. autoclass:: glom.PathAssignError

.. autoclass:: glom.PathDeleteError
