from __future__ import annotations

import requests
import responses
from responses import matchers

import feedparser
from feedparser import http
from feedparser.util import FeedParserDict


FEED = b"""<?xml version="1.0"?>
<rss version="2.0">
    <channel>
        <title>Example</title>
        <item><title>First entry</title></item>
    </channel>
</rss>
"""


def make_previous_result():
    return FeedParserDict(
        bozo=False,
        entries=[FeedParserDict(title="First entry")],
        feed=FeedParserDict(title="Example"),
        headers={"etag": 'W/"old"', "last-modified": "Mon, 03 Apr 2023 01:02:03 GMT"},
        etag='W/"old"',
        modified="Mon, 03 Apr 2023 01:02:03 GMT",
        modified_parsed=(2023, 4, 3, 1, 2, 3, 0, 93, 0),
        href="http://example.test/old",
        status=200,
        version="rss20",
    )


def test_conditional_headers_send_weak_etag_verbatim():
    url = "http://example.test/conditional-headers.xml"
    responses.add(
        responses.GET,
        url,
        body=FEED,
        headers={"ETag": 'W/"current"'},
        match=[
            matchers.header_matcher(
                {
                    "If-None-Match": 'W/"old"',
                    "If-Modified-Since": "Mon, 03 Apr 2023 01:02:03 GMT",
                }
            )
        ],
    )

    result = feedparser.parse(
        url,
        etag='W/"old"',
        modified="Mon, 03 Apr 2023 01:02:03 GMT",
    )

    assert result.status == 200
    assert result.etag == 'W/"current"'
    assert result.entries[0].title == "First entry"


def test_previous_result_supplies_both_validators():
    url = "http://example.test/previous-result-validators.xml"
    previous = make_previous_result()
    responses.add(
        responses.GET,
        url,
        body=FEED,
        match=[
            matchers.header_matcher(
                {
                    "If-None-Match": 'W/"old"',
                    "If-Modified-Since": "Mon, 03 Apr 2023 01:02:03 GMT",
                }
            )
        ],
    )

    result = feedparser.parse(url, previous_result=previous)

    assert result.status == 200
    assert result.entries[0].title == "First entry"


def test_not_modified_reuses_previous_result_without_mutating_it():
    url = "http://example.test/not-modified.xml"
    previous = make_previous_result()
    responses.add(
        responses.GET,
        url,
        status=304,
        headers={"Content-Length": "0"},
        body=b"",
    )

    result = feedparser.parse(
        url,
        etag=previous.etag,
        modified=previous.modified,
        previous_result=previous,
    )

    assert result is not previous
    assert result.status == 304
    assert result.not_modified is True
    assert result.entries == previous.entries
    assert result.feed == previous.feed
    # The server omitted its validators, so the old ones must survive.
    assert result.etag == 'W/"old"'
    assert result.modified == "Mon, 03 Apr 2023 01:02:03 GMT"
    # The caller's saved result must be untouched.
    assert previous.status == 200
    assert previous.href == "http://example.test/old"
    assert previous.etag == 'W/"old"'


def test_http_get_merges_not_modified_result():
    url = "http://example.test/http-get-not-modified.xml"
    previous = make_previous_result()
    result = {"headers": {}}
    responses.add(responses.GET, url, status=304, body=b"")

    body = http.get(
        url,
        result,
        etag=previous.etag,
        modified=previous.modified,
        previous_result=previous,
    )

    assert body == b""
    assert result["status"] == 304
    assert result["not_modified"] is True
    assert result["entries"] == previous.entries
    assert previous.status == 200


def test_empty_200_is_not_treated_as_not_modified():
    url = "http://example.test/empty-200.xml"
    previous = make_previous_result()
    responses.add(responses.GET, url, status=200, body=b"")

    result = feedparser.parse(url, previous_result=previous)

    assert result.status == 200
    assert "not_modified" not in result
    assert result.entries == []


def test_not_modified_keeps_final_redirect_url_and_status():
    url = "http://example.test/redirect-source.xml"
    target = "http://example.test/redirect-target.xml"
    previous = make_previous_result()
    responses.add(
        responses.GET,
        url,
        status=302,
        headers={"Location": target},
        body=b"",
    )
    responses.add(responses.GET, target, status=304, body=b"")

    result = feedparser.parse(url, previous_result=previous)

    assert result.status == 304
    assert result.href == target
    assert result.entries == previous.entries


def test_empty_200_keeps_old_validators_when_response_omits_them():
    url = "http://example.test/empty-200-validators.xml"
    previous = make_previous_result()
    responses.add(responses.GET, url, status=200, body=b"")

    result = feedparser.parse(url, previous_result=previous)

    assert result.etag == 'W/"old"'
    assert result.modified == "Mon, 03 Apr 2023 01:02:03 GMT"


def test_connection_failure_is_observable():
    url = "http://example.test/connection-failure.xml"
    error = requests.ConnectionError("no route")
    responses.add(responses.GET, url, body=error)

    result = feedparser.parse(url)

    assert result.bozo is True
    assert result.bozo_exception is error
    assert "status" not in result


def test_non_success_status_is_observable():
    url = "http://example.test/non-success.xml"
    responses.add(responses.GET, url, status=500, body=b"")

    result = feedparser.parse(url)

    assert result.status == 500
    assert result.bozo is False


def test_parse_failure_is_observable():
    url = "http://example.test/parse-failure.xml"
    responses.add(responses.GET, url, status=200, body=b"<not-a-feed")

    result = feedparser.parse(url)

    assert result.status == 200
    assert result.bozo
    assert result.bozo_exception is not None
