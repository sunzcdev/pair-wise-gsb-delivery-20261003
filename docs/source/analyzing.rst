.. _analyze:

Analyzing the Parsed Statement
==============================

When the :meth:`~sqlparse.parse` function is called the returned value
is a tree-ish representation of the analyzed statements. The returned
objects can be used by applications to retrieve further information about
the parsed SQL.


Base Classes
------------

All returned objects inherit from these base classes.
The :class:`~sqlparse.sql.Token` class represents a single token and
:class:`~sqlparse.sql.TokenList` class is a group of tokens.
The latter provides methods for inspecting its child tokens.

.. autoclass:: sqlparse.sql.Token
   :members:

.. autoclass:: sqlparse.sql.TokenList
   :members:


SQL Representing Classes
------------------------

The following classes represent distinct parts of a SQL statement.

.. autoclass:: sqlparse.sql.Statement
   :members:

.. autoclass:: sqlparse.sql.Comment
   :members:

.. autoclass:: sqlparse.sql.Identifier
   :members:

.. autoclass:: sqlparse.sql.IdentifierList
   :members:

.. autoclass:: sqlparse.sql.Where
   :members:

.. autoclass:: sqlparse.sql.Case
   :members:

.. autoclass:: sqlparse.sql.Parenthesis
   :members:

.. autoclass:: sqlparse.sql.If
   :members:

.. autoclass:: sqlparse.sql.For
   :members:

.. autoclass:: sqlparse.sql.Assignment
   :members:

.. autoclass:: sqlparse.sql.Comparison
   :members:


Transaction Control Events
--------------------------

The :mod:`sqlparse.transactions` module provides a restricted,
best-effort preview of the transaction control flow of a script, e.g.
for batch loaders.  It recognizes top-level ``BEGIN``/``COMMIT``/
``ROLLBACK``, ``SAVEPOINT``, ``RELEASE`` and ``ROLLBACK TO SAVEPOINT``
statements together with savepoint names and source positions.
Keywords inside procedure bodies, strings or comments are not reported,
and unrecognized statements are reported as ``UNKNOWN``.

The scan is purely syntactic: it does not connect to a database and
does not fully validate the SQL.  The recognized transaction start
syntax is selected explicitly through
:class:`~sqlparse.transactions.TransactionScanConfig`.

.. autofunction:: sqlparse.transactions.scan_transactions

.. autofunction:: sqlparse.transactions.check_savepoint_order

.. autoclass:: sqlparse.transactions.TransactionScanConfig
   :members:

.. autoclass:: sqlparse.transactions.TransactionEvent
   :members:

.. autoclass:: sqlparse.transactions.TransactionEventType
   :members:

.. autoclass:: sqlparse.transactions.TransactionReport
   :members:

.. autoclass:: sqlparse.transactions.SavepointIssue
   :members:

