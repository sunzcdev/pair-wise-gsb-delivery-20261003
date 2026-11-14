from __future__ import annotations

import io
import json

import pytest

import feedparser
from feedparser.exceptions import JSONFeedValidationError

VERSION = "https://jsonfeed.org/version/1.1"


def make_feed(items, **extra):
    doc = {"version": VERSION, "title": "Example", "items": items}
    doc.update(extra)
    return io.StringIO(json.dumps(doc))


def parse(items, **extra):
    return feedparser.parse(
        make_feed(items, **extra), tolerant_entries=True
    )


def enclosures(entry):
    return dict.__getitem__(entry, "enclosures")


def test_tolerant_mode_skips_bad_items_and_keeps_order():
    result = parse(
        [
            {"id": "one", "title": "First"},
            "not-an-object",
            {"id": "two", "url": 42},
            {"id": "three", "title": "Third"},
        ]
    )

    assert not result.bozo
    assert [entry.id for entry in result.entries] == ["one", "three"]
    assert result.skipped_entries == [
        {
            "index": 1,
            "path": "items/1",
            "reason": "a feed item must be an object",
        },
        {
            "index": 2,
            "path": "items/2/url",
            "reason": "the 'url' field must be a string",
        },
    ]


def test_source_index_is_preserved_after_skips():
    result = parse(
        [
            {"id": "a"},
            {"id": "bad", "title": 7},
            {"id": "b"},
            {"id": "also-bad", "summary": []},
            {"id": "c"},
        ]
    )

    assert [entry.id for entry in result.entries] == ["a", "b", "c"]
    assert [item["index"] for item in result.skipped_entries] == [1, 3]
    assert [item["path"] for item in result.skipped_entries] == [
        "items/1/title",
        "items/3/summary",
    ]


def test_boolean_is_not_a_valid_size():
    result = parse(
        [
            {
                "id": "bad",
                "attachments": [
                    {"url": "u", "mime_type": "m", "size_in_bytes": True}
                ],
            },
            {
                "id": "good",
                "attachments": [
                    {"url": "u", "mime_type": "m", "size_in_bytes": 5}
                ],
            },
        ]
    )

    assert [entry.id for entry in result.entries] == ["good"]
    assert result.skipped_entries[0] == {
        "index": 0,
        "path": "items/0/attachments/0/size_in_bytes",
        "reason": "the 'size_in_bytes' field must be an integer",
    }
    assert enclosures(result.entries[0])[0].length == 5


def test_integer_size_is_accepted():
    result = parse(
        [
            {
                "id": "good",
                "attachments": [
                    {"url": "u", "mime_type": "m", "size_in_bytes": 0}
                ],
            }
        ]
    )

    assert not result.bozo
    assert enclosures(result.entries[0])[0].length == 0


@pytest.mark.parametrize(
    "attachment, suffix",
    (
        ({"mime_type": "m"}, "attachments/0/url"),
        ({"url": "u"}, "attachments/0/mime_type"),
        ("not-an-object", "attachments/0"),
    ),
)
def test_bad_attachment_skips_the_whole_item(attachment, suffix):
    result = parse(
        [
            {"id": "bad", "attachments": [attachment]},
            {"id": "good"},
        ]
    )

    assert [entry.id for entry in result.entries] == ["good"]
    assert result.skipped_entries[0]["index"] == 0
    assert result.skipped_entries[0]["path"] == "items/0/" + suffix


def test_attachments_must_be_an_array():
    result = parse([{"id": "bad", "attachments": {}}])

    assert result.entries == []
    assert result.skipped_entries[0]["path"] == "items/0/attachments"


def test_bad_author_member_is_isolated_like_an_attachment():
    result = parse(
        [
            {"id": "bad", "authors": [{"name": "ok"}, "nope"]},
            {"id": "good", "author": {"name": "solo"}},
        ]
    )

    assert [entry.id for entry in result.entries] == ["good"]
    assert result.skipped_entries[0] == {
        "index": 0,
        "path": "items/0/authors/1",
        "reason": "an author must be an object",
    }
    assert result.entries[0].author == "solo"
    assert result.entries[0].authors == [{"name": "solo"}]


