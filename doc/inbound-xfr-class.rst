.. _inbound-xfr-class:

The dns.xfr.Inbound Class and make_query() function
---------------------------------------------------

The ``Inbound`` class provides support for inbound DNS zone transfers,
both AXFR and IXFR.  It is invoked by I/O code, i.e.
:py:func:`dns.query.inbound_xfr` or
:py:func:`dns.asyncquery.inbound_xfr`.  When a message related to the
transfer arrives, the I/O code calls the ``process_message()`` method
which adds the content to the pending transaction.

A transfer arrives in many messages, so its progress is observable
without looking at the zone.  :py:attr:`dns.xfr.Inbound.phase` is an
:py:class:`dns.xfr.InboundPhase`, moving through the stages

* ``START_SOA``, until the leading SOA has been seen;
* ``ZONE_IDENTITY``, once the leading SOA has established that the
  response is for this zone and class, before any zone data;
* ``DATA``, while zone data is being received into the pending
  transaction;
* ``END_SOA``, when the trailing SOA has been matched against the leading
  SOA;
* ``DONE``, once the new version has been committed.

Each phase entered is recorded in :py:attr:`dns.xfr.Inbound.history`.
The feed of data is held in a writer transaction until the closing SOA
matches the opening SOA, so readers keep seeing the previous version
while the transfer is in progress.  A transfer which is rejected is put
in the ``FAILED`` phase and its transaction is rolled back before
``process_message()`` raises, so a failure never leaves a partly
replaced zone.  The ``Inbound`` cannot then be reused; pass the same
transaction manager to a new ``Inbound`` to start a new transfer.

A transfer which raises also leaves the failed ``Inbound`` untouched for
inspection.  If the caller stops feeding messages and leaves the
``Inbound`` context without the closing SOA, the transfer is abandoned
in the same way.

The ``make_query()`` function is used to making the query message for
the query methods to use in more complex situations, e.g. with TSIG or
EDNS.

.. autoexception:: dns.xfr.TransferError
.. autoexception:: dns.xfr.SerialWentBackwards
.. autoexception:: dns.xfr.TransferFormError
.. autoexception:: dns.xfr.ZoneIdentityError
.. autoexception:: dns.xfr.OutOfZoneName
.. autoexception:: dns.xfr.ClassMismatch
.. autoexception:: dns.xfr.DuplicateEnd
.. autoexception:: dns.xfr.TransferAborted
.. autoexception:: dns.xfr.UseTCP

.. autoclass:: dns.xfr.InboundPhase
   :members:

.. autoclass:: dns.xfr.Inbound
   :members:

.. autofunction:: dns.xfr.make_query
