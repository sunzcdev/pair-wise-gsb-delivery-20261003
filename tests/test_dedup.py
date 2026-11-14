import time

import feedparser
from feedparser.dedup import deduplicate_entries
from feedparser.util import FeedParserDict


def _time(year, month=1, day=1):
    return time.struct_time((year, month, day, 0, 0, 0, 0, 1, 0))


def test_entries_without_identity_are_kept_as_is():
    entries = [
        FeedParserDict(title="a"),
        FeedParserDict(title="a"),
        FeedParserDict(summary="no id either"),
    ]
    result = deduplicate_entries(entries)
    assert result["entries"] == entries
    assert len(result["entries"]) == 3
    assert result["conflicts"] == []


def test_guid_is_used_as_identity():
    entries = [
        FeedParserDict(guid="same", updated_parsed=_time(2024)),
        FeedParserDict(id="same", updated_parsed=_time(2025)),
    ]
    result = deduplicate_entries(entries)
    assert result["entries"] == [entries[1]]
    assert result["conflicts"] == []


def test_distinct_ids_are_not_merged():
    entries = [
        FeedParserDict(id="one", title="x"),
        FeedParserDict(id="two", title="x"),
    ]
    result = deduplicate_entries(entries)
    assert result["entries"] == entries
    assert result["conflicts"] == []


def test_newer_update_time_wins():
    entries = [
        FeedParserDict(id="x", title="old", updated_parsed=_time(2024)),
        FeedParserDict(id="y", title="other", updated_parsed=_time(2019)),
        FeedParserDict(id="x", title="new", updated_parsed=_time(2025)),
    ]
    result = deduplicate_entries(entries)
    # The newer "x" entry occupies the position of its first occurrence.
    assert result["entries"] == [entries[2], entries[1]]
    assert result["entries"][0] is entries[2]
    assert result["conflicts"] == []


def test_equal_times_with_identical_content_keep_first():
    entries = [
        FeedParserDict(id="x", title="same", updated_parsed=_time(2024)),
        FeedParserDict(id="x", title="same", updated_parsed=_time(2024)),
    ]
    result = deduplicate_entries(entries)
    assert result["entries"] == [entries[0]]
    assert result["conflicts"] == []


def test_equal_times_with_conflicting_content_record_conflict():
    entries = [
        FeedParserDict(id="x", title="first", updated_parsed=_time(2024)),
        FeedParserDict(id="x", title="second", updated_parsed=_time(2024)),
        FeedParserDict(id="x", title="third", updated_parsed=_time(2024)),
    ]
    result = deduplicate_entries(entries)
    # The first occurrence is kept; the last one is not silently picked.
    assert result["entries"] == [entries[0]]
    assert len(result["conflicts"]) == 1
    conflict = result["conflicts"][0]
    assert conflict["id"] == "x"
    assert conflict["kept_index"] == 0
    assert conflict["indices"] == [0, 1, 2]


def test_missing_times_with_conflicting_content_record_conflict():
    entries = [
        FeedParserDict(id="x", title="first"),
        FeedParserDict(id="x", title="second"),
    ]
    result = deduplicate_entries(entries)
    assert result["entries"] == [entries[0]]
    assert result["conflicts"][0]["indices"] == [0, 1]


def test_missing_time_does_not_override_known_time():
    entries = [
        FeedParserDict(id="x", title="timed", updated_parsed=_time(2024)),
        FeedParserDict(id="x", title="timed"),
    ]
    result = deduplicate_entries(entries)
    assert result["entries"] == [entries[0]]
    assert result["conflicts"] == []


def test_input_entries_are_not_modified():
    entries = [
        FeedParserDict(id="x", title="first"),
        FeedParserDict(id="x", title="second"),
    ]
    snapshot = [dict(entry) for entry in entries]
    deduplicate_entries(entries)
    assert [dict(entry) for entry in entries] == snapshot
    assert len(entries) == 2


def test_view_entries_are_original_entries_not_merged_copies():
    entries = [
        FeedParserDict(id="x", title="new", updated_parsed=_time(2025)),
        FeedParserDict(id="x", title="old", updated_parsed=_time(2024)),
    ]
    result = deduplicate_entries(entries)
    assert result["entries"][0] is entries[0]


def test_parse_result_contains_dedup_view():
    document = b"""
    <rss version="2.0">
      <channel>
        <title>Example</title>
        <link>http://example.com/</link>
        <description>Example</description>
        <item>
          <guid>item-1</guid>
          <title>First version</title>
        </item>
        <item>
          <guid>item-2</guid>
          <title>Other item</title>
        </item>
        <item>
          <guid>item-1</guid>
          <title>Second version</title>
        </item>
      </channel>
    </rss>
    """
    result = feedparser.parse(document)
    # The original parse result is untouched.
    assert len(result["entries"]) == 3
    # The dedup view collapses the duplicate identity at its first position.
    assert [entry["id"] for entry in result["dedup"]["entries"]] == [
        "item-1",
        "item-2",
    ]
    assert result["dedup"]["entries"][0] is result["entries"][0]
    # The conflicting versions are recorded with their original indices.
    assert len(result["dedup"]["conflicts"]) == 1
    conflict = result["dedup"]["conflicts"][0]
    assert conflict["id"] == "item-1"
    assert conflict["kept_index"] == 0
    assert conflict["indices"] == [0, 2]


def test_parse_empty_document_has_empty_dedup_view():
    result = feedparser.parse(b"")
    assert result["dedup"]["entries"] == []
    assert result["dedup"]["conflicts"] == []
