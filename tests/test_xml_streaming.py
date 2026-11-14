"""Tests for the streaming XML record entry point."""

from __future__ import annotations

import tracemalloc
from io import BytesIO
from pathlib import Path

import pytest
from lxml import etree

from parsel import Selector, iter_xml_records
from parsel.selector import _normalize_record_tag

NAMESPACE = "http://example.org/feed"

COLLECTION = f"""<?xml version="1.0"?>
<catalog xmlns="{NAMESPACE}" xmlns:other="http://example.org/other">
  <record id="1"><name>first</name><other:note>note one</other:note></record>
  <record id="2"><name>second</name></record>
  <record id="3"><name>third</name></record>
</catalog>
""".encode()

# The same namespace URI is bound to different prefixes across records.
PREFIX_VARIANTS = f"""<?xml version="1.0"?>
<root xmlns:a="{NAMESPACE}">
  <a:record id="a1"><a:name>one</a:name></a:record>
  <b:record id="b1" xmlns:b="{NAMESPACE}"><b:name>two</b:name></b:record>
  <record id="d1" xmlns="{NAMESPACE}"><name>three</name></record>
</root>
""".encode()

NESTED_RECORDS = f"""<?xml version="1.0"?>
<root xmlns="{NAMESPACE}">
  <record id="outer-1">
    <record id="inner-1"><value>nested</value></record>
  </record>
  <record id="outer-2"><value>plain</value></record>
</root>
""".encode()

NO_NAMESPACE = b"""<?xml version="1.0"?>
<root>
  <item id="1"><value>a</value></item>
  <item id="2"><value>b</value></item>
</root>
"""


class CountingStream(BytesIO):
    """Binary stream that records whether the library closed it."""

    def __init__(self, data: bytes) -> None:
        super().__init__(data)
        self.close_count = 0

    def close(self) -> None:  # type: ignore[override]
        self.close_count += 1
        super().close()


def test_yields_a_selector_per_record_in_order() -> None:
    selectors = list(
        iter_xml_records(
            BytesIO(COLLECTION),
            f"{{{NAMESPACE}}}record",
            namespaces={"feed": NAMESPACE},
        )
    )
    assert len(selectors) == 3
    assert all(isinstance(selector, Selector) for selector in selectors)
    assert [selector.xpath("@id").get() for selector in selectors] == [
        "1",
        "2",
        "3",
    ]
    assert selectors[0].xpath("feed:name/text()").get() == "first"
    assert selectors[1].type == "xml"


def test_tuple_record_form() -> None:
    selectors = list(
        iter_xml_records(BytesIO(COLLECTION), (NAMESPACE, "record"))
    )
    assert [selector.xpath("@id").get() for selector in selectors] == [
        "1",
        "2",
        "3",
    ]


def test_local_name_matches_only_namespaceless_elements() -> None:
    selectors = list(iter_xml_records(BytesIO(NO_NAMESPACE), "item"))
    assert [selector.xpath("@id").get() for selector in selectors] == ["1", "2"]
    # A namespaced document must not be matched by a bare local name.
    assert list(iter_xml_records(BytesIO(COLLECTION), "record")) == []


def test_matching_is_independent_of_namespace_prefix() -> None:
    selectors = list(
        iter_xml_records(BytesIO(PREFIX_VARIANTS), f"{{{NAMESPACE}}}record")
    )
    assert [selector.xpath("@id").get() for selector in selectors] == [
        "a1",
        "b1",
        "d1",
    ]


def test_namespaces_are_available_on_yielded_selectors() -> None:
    selectors = list(
        iter_xml_records(
            BytesIO(COLLECTION),
            f"{{{NAMESPACE}}}record",
            namespaces={"feed": NAMESPACE, "o": "http://example.org/other"},
        )
    )
    assert selectors[0].xpath("feed:name/text()").get() == "first"
    assert selectors[0].xpath("o:note/text()").get() == "note one"
    assert selectors[0].css("feed|name::text").get() == "first"


