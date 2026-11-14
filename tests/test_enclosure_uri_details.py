from __future__ import annotations

import io

import responses

import feedparser


def parse_with_base(data, base="http://example.com/feeds/main.xml", **kwargs):
    return feedparser.parse(
        io.BytesIO(data),
        response_headers={"content-location": base},
        **kwargs,
    )


def test_rss_enclosure_details_use_local_xml_base():
    result = parse_with_base(
        b"""
        <rss version="2.0" xml:base="root/">
          <channel>
            <item>
              <enclosure url="one.mp3" length="1" type="audio/mpeg"/>
              <enclosure url="nested/two.mp3#part" length="2" type="audio/mpeg"/>
            </item>
            <item xml:base="other/">
              <enclosure url="three.mp3" length="3" type="audio/mpeg"/>
            </item>
            <item>
              <enclosure url="four.mp3" length="4" type="audio/mpeg"/>
            </item>
          </channel>
        </rss>
        """
    )

    first, second, third = result.entries
    assert set(first.enclosures[0]) == {"href", "length", "type"}
    assert first.enclosures[0].href == "http://example.com/feeds/root/one.mp3"
    assert first.enclosures[1].href == (
        "http://example.com/feeds/root/nested/two.mp3#part"
    )
    assert second.enclosures[0].href == "http://example.com/feeds/root/other/three.mp3"
    assert third.enclosures[0].href == "http://example.com/feeds/root/four.mp3"

    assert first.enclosure_details[0] == {
        "value": "one.mp3",
        "base": "http://example.com/feeds/root/",
        "href": "http://example.com/feeds/root/one.mp3",
    }
    assert first.enclosure_details[1]["value"] == "nested/two.mp3#part"
    assert second.enclosure_details[0]["base"] == (
        "http://example.com/feeds/root/other/"
    )
    assert third.enclosure_details[0]["base"] == "http://example.com/feeds/root/"


def test_atom_enclosure_details_keep_literal_values():
    result = parse_with_base(
        b"""
        <feed xmlns="http://www.w3.org/2005/Atom" xml:base="root/">
          <entry>
            <link rel="enclosure" href="one.mp3" length="1" type="audio/mpeg"/>
            <link rel="enclosure" href="#part" length="2" type="audio/mpeg"/>
            <link rel="enclosure" href="" length="3" type="audio/mpeg"/>
          </entry>
        </feed>
        """
    )

    entry = result.entries[0]
    assert [detail["value"] for detail in entry.enclosure_details] == [
        "one.mp3",
        "#part",
        "",
    ]
    assert [detail["href"] for detail in entry.enclosure_details] == [
        "http://example.com/feeds/root/one.mp3",
        "http://example.com/feeds/root/#part",
        "http://example.com/feeds/root/",
    ]


def test_json_attachments_are_enclosures_with_details():
    result = parse_with_base(
        b"""
        {
          "version": "https://jsonfeed.org/version/1",
          "title": "Example",
          "items": [
            {
              "id": "1",
              "attachments": [
                {"url": "media/a.mp3", "mime_type": "audio/mpeg"},
                {"url": "#part", "mime_type": "audio/mpeg"},
                {"url": "", "mime_type": "audio/mpeg"},
                {"url": "http://[invalid", "mime_type": "audio/mpeg"}
              ]
            }
          ]
        }
        """
    )

    entry = result.entries[0]
    assert len(entry.enclosures) == len(entry.enclosure_details) == 4
    assert [detail["value"] for detail in entry.enclosure_details] == [
        "media/a.mp3",
        "#part",
        "",
        "http://[invalid",
    ]
    assert [detail["href"] for detail in entry.enclosure_details] == [
        "http://example.com/feeds/media/a.mp3",
        "http://example.com/feeds/main.xml#part",
        "http://example.com/feeds/main.xml",
        "",
    ]


def test_enclosure_details_survive_disabled_html_uri_resolution():
    result = parse_with_base(
        b"""
        <feed xmlns="http://www.w3.org/2005/Atom">
          <entry>
            <link rel="enclosure" href="one.mp3" type="audio/mpeg"/>
          </entry>
        </feed>
        """,
        resolve_relative_uris=False,
    )

    assert result.entries[0].enclosure_details[0]["value"] == "one.mp3"
    assert result.entries[0].enclosure_details[0]["href"] == (
        "http://example.com/feeds/one.mp3"
    )


def test_final_response_address_is_the_attachment_base():
    responses.get(
        "http://example.com/start",
        status=302,
        headers={"Location": "http://example.com/final/feed.xml"},
    )
    responses.get(
        "http://example.com/final/feed.xml",
        body=b"""
        <feed xmlns="http://www.w3.org/2005/Atom">
          <entry>
            <link rel="enclosure" href="media/a.mp3" type="audio/mpeg"/>
          </entry>
        </feed>
        """,
        content_type="application/atom+xml",
    )

    result = feedparser.parse("http://example.com/start")

    detail = result.entries[0].enclosure_details[0]
    assert detail["base"] == "http://example.com/final/feed.xml"
    assert detail["href"] == "http://example.com/final/media/a.mp3"
