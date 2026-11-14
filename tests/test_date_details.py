import datetime
import json

import pytest

import feedparser
from feedparser import FeedParserDict
from feedparser.datetimes import _parse_date, _parse_date_detailed

UTC = datetime.timezone.utc


def make_rss(date):
    return f"""<?xml version="1.0"?>
    <rss version="2.0">
      <channel>
        <title>Example</title>
        <link>http://example.com/</link>
        <description>Example</description>
        <item><title>Item</title><pubDate>{date}</pubDate></item>
      </channel>
    </rss>"""


def make_rss_dc(date):
    return f"""<?xml version="1.0"?>
    <rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/">
      <channel>
        <title>Example</title>
        <link>http://example.com/</link>
        <description>Example</description>
        <item><title>Item</title><dc:date>{date}</dc:date></item>
      </channel>
    </rss>"""


def make_atom(published):
    return f"""<?xml version="1.0"?>
    <feed xmlns="http://www.w3.org/2005/Atom">
      <title>Example</title>
      <entry><title>Item</title><published>{published}</published></entry>
    </feed>"""


def make_json_feed(**item_dates):
    item = {"id": "1", "title": "Item"}
    item.update(item_dates)
    return json.dumps(
        {
            "version": "https://jsonfeed.org/version/1.1",
            "title": "Example",
            "items": [item],
        }
    )


def test_date_only_is_not_treated_as_second_precision():
    # RSS entry point: a date-only value must not look like a timestamp.
    result = feedparser.parse(make_rss("2026-11-14"))
    detail = result.entries[0].published_parsed_detail
    assert detail.raw == "2026-11-14"
    assert detail.precision == "date"
    assert detail.timezone_explicit is False
    assert detail.utc is None
    assert detail.error is None
    # The legacy 9-tuple is still available.
    assert tuple(result.entries[0].published_parsed)[:6] == (2026, 11, 14, 0, 0, 0)
    assert detail.parsed == result.entries[0].published_parsed


def test_dc_date_entry_point():
    result = feedparser.parse(make_rss_dc("2026-11-14"))
    detail = result.entries[0].updated_parsed_detail
    assert detail.precision == "date"
    assert detail.utc is None
    assert detail.error is None


def test_naive_time_does_not_borrow_local_timezone():
    # Atom entry point: no timezone declared, so no UTC value may be produced.
    result = feedparser.parse(make_atom("2026-11-14T10:20:30"))
    detail = result.entries[0].published_parsed_detail
    assert detail.precision == "second"
    assert detail.timezone_explicit is False
    assert detail.utc is None
    assert detail.error is None


def test_explicit_utc_timezone_is_normalized():
    result = feedparser.parse(make_atom("2026-11-14T10:20:30Z"))
    detail = result.entries[0].published_parsed_detail
    assert detail.precision == "second"
    assert detail.timezone_explicit is True
    assert detail.utc == datetime.datetime(2026, 11, 14, 10, 20, 30, tzinfo=UTC)
    assert detail.error is None


def test_explicit_numeric_offset_is_normalized():
    result = feedparser.parse(make_atom("2026-11-14T10:20:30+02:00"))
    detail = result.entries[0].published_parsed_detail
    assert detail.timezone_explicit is True
    assert detail.utc == datetime.datetime(2026, 11, 14, 8, 20, 30, tzinfo=UTC)


def test_minute_precision():
    result = feedparser.parse(make_atom("2026-11-14T10:20Z"))
    detail = result.entries[0].published_parsed_detail
    assert detail.precision == "minute"
    assert detail.utc == datetime.datetime(2026, 11, 14, 10, 20, tzinfo=UTC)


@pytest.mark.parametrize(
    "text, microsecond",
    (
        ("2026-11-14T10:20:30.123456Z", 123456),
        ("2026-11-14T10:20:30.5Z", 500000),
        ("2026-11-14T10:20:30.25+05:30", 250000),
    ),
)
def test_fractional_seconds_are_distinguished(text, microsecond):
    result = feedparser.parse(make_atom(text))
    detail = result.entries[0].published_parsed_detail
    assert detail.precision == "subsecond"
    assert detail.timezone_explicit is True
    assert detail.utc is not None
    assert detail.utc.microsecond == microsecond


def test_naive_fractional_seconds_have_no_utc_value():
    # MSSQL-style value with fractional seconds but no timezone.
    result = feedparser.parse(make_rss("2026-11-14 10:20:30.25"))
    detail = result.entries[0].published_parsed_detail
    assert detail.precision == "subsecond"
    assert detail.timezone_explicit is False
    assert detail.utc is None
    assert detail.error is None


def test_rfc822_named_timezone():
    result = feedparser.parse(make_rss("Sat, 14 Nov 2026 10:20:30 GMT"))
    detail = result.entries[0].published_parsed_detail
    assert detail.precision == "second"
    assert detail.timezone_explicit is True
    assert detail.utc == datetime.datetime(2026, 11, 14, 10, 20, 30, tzinfo=UTC)


def test_rfc822_numeric_offset():
    result = feedparser.parse(make_rss("Sat, 14 Nov 2026 10:20:30 -0500"))
    detail = result.entries[0].published_parsed_detail
    assert detail.timezone_explicit is True
    assert detail.utc == datetime.datetime(2026, 11, 14, 15, 20, 30, tzinfo=UTC)


