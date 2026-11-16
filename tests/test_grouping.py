"""Tests for decoded-text candidate grouping (``chardet.detect_groups``)."""

from __future__ import annotations

import chardet
from chardet import _request, grouping
from chardet.pipeline import DetectionResult

#: Candidates used by the synthetic grouping tests, best-ranked first.
#: cp1252, iso8859-1 and iso8859-15 agree on the whole 0xE0-0xFF accent
#: range; utf-8 and ascii cannot decode those bytes at all.
_LATIN_CANDIDATES = ("cp1252", "iso8859-1", "iso8859-15", "utf-8", "ascii")

#: Text whose non-ASCII bytes (0xE0-0xFF accents only) decode identically
#: under cp1252 and iso8859-1: no distinguishing bytes.
SHARED_RANGE_TEXT = "Café crème brûlée — voilà, señor, très naïve! " * 4
SHARED_RANGE_TEXT = SHARED_RANGE_TEXT.replace("—", "-")  # em dash is cp1252-only
SHARED_RANGE_BYTES = SHARED_RANGE_TEXT.encode("cp1252")

#: Text that adds bytes where cp1252 and iso8859-1 disagree: 0x80 (euro),
#: 0x93/0x94 (curly quotes), 0x97 (em dash) are cp1252 punctuation but C1
#: control characters under iso8859-1.
DISTINGUISHING_TEXT = 'Café "€100" — très naïve! ' * 4
DISTINGUISHING_BYTES = DISTINGUISHING_TEXT.encode("cp1252")


def _results(*encodings: str) -> list[DetectionResult]:
    """Build synthetic ranked results, confidence decreasing with rank."""
    return [
        DetectionResult(encoding=enc, confidence=0.99 - 0.01 * i, language=None)
        for i, enc in enumerate(encodings)
    ]


def _group_of(groups: tuple[grouping.CandidateGroup, ...], encoding: str) -> int:
    """Return the index of the group containing *encoding*."""
    for i, group in enumerate(groups):
        if any(m.encoding == encoding for m in group.members):
            return i
    msg = f"{encoding} not in any group"
    raise AssertionError(msg)


class TestGroupCandidates:
    """Grouping by the text the examined bytes decode to."""

    def test_shared_range_bytes_land_in_one_group(self) -> None:
        """Without distinguishing bytes, the Latin-1 family decodes identically."""
        groups, undecodable = grouping.group_candidates(
            _results(*_LATIN_CANDIDATES), SHARED_RANGE_BYTES
        )
        assert len(groups) == 1
        assert [m.encoding for m in groups[0].members] == [
            "cp1252",
            "iso8859-1",
            "iso8859-15",
        ]
        assert groups[0].decoded_text == SHARED_RANGE_TEXT
        # utf-8 and ascii cannot decode the high bytes at all.
        assert [r.encoding for r in undecodable] == ["utf-8", "ascii"]

    def test_distinguishing_bytes_split_the_group(self) -> None:
        """Bytes 0x80-0x97 split cp1252 from iso8859-1; utf-8/ascii cannot decode."""
        groups, undecodable = grouping.group_candidates(
            _results(*_LATIN_CANDIDATES), DISTINGUISHING_BYTES
        )
        groups = tuple(groups)
        assert _group_of(groups, "cp1252") != _group_of(groups, "iso8859-1")
        assert _group_of(groups, "cp1252") != _group_of(groups, "iso8859-15")
        assert {r.encoding for r in undecodable} == {"utf-8", "ascii"}

    def test_grouping_changes_with_the_input_not_the_codec_facts(self) -> None:
        """The same two encodings group together or apart depending on the input.

        This is the two-input comparison: one input carries no byte where
        cp1252 and iso8859-1 differ, the other does.  A static alias or
        superset table would give the same answer for both; decoding the
        examined bytes must not.
        """
        candidates = _results("cp1252", "iso8859-1")
        shared_groups, _ = grouping.group_candidates(candidates, SHARED_RANGE_BYTES)
        distinct_groups, _ = grouping.group_candidates(candidates, DISTINGUISHING_BYTES)
        assert len(shared_groups) == 1
        assert len(distinct_groups) == 2
        # The shared-range grouping is a fact about this input only: the
        # second input proves the encodings are not globally equivalent.
        assert _group_of(tuple(distinct_groups), "cp1252") != _group_of(
            tuple(distinct_groups), "iso8859-1"
        )

    def test_members_keep_ranking_order_and_scores(self) -> None:
        results = _results("cp1252", "iso8859-1", "iso8859-15")
        groups, _ = grouping.group_candidates(results, SHARED_RANGE_BYTES)
        members = groups[0].members
        assert [m.encoding for m in members] == ["cp1252", "iso8859-1", "iso8859-15"]
        assert [m.confidence for m in members] == [0.99, 0.98, 0.97]

    def test_groups_ordered_by_best_member(self) -> None:
        results = _results("utf-8", "cp1252", "iso8859-1")
        groups, _ = grouping.group_candidates(results, DISTINGUISHING_BYTES)
        # utf-8 is undecodable here; cp1252's group must precede iso8859-1's.
        assert groups[0].members[0].encoding == "cp1252"
        assert groups[1].members[0].encoding == "iso8859-1"

    def test_undecodable_candidates_are_listed_separately(self) -> None:
        """A candidate that cannot decode the window joins no group."""
        groups, undecodable = grouping.group_candidates(
            _results("utf-8", "cp1252"), b"lone continuation: \xff"
        )
        assert [m.encoding for g in groups for m in g.members] == ["cp1252"]
        assert [r.encoding for r in undecodable] == ["utf-8"]

    def test_none_encoding_is_not_a_candidate(self) -> None:
        """The binary verdict names no codec and joins neither list."""
        results = [
            DetectionResult(
                encoding=None,
                confidence=0.95,
                language=None,
                mime_type="application/octet-stream",
            ),
            *_results("cp1252"),
        ]
        groups, undecodable = grouping.group_candidates(results, b"\x00\x01\x02")
        assert [m.encoding for g in groups for m in g.members] == ["cp1252"]
        assert undecodable == []

    def test_truncated_multibyte_tail_is_not_undecodable(self) -> None:
        """A window cut mid-character still groups, like the validity filter."""
        window = "日本語テキスト".encode()[:-1]  # drop one byte of the last char
        groups, undecodable = grouping.group_candidates(_results("utf-8"), window)
        assert undecodable == []
        assert len(groups) == 1