def test_nested_same_name_records_yield_outermost_only() -> None:
    namespaces = {"feed": NAMESPACE}
    selectors = list(
        iter_xml_records(
            BytesIO(NESTED_RECORDS),
            f"{{{NAMESPACE}}}record",
            namespaces=namespaces,
        )
    )
    assert [selector.xpath("@id").get() for selector in selectors] == [
        "outer-1",
        "outer-2",
    ]
    # The nested record remains queryable inside its outer record.
    nested = selectors[0].xpath(".//feed:record/@id", namespaces=namespaces).getall()
    assert nested == ["inner-1"]
    assert (
        selectors[0]
        .xpath(".//feed:value/text()", namespaces=namespaces)
        .get()
        == "nested"
    )


def test_deeply_nested_records_keep_document_order() -> None:
    xml = f"""<root xmlns="{NAMESPACE}">
      <record id="a"><record id="b"><record id="c"/></record></record>
      <record id="d"/>
    </root>""".encode()
    selectors = list(iter_xml_records(BytesIO(xml), f"{{{NAMESPACE}}}record"))
    assert [selector.xpath("@id").get() for selector in selectors] == ["a", "d"]


def test_yielded_selectors_survive_continued_iteration() -> None:
    iterator = iter(
        iter_xml_records(BytesIO(COLLECTION), f"{{{NAMESPACE}}}record")
    )
    first = next(iterator)
    second = next(iterator)
    # Consume the rest of the stream, which reuses/clears parser nodes.
    rest = list(iterator)
    assert first.xpath("@id").get() == "1"
    assert second.xpath("@id").get() == "2"
    assert [selector.xpath("@id").get() for selector in rest] == ["3"]


def test_yielded_selectors_survive_source_closure() -> None:
    stream = BytesIO(COLLECTION)
    selectors = list(iter_xml_records(stream, f"{{{NAMESPACE}}}record"))
    stream.close()
    assert [selector.xpath("@id").get() for selector in selectors] == [
        "1",
        "2",
        "3",
    ]


def test_caller_keeps_ownership_of_the_stream() -> None:
    stream = CountingStream(COLLECTION)
    selectors = list(
        iter_xml_records(stream, f"{{{NAMESPACE}}}record")
    )
    assert len(selectors) == 3
    assert stream.close_count == 0
    assert not stream.closed
    stream.close()
    # Selectors remain usable after the caller closes the stream.
    assert selectors[0].xpath("@id").get() == "1"


def test_abandoned_iteration_does_not_close_stream() -> None:
    stream = CountingStream(COLLECTION)
    iterator = iter(
        iter_xml_records(stream, f"{{{NAMESPACE}}}record")
    )
    next(iterator)
    del iterator
    assert stream.close_count == 0
    assert not stream.closed
    stream.close()


def test_accepts_a_file_path() -> None:
    # Use an existing directory: restricted sandboxes may forbid creating new
    # subdirectories while still allowing files inside existing ones.
    path = Path(__file__).with_name("streaming_collection_fixture.xml")
    path.write_bytes(COLLECTION)
    try:
        selectors = list(
            iter_xml_records(path, f"{{{NAMESPACE}}}record")
        )
        assert [selector.xpath("@id").get() for selector in selectors] == [
            "1",
            "2",
            "3",
        ]
    finally:
        path.unlink()


def test_no_matching_records_yields_nothing() -> None:
    assert (
        list(iter_xml_records(BytesIO(COLLECTION), f"{{{NAMESPACE}}}missing"))
        == []
    )


def test_document_without_records_yields_nothing() -> None:
    assert list(iter_xml_records(BytesIO(b"<root/>"), "record")) == []
    assert list(
        iter_xml_records(BytesIO(b'<?xml version="1.0"?><root></root>'), "record")
    ) == []


def test_record_can_be_the_document_root() -> None:
    selectors = list(
        iter_xml_records(BytesIO(b"<record><a>x</a></record>"), "record")
    )
    assert len(selectors) == 1
    assert selectors[0].root.getparent() is None
    assert selectors[0].xpath("a/text()").get() == "x"


def test_truncated_xml_raises_syntax_error() -> None:
    truncated = COLLECTION[: COLLECTION.index(b'id="3"')]
    with pytest.raises(etree.XMLSyntaxError):
        list(iter_xml_records(BytesIO(truncated), f"{{{NAMESPACE}}}record"))


