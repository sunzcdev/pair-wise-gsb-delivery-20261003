..  _advanced.extension-preservation:

Extension Preservation
======================

Feeds often carry custom XML extension elements that the built-in namespace
handlers do not consume.  Historically these were only exposed as flat
``prefix_element`` fields, which cannot represent nested structure: repeated
child elements overwrite each other, child order is lost, and attributes hide
the text content.

When extension preservation is enabled, every element that is *not* consumed
by a built-in handler is additionally stored as an ordered, nested structure
keyed by namespace URI and local name.

..  code-block:: pycon

    >>> import feedparser
    >>> d = feedparser.parse('examples/custom_extension.xml', preserve_extensions=True)
    >>> d.feed.extensions[0]['name']
    'rating'
    >>> d.feed.extensions[0]['namespace']
    'http://example.com/x'
    >>> d.feed.extensions[0]['attributes']
    {'scheme': 'stars'}
    >>> d.entries[0].extensions[0].children[0].text
    'alpha'

The feature is off by default.  Enable it per call with
``preserve_extensions=True`` or globally by setting
:data:`feedparser.PRESERVE_EXTENSIONS` to ``1``.


Preserved structure
-------------------

Preserved elements are collected in an ``extensions`` list on the feed
dictionary and on each entry dictionary, in document order.  Each element is
a dictionary with the following keys:

``name``
    The local name of the element, without any prefix.

``namespace``
    The namespace URI the element is bound to, or ``''`` if the element is
    not in a namespace.  Elements are keyed by URI, never by prefix, so a
    prefix that is rebound to a different URI in another part of the document
    is never mixed up with the original binding.

``attributes``
    A dictionary of the element's attributes.  ``xmlns`` declarations are
    not included.

``text``
    The element's own character data.  Text of child elements that are
    consumed by built-in handlers is not included.

``children``
    A list of preserved child elements, in document order.  Repeated child
    elements accumulate in the list; later occurrences never overwrite
    earlier ones.

Enabling preservation does not change how built-in fields such as
``title`` and ``link`` are generated, and the legacy flat ``prefix_element``
fields are still produced.


Limits and diagnostics
----------------------

Preservation is bounded so that hostile or pathological feeds cannot cause
unbounded memory use:

*   ``extension_max_depth`` (default :data:`feedparser.EXTENSION_MAX_DEPTH`,
    ``8``) caps how deeply preserved elements may nest.  Deeper subtrees are
    skipped entirely.

*   ``extension_max_text_length`` (default
    :data:`feedparser.EXTENSION_MAX_TEXT_LENGTH`, ``32768``) caps the total
    number of characters of text preserved across all extension elements in
    the feed.  Further text is dropped.

When a limit is exceeded, a message is added to
``result['extension_diagnostics']`` and collection stays bounded; parsing
otherwise continues normally.

Extension preservation never triggers external entity reads.  The strict
parser disables external general and parameter entities, and the loose
parser only expands entities from the feed's (sanitized) internal DTD
subset.


Strict vs. loose parsing
------------------------

The information available to preservation differs between the two parsers:

*   The **strict parser** (used for well-formed feeds) receives exact
    namespace URIs from the SAX parser, so ``namespace`` values are always
    correct, including for prefixes that are rebound in nested scopes.
    Entity references in extension text are expanded by the XML parser.

*   The **loose parser** (used as a fallback for ill-formed feeds) resolves
    namespace URIs from the ``xmlns`` attributes it encounters, tracked as a
    scoped stack.  This reproduces correct bindings for typical documents,
    but in severely ill-formed markup the bindings may be approximate, and
    entity references that are neither built-in nor declared in the internal
    DTD subset are preserved literally (for example, ``&foo;``).


Example
-------

The sample feed ``examples/custom_extension.xml`` contains a custom
``x:rating`` element at the channel level and a nested ``x:meta`` structure
with repeated ``x:tag`` children and a rebound prefix inside an entry:

..  literalinclude:: examples/examples/custom_extension.xml
    :language: xml
