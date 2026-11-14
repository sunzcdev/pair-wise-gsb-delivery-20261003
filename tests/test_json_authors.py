import json

import feedparser


def parse_json(data):
    return feedparser.parse(json.dumps(data).encode())


def feed(items, **fields):
    data = {
        "version": "https://jsonfeed.org/version/1.1",
        "items": items,
    }
    data.update(fields)
    return data


def test_feed_authors_are_mapped_in_order():
    result = parse_json(
        feed(
            [],
            authors=[
                {
                    "name": "One",
                    "url": "mailto:one@example.com",
                    "avatar": "https://example.com/one.png",
                },
                {"name": "Two", "url": "https://example.com/two"},
            ],
        )
    )

    assert not result.bozo
    assert result.feed.authors == [
        {
            "name": "One",
            "email": "one@example.com",
            "avatar": "https://example.com/one.png",
        },
        {"name": "Two", "href": "https://example.com/two"},
    ]
    assert result.feed.authors[1].url == "https://example.com/two"
    assert result.feed.author == "Two"
    assert result.feed.author_detail.name == "Two"


def test_items_inherit_feed_authors_without_sharing_state():
    result = parse_json(
        feed(
            [{"id": "one"}, {"id": "two"}],
            authors=[{"name": "Feed author", "avatar": "https://example.com/a.png"}],
        )
    )

    first, second = result.entries
    assert first.authors == [
        {"name": "Feed author", "avatar": "https://example.com/a.png"}
    ]
    assert first.authors is not result.feed.authors
    assert first.authors is not second.authors
    assert first.authors[0] is not result.feed.authors[0]

    first.authors[0]["name"] = "Changed"
    first.authors.append({"name": "Another"})
    assert result.feed.authors == [
        {"name": "Feed author", "avatar": "https://example.com/a.png"}
    ]


def test_item_author_declarations_override_inheritance():
    result = parse_json(
        feed(
            [
                {"id": "empty", "authors": []},
                {"id": "own", "authors": [{"name": "Own author"}]},
                {"id": "legacy", "author": {"name": "Legacy author"}},
                {
                    "id": "both",
                    "authors": [{"name": "Preferred"}],
                    "author": {"name": "Ignored"},
                },
            ],
            authors=[{"name": "Feed author"}],
        )
    )

    empty, own, legacy, both = result.entries
    assert empty.authors == []
    assert "author" not in empty
    assert own.authors == [{"name": "Own author"}]
    assert legacy.authors == [{"name": "Legacy author"}]
    assert both.authors == [{"name": "Preferred"}]


def test_legacy_author_field_maps_to_authors():
    result = parse_json(
        feed(
            [{"id": "one"}, {"id": "two", "author": {"name": "Item author"}}],
            author={"name": "Feed author", "url": "mailto:feed@example.com"},
        )
    )

    assert result.feed.authors == [
        {"name": "Feed author", "email": "feed@example.com"}
    ]
    assert result.entries[0].authors == result.feed.authors
    assert result.entries[1].authors == [{"name": "Item author"}]


def test_bad_author_members_are_diagnosed_without_losing_valid_authors():
    result = parse_json(
        feed(
            [],
            authors=[{"name": "One"}, "bad", {"name": "Two"}],
        )
    )

    assert result.bozo
    assert "feed.authors[1]" in str(result.bozo_exception)
    assert result.feed.authors == [{"name": "One"}, {"name": "Two"}]
