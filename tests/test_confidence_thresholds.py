# tests/test_confidence_thresholds.py
"""Tests for the ``min_confidence`` / ``min_confidence_gap`` options."""

from __future__ import annotations

import pytest

from chardet import UniversalDetector, detect, detect_all
from chardet._request import DetectionRequest, _group_equivalent
from chardet.enums import EncodingEra
from chardet.pipeline import DetectionResult
from chardet.pipeline.orchestrator import run_pipeline

_FRENCH = "Le café était fermé, alors nous sommes allés à la gare.".encode("cp1252")
_GREEK = "Καλημέρα κόσμε, αυτό είναι ένα δοκιμαστικό κείμενο στα ελληνικά.".encode(
    "iso8859-7"
)


def _statistical(encoding: str, confidence: float) -> DetectionResult:
    return DetectionResult(encoding, confidence, None, "text/plain", statistical=True)


# ---------------------------------------------------------------------------
# The default is untouched
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "data",
    [_FRENCH, _GREEK, b"Hello, world!", b"", b"\x00\x01\x02\x03" * 20],
    ids=["french", "greek", "ascii", "empty", "binary"],
)
def test_defaults_leave_detect_unchanged(data: bytes):
    assert detect(data, min_confidence=None, min_confidence_gap=None) == detect(data)


def test_detect_all_has_no_threshold_options():
    # detect_all already exposes the whole ranking; the thresholds are a
    # single-verdict concept and would change nothing there.
    assert detect_all(_FRENCH) == detect_all(_FRENCH, ignore_threshold=False)


# ---------------------------------------------------------------------------
# min_confidence
# ---------------------------------------------------------------------------


def test_min_confidence_below_top_keeps_the_answer():
    assert detect(_FRENCH, min_confidence=0.01) == detect(_FRENCH)


def test_min_confidence_above_top_refuses_the_answer():
    result = detect(_FRENCH, min_confidence=0.999)
    assert result["encoding"] is None
    assert result["confidence"] == 0.0
    assert result["language"] is None
    assert result["mime_type"] is None
    # The refused candidates come along, best first, rendered like results.
    candidates = result["candidates"]
    assert candidates[0]["encoding"] == detect(_FRENCH)["encoding"]
    confidences = [c["confidence"] for c in candidates]
    assert confidences == sorted(confidences, reverse=True)
    assert all(
        set(c) == {"encoding", "confidence", "language", "mime_type"}
        for c in candidates
    )


def test_min_confidence_boundary_accepts_equal_confidence():
    top = detect(_FRENCH)["confidence"]
    assert detect(_FRENCH, min_confidence=top)["encoding"] is not None
    assert detect(_FRENCH, min_confidence=top + 1e-9)["encoding"] is None


# ---------------------------------------------------------------------------
# min_confidence_gap
# ---------------------------------------------------------------------------


def test_zero_gap_keeps_the_answer():
    assert detect(_FRENCH, min_confidence_gap=0.0) == detect(_FRENCH)


def test_full_gap_refuses_the_answer():
    # No statistical margin can reach 1.0, so this always refuses.
    assert detect(_FRENCH, min_confidence_gap=1.0)["encoding"] is None


def test_gap_is_measured_against_the_second_distinct_encoding():
    """Decode-identical candidates are grouped before the gap is measured."""
    window = b"caf\xe9"  # decodes identically under cp1252 and iso8859-1
    assert window.decode("cp1252") == window.decode("iso8859-1")
    results = [
        _statistical("cp1252", 0.50),
        _statistical("iso8859-1", 0.45),  # same text as the winner: same group
        _statistical("cp437", 0.30),  # 0xE9 reads differently: the real runner-up
    ]

    request = DetectionRequest(min_confidence_gap=0.15)
    # Grouped gap is 0.50 - 0.30 = 0.20 >= 0.15: the answer stands.
    assert request.render(results, window)[0]["encoding"] == "Windows-1252"
    # Had iso8859-1 counted as second, the gap would be 0.05 < 0.15.
    request = DetectionRequest(min_confidence_gap=0.25)
    refused = request.render(results, window)[0]
    assert refused["encoding"] is None
    assert [c["encoding"] for c in refused["candidates"]] == [
        "Windows-1252",
        "cp437",
    ]


