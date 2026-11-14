from w3lib.robots import (
    RobotsRules,
    UnknownDirective,
    robots_directives,
)


def _names(rules: RobotsRules) -> list[str]:
    return [directive.name for directive in rules.directives]


def _by_name(rules: RobotsRules, name: str):
    directive = rules.get(name)
    assert directive is not None
    return directive


class TestRobotsDirectivesMeta:
    def test_empty_inputs(self):
        rules = robots_directives("", None, "bot")
        assert rules.crawler == "bot"
        assert rules.directives == ()
        assert rules.unknown == ()
        assert rules.indexable is True
        assert rules.followable is True
        assert rules.get("noindex") is None

    def test_basic_meta_directives_and_source(self):
        page = '<meta name="robots" content="noindex, nofollow">'
        rules = robots_directives(page, None, "bot")
        assert _names(rules) == ["noindex", "nofollow"]
        noindex = _by_name(rules, "noindex")
        assert noindex.value is None
        assert noindex.source.location == "meta"
        assert noindex.source.target == "robots"
        assert noindex.source.raw == "noindex"
        assert noindex.source.declaration == "noindex, nofollow"
        assert noindex.source.meta_index == 0
        assert noindex.source.header_index is None
        assert rules.indexable is False
        assert rules.followable is False

    def test_meta_is_case_insensitive_for_tag_and_tokens(self):
        page = '<META NAME="Robots" CONTENT="NOINDEX, NoArchive">'
        rules = robots_directives(page, None, "bot")
        assert _names(rules) == ["noindex", "noarchive"]
        # The token is stored as written, the canonical name is lowercased.
        assert _by_name(rules, "noarchive").source.raw == "NoArchive"

    def test_whitespace_and_empty_tokens_are_tolerated(self):
        page = '<meta name="robots" content=" noindex , , nofollow  ">'
        rules = robots_directives(page, None, "bot")
        assert _names(rules) == ["noindex", "nofollow"]

    def test_meta_without_content_declares_nothing(self):
        rules = robots_directives(
            '<meta name="robots">', None, "bot"
        )
        assert rules.directives == ()

    def test_non_robots_meta_is_ignored(self):
        page = '<meta name="description" content="noindex">'
        rules = robots_directives(page, None, "bot")
        assert rules.directives == ()

    def test_bytes_input(self):
        page = b'<meta name="robots" content="noindex">'
        rules = robots_directives(page, None, "bot")
        assert _names(rules) == ["noindex"]

    def test_entities_in_attribute_values_are_decoded(self):
        page = '<meta name="robots" content="noindex&#44; nofollow">'
        rules = robots_directives(page, None, "bot")
        assert _names(rules) == ["noindex", "nofollow"]


class TestShorthandExpansion:
    def test_none_expands_to_noindex_nofollow(self):
        rules = robots_directives(
            '<meta name="robots" content="none">', None, "bot"
        )
        assert _names(rules) == ["noindex", "nofollow"]
        for directive in rules.directives:
            assert directive.source.raw == "none"

    def test_all_expands_to_index_follow(self):
        rules = robots_directives(
            '<meta name="robots" content="all">', None, "bot"
        )
        assert _names(rules) == ["index", "follow"]

    def test_none_then_index_keeps_stricter_noindex(self):
        rules = robots_directives(
            '<meta name="robots" content="none, index, follow">', None, "bot"
        )
        assert _names(rules) == ["noindex", "nofollow"]

    def test_noindex_then_all_keeps_stricter_noindex(self):
        rules = robots_directives(
            '<meta name="robots" content="noindex, all">', None, "bot"
        )
        assert _names(rules) == ["noindex", "follow"]

    def test_all_then_nofollow_keeps_stricter_nofollow(self):
        rules = robots_directives(
            '<meta name="robots" content="all, nofollow">', None, "bot"
        )
        assert _names(rules) == ["index", "nofollow"]


class TestCrawlerScoping:
    def test_generic_and_named_rules_apply_together(self):
        page = (
            '<meta name="robots" content="noarchive">'
            '<meta name="mybot" content="noindex">'
        )
        rules = robots_directives(page, None, "mybot")
        assert _names(rules) == ["noindex", "noarchive"]
        assert _by_name(rules, "noindex").source.target == "mybot"
        assert _by_name(rules, "noarchive").source.target == "robots"

    def test_crawler_name_matched_case_insensitively(self):
        page = '<meta name="MyBot" content="noindex">'
        rules = robots_directives(page, None, "mYbOt")
        assert _names(rules) == ["noindex"]
        # The target preserves the name as written in the declaration.
        assert _by_name(rules, "noindex").source.target == "MyBot"

    def test_other_crawlers_are_excluded(self):
        page = (
            '<meta name="robots" content="noarchive">'
            '<meta name="otherbot" content="noindex, nofollow">'
        )
        rules = robots_directives(page, None, "mybot")
        assert _names(rules) == ["noarchive"]

    def test_named_rule_is_stricter_than_generic(self):
        page = (
            '<meta name="robots" content="index, follow">'
            '<meta name="mybot" content="noindex">'
        )
        rules = robots_directives(page, None, "mybot")
        assert _names(rules) == ["noindex", "follow"]

    def test_generic_rule_is_stricter_than_named(self):
        page = (
            '<meta name="robots" content="nofollow">'
            '<meta name="mybot" content="index, follow">'
        )
        rules = robots_directives(page, None, "mybot")
        assert _names(rules) == ["index", "nofollow"]

    def test_meta_index_counts_only_meta_tags(self):
        page = (
            '<meta charset="utf-8">'
            '<meta name="description" content="x">'
            '<meta name="robots" content="noindex">'
            '<meta name="mybot" content="nofollow">'
        )
        rules = robots_directives(page, None, "mybot")
        assert _by_name(rules, "noindex").source.meta_index == 2
        assert _by_name(rules, "nofollow").source.meta_index == 3

    def test_crawler_name_with_surrounding_whitespace(self):
        page = '<meta name="mybot" content="noindex">'
        rules = robots_directives(page, None, "  mybot  ")
        assert _names(rules) == ["noindex"]


class TestMerging:
    def test_strictest_boolean_wins_across_meta_tags(self):
        page = (
            '<meta name="robots" content="index">'
            '<meta name="robots" content="noindex">'
        )
        rules = robots_directives(page, None, "bot")
        directive = _by_name(rules, "noindex")
        assert directive.source.meta_index == 1

    def test_earlier_declaration_wins_equal_strictness(self):
        page = (
            '<meta name="robots" content="noindex">'
            '<meta name="robots" content="noindex">'
        )
        rules = robots_directives(page, None, "bot")
        assert _by_name(rules, "noindex").source.meta_index == 0

    def test_smaller_numeric_limit_is_stricter(self):
        page = (
            '<meta name="robots" content="max-snippet: 10">'
            '<meta name="robots" content="max-snippet: 2">'
        )
        rules = robots_directives(page, None, "bot")
        directive = _by_name(rules, "max-snippet")
        assert directive.value == "2"
        assert directive.source.meta_index == 1

    def test_numeric_zero_is_strictest(self):
        page = (
            '<meta name="robots" content="max-video-preview: 5">'
            '<meta name="robots" content="max-video-preview: 0">'
        )
        rules = robots_directives(page, None, "bot")
        assert _by_name(rules, "max-video-preview").value == "0"

    def test_image_preview_ordering(self):
        page = (
            '<meta name="robots" content="max-image-preview: large">'
            '<meta name="robots" content="max-image-preview: none">'
        )
        rules = robots_directives(page, None, "bot")
        directive = _by_name(rules, "max-image-preview")
        assert directive.value == "none"

    def test_image_preview_standard_beats_large(self):
        page = (
            '<meta name="robots" content="max-image-preview: large">'
            '<meta name="robots" content="max-image-preview: standard">'
        )
        rules = robots_directives(page, None, "bot")
        assert _by_name(rules, "max-image-preview").value == "standard"

    def test_distinct_families_coexist(self):
        page = (
            '<meta name="robots" '
            'content="noindex, nofollow, noarchive, nosnippet, '
            'notranslate, noimageindex">'
        )
        rules = robots_directives(page, None, "bot")
        assert _names(rules) == [
            "noindex",
            "nofollow",
            "noarchive",
            "nosnippet",
            "notranslate",
            "noimageindex",
        ]


