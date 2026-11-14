from __future__ import annotations

import feedparser
from feedparser import select_content
from feedparser.content import (
    EXTERNAL_ONLY,
    NO_MATCHING_LANGUAGE,
    NO_MATCHING_MEDIA_TYPE,
    NO_REPRESENTATIONS,
)


def make_entry(*representations, **extra):
    entry = feedparser.FeedParserDict(extra)
    if representations:
        entry["content"] = [feedparser.FeedParserDict(r) for r in representations]
    return entry


def test_selects_most_preferred_media_type():
    entry = make_entry(
        {"type": "text/plain", "value": "plain"},
        {"type": "text/html", "value": "<b>html</b>"},
    )
    result = select_content(entry, media_types=["text/html", "text/plain"])
    assert result.found
    assert result.value == "<b>html</b>"
    assert result.index == 1
    assert result.media_type == "text/html"


def test_media_type_comparison_ignores_case_and_parameters():
    entry = make_entry({"type": "text/html; charset=utf-8", "value": "html"})
    result = select_content(entry, media_types=["Text/HTML"])
    assert result.found
    assert result.value == "html"


def test_media_type_shorthand_aliases_match_canonical_types():
    entry = make_entry({"type": "text/html", "value": "html"})
    result = select_content(entry, media_types=["html"])
    assert result.found
    assert result.media_type == "text/html"


def test_exact_language_beats_family_fallback():
    entry = make_entry(
        {"type": "text/html", "language": "zh", "value": "family"},
        {"type": "text/html", "language": "zh-CN", "value": "exact"},
    )
    result = select_content(entry, languages=["zh-CN"])
    assert result.found
    assert result.value == "exact"
    assert result.language_match == "exact"


def test_language_family_fallback_is_used_when_no_exact_match():
    entry = make_entry(
        {"type": "text/html", "language": "zh-CN", "value": "simplified"},
    )
    result = select_content(entry, languages=["zh"])
    assert result.found
    assert result.value == "simplified"
    assert result.language_match == "family"


def test_language_preference_order_is_respected():
    entry = make_entry(
        {"type": "text/html", "language": "de", "value": "german"},
        {"type": "text/html", "language": "fr", "value": "french"},
    )
    result = select_content(entry, languages=["fr", "de"])
    assert result.found
    assert result.value == "french"


def test_language_tags_compare_case_insensitively():
    entry = make_entry(
        {"type": "text/html", "language": "ZH_cn", "value": "simplified"},
    )
    result = select_content(entry, languages=["zh-CN"])
    assert result.found
    assert result.language_match == "exact"


def test_untagged_language_is_rejected_by_default():
    entry = make_entry({"type": "text/html", "value": "untagged"})
    result = select_content(entry, languages=["en"])
    assert not result.found
    assert result.reason == NO_MATCHING_LANGUAGE
    assert result.value is None


def test_untagged_language_is_last_resort_when_allowed():
    entry = make_entry(
        {"type": "text/html", "value": "untagged"},
        {"type": "text/html", "language": "en", "value": "tagged"},
    )
    result = select_content(entry, languages=["en"], allow_untagged_language=True)
    assert result.found
    assert result.value == "tagged"

    result = select_content(entry, languages=["de"], allow_untagged_language=True)
    assert result.found
    assert result.value == "untagged"
    assert result.language_match == "untagged"


def test_equal_priority_is_stable_by_source_order():
    entry = make_entry(
        {"type": "text/html", "language": "en", "value": "first"},
        {"type": "text/html", "language": "en", "value": "second"},
    )
    result = select_content(entry, languages=["en"])
    assert result.found
    assert result.value == "first"
    assert result.index == 0


def test_external_src_is_not_treated_as_inline_content():
    entry = make_entry(
        {
            "type": "text/html",
            "src": "http://example.com/content",
            "value": "",
        },
    )
    result = select_content(entry)
    assert not result.found
    assert result.reason == EXTERNAL_ONLY


def test_external_src_is_selectable_only_when_explicitly_allowed():
    entry = make_entry(
        {
            "type": "text/html",
            "src": "http://example.com/content",
            "value": "",
        },
    )
    result = select_content(entry, allow_external=True)
    assert result.found
    assert result.external
    assert result.src == "http://example.com/content"
    assert not result.value


def test_missing_content_is_a_distinguishable_no_result():
    entry = make_entry(summary="a summary, not content")
    result = select_content(entry)
    assert not result.found
    assert result.reason == NO_REPRESENTATIONS
    assert result.representation is None
    assert result.value is None


def test_summary_is_never_substituted_for_content():
    entry = make_entry(
        {"type": "application/pdf", "value": "pdf"},
        summary="a summary, not content",
    )
    result = select_content(entry, media_types=["text/html"])
    assert not result.found
    assert result.reason == NO_MATCHING_MEDIA_TYPE
    assert result.value != "a summary, not content"


def test_all_raw_representations_are_preserved():
    entry = make_entry(
        {"type": "text/plain", "value": "plain"},
        {"type": "text/html", "value": "html"},
    )
    result = select_content(entry, media_types=["text/html"])
    assert [r["value"] for r in result.representations] == ["plain", "html"]
    assert [c["value"] for c in entry["content"]] == ["plain", "html"]


def test_json_feed_style_single_content_dict():
    entry = feedparser.FeedParserDict(
        {"content": {"type": "text", "value": "json text"}}
    )
    result = select_content(entry, media_types=["text/plain"])
    assert result.found
    assert result.value == "json text"


def test_selection_is_falsey_when_nothing_found():
    entry = make_entry()
    assert not select_content(entry)


def test_selects_from_a_parsed_atom_entry():
    xml = """<?xml version="1.0" encoding="utf-8"?>
    <feed xmlns="http://www.w3.org/2005/Atom" xml:lang="en">
      <title>Example</title>
      <entry>
        <title>Item</title>
        <content type="text" xml:lang="fr">bonjour</content>
        <content type="html" xml:lang="en">&lt;b&gt;hello&lt;/b&gt;</content>
      </entry>
    </feed>
    """
    entry = feedparser.parse(xml).entries[0]
    assert len(entry["content"]) == 2

    result = select_content(entry, media_types=["text/html"], languages=["en"])
    assert result.found
    assert result.value == "<b>hello</b>"
    assert result.media_type == "text/html"
    assert result.language_match == "exact"
    # The parsed entry keeps every raw representation.
    assert len(result.representations) == 2
    assert len(entry["content"]) == 2
