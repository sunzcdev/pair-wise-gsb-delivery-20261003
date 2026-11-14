.. _reference.bozo_diagnostics:

``bozo_diagnostics``
====================

An optional, ordered list of problems found while parsing a feed.  The list is
present only when at least one problem was recorded.

Each item is a plain mapping with stable, serializable values:

``phase``
    The parsing phase.  One of ``fetch``, ``encoding``, ``strict``, ``loose``,
    or ``json``.

``stage``
    A descriptive form of the phase: ``fetch``, ``encoding``,
    ``strict_parse``, ``lenient_recovery``, or ``json_parse``.

``exception`` and ``type``
    The exception class name.  These are strings, not exception instances.

``message``
    A whitespace-normalized and length-bounded description.  It never contains
    the complete source document.

``line`` and ``column``
    The one-based source position when the parser reports one, otherwise
    ``None``.

The list preserves the order in which problems occurred.  A successful loose
or JSON recovery does not remove earlier diagnostics.  Items never retain
exceptions, streams, files, or parser objects.

See :ref:`advanced.bozo` for details.
