"""Tests for HTTP cache invalidation after unsafe requests"""

from requests_cache import CachedSession
from tests.conftest import MOCKED_URL, MOCKED_URL_JSON, MOCKED_URL_VARY, mount_mock_adapter

EXPORT_URL = 'http+mock://requests-cache.com/export'
EXPORT_TARGET_URL = 'http+mock://requests-cache.com/exports/1'
OTHER_ORIGIN_URL = 'http+mock://other-site.com/exports/1'


def test_invalidation__success(mock_session):
    mock_session.mock_adapter.register_uri('GET', EXPORT_URL, status_code=200, text='old export')
    mock_session.mock_adapter.register_uri(
        'GET', EXPORT_TARGET_URL, status_code=200, text='old target'
    )
    mock_session.get(EXPORT_URL)
    mock_session.get(EXPORT_TARGET_URL)
    mock_session.get(MOCKED_URL_JSON)

    mock_session.mock_adapter.register_uri(
        'POST',
        EXPORT_URL,
        status_code=201,
        headers={'Location': '/exports/1', 'Content-Location': 'exports/1'},
    )
    response = mock_session.post(EXPORT_URL)

    assert response.status_code == 201
    assert mock_session.get(EXPORT_URL).from_cache is False
    assert mock_session.get(EXPORT_TARGET_URL).from_cache is False
    assert mock_session.get(MOCKED_URL_JSON).from_cache is True


def test_invalidation__failure_preserves_cache(mock_session):
    mock_session.get(MOCKED_URL)
    mock_session.get(MOCKED_URL_JSON)
    mock_session.mock_adapter.register_uri(
        'POST',
        MOCKED_URL,
        status_code=500,
        headers={'Location': MOCKED_URL_JSON},
    )

    assert mock_session.post(MOCKED_URL).status_code == 500
    assert mock_session.get(MOCKED_URL).from_cache is True
    assert mock_session.get(MOCKED_URL_JSON).from_cache is True


def test_invalidation__cross_origin_location_ignored(mock_session):
    mock_session.mock_adapter.register_uri(
        'GET', OTHER_ORIGIN_URL, status_code=200, text='other origin'
    )
    mock_session.get(OTHER_ORIGIN_URL)
    mock_session.mock_adapter.register_uri(
        'POST',
        EXPORT_URL,
        status_code=201,
        headers={'Location': OTHER_ORIGIN_URL},
    )

    mock_session.post(EXPORT_URL)

    assert mock_session.get(OTHER_ORIGIN_URL).from_cache is True


def test_invalidation__all_vary_variants(mock_session):
    for accept in ('text/html', 'application/json'):
        mock_session.get(MOCKED_URL_VARY, headers={'Accept': accept})
    assert len(mock_session.cache.responses) == 2

    mock_session.mock_adapter.register_uri(
        'POST',
        EXPORT_URL,
        status_code=201,
        headers={'Location': MOCKED_URL_VARY},
    )
    mock_session.post(EXPORT_URL)

    for accept in ('text/html', 'application/json'):
        response = mock_session.get(MOCKED_URL_VARY, headers={'Accept': accept})
        assert response.from_cache is False


def test_invalidate__deletes_all_url_variants(mock_session):
    for accept in ('text/html', 'application/json'):
        mock_session.get(MOCKED_URL_VARY, headers={'Accept': accept})

    mock_session.cache.invalidate(MOCKED_URL_VARY)

    assert len(mock_session.cache.responses) == 0


def test_invalidation__all_vary_variants_across_sessions(mock_session, tempfile_path):
    for accept in ('text/html', 'application/json'):
        mock_session.get(MOCKED_URL_VARY, headers={'Accept': accept})

    session = mount_mock_adapter(CachedSession(cache_name=tempfile_path, backend='sqlite'))
    session.mock_adapter.register_uri(
        'POST',
        EXPORT_URL,
        status_code=201,
        headers={'Location': MOCKED_URL_VARY},
    )
    session.post(EXPORT_URL)

    assert len(session.cache.responses) == 0


def test_invalidation__disabled(mock_session):
    mock_session.settings.cache_invalidation = False
    mock_session.mock_adapter.register_uri('GET', EXPORT_URL, status_code=200)
    mock_session.get(EXPORT_URL)
    mock_session.mock_adapter.register_uri('POST', EXPORT_URL, status_code=201)

    mock_session.post(EXPORT_URL)

    assert mock_session.get(EXPORT_URL).from_cache is True
