"""A side-effect-free decision layer for cache behavior.

This module translates cache settings, keyword arguments, and ``Cache-Control`` request headers
into a concrete, predictable plan (:py:class:`CachePlan`) describing what a request is allowed to
do **before** anything is sent:

* ``read_cache``: Read a response from the cache
* ``contact_origin``: Send a request to the origin server
* ``write_cache``: Write a response to the cache

Computing a plan never modifies the :py:class:`~requests.PreparedRequest`, never modifies existing
cache entries, and never sends a request. Invalid option combinations are rejected with
:py:class:`CachePolicyError`, which is deliberately **not** a subclass of
``requests.RequestException`` so that decision errors can be told apart from real network
failures.
"""

from typing import TYPE_CHECKING, List, Optional

from attrs import define, field

from ..models import RichMixin
from . import DO_NOT_CACHE, ExpirationTime
from .directives import CacheDirectives
from .settings import CacheSettings

if TYPE_CHECKING:
    from .actions import CacheActions


class CachePolicyError(ValueError):
    """Raised when cache options and/or ``Cache-Control`` request headers are mutually
    contradictory, so no predictable cache decision can be made.

    This is raised before any request is sent, and is intentionally unrelated to
    ``requests.RequestException`` so that decision errors are not confused with network errors.
    """


def validate_cache_options(directives: CacheDirectives, settings: CacheSettings):
    """Reject mutually contradictory cache options with an explainable reason.

    ``only-if-cached`` forbids contacting the origin server (for the initial request, redirects,
    and background refreshes alike), while ``no-cache`` (``force_refresh``) requires sending a new
    request. Combining them is a contradiction, regardless of whether they came from keyword
    arguments, request headers, or session settings.
    """
    only_if_cached = settings.only_if_cached or directives.only_if_cached
    if only_if_cached and directives.no_cache:
        raise CachePolicyError(
            "Conflicting cache options: 'only-if-cached' and 'no-cache' (force_refresh). "
            "'only-if-cached' forbids contacting the origin server, while force_refresh requires "
            'sending a new request. Please use one or the other.'
        )


@define(repr=False)
class CachePlan(RichMixin):
    """The result of evaluating cache settings and headers for a request, without sending it.

    The primary outputs indicate whether each cache operation is **allowed**:

    * ``read_cache``: Reading a response from the cache is allowed
    * ``contact_origin``: Contacting the origin server is allowed
    * ``write_cache``: Writing a response to the cache is allowed (response headers may still
      forbid it for a specific response)

    The remaining flags describe what **will** happen given the current cache contents:

    * ``error_504``: A 504 response will be returned because contacting the origin server is
      forbidden and no usable cached response is available
    * ``send_request``: A new request will be sent
    * ``resend_request``: A new request will be sent to refresh a stale cached response
    * ``resend_async``: A stale response will be returned, and refreshed by a non-blocking request

    ``reasons`` contains human-readable explanations for each decision, including why certain
    option combinations take precedence over others.
    """

    cache_key: Optional[str] = None
    read_cache: bool = False
    contact_origin: bool = False
    write_cache: bool = False
    error_504: bool = False
    send_request: bool = False
    resend_request: bool = False
    resend_async: bool = False
    expire_after: ExpirationTime = None
    reasons: List[str] = field(factory=list)

    @classmethod
    def from_actions(cls, actions: 'CacheActions') -> 'CachePlan':
        """Build a plan from the actions computed for a request"""
        from .actions import _is_method_allowed

        directives = actions._directives
        settings = actions._settings
        only_if_cached = settings.only_if_cached or directives.only_if_cached

        reasons = []
        if settings.disabled:
            reasons.append(
                'The cache is disabled: nothing is read from or written to the cache, and all '
                'requests are sent to the origin server'
            )
        if not _is_method_allowed(actions._request, settings):
            reasons.append(
                f'Request method {actions._request.method} is not in allowable_methods: '
                'the cache will not be used'
            )
        if directives.no_store:
            reasons.append(
                "'no-store' is set: neither reading from nor writing to the cache is allowed"
            )
        if directives.no_cache:
            reasons.append(
                "'no-cache' (force_refresh) is set: skipping the cache read and always sending "
                'a new request'
            )
        if only_if_cached:
            reasons.append(
                "'only-if-cached' is set: the origin server will not be contacted, including for "
                'redirects and background refreshes; a 504 response is returned if no usable '
                'cached response exists'
            )
        if actions._refresh:
            reasons.append(
                "'must-revalidate' (refresh) is set: revalidating with the origin server before "
                'using a cached response'
            )
        if actions.expire_after == DO_NOT_CACHE or directives.actual_no_cache:
            reasons.append('DO_NOT_CACHE expiration is set: skipping both cache read and write')
        if settings.read_only:
            reasons.append("'read_only' is set: responses will not be written to the cache")
        if actions.error_504:
            reasons.append(
                'No usable cached response is available and contacting the origin server is '
                'forbidden: a 504 response will be returned'
            )
        if actions.resend_async:
            reasons.append(
                'A stale response will be returned while a non-blocking request refreshes it'
            )

        # Mirror the pre-response write criteria from CacheActions
        write_allowed = not (
            actions.skip_write
            or settings.disabled
            or settings.read_only
            or actions.expire_after == DO_NOT_CACHE
            or directives.actual_no_cache
            or not _is_method_allowed(actions._request, settings)
        )

        return cls(
            cache_key=actions.cache_key,
            read_cache=not actions.skip_read,
            contact_origin=not only_if_cached,
            write_cache=write_allowed,
            error_504=actions.error_504,
            send_request=actions.send_request,
            resend_request=actions.resend_request,
            resend_async=actions.resend_async,
            expire_after=actions.expire_after,
            reasons=reasons,
        )
