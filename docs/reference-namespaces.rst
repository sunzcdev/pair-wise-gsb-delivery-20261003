.. _reference.namespaces:

``namespaces``
=====================

A dictionary of all :abbr:`XML (Extensible Markup Language)` namespaces defined
in the feed, as ``{prefix: namespaceURI}``.

.. note::

    The prefixes listed in the ``namespaces`` dictionary may not match the
    prefixes defined in the original feed.  See :ref:`advanced.namespaces` for more
    details.

.. note::

    ``namespaces`` is a document-level summary.  If a prefix is rebound to a
    different namespace URI in part of the feed, the first (outermost)
    binding is reported here; the scoped bindings are always used when
    routing elements to extension handlers.

.. tip::

    This element always exists, although it may be an empty dictionary if the feed
    does not define any namespaces (such as an :abbr:`RSS (Rich Site Summary)` 2.0
    feed with no extensions).