@pytest.mark.parametrize(
    "field, value, suffix",
    (
        ("author", "nope", "author/0"),
        ("authors", ["nope"], "authors/0"),
    ),
)
def test_feed_level_author_errors_are_not_ignored(field, value, suffix):
    result = feedparser.parse(
        make_feed([{"id": "a"}], **{field: value}),
        tolerant_entries=True,
    )

    assert result.bozo
    assert isinstance(result.bozo_exception, JSONFeedValidationError)
    assert result.bozo_exception.path == suffix
    assert result.entries == []
    assert result.skipped_entries == []


def test_missing_items_field_stops_parsing():
    doc = io.StringIO(json.dumps({"version": VERSION, "title": "no items"}))
    result = feedparser.parse(doc, tolerant_entries=True)

    assert result.bozo
    assert isinstance(result.bozo_exception, JSONFeedValidationError)
    assert result.bozo_exception.path == "items"


def test_items_field_must_be_an_array():
    doc = io.StringIO(
        json.dumps({"version": VERSION, "items": "not-an-array"})
    )
    result = feedparser.parse(doc, tolerant_entries=True)

    assert result.bozo
    assert result.bozo_exception.path == "items"


def test_top_level_value_must_be_an_object():
    result = feedparser.parse(
        io.StringIO("[1, 2, 3]"),
        response_headers={"content-type": "application/feed+json"},
        tolerant_entries=True,
    )

    assert result.bozo
    assert isinstance(result.bozo_exception, JSONFeedValidationError)
    assert result.bozo_exception.path == ""


def test_json_syntax_error_still_stops_parsing():
    result = feedparser.parse(
        io.StringIO('{"version": "x", "items": [}'),
        tolerant_entries=True,
    )

    assert result.bozo
    assert isinstance(result.bozo_exception, json.JSONDecodeError)
    assert result.entries == []


def test_default_mode_preserves_legacy_behavior():
    # Wrong field types are stored as-is; no validation diagnostics exist.
    result = feedparser.parse(
        make_feed(["not-an-object", {"id": "two", "url": 42}])
    )

    assert not result.bozo
    assert result.entries[1].link == 42
    assert result.skipped_entries == []


def test_default_mode_still_raises_on_missing_attachment_keys():
    result = feedparser.parse(
        make_feed([{"id": "a", "attachments": [{"mime_type": "m"}]}])
    )

    assert result.bozo
    assert isinstance(result.bozo_exception, KeyError)


def test_programming_errors_are_not_swallowed(monkeypatch):
    class ExplodingItem(dict):
        def __contains__(self, key):
            return True

        def __getitem__(self, key):
            raise RuntimeError("programming error")

    import feedparser.parsers.json as json_parser

    monkeypatch.setattr(json_parser.json, "load", lambda fp: fp.read())

    class FakeFile:
        def read(self):
            return {
                "version": VERSION,
                "items": [ExplodingItem()],
            }

    with pytest.raises(RuntimeError, match="programming error"):
        json_parser.JSONParser(tolerant_entries=True).feed(FakeFile())


def test_skipped_entries_diagnostic_is_exposed_for_json_feeds():
    result = parse([{"id": "good"}, {"id": "bad", "title": 9}])

    assert isinstance(result.skipped_entries, list)
    assert result.skipped_entries[0]["index"] == 1
    assert result.skipped_entries[0]["path"] == "items/1/title"


def test_valid_feed_reports_no_skipped_entries():
    result = parse(
        [
            {
                "id": "a",
                "title": "A",
                "authors": [
                    {"name": "n", "url": "mailto:a@example.com"}
                ],
                "attachments": [
                    {"url": "u", "mime_type": "m", "size_in_bytes": 1}
                ],
            }
        ]
    )

    assert not result.bozo
    assert result.skipped_entries == []
    entry = result.entries[0]
    assert entry.authors == [{"name": "n", "email": "a@example.com"}]
    assert entry.author_detail == {"name": "n", "email": "a@example.com"}
    assert enclosures(entry)[0] == {
        "href": "u",
        "type": "m",
        "length": 1,
    }
