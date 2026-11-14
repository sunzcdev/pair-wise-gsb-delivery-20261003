from __future__ import annotations

import io
import pathlib
import xml.sax

import feedparser
from feedparser.diagnostics import record_diagnostic


def test_diagnostics_preserve_encoding_and_strict_parse_order():
    path = (
        pathlib.Path(__file__).parent
        / "diagnostics"
        / "encoding_override_then_loose.xml"
    )
    result = feedparser.parse(str(path))

    assert result["bozo"]
    assert isinstance(result["bozo_exception"], xml.sax.SAXException)
    assert result["parse_path"] == "loose"
    assert [item["phase"] for item in result["bozo_diagnostics"]] == [
        "encoding",
        "strict",
    ]
    assert result["feed"]["title"] == "Encoding conflict"
    assert result["entries"][0]["title"] == "Recovered entry"


def test_diagnostics_are_stable_plain_data():
    path = (
        pathlib.Path(__file__).parent
        / "diagnostics"
        / "encoding_override_then_loose.xml"
    )
    source = path.read_text(encoding="utf-8")
    result = feedparser.parse(str(path))

    for diagnostic in result["bozo_diagnostics"]:
        assert set(diagnostic) == {
            "phase",
            "stage",
            "exception",
            "type",
            "message",
            "line",
            "column",
        }
        assert not isinstance(diagnostic["message"], BaseException)
        assert not isinstance(diagnostic["message"], io.IOBase)
        assert diagnostic["message"] != source
        for value in diagnostic.values():
            assert value is None or isinstance(value, (str, int))


def test_same_exception_is_not_recorded_twice():
    result = {}
    error = ValueError("the same problem")

    record_diagnostic(result, "strict", error)
    record_diagnostic(result, "strict", error)

    assert len(result["bozo_diagnostics"]) == 1


def test_diagnostic_message_is_bounded():
    result = {}
    error = ValueError("x" * 10_000)

    diagnostic = record_diagnostic(result, "fetch", error)

    assert len(diagnostic["message"]) <= 500
    assert diagnostic["message"].endswith("...")


def test_clean_feed_has_no_optional_diagnostics():
    result = feedparser.parse("<rss version='2.0'><channel /></rss>")

    assert result["bozo"] is False
    assert "bozo_exception" not in result
    assert "bozo_diagnostics" not in result
    assert result["parse_path"] == "strict"
