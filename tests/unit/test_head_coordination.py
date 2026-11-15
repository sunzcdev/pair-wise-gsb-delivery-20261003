"""Tests for coordinating HEAD responses with previously cached GET responses."""

from datetime import timedelta

from requests_mock import Adapter

from requests_cache import CachedSession
from tests.conftest import START_DT, time_travel

URL = 'https://example.com/head-coordination'


def _headers(etag='"v1"', length='5', **kwargs):
    return {
        'ETag': etag,
        'Content-Length': length,
        'Content-Type': 'text/plain',
        **kwargs,
    }


def _make_session(**kwargs):
    session = CachedSession(backend='memory', **kwargs)
    adapter = Adapter()
    session.mount('https://', adapter)
    return session, adapter


def test_head_freshens_cached_get_without_changing_body():
    session, adapter = _make_session(expire_after=1)
    adapter.register_uri('GET', URL, headers=_headers(), text='hello')
    adapter.register_uri('HEAD', URL, headers=_headers(), text='')

    with time_travel(START_DT):
        session.get(URL)
    with time_travel(START_DT + timedelta(seconds=2)):
        head_response = session.head(URL, expire_after=10)
        assert head_response.from_cache is False
        assert head_response.content == b''

        get_response = session.get(URL)

    assert get_response.from_cache is True
    assert get_response.text == 'hello'


def test_head_with_changed_validator_invalidates_get():
    session, adapter = _make_session(expire_after=1)
    adapter.register_uri(
        'GET',
        URL,
        [
            {'headers': _headers('"v1"', '5'), 'text': 'hello'},
            {'headers': _headers('"v2"', '7'), 'text': 'updated'},
        ],
    )
    adapter.register_uri('HEAD', URL, headers=_headers('"v2"', '7'), text='')

    with time_travel(START_DT):
        session.get(URL)
    with time_travel(START_DT + timedelta(seconds=2)):
        session.head(URL, expire_after=10)
        response = session.get(URL)

    assert response.from_cache is False
    assert response.text == 'updated'


def test_head_with_changed_length_invalidates_get():
    session, adapter = _make_session(expire_after=1)
    adapter.register_uri(
        'GET',
        URL,
        [
            {'headers': _headers('"v1"', '5'), 'text': 'hello'},
            {'headers': _headers('"v1"', '9'), 'text': 'different'},
        ],
    )
    adapter.register_uri('HEAD', URL, headers=_headers('"v1"', '9'), text='')

    with time_travel(START_DT):
        session.get(URL)
    with time_travel(START_DT + timedelta(seconds=2)):
        session.head(URL, expire_after=10)
        response = session.get(URL)

    assert response.from_cache is False
    assert response.text == 'different'


def test_head_without_validator_does_not_assume_unchanged():
    session, adapter = _make_session(expire_after=1)
    adapter.register_uri(
        'GET',
        URL,
        [
            {'headers': {'Content-Length': '5'}, 'text': 'hello'},
            {'headers': {'Content-Length': '5'}, 'text': 'hello'},
        ],
    )
    adapter.register_uri('HEAD', URL, headers={'Content-Length': '5'}, text='')

    with time_travel(START_DT):
        session.get(URL)
    with time_travel(START_DT + timedelta(seconds=2)):
        session.head(URL, expire_after=10)
        response = session.get(URL)

    assert response.from_cache is False


def test_head_without_cached_get_is_ordinary_head_caching():
    session, adapter = _make_session(expire_after=10)
    adapter.register_uri('HEAD', URL, headers=_headers(), text='')
    adapter.register_uri('GET', URL, headers=_headers(), text='hello')

    with time_travel(START_DT):
        head_1 = session.head(URL)
        head_2 = session.head(URL)
        get_1 = session.get(URL)
        get_2 = session.get(URL)

    assert head_1.from_cache is False
    assert head_2.from_cache is True
    assert head_1.content == head_2.content == b''
    assert get_1.from_cache is False
    assert get_1.text == 'hello'
    assert get_2.from_cache is True


def test_head_only_freshens_matching_vary_variant():
    session, adapter = _make_session(expire_after=1)
    json_headers = _headers('"json"', '4', Vary='Accept')
    html_headers = _headers('"html"', '4', Vary='Accept')

    def is_json(request):
        return request.headers.get('Accept') == 'application/json'

    adapter.register_uri('GET', URL, additional_matcher=is_json, headers=json_headers, text='json')
    adapter.register_uri(
        'GET',
        URL,
        additional_matcher=lambda request: not is_json(request),
        headers=html_headers,
        text='html',
    )
    adapter.register_uri('HEAD', URL, additional_matcher=is_json, headers=json_headers, text='')

    with time_travel(START_DT):
        session.get(URL, headers={'Accept': 'application/json'})
        session.get(URL, headers={'Accept': 'text/html'})
    with time_travel(START_DT + timedelta(seconds=2)):
        session.head(URL, headers={'Accept': 'application/json'}, expire_after=10)
        json_response = session.get(URL, headers={'Accept': 'application/json'})
        html_response = session.get(URL, headers={'Accept': 'text/html'})

    assert json_response.from_cache is True
    assert json_response.text == 'json'
    assert html_response.from_cache is False
    assert html_response.text == 'html'


def test_changed_vary_variant_does_not_invalidate_other_variants():
    session, adapter = _make_session(expire_after=1)
    json_headers = _headers('"json-v1"', '8', Vary='Accept')
    changed_json_headers = _headers('"json-v2"', '8', Vary='Accept')
    html_headers = _headers('"html"', '4', Vary='Accept')

    def is_json(request):
        return request.headers.get('Accept') == 'application/json'

    adapter.register_uri(
        'GET',
        URL,
        [
            {'headers': json_headers, 'text': 'json-old'},
            {'headers': changed_json_headers, 'text': 'json-new'},
        ],
        additional_matcher=is_json,
    )
    adapter.register_uri(
        'GET',
        URL,
        additional_matcher=lambda request: not is_json(request),
        headers=html_headers,
        text='html',
    )
    adapter.register_uri(
        'HEAD', URL, additional_matcher=is_json, headers=changed_json_headers, text=''
    )

    with time_travel(START_DT):
        session.get(URL, headers={'Accept': 'application/json'}, expire_after=1)
        session.get(URL, headers={'Accept': 'text/html'}, expire_after=10)
    with time_travel(START_DT + timedelta(seconds=2)):
        session.head(URL, headers={'Accept': 'application/json'}, expire_after=10)
        json_response = session.get(URL, headers={'Accept': 'application/json'})
        html_response = session.get(URL, headers={'Accept': 'text/html'})

    assert json_response.from_cache is False
    assert json_response.text == 'json-new'
    assert html_response.from_cache is True
    assert html_response.text == 'html'


def test_head_coordination_can_be_disabled():
    session, adapter = _make_session(expire_after=1, head_coordination=False)
    adapter.register_uri(
        'GET',
        URL,
        [
            {'headers': _headers('"v1"', '5'), 'text': 'hello'},
            {'headers': _headers('"v2"', '7'), 'text': 'updated'},
        ],
    )
    adapter.register_uri('HEAD', URL, headers=_headers('"v2"', '7'), text='')

    with time_travel(START_DT):
        session.get(URL)
    with time_travel(START_DT + timedelta(seconds=2)):
        session.head(URL, expire_after=10)
        response = session.get(URL)

    assert response.from_cache is False
    assert response.text == 'updated'