def test_group_equivalent_groups_aliases_and_identical_decodes():
    results = [
        _statistical("cp1252", 0.5),
        _statistical("iso8859-1", 0.4),  # identical decode on this window
        _statistical("cp437", 0.3),  # different decode
        _statistical("not-a-codec", 0.2),  # undecodable name: own group
        _statistical("not-a-codec", 0.1),  # same undecodable name: same group
    ]
    groups = _group_equivalent(results, b"caf\xe9")
    assert [[r.encoding for r in group] for group in groups] == [
        ["cp1252", "iso8859-1"],
        ["cp437"],
        ["not-a-codec", "not-a-codec"],
    ]


# ---------------------------------------------------------------------------
# Definitive evidence, empty input, and binary stand apart
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("data", "expected_encoding"),
    [
        (b"\xef\xbb\xbfHello", "UTF-8-SIG"),  # BOM
        ("Héllo wörld café".encode(), "utf-8"),  # UTF-8 structure
        (b"Hello world", "ascii"),  # ASCII
        (  # null-byte pattern
            "Hello world, this is a test of UTF-16 detection.".encode("utf-16-le"),
            "utf-16-le",
        ),
        (b'<meta charset="iso-8859-1"><p>x</p>', "ISO-8859-1"),  # declaration
    ],
    ids=["bom", "utf8", "ascii", "utf16le", "markup"],
)
def test_definitive_results_ignore_the_thresholds(
    data: bytes, expected_encoding: str
):
    result = detect(data, min_confidence=1.0, min_confidence_gap=1.0)
    assert result["encoding"] == expected_encoding
    assert "candidates" not in result


def test_empty_input_has_its_own_result():
    result = detect(b"", min_confidence=1.0, min_confidence_gap=1.0)
    assert result["encoding"] == "utf-8"
    assert result["confidence"] == 0.10
    assert "candidates" not in result


def test_binary_has_its_own_result():
    result = detect(
        b"\x00\x01\x02\x03" * 20, min_confidence=1.0, min_confidence_gap=1.0
    )
    assert result["encoding"] is None
    assert result["confidence"] == 0.95
    assert result["mime_type"] == "application/octet-stream"
    assert "candidates" not in result


def test_no_match_fallback_has_its_own_result():
    result = detect(
        b"\x80\x81",
        include_encodings=["cp1252"],
        no_match_encoding="cp1252",
        min_confidence=1.0,
        min_confidence_gap=1.0,
    )
    assert result["encoding"] == "Windows-1252"
    assert result["confidence"] == 0.10
    assert "candidates" not in result


# ---------------------------------------------------------------------------
# UniversalDetector
# ---------------------------------------------------------------------------


def test_universal_detector_refuses_like_detect():
    detector = UniversalDetector(min_confidence=0.999)
    detector.feed(_FRENCH)
    result = detector.close()
    assert result["encoding"] is None
    assert result["candidates"][0]["encoding"] == detect(_FRENCH)["encoding"]
    assert detector.result["encoding"] is None


def test_universal_detector_default_unchanged():
    detector = UniversalDetector()
    detector.feed(_FRENCH)
    assert detector.close() == detect(_FRENCH)


# ---------------------------------------------------------------------------
# Validation
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("value", [-0.1, 1.5, float("nan"), "0.5", True])
@pytest.mark.parametrize("option", ["min_confidence", "min_confidence_gap"])
def test_invalid_thresholds_raise(option: str, value: object):
    with pytest.raises(ValueError, match=option):
        detect(b"abc", **{option: value})  # ty: ignore[invalid-argument-type]
    with pytest.raises(ValueError, match=option):
        UniversalDetector(**{option: value})  # ty: ignore[invalid-argument-type]


def test_thresholds_accept_ints():
    assert detect(_FRENCH, min_confidence=0, min_confidence_gap=0) == detect(_FRENCH)


# ---------------------------------------------------------------------------
# Pipeline provenance
# ---------------------------------------------------------------------------


def test_statistical_results_are_tagged():
    results = run_pipeline(_FRENCH, EncodingEra.ALL, full_ranking=True)
    assert results[0].statistical
    assert all(r.statistical for r in results)


def test_definitive_results_are_not_tagged():
    assert not run_pipeline(b"\xef\xbb\xbfHello", EncodingEra.ALL)[0].statistical
    assert not run_pipeline(b"Hello world", EncodingEra.ALL)[0].statistical
    assert not run_pipeline(b"", EncodingEra.ALL)[0].statistical
    assert not run_pipeline(b"\x00\x01\x02\x03" * 20, EncodingEra.ALL)[0].statistical


def test_statistical_flag_is_not_part_of_equality_or_dict():
    a = DetectionResult("cp1252", 0.5, None, "text/plain")
    b = DetectionResult("cp1252", 0.5, None, "text/plain", statistical=True)
    assert a == b
    assert "statistical" not in b.to_dict()
