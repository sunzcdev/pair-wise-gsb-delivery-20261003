from unittest.mock import MagicMock

from requests import Request

from requests_cache.models import CachedResponse
from requests_cache.policy import CacheActions, CacheSettings


BASIC_URL = 'https://img.site.com/base/img.jpg'


def _revalidate(read_only, headers_changed):
    request = Request(method='GET', url=BASIC_URL).prepare()
    actions = CacheActions.from_request(
        'key', request, CacheSettings(cache_control=True, read_only=read_only)
    )
    cached = CachedResponse(headers={'ETag': '"v1"'})
    cached.expires = None
    new_headers = (
        {'ETag': '"v1"', 'X-Custom-Header': 'new'}
        if headers_changed
        else {'ETag': '"v1"'}
    )
    not_modified = MagicMock(status_code=304, headers=new_headers, request=request)
    actions.update_revalidated_response(not_modified, cached)
    return actions.skip_write


def test_skip_write_matrix():
    # Mirrors the existing test_update_revalidated_response__skip_write expectations
    assert _revalidate(read_only=False, headers_changed=False) is True
    assert _revalidate(read_only=False, headers_changed=True) is False
    assert _revalidate(read_only=True, headers_changed=False) is True
    assert _revalidate(read_only=True, headers_changed=True) is True
