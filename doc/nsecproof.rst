.. module:: dns.nsecproof
.. _nsecproof:

NSEC Proof of Nonexistence
==========================

.. versionadded:: 2.10.0

The :py:mod:`dns.nsecproof` module proves negative answers (NXDOMAIN
and NODATA) from NSEC records, as described in RFC 4035 section 5.4
and updated by RFC 6840 section 4.  It is a standalone entry point,
separate from NSEC record parsing and from cryptographic signature
verification: the caller must validate the RRSIGs of the NSEC RRsets
(e.g. with :py:func:`dns.dnssec.validate`) before calling
:py:func:`dns.nsecproof.prove`.  NSEC3 (RFC 5155) proofs are not
supported.

NSEC Proof Functions
--------------------

.. autofunction:: dns.nsecproof.prove

NSEC Proof Results
------------------

.. autoclass:: dns.nsecproof.Proof
   :members:

.. autoclass:: dns.nsecproof.Result
   :members:
   :undoc-members: