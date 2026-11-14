"""Tests for element-scoped namespace bindings.

A namespace prefix (or the default namespace) may be rebound on any
element, and the binding applies to that element and its descendants
only.  These tests verify that feedparser restores the enclosing
bindings when the element ends, that the strict and loose parsers agree
within their recognizable XML boundaries, and that a damaged document
cannot leak one entry's local bindings into the next entry.
"""

import io
import xml.sax
import xml.sax.handler

import feedparser
from feedparser.api import LooseFeedParser, StrictFeedParser


def parse_strict(document):
    parser = StrictFeedParser("http://example.com/", None, "utf-8")
    sax_parser = xml.sax.make_parser()
    sax_parser.setFeature(xml.sax.handler.feature_namespaces, 1)
    sax_parser.setContentHandler(parser)
    sax_parser.parse(io.StringIO(document))
    return parser


def parse_loose(document):
    parser = LooseFeedParser("http://example.com/", None, "utf-8", {})
    parser.feed(document)
    return parser


def test_prefix_rebinding_restored_strict():
    parser = parse_strict(
        '<feed xmlns="http://www.w3.org/2005/Atom">'
        '<entry xmlns:x="http://www.itunes.com/DTDs/Podcast-1.0.dtd">'
        "<x:block>yes</x:block></entry>"
        '<entry xmlns:x="http://example.com/other">'
        "<x:block>no</x:block></entry>"
        "</feed>"
    )
    assert parser.entries[0]["itunes_block"] == 1
    assert "itunes_block" not in parser.entries[1]
    assert parser.entries[1]["x_block"] == "no"


def test_prefix_rebinding_restored_loose():
    parser = parse_loose(
        '<feed xmlns="http://www.w3.org/2005/Atom">'
        '<entry xmlns:x="http://www.itunes.com/DTDs/Podcast-1.0.dtd">'
        "<x:block>yes</x:block></entry>"
        '<entry xmlns:x="http://example.com/other">'
        "<x:block>no</x:block></entry>"
        "</feed>"
    )
    assert parser.entries[0]["itunes_block"] == 1
    assert "itunes_block" not in parser.entries[1]
    assert parser.entries[1]["x_block"] == "no"


def test_ill_formed_feed_does_not_leak_entry_scope():
    # The first <entry> is never closed.  The strict parser rejects the
    # document and the loose parser must not let the first entry's local
    # prefix binding pollute the second entry.
    result = feedparser.parse(
        '<feed xmlns="http://www.w3.org/2005/Atom">'
        '<entry xmlns:x="http://www.itunes.com/DTDs/Podcast-1.0.dtd">'
        "<x:block>yes</x:block>"
        "<entry>"
        "<x:block>no</x:block>"
        "</entry>"
        "</feed>"
    )
    assert result.bozo == 1
    assert len(result.entries) == 2
    assert result.entries[0]["itunes_block"] == 1
    assert "itunes_block" not in result.entries[1]
    assert result.entries[1]["x_block"] == "no"


def test_ill_formed_feed_restores_scope_on_ancestor_end_tag():
    # <wrapper> is never closed, but the recognizable </entry> boundary
    # must still discard the bindings declared inside it.
    result = feedparser.parse(
        '<feed xmlns="http://www.w3.org/2005/Atom">'
        "<entry>"
        '<wrapper xmlns:x="http://www.itunes.com/DTDs/Podcast-1.0.dtd">'
        "<x:block>yes</x:block>"
        "</entry>"
        "<entry>"
        "<x:block>no</x:block>"
        "</entry>"
        "</feed>"
    )
    assert result.bozo == 1
    assert len(result.entries) == 2
    assert result.entries[0]["itunes_block"] == 1
    assert "itunes_block" not in result.entries[1]
    assert result.entries[1]["x_block"] == "no"


def test_conflicting_default_namespace_rebinding_strict():
    parser = parse_strict(
        '<feed xmlns="http://www.w3.org/2005/Atom">'
        "<entry>"
        '<wrapper xmlns="http://example.com/other"><title>t</title></wrapper>'
        "</entry>"
        "</feed>"
    )
    # The summary keeps the document-level binding...
    assert parser.namespaces_in_use[""] == "http://www.w3.org/2005/Atom"
    # ...and the conflicting local binding is recorded as a diagnostic.
    assert any("rebound" in message for message in parser.namespace_diagnostics)


def test_conflicting_default_namespace_rebinding_loose():
    parser = parse_loose(
        '<feed xmlns="http://www.w3.org/2005/Atom">'
        "<entry>"
        '<wrapper xmlns="http://example.com/other"><title>t</title></wrapper>'
        "</entry>"
        "</feed>"
    )
    assert parser.namespaces_in_use[""] == "http://www.w3.org/2005/Atom"
    assert any("rebound" in message for message in parser.namespace_diagnostics)


def test_no_diagnostics_for_consistent_declarations():
    parser = parse_strict(
        '<feed xmlns="http://www.w3.org/2005/Atom" '
        'xmlns:itunes="http://www.itunes.com/DTDs/Podcast-1.0.dtd">'
        '<entry xmlns:x="http://www.itunes.com/DTDs/Podcast-1.0.dtd">'
        "<x:block>yes</x:block></entry>"
        "</feed>"
    )
    assert parser.namespace_diagnostics == []
    assert parser.namespaces_in_use == {
        "": "http://www.w3.org/2005/Atom",
        "itunes": "http://www.itunes.com/DTDs/Podcast-1.0.dtd",
    }

def test_xhtml_content_item_tag_is_not_an_entry_boundary():
    # A literal <item> tag inside XHTML content must not be mistaken for
    # a new entry and must not discard the entry's namespace scope.
    document = (
        '<feed xmlns="http://www.w3.org/2005/Atom">'
        '<entry xmlns:x="http://www.itunes.com/DTDs/Podcast-1.0.dtd">'
        '<content type="xhtml">'
        '<div xmlns="http://www.w3.org/1999/xhtml"><item>markup</item></div>'
        "</content>"
        "<x:block>yes</x:block>"
        "</entry>"
        "</feed>"
    )
    for parser in (parse_strict(document), parse_loose(document)):
        assert parser.entries[0]["itunes_block"] == 1
