# tests/test_segments.py
from __future__ import annotations

from itertools import pairwise

import pytest

import chardet
from chardet import EncodingChange, Segment, SegmentReport

UTF8_TEXT = ("这是UTF-8编码的中文文本。" * 40).encode("utf-8")
GBK_TEXT = ("这是GBK编码的中文文本。" * 40).encode("gbk")
ASCII_TEXT = b"Hello world, plain ASCII. " * 30
GBK_FAMILY = {"GB18030", "GBK", "GB2312"}


def test_concatenated_encodings_report_one_change():
    data = UTF8_TEXT + GBK_TEXT
    report = chardet.detect_segments(data, window_size=256)
    assert isinstance(report, SegmentReport)
    assert len(report.changes) == 1
    change = report.changes[0]
    assert isinstance(change, EncodingChange)
    assert change.from_encoding == "utf-8"
    assert change.to_encoding in GBK_FAMILY
    # The true junction is at len(UTF8_TEXT); the reported offset is the
    # start of the first window past it.
    assert len(UTF8_TEXT) < change.offset <= len(UTF8_TEXT) + 256


def test_segments_cover_the_input_contiguously():
    data = UTF8_TEXT + GBK_TEXT
    report = chardet.detect_segments(data, window_size=256)
    assert report.segments
    assert report.segments[0].start == 0
    assert report.segments[-1].end == len(data)
    for previous, following in pairwise(report.segments):
        assert previous.end == following.start
    for segment in report.segments:
        assert isinstance(segment, Segment)
        assert segment.end - segment.start <= 256
        assert segment.candidates
        assert segment.encoding == segment.candidates[0]["encoding"]
        assert segment.confidence == segment.candidates[0]["confidence"]


def test_candidates_are_detection_dicts_not_decoded_text():
    report = chardet.detect_segments(UTF8_TEXT, window_size=256)
    for segment in report.segments:
        for candidate in segment.candidates:
            assert set(candidate) == {
                "encoding",
                "confidence",
                "language",
                "mime_type",
            }


def test_ascii_region_forces_no_switch():
    data = UTF8_TEXT + ASCII_TEXT + GBK_TEXT
    report = chardet.detect_segments(data, window_size=256)
    # The ASCII run is "no verdict", not a switch to ASCII and back.
    assert len(report.changes) == 1
    assert report.changes[0].from_encoding == "utf-8"
    assert report.changes[0].to_encoding in GBK_FAMILY


def test_single_encoding_reports_no_change():
    report = chardet.detect_segments(UTF8_TEXT, window_size=256)
    assert report.changes == ()
    # Every window with a confident verdict names UTF-8; a few small
    # windows land on a low-confidence guess, which grounds no change.
    verdicts = [
        segment.encoding
        for segment in report.segments
        if segment.confidence > chardet.MINIMUM_THRESHOLD
    ]
    assert verdicts
    assert all(encoding == "utf-8" for encoding in verdicts)


def test_clipped_multibyte_at_window_edge_is_not_a_switch():
    # 100 is not a multiple of 3, so every window edge cuts a UTF-8
    # character; the fragments must not ground an encoding change.
    report = chardet.detect_segments(UTF8_TEXT, window_size=100)
    assert report.changes == ()


def test_isolated_window_deviation_is_suppressed():
    # A lone low-evidence window between two agreeing windows is
    # boundary noise, not a switch.
    report = chardet.detect_segments(UTF8_TEXT, window_size=128)
    assert report.changes == ()


def test_empty_input_has_no_segments():
    report = chardet.detect_segments(b"")
    assert report.segments == ()
    assert report.changes == ()
    assert report.sampled_bytes == 0
    assert report.total_bytes == 0
    assert not report.truncated


def test_max_bytes_bounds_the_sample():
    data = UTF8_TEXT + GBK_TEXT
    report = chardet.detect_segments(data, window_size=256, max_bytes=512)
    assert report.sampled_bytes == 512
    assert report.total_bytes == len(data)
    assert report.truncated
    assert report.segments[-1].end == 512


def test_overlapping_windows():
    report = chardet.detect_segments(UTF8_TEXT + GBK_TEXT, window_size=256, step=128)
    starts = [segment.start for segment in report.segments]
    assert starts == list(range(0, report.sampled_bytes, 128))
    assert len(report.changes) == 1


def test_bytearray_input_accepted():
    report = chardet.detect_segments(bytearray(UTF8_TEXT), window_size=256)
    assert report.changes == ()
    assert report.segments[0].encoding == "utf-8"


@pytest.mark.parametrize(
    ("window_size", "step"),
    [(0, None), (-1, None), (True, None), (256, 0), (256, -2), (256, False)],
)
def test_invalid_window_arguments_rejected(window_size, step):
    with pytest.raises(ValueError, match="must be a positive integer"):
        chardet.detect_segments(b"data", window_size=window_size, step=step)


def test_detect_still_returns_a_single_result():
    result = chardet.detect(UTF8_TEXT + GBK_TEXT)
    assert isinstance(result, dict)
    assert result["encoding"] is not None
