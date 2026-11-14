.. _reference.parse_path:

``parse_path``
==============

The parser that was finally used for a non-empty input: ``strict``, ``loose``,
or ``json``.

``loose`` indicates that strict XML parsing failed (or was unavailable) and
the lenient parser recovered the feed.  This does not mean the input was
well-formed; check ``bozo`` and :ref:`reference.bozo_diagnostics` when both are
present.

The attribute may be absent when fetching fails or no parsing is attempted.
