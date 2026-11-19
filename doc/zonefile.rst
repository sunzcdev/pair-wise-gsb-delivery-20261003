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

$INCLUDE Directives
===================

The ``$INCLUDE`` directive reads another file as if its contents appeared at
the point of the directive (RFC 1035 section 5.1).  Two aspects of this have
security implications which callers loading zone files from an untrusted
source should control: where a filename is looked for, and how far the
inclusion may wander from that place.

Relative filenames in an ``$INCLUDE`` are resolved against the directory of
the file which contains the directive, not against the process working
directory.  This means that a file can include its neighbors, and that the
behavior of a zone file does not depend on where the process happens to be
running.

Includes are confined to the directory of the file being read, which is the
directory the caller declared by naming that file.  A file may be included
only if its canonical path, i.e. after resolving ``..`` components and
symbolic links, is inside the allowed directory, so neither an absolute path,
a parent-directory reference, nor a link can escape.  Input which is not a
file, e.g. a string with no ``filename``, has no directory to be confined to
and keeps the historical behavior of allowing any file.  Pass
``include_directories`` to ``dns.zone.from_text()`` or
``dns.zone.from_file()`` to name the allowed directories explicitly; an empty
list prohibits includes entirely.  To read a trusted zone file without any
confinement, pass ``include_resolver=dns.zonefile.IncludeResolver(None)``.

``max_include_depth`` limits how deeply includes may nest.  The outermost
file is not counted, so the default of
:py:data:`dns.zonefile.DEFAULT_MAX_INCLUDE_DEPTH` allows that many levels of
nested includes.  A file which is already being read may not be included
again, as that is a cycle; the same file may be included any number of times
elsewhere, however, as that is both legal and useful.  The current include
chain is reported, with line numbers, when an include fails.

The resolver which makes these decisions is available directly as
:py:class:`dns.zonefile.IncludeResolver`, and can be passed to
``dns.zone.from_text()`` or ``dns.zone.from_file()`` as
``include_resolver`` when more control is needed.

.. autoclass:: dns.zonefile.IncludeResolver
   :members:

.. autoclass:: dns.zonefile.IncludeSource
   :members:

.. autoclass:: dns.zonefile.IncludeOrigin
   :members:

.. autoexception:: dns.zonefile.IncludeError
.. autoexception:: dns.zonefile.IncludeNotAllowed
.. autoexception:: dns.zonefile.IncludeCycle
.. autoexception:: dns.zonefile.IncludeTooDeep