def test_malformed_xml_raises_syntax_error() -> None:
    bad = b"<root><record><a></b></record></root>"
    with pytest.raises(etree.XMLSyntaxError, match="Opening and ending tag"):
        list(iter_xml_records(BytesIO(bad), "record"))


def test_external_general_entity_is_not_resolved() -> None:
    xml = b"""<?xml version="1.0"?>
    <!DOCTYPE root [
      <!ENTITY secret SYSTEM "file:///nonexistent/should-not-be-read.txt">
    ]>
    <root><record>&secret;</record></root>"""
    selectors = list(iter_xml_records(BytesIO(xml), "record"))
    assert len(selectors) == 1
    # The unresolved reference is kept as text instead of being fetched.
    assert "&secret;" in selectors[0].get()


def test_external_parameter_entity_is_not_loaded() -> None:
    xml = b"""<?xml version="1.0"?>
    <!DOCTYPE root [
      <!ENTITY % remote SYSTEM "file:///nonexistent/should-not-load.dtd">
      %remote;
    ]>
    <root><record>ok</record></root>"""
    selectors = list(iter_xml_records(BytesIO(xml), "record"))
    assert [selector.get() for selector in selectors] == ["<record>ok</record>"]


def test_dtd_entity_expansion_does_not_explode_memory() -> None:
    tracemalloc.start()
    before = tracemalloc.take_snapshot()
    xml = b"""<?xml version="1.0"?>
    <!DOCTYPE root [
      <!ENTITY lol "lol">
      <!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;">
      <!ENTITY lol3 "&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;">
    ]>
    <root><record>&lol3;</record></root>"""
    selectors = list(iter_xml_records(BytesIO(xml), "record"))
    assert len(selectors) == 1
    after = tracemalloc.take_snapshot()
    tracemalloc.stop()
    largest = max(stat.size_diff for stat in after.compare_to(before, "lineno"))
    assert largest < 1024 * 1024


def test_memory_does_not_grow_with_record_count() -> None:
    many_records = b"<root>" + b"<record><v>x</v></record>" * 5000 + b"</root>"
    tracemalloc.start()
    before = tracemalloc.take_snapshot()
    count = 0
    for selector in iter_xml_records(BytesIO(many_records), "record"):
        assert selector.xpath("v/text()").get() == "x"
        count += 1
    assert count == 5000
    after = tracemalloc.take_snapshot()
    tracemalloc.stop()
    # Streaming keeps only one record at a time plus a small parser buffer.
    largest = max(stat.size_diff for stat in after.compare_to(before, "lineno"))
    assert largest < 1024 * 1024


def test_text_stream_is_rejected() -> None:
    # lxml treats a str as a file name; either way it cannot be parsed as a
    # binary stream.
    with pytest.raises((TypeError, OSError)):
        list(
            iter_xml_records(
                BytesIO(COLLECTION).read().decode(),  # type: ignore[arg-type]
                f"{{{NAMESPACE}}}record",
            )
        )


@pytest.mark.parametrize(
    ("record", "expected"),
    [
        ("item", (None, "item")),
        ("{urn:x}item", ("urn:x", "item")),
        (("urn:x", "item"), ("urn:x", "item")),
        ((None, "item"), (None, "item")),
    ],
)
def test_normalize_record_tag(
    record: str | tuple[str | None, str], expected: tuple[str | None, str]
) -> None:
    assert _normalize_record_tag(record) == expected


@pytest.mark.parametrize("record", ["", "{}item", "{urn:x}", "{item"])
def test_invalid_record_name(record: str) -> None:
    with pytest.raises(ValueError):
        list(iter_xml_records(BytesIO(NO_NAMESPACE), record))


def test_streaming_tree_is_pruned_between_records() -> None:
    iterator = iter(iter_xml_records(BytesIO(COLLECTION), f"{{{NAMESPACE}}}record"))
    first = next(iterator)
    second = next(iterator)
    # Detached records have no parent and remain fully populated.
    assert first.root.getparent() is None
    assert second.root.getparent() is None
    assert first.xpath("@id").get() == "1"
    assert second.xpath("@id").get() == "2"
    list(iterator)