def test_rfc822_without_timezone():
    result = feedparser.parse(make_rss("14 Nov 2026 10:20:30"))
    detail = result.entries[0].published_parsed_detail
    assert detail.precision == "second"
    assert detail.timezone_explicit is False
    assert detail.utc is None
    assert detail.error is None


def test_asctime_named_timezone():
    result = feedparser.parse(make_rss("Sat Nov 14 10:20:30 PST 2026"))
    detail = result.entries[0].published_parsed_detail
    assert detail.timezone_explicit is True
    assert detail.utc == datetime.datetime(2026, 11, 14, 18, 20, 30, tzinfo=UTC)


def test_invalid_leap_day_is_not_corrected():
    # 2023 is not a leap year; the value must not become another date.
    result = feedparser.parse(make_atom("2023-02-29"))
    entry = result.entries[0]
    detail = entry.published_parsed_detail
    assert detail.raw == "2023-02-29"
    assert detail.utc is None
    assert detail.error is not None
    # The original text is still in the result.
    assert entry.published == "2023-02-29"


def test_valid_leap_day():
    result = feedparser.parse(make_atom("2024-02-29"))
    detail = result.entries[0].published_parsed_detail
    assert detail.error is None
    assert detail.precision == "date"


@pytest.mark.parametrize(
    "text",
    (
        "2026-13-14",  # month out of range
        "2026-11-40",  # day out of range
        "2026-11-14T25:20:30Z",  # hour out of range
        "2026-11-14T10:61:30Z",  # minute out of range
        "2026-11-14T10:20:61Z",  # second out of range
        "2023-366",  # ordinal day out of range for a non-leap year
    ),
)
def test_out_of_range_values_are_not_corrected(text):
    result = feedparser.parse(make_atom(text))
    entry = result.entries[0]
    detail = entry.published_parsed_detail
    assert detail.raw == text
    assert detail.utc is None
    assert detail.error is not None
    assert entry.published == text


def test_unrecognized_date():
    result = feedparser.parse(make_atom("not a date"))
    entry = result.entries[0]
    detail = entry.published_parsed_detail
    assert detail.raw == "not a date"
    assert detail.parsed is None
    assert detail.utc is None
    assert detail.error is not None
    assert entry.published == "not a date"


def test_json_feed_date_only():
    # JSON Feed entry point.
    result = feedparser.parse(make_json_feed(date_published="2026-11-14"))
    detail = result.entries[0].published_parsed_detail
    assert detail.raw == "2026-11-14"
    assert detail.precision == "date"
    assert detail.timezone_explicit is False
    assert detail.utc is None
    assert detail.error is None
    assert tuple(result.entries[0].published_parsed)[:6] == (2026, 11, 14, 0, 0, 0)


def test_json_feed_timestamp_with_timezone():
    result = feedparser.parse(make_json_feed(date_modified="2026-11-14T10:20:30.5Z"))
    detail = result.entries[0].updated_parsed_detail
    assert detail.precision == "subsecond"
    assert detail.timezone_explicit is True
    assert detail.utc == datetime.datetime(2026, 11, 14, 10, 20, 30, 500000, tzinfo=UTC)


def test_json_feed_invalid_date():
    result = feedparser.parse(make_json_feed(date_published="2023-02-29"))
    detail = result.entries[0].published_parsed_detail
    assert detail.raw == "2023-02-29"
    assert detail.utc is None
    assert detail.error is not None


def test_feed_level_updated_detail():
    result = feedparser.parse(make_rss("2026-11-14"))
    # The same value appears at the feed level via lastBuildDate-like
    # mappings only when present; check the channel-level element directly.
    assert "updated_parsed_detail" not in result.feed
    feed_with_date = feedparser.parse(
        make_rss("2026-11-14").replace(
            "<description>Example</description>",
            "<description>Example</description>"
            "<lastBuildDate>Sat, 14 Nov 2026 10:20:30 GMT</lastBuildDate>",
        )
    )
    detail = feed_with_date.feed.updated_parsed_detail
    assert detail.utc == datetime.datetime(2026, 11, 14, 10, 20, 30, tzinfo=UTC)


def test_keymap_aliases():
    mapping = FeedParserDict({"updated_parsed_detail": 1, "published_parsed_detail": 2})
    assert mapping["date_parsed_detail"] == 1
    assert mapping["modified_parsed_detail"] == 1
    assert mapping["issued_parsed_detail"] == 2


def test_entry_keymap_aliases():
    result = feedparser.parse(make_atom("2026-11-14T10:20:30Z"))
    entry = result.entries[0]
    assert entry["issued_parsed_detail"] is entry["published_parsed_detail"]


def test_empty_value():
    detail = _parse_date_detailed("")
    assert detail.error is not None
    assert detail.utc is None
    detail = _parse_date_detailed(None)
    assert detail.raw is None
    assert detail.error is not None


def test_legacy_parse_date_is_unchanged():
    # The legacy 9-tuple behavior is retained as-is.
    assert _parse_date("2026-11-14")[:6] == (2026, 11, 14, 0, 0, 0)
    assert _parse_date("not a date") is None
