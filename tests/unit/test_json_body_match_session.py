"""Session-level tests for explicit JSON body matching, using the in-memory backend."""

import pytest
import requests_mock

from requests_cache import CachedSession

URL = "http+mock://example.com/items"


@pytest.fixture
def session_factory():
    created = []

    def _factory(mode):
        session = CachedSession(
            backend="memory",
            allowable_methods=("GET", "POST"),
            json_body_match=mode,
        )
        adapter = requests_mock.Adapter()
        adapter.register_uri("POST", URL, text="ok")
        session.mount("http+mock://", adapter)
        created.append(session)
        return session

    yield _factory

    for session in created:
        session.close()


def test_reject_mode_bypasses_cache_for_invalid_json(session_factory):
    session = session_factory("reject")
    first = session.post(
        URL, data=b"not json", headers={"Content-Type": "application/json"}
    )
    second = session.post(
        URL, data=b"not json", headers={"Content-Type": "application/json"}
    )
    assert not first.from_cache
    assert not second.from_cache
    assert len(session.cache.responses) == 0


def test_reject_mode_bypasses_cache_for_duplicate_keys(session_factory):
    session = session_factory("reject")
    response = session.post(
        URL,
        data=b'{"a":1,"a":2}',
        headers={"Content-Type": "application/json"},
    )
    assert not response.from_cache
    assert len(session.cache.responses) == 0


def test_reject_mode_caches_valid_json_and_ignores_key_order(session_factory):
    session = session_factory("reject")
    first = session.post(
        URL, data=b'{"a":1,"b":2}', headers={"Content-Type": "application/json"}
    )
    second = session.post(
        URL, data=b'{"b": 2, "a": 1}', headers={"Content-Type": "application/json"}
    )
    assert not first.from_cache
    assert second.from_cache


def test_reject_mode_keeps_type_distinction(session_factory):
    session = session_factory("reject")
    session.post(URL, data=b"1", headers={"Content-Type": "application/json"})
    response = session.post(
        URL, data=b'"1"', headers={"Content-Type": "application/json"}
    )
    assert not response.from_cache


def test_fallback_mode_caches_invalid_json_by_raw_bytes(session_factory):
    session = session_factory("fallback")
    first = session.post(
        URL, data=b"not json", headers={"Content-Type": "application/json"}
    )
    second = session.post(
        URL, data=b"not json", headers={"Content-Type": "application/json"}
    )
    assert not first.from_cache
    assert second.from_cache


def test_default_mode_does_not_enable_strict_matching(session_factory):
    session = session_factory(None)
    first = session.post(URL, json=[3, 1, 2])
    second = session.post(URL, json=[2, 1, 3])
    assert not first.from_cache
    # Legacy behavior treats reordered arrays as equivalent
    assert second.from_cache


def test_invalid_json_body_match_setting_is_rejected():
    with pytest.raises(ValueError):
        CachedSession(backend="memory", json_body_match="bogus")
