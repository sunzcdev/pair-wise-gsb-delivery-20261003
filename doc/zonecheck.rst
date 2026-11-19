.. module:: dns.zonecheck
.. _zonecheck:

Zone Transaction Structural Validation
======================================

The :py:mod:`dns.zonecheck` module provides an explicit commit-time validator
for zone transactions.  When a zone is built up over several separate
transactions, a change that is harmless on its own can leave the resulting
whole zone in a structurally invalid state.  The validator therefore inspects
the *complete current view* of a transaction, not just the names the
transaction touched, and refuses to let an invalid view be published.

The validator is strictly read-only.  It never deletes or rewrites a
conflicting record to "fix" the zone; it reports each problem as a
:py:class:`~dns.zonecheck.Violation` that names both the owner name and the
offending RRset.

Rules
-----

Validation rules are opt-in through the :py:class:`~dns.zonecheck.Rule`
enumeration, and only the documented rules are accepted.  Configuring an
unknown rule raises :py:class:`~dns.zonecheck.UnknownRule` when the validator
is constructed.

.. autoclass:: Rule
   :members:

.. autodata:: ALL_RULES

Attaching a validator
---------------------

A :py:class:`CommitValidator` can be attached to a transaction with
:py:meth:`~CommitValidator.attach`, or used directly as a transaction setup
callable (it implements ``__call__``) such as the ``transaction_setup``
argument of :py:func:`dns.zone.from_text`.  Once attached,
:py:meth:`~dns.transaction.Transaction.commit` runs the checks against the
full view before publishing; on failure the transaction is rolled back and a
:py:class:`CommitCheckFailure` is raised, so no invalid version is published.

For ad-hoc inspection without enforcing anything at commit time, use
:py:meth:`CommitValidator.validate` or the :py:func:`validate` convenience
function, which return a :py:class:`ValidationReport`.

::

   import dns.rdataset
   import dns.versioned
   import dns.zone
   import dns.zonecheck

   zone = dns.versioned.Zone("example.")
   validator = dns.zonecheck.CommitValidator()  # every supported rule

   with zone.writer() as txn:
       validator.attach(txn)
       txn.add("child", dns.rdataset.from_text("IN", "NS", 300, "ns1.child"))
       txn.add("ns1.child", dns.rdataset.from_text("IN", "A", 300, "192.0.2.10"))
       # commit now runs the structural checks and rolls back on failure

A subset of rules may be selected, and an empty rule set always passes::

   validator = dns.zonecheck.CommitValidator(
       [dns.zonecheck.Rule.APEX_SOA, dns.zonecheck.Rule.APEX_NS]
   )

Reports and violations
----------------------

.. autoclass:: Violation
   :members:

.. autoclass:: ValidationReport
   :members:

The validator and exceptions
----------------------------

.. autoclass:: CommitValidator
   :members:

.. autoexception:: CommitCheckFailure
   :members:

.. autoexception:: UnknownRule

.. autoexception:: UnknownOrigin

.. autofunction:: validate
