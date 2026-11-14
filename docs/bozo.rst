.. _advanced.bozo:

Bozo Detection
==============

:program:`Universal Feed Parser` can parse feeds whether they are well-formed
:abbr:`XML (Extensible Markup Language)` or not.  However, since some
applications may wish to reject or warn users about non-well-formed feeds,
:program:`Universal Feed Parser` sets the ``bozo`` bit when it detects that a
feed is not well-formed.  Thanks to `Tim Bray
<http://www.tbray.org/ongoing/When/200x/2004/01/11/PostelPilgrim>`_ for
suggesting this terminology.

Detecting a non-well-formed feed
--------------------------------

..  code-block:: pycon

    >>> d = feedparser.parse('$READTHEDOCS_CANONICAL_URL/examples/atom10.xml')
    >>> d.bozo
    0
    >>> d = feedparser.parse('http://feedparser.org/tests/illformed/rss/aaa_illformed.xml')
    >>> d.bozo
    1
    >>> d.bozo_exception
    <xml.sax._exceptions.SAXParseException instance at 0x00BAAA08>
    >>> exc = d.bozo_exception
    >>> exc.getMessage()
    "expected '>'\\n"
    >>> exc.getLineNumber()
    6


There are many reasons an :abbr:`XML (Extensible Markup Language)` document
could be non-well-formed besides this example (incomplete end tags)  See
:ref:`advanced.encoding` for some other ways to trip the bozo bit.

Ordered diagnostics
-------------------

``bozo_exception`` records only one problem.  If a feed needs an encoding
fallback and also fails strict XML parsing, the later error can hide the
earlier one.  When a problem is recorded, the parse result includes an
optional :ref:`bozo_diagnostics <reference.bozo_diagnostics>` list in
occurrence order.  Items distinguish the ``fetch``, ``encoding``, ``strict``,
and ``loose`` phases (with ``json`` used for JSON parsing), and
:ref:`parse_path <reference.parse_path>` records the parser that was finally
used.

The legacy ``bozo`` and ``bozo_exception`` fields keep their existing meaning.
In particular, a successful loose recovery leaves ``bozo`` set and does not
discard diagnostics from earlier phases.
