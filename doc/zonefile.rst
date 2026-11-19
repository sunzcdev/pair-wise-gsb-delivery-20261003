.. _zonefile:

The RRSet Reader
----------------

:py:func:`dns.zonefile.read_rrsets` reads one or more RRsets from text format.  It
is designed to be used in situations where you are processing DNS data in
text format, but do not want or need a valid zone.  For example, a DNS registry
web application might want to allow the user to input RRs.

.. autofunction:: dns.zonefile.read_rrsets


Examples
========

Read RRSets with name, TTL, and rdclass forced::

  input = '''
  mx 10 a
  mx 20 b
  ns ns1
  '''
  rrsets = dns.zonefile.read_rrsets(input, name='name', ttl=300)

Read RRSets with name, TTL, rdclass, and rdtype forced::

  input = '''
  10 a
  20 b
  '''
  rrsets = dns.zonefile.read_rrsets(input, name='name', ttl=300, rdtype='mx')

Note that in this case the length of rrsets will always be one.

Read relativized RRsets with unforced rdclass (but which must match
default_rdclass)::

  input = '''
  name1 20 MX 10 a.example.
  name2 30 IN MX 20 b
  '''
  rrsets = dns.zonefile.read_rrsets(input, origin='example', relativize=True,
                                    rdclass=None)

The dns.zonefile.Reader Class
=============================

The ``Reader`` class reads data in DNS zonefile format, or various
restrictions of that format, and converts it to a sequence of operations
in a transaction.

This class is primarily used by :py:func:`dns.zone.from_text` and
:py:func:`dns.zonefile.read_rrsets`, but may be useful for other software which needs
to process the zonefile format.

.. autoclass:: dns.zonefile.Reader
   :members:

Reading Budgets
===============

A short zone file can expand into a very large zone through ``$GENERATE``
directives.  The :py:class:`dns.zonefile.Reader` and the
:py:func:`dns.zone.from_text` and :py:func:`dns.zone.from_file` convenience
functions therefore accept two optional budgets:

* ``rr_budget`` limits the total number of RRs that may be read.  Ordinary
  records, every RR produced by a ``$GENERATE`` range, and records read from
  ``$INCLUDE`` files all count against it.  The number of iterations of a
  ``$GENERATE`` range is fixed by its start, stop, and step values, so the
  exact number of RRs the range will produce is checked before the range is
  expanded; an over-large directive fails outright instead of generating as
  many records as fit and silently dropping the rest.

* ``name_budget`` limits the number of distinct owner names that may be read.
  The same owner name used more than once only counts once.  Distinct names
  cannot be predicted from a ``$GENERATE`` range without expanding its
  templates, so this budget is enforced as names are actually encountered,
  record by record.

When either budget is exceeded, :py:exc:`dns.zonefile.BudgetExceeded` is
raised.  The read runs inside a transaction, so on any failure the
transaction is rolled back: a partially built zone is never returned, and if
the target was an existing zone, its original content is preserved.

.. autoexception:: dns.zonefile.BudgetExceeded
   :members:

