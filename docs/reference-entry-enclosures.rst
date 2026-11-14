.. _reference.entry.enclosures:

``entries[i].enclosures``
================================

A list of links to external files associated with this entry.

Some aggregators automatically download enclosures (although this technique has
`known problems <http://gonze.com/weblog/story/5-17-4>`_).  Some aggregators
render each enclosure as a link.  Most aggregators ignore them.

The :abbr:`RSS (Rich Site Summary)` specification states that there can be at
most one enclosure per item.  However, because some feeds break this rule,
:program:`Universal Feed Parser` captures all of them and makes them available
as a list.

.. rubric:: Comes from

- /atom10:feed/atom10:entry/atom10:link[@rel="enclosure"]
- /rss/channel/item/enclosure
- /json/items/attachments


.. _reference.entry.enclosures.href:

``entries[i].enclosures[j].href``
----------------------------------------

The :abbr:`URL (Uniform Resource Locator)` of the linked file.

If this is a relative :abbr:`URI (Uniform Resource Identifier)`, it is
:ref:`resolved according to a set of rules <advanced.base>`.


.. _reference.entry.enclosure_details:

``entries[i].enclosure_details``
----------------------------------------

A list of URI details for the attachments.  This list is parallel to
``entries[i].enclosures``, so ``enclosure_details[j]`` describes
``enclosures[j]``.  The existing ``enclosures`` structure is unchanged.

The details are always present for RSS enclosures, Atom enclosure links, and
JSON Feed attachments, including when relative URI resolution is disabled.


.. _reference.entry.enclosure_details.value:

``entries[i].enclosure_details[j].value``
--------------------------------------------------

The value of the attachment URI exactly as it appeared in the source feed.
An absent URI is represented by ``None``; an explicitly empty URI is ``""``.


.. _reference.entry.enclosure_details.base:

``entries[i].enclosure_details[j].base``
-------------------------------------------------

The effective base :abbr:`URI (Uniform Resource Identifier)` used to resolve
the attachment.  It reflects the response's final address, ``Content-Location``
header, and the ``xml:base`` attributes on the attachment and its ancestors.
An ``xml:base`` declared locally only applies to that element's subtree.


.. _reference.entry.enclosure_details.href:

``entries[i].enclosure_details[j].href``
-------------------------------------------------

The result of resolving ``value`` against ``base``.  Invalid values are
preserved in ``value`` and resolve to an empty string.  URIs that differ only
by fragment remain separate attachments and retain their fragments here.


.. _reference.entry.enclosures.length:

``entries[i].enclosures[j].length``
------------------------------------------

The length of the linked file.


.. _reference.entry.enclosures.type:

``entries[i].enclosures[j].type``
----------------------------------------

The content type of the linked file.
