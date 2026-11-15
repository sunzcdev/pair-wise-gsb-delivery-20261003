"""Tests for the explicit provenance mode of Selector."""

from __future__ import annotations

import pytest
from lxml import etree

from parsel import Provenance, Selector

HTML = """<html>
<body>
<div class="item">
  <a href="/1">one</a>
</div>
<div class="item">
  <a href="/2">one</a>
</div>
</body>
</html>"""


class TestProvenanceMode:
    def test_off_by_default(self) -> None:
        sel = Selector(text=HTML)
        assert sel.provenance is None
        assert sel.xpath("//a")[0].provenance is None
        assert sel.css("a")[0].provenance is None
        assert sel.xpath("//a/text()")[0].provenance is None

    def test_root_provenance(self) -> None:
        sel = Selector(text=HTML, provenance=True)
        prov = sel.provenance
        assert isinstance(prov, Provenance)
        assert prov.kind == "element"
        assert prov.line == 1
        assert prov.path == "/html"
        assert prov.query_chain == ()
        assert prov.unknown_reason is None
        assert not prov.is_unknown

    def test_node_results(self) -> None:
        sel = Selector(text=HTML, provenance=True)
        first, second = sel.xpath("//a")
        assert first.provenance.kind == "element"
        assert first.provenance.line == 4
        assert first.provenance.path == "/html/body/div[1]/a"
        assert first.provenance.query_chain == ("//a",)
        assert second.provenance.line == 7
        assert second.provenance.path == "/html/body/div[2]/a"

    def test_query_chain_accumulates(self) -> None:
        sel = Selector(text=HTML, provenance=True)
        link = sel.css("div.item").xpath(".//a")[0]
        assert link.provenance.query_chain == ("div.item", ".//a")
        text = link.xpath("./text()")[0]
        assert text.provenance.query_chain == ("div.item", ".//a", "./text()")

    def test_css_chain_keeps_css_query(self) -> None:
        sel = Selector(text=HTML, provenance=True)
        link = sel.css("div.item > a")[0]
        assert link.provenance.query_chain == ("div.item > a",)

    def test_attribute_results_point_to_owner(self) -> None:
        sel = Selector(text=HTML, provenance=True)
        first, second = sel.xpath("//a/@href")
        assert first.get() == "/1"
        assert first.provenance.kind == "attribute"
        assert first.provenance.attribute == "href"
        assert first.provenance.line == 4
        assert first.provenance.path == "/html/body/div[1]/a"
        assert second.provenance.path == "/html/body/div[2]/a"

    def test_text_results_point_to_owner(self) -> None:
        sel = Selector(text=HTML, provenance=True)
        first = sel.xpath("//a/text()")[0]
        assert first.get() == "one"
        assert first.provenance.kind == "text"
        assert first.provenance.line == 4
        assert first.provenance.path == "/html/body/div[1]/a"

    def test_same_value_from_different_nodes_is_not_merged(self) -> None:
        sel = Selector(text=HTML, provenance=True)
        texts = sel.xpath("//a/text()")
        assert texts.getall() == ["one", "one"]
        first, second = (t.provenance for t in texts)
        assert first is not second
        assert first != second
        assert len({first, second}) == 2
        assert first.path != second.path

    def test_slice_keeps_provenance(self) -> None:
        sel = Selector(text=HTML, provenance=True)
        links = sel.xpath("//a")
        sliced = links[0:1]
        assert sliced[0].provenance is links[0].provenance
        assert sliced[0].provenance.path == "/html/body/div[1]/a"

    def test_selectorlist_flattening_keeps_provenance(self) -> None:
        sel = Selector(text=HTML, provenance=True)
        links = sel.css("div.item").xpath(".//a")
        assert [link.provenance.path for link in links] == [
            "/html/body/div[1]/a",
            "/html/body/div[2]/a",
        ]
        assert all(
            link.provenance.query_chain == ("div.item", ".//a") for link in links
        )

    def test_get_and_getall_unchanged(self) -> None:
        tracked = Selector(text=HTML, provenance=True)
        plain = Selector(text=HTML)
        for query in ("//a", "//a/text()", "//a/@href"):
            assert tracked.xpath(query).getall() == plain.xpath(query).getall()
            assert tracked.xpath(query).get() == plain.xpath(query).get()
        assert tracked.css("div.item").getall() == plain.css("div.item").getall()

    def test_manual_root_is_marked_unknown(self) -> None:
        root = etree.fromstring("<doc><a>x</a></doc>")
        sel = Selector(root=root, provenance=True)
        prov = sel.provenance
        assert prov.is_unknown
        assert prov.line is None
        assert prov.path is None
        assert prov.unknown_reason is not None

    def test_json_is_marked_unknown(self) -> None:
        sel = Selector(text='{"a": [1, 2]}', provenance=True)
        assert sel.type == "json"
        prov = sel.provenance
        assert prov.is_unknown
        assert prov.kind == "json"
        assert prov.unknown_reason is not None

    def test_jmespath_results_are_marked_unknown(self) -> None:
        sel = Selector(text='{"a": [1, 2]}', provenance=True)
        values = sel.jmespath("a")
        assert values.getall() == [1, 2]
        for value in values:
            assert value.provenance.kind == "json"
            assert value.provenance.is_unknown
            assert value.provenance.unknown_reason is not None
            assert value.provenance.query_chain == ("a",)

    def test_jsonpath_results_are_marked_unknown(self) -> None:
        pytest.importorskip("jsonpath_rfc9535")
        sel = Selector(text='{"a": [1, 2]}', provenance=True)
        values = sel.jsonpath("$.a[*]")
        assert values.getall() == [1, 2]
        for value in values:
            assert value.provenance.kind == "json"
            assert value.provenance.is_unknown
            assert value.provenance.query_chain == ("$.a[*]",)

    def test_non_node_results_are_marked_unknown(self) -> None:
        sel = Selector(text=HTML, provenance=True)
        count = sel.xpath("count(//a)")[0]
        assert count.get() == "2.0"
        assert count.provenance.kind == "unknown"
        assert count.provenance.is_unknown
        assert count.provenance.unknown_reason is not None

    def test_provenance_is_a_snapshot_at_extraction_time(self) -> None:
        sel = Selector(text=HTML, provenance=True)
        link = sel.xpath("//a")[0]
        prov = link.provenance
        assert prov.path == "/html/body/div[1]/a"
        link.drop()
        # The node is gone from the document, but the provenance record is
        # the snapshot taken at extraction time: it is neither updated nor
        # repointed to another node.
        assert link.provenance is prov
        assert link.provenance.path == "/html/body/div[1]/a"
        assert link.provenance.line == 4
        # A new query sees the mutated document and gets its own snapshot;
        # the old record is not repointed to it.
        remaining = sel.xpath("//a")[0]
        assert remaining.provenance.path == "/html/body/div[2]/a"
        assert link.provenance.path == "/html/body/div[1]/a"

    def test_provenance_does_not_copy_the_dom(self) -> None:
        sel = Selector(text=HTML, provenance=True)
        prov = sel.xpath("//a")[0].provenance
        for value in vars(prov).values():
            assert not isinstance(value, etree._Element)

    def test_immutable(self) -> None:
        sel = Selector(text=HTML, provenance=True)
        prov = sel.xpath("//a")[0].provenance
        with pytest.raises(AttributeError):
            prov.line = 10  # type: ignore[misc]

    def test_xml_type(self) -> None:
        xml = "<root>\n  <item>1</item>\n  <item>2</item>\n</root>"
        sel = Selector(text=xml, type="xml", provenance=True)
        items = sel.xpath("//item")
        assert [item.provenance.line for item in items] == [2, 3]
        assert [item.provenance.path for item in items] == [
            "/root/item[1]",
            "/root/item[2]",
        ]
