import feedparser

FEED = """<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0" xmlns:x="http://example.com/x">
<channel>
<title>Test feed</title>
<link>http://example.com/</link>
<x:rating scheme="stars">4</x:rating>
<item>
<title>Item 1</title>
<link>http://example.com/1</link>
<x:meta>
<x:tag>alpha</x:tag>
<x:tag>beta</x:tag>
<x:note xmlns:x="http://example.com/other">rebound</x:note>
</x:meta>
</item>
</channel>
</rss>"""


def test_disabled_by_default():
    result = feedparser.parse(FEED)
    assert "extensions" not in result.feed
    assert "extensions" not in result.entries[0]
    assert "extension_diagnostics" not in result
    # The legacy flat fields are still produced.
    assert result.feed.x_rating == {"scheme": "stars"}


def test_basic_structure():
    result = feedparser.parse(FEED, preserve_extensions=True)
    assert result.extension_diagnostics == []

    (rating,) = result.feed.extensions
    assert rating.name == "rating"
    assert rating.namespace == "http://example.com/x"
    assert rating.attributes == {"scheme": "stars"}
    assert rating.text == "4"
    assert rating.children == []

    (meta,) = result.entries[0].extensions
    assert meta.name == "meta"
    assert meta.namespace == "http://example.com/x"


def test_repeated_children_keep_document_order():
    result = feedparser.parse(FEED, preserve_extensions=True)
    meta = result.entries[0].extensions[0]
    tags = [child for child in meta.children if child.name == "tag"]
    assert [tag.text for tag in tags] == ["alpha", "beta"]


def test_same_prefix_different_uris_not_mixed():
    result = feedparser.parse(FEED, preserve_extensions=True)
    meta = result.entries[0].extensions[0]
    note = meta.children[-1]
    assert note.name == "note"
    assert note.namespace == "http://example.com/other"
    assert note.text == "rebound"
    # Siblings keep the outer binding.
    assert meta.children[0].namespace == "http://example.com/x"


def test_builtin_fields_unchanged():
    plain = feedparser.parse(FEED)
    preserved = feedparser.parse(FEED, preserve_extensions=True)
    assert preserved.feed.title == plain.feed.title == "Test feed"
    assert preserved.feed.link == plain.feed.link == "http://example.com/"
    assert preserved.entries[0].title == plain.entries[0].title == "Item 1"
    # The legacy flat fields are still produced when preservation is on.
    assert preserved.feed.x_rating == {"scheme": "stars"}


def test_depth_limit():
    xml = """<?xml version="1.0" encoding="utf-8"?>
    <rss version="2.0" xmlns:x="http://example.com/x">
    <channel>
    <x:a><x:b><x:c>deep</x:c></x:b></x:a>
    </channel>
    </rss>"""
    result = feedparser.parse(xml, preserve_extensions=True, extension_max_depth=2)
    (a,) = result.feed.extensions
    (b,) = a.children
    assert b.name == "b"
    assert b.children == []
    assert "c" not in b.text
    assert len(result.extension_diagnostics) == 1
    assert "depth limit of 2" in result.extension_diagnostics[0]


def test_text_limit():
    xml = """<?xml version="1.0" encoding="utf-8"?>
    <rss version="2.0" xmlns:x="http://example.com/x">
    <channel>
    <x:a>0123456789</x:a>
    <x:b>more text</x:b>
    </channel>
    </rss>"""
    result = feedparser.parse(
        xml, preserve_extensions=True, extension_max_text_length=5
    )
    a, b = result.feed.extensions
    assert a.text == "01234"
    assert b.text == ""
    assert len(result.extension_diagnostics) == 1
    assert "text limit of 5" in result.extension_diagnostics[0]


def test_handled_elements_inside_extensions_are_not_duplicated():
    xml = """<?xml version="1.0" encoding="utf-8"?>
    <rss version="2.0" xmlns:x="http://example.com/x"
         xmlns:dc="http://purl.org/dc/elements/1.1/">
    <channel>
    <x:wrapper>before<dc:date>2020-01-01</dc:date>after</x:wrapper>
    </channel>
    </rss>"""
    result = feedparser.parse(xml, preserve_extensions=True)
    (wrapper,) = result.feed.extensions
    # The handled dc:date element is consumed by the built-in handler;
    # its text is not attributed to the extension element.
    assert "2020-01-01" not in wrapper.text
    assert result.feed.updated == "2020-01-01"


def test_loose_parser(use_loose_parser):
    result = feedparser.parse(FEED, preserve_extensions=True)
    assert result.extension_diagnostics == []
    (rating,) = result.feed.extensions
    assert rating.namespace == "http://example.com/x"
    assert rating.attributes == {"scheme": "stars"}
    meta = result.entries[0].extensions[0]
    assert [child.text for child in meta.children[:2]] == ["alpha", "beta"]
    assert meta.children[2].namespace == "http://example.com/other"


def test_no_external_entity_read():
    xml = """<?xml version="1.0" encoding="utf-8"?>
    <!DOCTYPE rss [
    <!ENTITY xxe SYSTEM "file:///etc/hostname">
    <!ENTITY safe "expanded">
    ]>
    <rss version="2.0" xmlns:x="http://example.com/x">
    <channel>
    <x:a>&safe;</x:a>
    <x:b>&xxe;</x:b>
    </channel>
    </rss>"""
    result = feedparser.parse(xml, preserve_extensions=True)
    text = repr(result.feed.get("extensions", []))
    assert "file:///etc/hostname" not in text


def test_module_level_default(monkeypatch):
    monkeypatch.setattr(feedparser, "PRESERVE_EXTENSIONS", 1)
    result = feedparser.parse(FEED)
    assert result.feed.extensions[0].name == "rating"
