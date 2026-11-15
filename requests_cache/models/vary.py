"""Data model for tracking a family of content-negotiated (``Vary``) variants"""

from __future__ import annotations

from typing import List, Optional, Tuple

from attrs import define, field

from .base import RichMixin


def normalize_vary(value: Optional[str]) -> Tuple[str, ...]:
    """Parse a ``Vary`` header value into a stable, lowercase tuple of field names.

    ``None``/empty values produce an empty tuple. A wildcard (``Vary: *``) is represented by a
    dedicated sentinel so it can never be confused with a concrete set of headers.
    """
    if not value:
        return ()
    fields = [part.strip().lower() for part in value.split(',') if part.strip()]
    if '*' in fields:
        return ('*',)
    return tuple(sorted(set(fields)))


@define(repr=False)
class VaryFamily(RichMixin):
    """The inspectable identity of a group of cached responses sharing one request target.

    The family records the negotiation rule currently in effect (the normalized ``Vary`` header
    set) together with every cache key belonging to that rule. When an origin changes its
    ``Vary`` set, the old family is discarded atomically and a new one takes its place, so stale
    variants can never be matched against an outdated rule.

    Attributes:
        vary: Normalized, sorted tuple of request header names the origin varies on
        anchor_key: Cache key of the primary (base) entry for the request target
        member_keys: All cache keys currently owned by this family, including ``anchor_key``
    """

    vary: Tuple[str, ...] = field(default=())
    anchor_key: str = field(default='')
    member_keys: List[str] = field(factory=list)

    @property
    def is_wildcard(self) -> bool:
        """``True`` when the origin sent ``Vary: *``; such a family owns no reusable variants"""
        return self.vary == ('*',)

    def contains(self, key: str) -> bool:
        return key in self.member_keys

    def add(self, key: str) -> None:
        if key not in self.member_keys:
            self.member_keys.append(key)
