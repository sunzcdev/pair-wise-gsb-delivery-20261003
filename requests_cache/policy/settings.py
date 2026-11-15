from typing import Dict, Iterable, Optional, Union

from attrs import define, field

from .._utils import get_valid_kwargs
from ..models import RichMixin
from . import ExpirationPattern, ExpirationTime, FilterCallback, KeyCallback

ALL_METHODS = ('GET', 'HEAD', 'OPTIONS', 'POST', 'PUT', 'PATCH', 'DELETE')
DEFAULT_CACHE_NAME = 'http_cache'
DEFAULT_METHODS = ('GET', 'HEAD')
DEFAULT_STATUS_CODES = (200,)

# Common auth headers and params that are excluded from cache keys and redacted from cached responses
AUTH_HEADERS = (
    'Authorization',
    'Proxy-Authorization',
    'X-API-Key',
    'X-Auth-Token',
    'X-API-Token',
    'X-Access-Token',
)
AUTH_PARAMS = ('access_token', 'api_key', 'apikey')
DEFAULT_IGNORED_PARAMS = AUTH_HEADERS + AUTH_PARAMS


@define(repr=False)
class CacheSettings(RichMixin):
    """Class used internally to store settings that affect caching behavior. This allows settings
    to be used across multiple modules, but exposed to the user in a single property
    (:py:attr:`.CachedSession.settings`). These values can safely be modified after initialization.
    See :py:class:`.CachedSession` and :ref:`user-guide` for usage details.
    """

    allowable_codes: Iterable[int] = field(default=DEFAULT_STATUS_CODES)
    allowable_methods: Iterable[str] = field(default=DEFAULT_METHODS)
    always_revalidate: bool = field(default=False)
    autoclose: bool = field(default=True)
    cache_control: bool = field(default=False)
    content_root_key: Optional[str] = field(default=None)
    disabled: bool = field(default=False)
    expire_after: ExpirationTime = field(default=None)
    filter_fn: FilterCallback = field(default=None)
    head_coordination: Union[bool, str] = field(
        default=True, converter=lambda value: _normalize_head_coordination(value)
    )
    ignored_parameters: Iterable[str] = field(default=DEFAULT_IGNORED_PARAMS)
    key_fn: KeyCallback = field(default=None)
    match_headers: Union[Iterable[str], bool] = field(default=False)
    only_if_cached: bool = field(default=False)
    read_only: bool = field(default=False)
    stale_if_error: Union[bool, ExpirationTime] = field(default=False)
    stale_while_revalidate: Union[bool, ExpirationTime] = field(default=False)
    urls_expire_after: Dict[ExpirationPattern, ExpirationTime] = field(factory=dict)

    @classmethod
    def from_kwargs(cls, **kwargs):
        """Constructor with some additional steps:

        * Handle some deprecated argument names
        * Ignore invalid settings, for easier initialization from mixed ``**kwargs``
        """
        kwargs = cls._rename_kwargs(kwargs)
        kwargs = get_valid_kwargs(cls.__init__, kwargs)
        return cls(**kwargs)

    @staticmethod
    def _rename_kwargs(kwargs):
        if 'old_data_on_error' in kwargs:
            kwargs['stale_if_error'] = kwargs.pop('old_data_on_error')
        if 'include_get_headers' in kwargs:
            kwargs['match_headers'] = kwargs.pop('include_get_headers')
        # Accept a couple of descriptive aliases for the HEAD coordination policy
        for alias in (
            'head_policy',
            'head_cache_policy',
            'head_coordination_policy',
            'coordinate_head',
        ):
            if alias in kwargs and 'head_coordination' not in kwargs:
                kwargs['head_coordination'] = kwargs.pop(alias)
        if 'head_coordination' in kwargs:
            kwargs['head_coordination'] = _normalize_head_coordination(
                kwargs['head_coordination']
            )
        return kwargs

    @property
    def head_policy(self) -> str:
        """Descriptive alias for ``head_coordination``."""
        return 'coordinate' if self.head_coordination else 'ordinary'

    @head_policy.setter
    def head_policy(self, value: Union[bool, str]):
        self.head_coordination = _normalize_head_coordination(value)

    @property
    def head_cache_policy(self) -> str:
        """Backwards-compatible alias for ``head_policy``."""
        return self.head_policy

    @head_cache_policy.setter
    def head_cache_policy(self, value: Union[bool, str]):
        self.head_policy = value


def _normalize_head_coordination(value: Union[bool, str]) -> bool:
    """Normalize the accepted HEAD coordination policy names to a boolean."""
    if isinstance(value, str):
        value = value.strip().lower()
        if value in ('coordinate', 'coordinated', 'freshen', 'refresh', 'update', 'extend'):
            return True
        if value in ('ordinary', 'separate', 'ignore', 'off', 'disabled', 'none'):
            return False
    return bool(value)