class TestHeaders:
    def test_generic_header(self):
        headers = {"X-Robots-Tag": "noindex, nofollow"}
        rules = robots_directives("", headers, "bot")
        assert _names(rules) == ["noindex", "nofollow"]
        directive = _by_name(rules, "noindex")
        assert directive.source.location == "header"
        assert directive.source.target == "robots"
        assert directive.source.header_index == 0
        assert directive.source.meta_index is None

    def test_header_crawler_prefix(self):
        headers = {"X-Robots-Tag": "googlebot: noindex"}
        rules = robots_directives("", headers, "GoogleBot")
        assert _names(rules) == ["noindex"]
        assert _by_name(rules, "noindex").source.target == "googlebot"

    def test_header_prefix_for_other_crawler_ignored(self):
        headers = {"X-Robots-Tag": "bingbot: noindex"}
        rules = robots_directives("", headers, "googlebot")
        assert rules.directives == ()

    def test_generic_and_prefixed_headers_combine(self):
        headers = {
            "X-Robots-Tag": ["noarchive", "googlebot: noindex"],
        }
        rules = robots_directives("", headers, "googlebot")
        assert _names(rules) == ["noindex", "noarchive"]
        assert _by_name(rules, "noindex").source.header_index == 1
        assert _by_name(rules, "noarchive").source.header_index == 0

    def test_known_valued_directive_is_not_a_crawler_prefix(self):
        headers = {"X-Robots-Tag": "max-snippet: 0"}
        rules = robots_directives("", headers, "max-snippet")
        assert _names(rules) == ["max-snippet"]
        assert _by_name(rules, "max-snippet").source.target == "robots"

    def test_bytes_headers(self):
        headers = {b"X-Robots-Tag": [b"noindex", b"googlebot: nofollow"]}
        rules = robots_directives(b"", headers, "googlebot")
        assert _names(rules) == ["noindex", "nofollow"]

    def test_header_wins_tie_against_meta(self):
        headers = {"X-Robots-Tag": "noindex"}
        page = '<meta name="robots" content="noindex">'
        rules = robots_directives(page, headers, "bot")
        assert _by_name(rules, "noindex").source.location == "header"

    def test_stricter_meta_overrides_permissive_header(self):
        headers = {"X-Robots-Tag": "index"}
        page = '<meta name="robots" content="noindex">'
        rules = robots_directives(page, headers, "bot")
        assert _by_name(rules, "noindex").source.location == "meta"

    def test_other_header_names_ignored(self):
        headers = {"Content-Type": "text/html", "X-Robots": "noindex"}
        rules = robots_directives("", headers, "bot")
        assert rules.directives == ()

    def test_crawler_prefix_keeps_commas_inside_a_date_value(self):
        headers = {
            "X-Robots-Tag": (
                "mybot: unavailable-after: Wed, 21 Oct 2026 07:28:00 GMT"
            )
        }
        rules = robots_directives("", headers, "mybot")
        directive = _by_name(rules, "unavailable-after")
        assert directive.value == "Wed, 21 Oct 2026 07:28:00 GMT"
        assert directive.source.target == "mybot"
        assert rules.unknown == ()


