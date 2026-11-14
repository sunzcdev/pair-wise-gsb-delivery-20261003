import io
import json
import pathlib

import pytest

import feedparser

from .helpers import fail_unless_eval, get_test_data

paths = list(pathlib.Path("tests/json").rglob("*.json"))


@pytest.mark.parametrize("path", paths)
def test_json(path):
    text = path.read_text()
    description, eval_string, skip_unless = get_test_data(str(path), text)
    fail_unless_eval(str(path), eval_string)


html_content = "<a href=\"/boo.html\">boo</a><script>alert('boo!')</script>"

json_feed = json.dumps(
    {
        "version": "https://jsonfeed.org/version/1",
        "title": "Example",
        "items": [{"id": "1", "content_html": html_content}],
    }
)

atom_feed = (
    '<feed xmlns="http://www.w3.org/2005/Atom">'
    "<title>Example</title>"
    "<entry>"
    "<id>1</id>"
    '<content type="html">'
    + html_content.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
    + "</content>"
    "</entry>"
    "</feed>"
)

response_headers = {"content-location": "http://example.com/feed"}


@pytest.mark.parametrize("sanitize_html", (True, False))
@pytest.mark.parametrize("resolve_relative_uris", (True, False))
def test_json_options_match_atom(sanitize_html, resolve_relative_uris):
    """JSON and equivalent Atom content must be processed identically."""
    kwargs = {
        "response_headers": response_headers,
        "sanitize_html": sanitize_html,
        "resolve_relative_uris": resolve_relative_uris,
    }
    json_result = feedparser.parse(io.StringIO(json_feed), **kwargs)
    atom_result = feedparser.parse(io.StringIO(atom_feed), **kwargs)
    json_value = json_result.entries[0].content[0].value
    atom_value = atom_result.entries[0].content[0].value
    assert json_value == atom_value
    assert ("<script>" in json_value) is not sanitize_html
    assert ('href="http://example.com/boo.html"' in json_value) is (
        resolve_relative_uris
    )


def test_json_sanitize_html_does_not_affect_uri_resolution():
    """Disabling sanitization must not disable relative URI resolution."""
    result = feedparser.parse(
        io.StringIO(json_feed),
        response_headers=response_headers,
        sanitize_html=False,
        resolve_relative_uris=True,
    )
    value = result.entries[0].content[0].value
    assert "<script>" in value
    assert 'href="http://example.com/boo.html"' in value


def test_json_content_text_and_content_html_are_both_kept():
    """Text and HTML representations are both kept; text is never sanitized."""
    data = json.dumps(
        {
            "version": "https://jsonfeed.org/version/1",
            "title": "Example",
            "items": [
                {
                    "id": "1",
                    "content_text": "plain <b>text</b> is not HTML",
                    "content_html": "<p>hi</p><script>alert('boo!')</script>",
                }
            ],
        }
    )
    result = feedparser.parse(io.StringIO(data))
    content = result.entries[0].content
    assert len(content) == 2
    assert content[0].type == "text"
    assert content[0].value == "plain <b>text</b> is not HTML"
    assert content[1].type == "html"
    assert "<script>" not in content[1].value


def test_json_options_do_not_leak_between_calls():
    """Two consecutive parse() calls must not affect each other."""
    first = feedparser.parse(io.StringIO(json_feed), sanitize_html=False)
    assert "<script>" in first.entries[0].content[0].value
    second = feedparser.parse(io.StringIO(json_feed))
    assert "<script>" not in second.entries[0].content[0].value


def test_json_explicit_arguments_override_global_defaults(monkeypatch):
    """Explicit parse() arguments take precedence over global defaults."""
    monkeypatch.setattr(feedparser, "SANITIZE_HTML", False)
    monkeypatch.setattr(feedparser, "RESOLVE_RELATIVE_URIS", False)
    default = feedparser.parse(io.StringIO(json_feed))
    assert "<script>" in default.entries[0].content[0].value
    explicit = feedparser.parse(
        io.StringIO(json_feed),
        response_headers=response_headers,
        sanitize_html=True,
        resolve_relative_uris=True,
    )
    value = explicit.entries[0].content[0].value
    assert "<script>" not in value
    assert 'href="http://example.com/boo.html"' in value