class TestRenderGroups:
    """The request-level rendering: threshold and output-name mapping."""

    def _request(self, **kwargs: object) -> _request.DetectionRequest:
        return _request.DetectionRequest.resolve(stacklevel=2, **kwargs)  # type: ignore[arg-type]

    def test_output_names_do_not_lose_members(self) -> None:
        """prefer_superset maps ISO-8859-1 and Windows-1252 to one name; both stay."""
        request = self._request(prefer_superset=True)
        rendered = request.render_groups(
            _results("iso8859-1", "cp1252"), SHARED_RANGE_BYTES
        )
        assert rendered.output_names == {"Windows-1252": ("iso8859-1", "cp1252")}

    def test_output_names_cover_undecodable_members(self) -> None:
        request = self._request()
        rendered = request.render_groups(
            _results("utf-8", "cp1252"), b"lone continuation: \xff"
        )
        assert rendered.output_names["utf-8"] == ("utf-8",)
        assert rendered.output_names["Windows-1252"] == ("cp1252",)

    def test_compat_names_false_uses_codec_names(self) -> None:
        request = self._request(compat_names=False)
        rendered = request.render_groups(_results("cp1252"), SHARED_RANGE_BYTES)
        assert rendered.output_names == {"cp1252": ("cp1252",)}

    def test_threshold_filters_low_confidence(self) -> None:
        request = self._request()
        results = [
            DetectionResult("cp1252", 0.9, None),
            DetectionResult("iso8859-1", 0.1, None),
        ]
        rendered = request.render_groups(results, SHARED_RANGE_BYTES)
        members = [m.encoding for g in rendered.groups for m in g.members]
        assert members == ["cp1252"]

    def test_ignore_threshold_keeps_low_confidence(self) -> None:
        request = self._request()
        results = [
            DetectionResult("cp1252", 0.9, None),
            DetectionResult("iso8859-1", 0.1, None),
        ]
        rendered = request.render_groups(
            results, SHARED_RANGE_BYTES, ignore_threshold=True
        )
        members = [m.encoding for g in rendered.groups for m in g.members]
        assert members == ["cp1252", "iso8859-1"]

    def test_window_size_records_the_examined_bytes(self) -> None:
        request = self._request()
        rendered = request.render_groups(_results("cp1252"), SHARED_RANGE_BYTES)
        assert rendered.window_size == len(SHARED_RANGE_BYTES)


class TestDetectGroups:
    """End-to-end grouping through the public API."""

    def test_every_candidate_appears_exactly_once(self) -> None:
        result = chardet.detect_groups(DISTINGUISHING_BYTES, ignore_threshold=True)
        grouped = [m.encoding for g in result.groups for m in g.members]
        undecodable = [r.encoding for r in result.undecodable]
        mapped = [m for members in result.output_names.values() for m in members]
        assert sorted(grouped + undecodable) == sorted(mapped)
        assert len(grouped) + len(undecodable) == len(set(grouped + undecodable))

    def test_groups_split_on_distinguishing_bytes(self) -> None:
        result = chardet.detect_groups(DISTINGUISHING_BYTES, ignore_threshold=True)
        encodings = [m.encoding for g in result.groups for m in g.members]
        if "cp1252" in encodings and "iso8859-1" in encodings:
            assert _group_of(result.groups, "cp1252") != _group_of(
                result.groups, "iso8859-1"
            )

    def test_groups_merge_without_distinguishing_bytes(self) -> None:
        result = chardet.detect_groups(SHARED_RANGE_BYTES, ignore_threshold=True)
        encodings = [m.encoding for g in result.groups for m in g.members]
        if "cp1252" in encodings and "iso8859-1" in encodings:
            assert _group_of(result.groups, "cp1252") == _group_of(
                result.groups, "iso8859-1"
            )

    def test_pure_ascii_input_groups_ascii_and_utf8(self) -> None:
        """The short-ASCII case: the examined bytes cannot tell encodings apart."""
        result = chardet.detect_groups(b"Hello, world!", ignore_threshold=True)
        all_members = [m.encoding for g in result.groups for m in g.members]
        assert all_members  # the ASCII fast path may answer with one candidate
        for group in result.groups:
            assert group.decoded_text == "Hello, world!"

    def test_empty_input(self) -> None:
        result = chardet.detect_groups(b"")
        assert len(result.groups) == 1
        assert result.groups[0].decoded_text == ""
        assert result.groups[0].members[0].encoding == "utf-8"
        assert result.window_size == 0

    def test_max_bytes_bounds_the_equivalence_claim(self) -> None:
        """Grouping covers byte_str[:max_bytes], not the whole input."""
        data = SHARED_RANGE_BYTES + DISTINGUISHING_BYTES
        result = chardet.detect_groups(data, max_bytes=len(SHARED_RANGE_BYTES))
        assert result.window_size == len(SHARED_RANGE_BYTES)

    def test_detect_groups_in_public_api(self) -> None:
        assert "detect_groups" in chardet.__all__
        assert "DetectionGroups" in chardet.__all__
        assert "CandidateGroup" in chardet.__all__