class TestValuedDirectives:
    def test_max_snippet_value_kept_as_canonical_text(self):
        page = '<meta name="robots" content="max-snippet: 20">'
        rules = robots_directives(page, None, "bot")
        directive = _by_name(rules, "max-snippet")
        assert directive.value == "20"
        assert directive.source.raw == "max-snippet: 20"

    def test_unavailable_after_rfc_date_accepted(self):
        page = (
            '<meta name="robots" '
            'content="unavailable-after: Wed, 21 Oct 2026 07:28:00 GMT">'
        )
        rules = robots_directives(page, None, "bot")
        directive = _by_name(rules, "unavailable-after")
        assert directive.value == "Wed, 21 Oct 2026 07:28:00 GMT"

    def test_commas_inside_a_date_do_not_start_new_directives(self):
        page = (
            '<meta name="robots" '
            'content="noindex, unavailable-after: Wed, 21 Oct 2026 07:28:00 '
            'GMT, nofollow">'
        )
        rules = robots_directives(page, None, "bot")
        assert _names(rules) == ["noindex", "nofollow", "unavailable-after"]
        assert rules.unknown == ()

    def test_valued_directive_as_last_token_keeps_its_full_value(self):
        page = (
            '<meta name="robots" '
            'content="noindex, unavailable-after: Wed, 21 Oct 2026 07:28:00 '
            'GMT">'
        )
        rules = robots_directives(page, None, "bot")
        assert _by_name(rules, "unavailable-after").value == (
            "Wed, 21 Oct 2026 07:28:00 GMT"
        )

    def test_unknown_valued_token_does_not_swallow_next_directive(self):
        page = '<meta name="robots" content="custom: a, b, noindex">'
        rules = robots_directives(page, None, "bot")
        assert _names(rules) == ["noindex"]
        assert [item.token for item in rules.unknown] == ["custom: a", "b"]

    def test_unavailable_after_iso_date_accepted(self):
        page = '<meta name="robots" content="unavailable-after: 2026-10-21T07:28:00">'
        rules = robots_directives(page, None, "bot")
        assert _by_name(rules, "unavailable-after").value == "2026-10-21T07:28:00"

    def test_earlier_unavailable_after_is_stricter(self):
        page = (
            '<meta name="robots" '
            'content="unavailable-after: Wed, 21 Oct 2026 07:28:00 GMT">'
            '<meta name="robots" '
            'content="unavailable-after: Wed, 21 Oct 2020 07:28:00 GMT">'
        )
        rules = robots_directives(page, None, "bot")
        assert "2020" in _by_name(rules, "unavailable-after").value

    def test_aware_and_naive_dates_stay_comparable(self):
        headers = {
            "X-Robots-Tag": "unavailable-after: Wed, 21 Oct 2026 07:28:00 GMT"
        }
        page = (
            '<meta name="robots" '
            'content="unavailable-after: 2020-10-21T07:28:00">'
        )
        rules = robots_directives(page, headers, "bot")
        assert "2020" in _by_name(rules, "unavailable-after").value


class TestUnknownDirectives:
    def test_unknown_token_is_diagnostic_only(self):
        page = '<meta name="robots" content="noindex, made-up-rule">'
        rules = robots_directives(page, None, "bot")
        assert _names(rules) == ["noindex"]
        assert len(rules.unknown) == 1
        unknown = rules.unknown[0]
        assert isinstance(unknown, UnknownDirective)
        assert unknown.token == "made-up-rule"
        assert unknown.source.location == "meta"

    def test_invalid_numeric_value_is_not_guessed(self):
        page = '<meta name="robots" content="max-snippet: many">'
        rules = robots_directives(page, None, "bot")
        assert rules.get("max-snippet") is None
        assert rules.unknown[0].token == "max-snippet: many"

    def test_invalid_preview_value_is_not_guessed(self):
        page = '<meta name="robots" content="max-image-preview: huge">'
        rules = robots_directives(page, None, "bot")
        assert rules.get("max-image-preview") is None
        assert rules.unknown[0].token == "max-image-preview: huge"

    def test_invalid_date_is_not_guessed(self):
        page = '<meta name="robots" content="unavailable-after: someday">'
        rules = robots_directives(page, None, "bot")
        assert rules.get("unavailable-after") is None
        assert rules.unknown[0].token == "unavailable-after: someday"

    def test_unknown_tokens_from_other_crawlers_are_not_reported(self):
        page = (
            '<meta name="otherbot" content="mystery">'
            '<meta name="robots" content="noindex">'
        )
        rules = robots_directives(page, None, "bot")
        assert rules.unknown == ()

    def test_unknown_tokens_in_other_crawler_headers_are_not_reported(self):
        headers = {"X-Robots-Tag": "otherbot: mystery, noindex"}
        rules = robots_directives("", headers, "bot")
        assert rules.directives == ()
        assert rules.unknown == ()

    def test_unknown_tokens_keep_their_order(self):
        page = '<meta name="robots" content="zzz, aaa, noindex">'
        rules = robots_directives(page, None, "bot")
        assert [item.token for item in rules.unknown] == ["zzz", "aaa"]


class TestMarkupContexts:
    def test_meta_inside_comment_is_not_a_declaration(self):
        page = (
            '<!-- <meta name="robots" content="noindex"> -->'
            '<meta name="robots" content="nofollow">'
        )
        rules = robots_directives(page, None, "bot")
        assert _names(rules) == ["nofollow"]

    def test_meta_inside_script_is_not_a_declaration(self):
        page = (
            '<script>var s = "<meta name=\'robots\' content=\'noindex\'>";</script>'
            '<meta name="robots" content="nofollow">'
        )
        rules = robots_directives(page, None, "bot")
        assert _names(rules) == ["nofollow"]

    def test_meta_inside_noscript_is_not_a_declaration(self):
        page = (
            '<noscript><meta name="robots" content="noindex"></noscript>'
            '<meta name="robots" content="nofollow">'
        )
        rules = robots_directives(page, None, "bot")
        assert _names(rules) == ["nofollow"]

    def test_meta_inside_attribute_text_is_not_a_declaration(self):
        page = (
            '<a title="<meta name=robots content=noindex>">link</a>'
            '<meta name="robots" content="nofollow">'
        )
        rules = robots_directives(page, None, "bot")
        assert _names(rules) == ["nofollow"]

    def test_meta_in_text_node_is_not_a_declaration(self):
        page = 'just text: <meta name="robots" content="noindex"> is a tag'
        rules = robots_directives(page, None, "bot")
        # The meta tag itself is real markup; only plain text is inert.
        assert _names(rules) == ["noindex"]

    def test_unrelated_meta_inside_script_does_not_raise_index(self):
        page = (
            "<script>var a = 1;</script>"
            '<meta name="description" content="x">'
            '<meta name="robots" content="noindex">'
        )
        rules = robots_directives(page, None, "bot")
        assert _by_name(rules, "noindex").source.meta_index == 1

    def test_comment_inside_head(self):
        page = (
            "<head><!-- robots? <meta name=robots content=noindex> --></head>"
            '<meta name="robots" content="nofollow">'
        )
        rules = robots_directives(page, None, "bot")
        assert _names(rules) == ["nofollow"]


class TestEndToEndExplanation:
    def test_final_decision_is_explained_by_sources(self):
        headers = {
            "X-Robots-Tag": [
                "noarchive",
                "mybot: max-snippet: 0",
                "otherbot: noindex",
            ]
        }
        page = (
            '<meta name="robots" content="noindex, all">'
            '<meta name="mybot" content="nofollow">'
        )
        rules = robots_directives(page, headers, "MyBot")

        # noindex comes from the generic meta tag (the "all" shorthand is
        # weaker), nofollow from the crawler-specific meta tag, noarchive
        # from the generic header, and max-snippet from the named header.
        noindex = _by_name(rules, "noindex")
        assert noindex.source.location == "meta"
        assert noindex.source.target == "robots"

        nofollow = _by_name(rules, "nofollow")
        assert nofollow.source.target == "mybot"

        noarchive = _by_name(rules, "noarchive")
        assert noarchive.source.location == "header"

        snippet = _by_name(rules, "max-snippet")
        assert snippet.value == "0"
        assert snippet.source.location == "header"
        assert snippet.source.target == "mybot"

        # Rules of another crawler never leak in.
        assert rules.get("otherbot") is None
        assert rules.indexable is False
        assert rules.followable is False

    def test_default_allowance_when_nothing_applies(self):
        headers = {"X-Robots-Tag": "otherbot: noindex"}
        page = '<meta name="otherbot" content="nofollow">'
        rules = robots_directives(page, headers, "mybot")
        assert rules.directives == ()
        assert rules.indexable is True
        assert rules.followable is True
