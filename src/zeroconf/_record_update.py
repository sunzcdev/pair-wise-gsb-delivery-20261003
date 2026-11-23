"""A pure python implementation of multicast DNS service discovery.

Licensed under LGPL-2.1-or-later; see COPYING for details. This file is
part of a continuously modified work; modification dates are recorded
in the project's git history.
"""

from __future__ import annotations

from ._dns import DNSRecord

_DNSRecord = DNSRecord


class RecordUpdate:
    __slots__ = ("new", "old", "source")

    def __init__(self, new: DNSRecord, old: DNSRecord | None = None, source: str | None = None) -> None:
        """RecordUpdate represents a change in a DNS record.

        source is the sender address of the packet that carried the
        record, or None when the update did not come off the wire.
        """
        self._fast_init(new, old, source)

    def __getitem__(self, index: int) -> DNSRecord | None:
        """Get the new or old record."""
        if index == 0:
            return self.new
        if index == 1:
            return self.old
        raise IndexError(index)

    def _fast_init(self, new: _DNSRecord, old: _DNSRecord | None, source: str | None = None) -> None:
        """Fast init for RecordUpdate."""
        self.new = new
        self.old = old
        self.source = source
