.. module:: dns.tsig
.. _tsig:

TSIG
====

Dnspython supports TSIG (`RFC 8945 <https://tools.ietf.org/html/rfc8945>`_)
secret key transaction authentication for DNS, including the signing and
validation of individual messages and of multi-message transfers such as
TCP AXFR/IXFR.

Keys
----

.. autoclass:: dns.tsig.Key
   :members:

.. autofunction:: dns.tsig.get_context

Signing and Validation Functions
--------------------------------

.. autofunction:: dns.tsig.sign
.. autofunction:: dns.tsig.validate

Multi-Message Chain Verification
--------------------------------

A valid TSIG on one packet of a multi-message transfer does not make the
whole transfer trusted.  The :py:class:`~dns.tsig.TSIGChainVerifier` is an
explicit, single-use verification context that maintains the running TSIG
digest across messages, records the first packet and the chain state, and
only confirms the signature on the final packet.  A transfer is fully
verified only when :py:meth:`~dns.tsig.TSIGChainVerifier.finish` returns a
:py:class:`~dns.tsig.TSIGChainResult`.

If verification fails at any point, the verifier enters a terminal failed
state and refuses further use; create a new instance to retry a transfer.

.. autoclass:: dns.tsig.TSIGChainState
   :members:

.. autoclass:: dns.tsig.TSIGChainResult
   :members:

.. autoclass:: dns.tsig.TSIGChainVerifier
   :members:
