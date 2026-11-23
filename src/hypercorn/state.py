from __future__ import annotations

from collections.abc import Iterator, Mapping
from contextlib import contextmanager
from types import MappingProxyType
from typing import Any


class StatePublication(Mapping[str, Any]):
    """An immutable view of a single release of a :class:`StateStore`.

    A publication pairs together all the values that were current when
    it was acquired, so a scope holding a publication always reads a
    consistent (matching) set of values even after newer releases have
    been published. This allows long lived scopes, such as WebSockets,
    to continue using the release they acquired whilst new scopes
    acquire the latest release.
    """

    __slots__ = ("_values", "_version")

    def __init__(self, version: int, values: Mapping[str, Any]) -> None:
        self._version = version
        self._values = MappingProxyType(dict(values))

    @property
    def version(self) -> int:
        """The version of the release this publication was acquired from."""
        return self._version

    def __getitem__(self, key: str) -> Any:
        return self._values[key]

    def __iter__(self) -> Iterator[str]:
        return iter(self._values)

    def __len__(self) -> int:
        return len(self._values)

    def __repr__(self) -> str:
        return f"StatePublication(version={self._version}, values={self._values!r})"


class StateStore(Mapping[str, Any]):
    """A versioned container for shared (lifespan) state values.

    Values that must be used together, for example a paired reader and
    writer resource, should be updated as a single release via
    :meth:`publish` or :meth:`prepare`. Scopes that :meth:`acquire` the
    state afterwards always observe a consistent set of values from one
    release, never a mixture of two releases.

    A release is only visible once it has been successfully published;
    if preparing a release fails the staged changes are discarded and
    the most recent successful release remains current.
    """

    __slots__ = ("_values", "_version")

    def __init__(self, initial: Mapping[str, Any] | None = None) -> None:
        self._values = dict(initial) if initial is not None else {}
        self._version = 0

    @property
    def version(self) -> int:
        """The version of the most recent successfully published release."""
        return self._version

    def acquire(self) -> StatePublication:
        """Acquire the current release as an immutable publication.

        The returned publication is unaffected by any subsequent
        releases, allowing the acquirer to keep using the values it
        acquired for as long as it needs them.
        """
        return StatePublication(self._version, self._values)

    def publish(
        self, updates: Mapping[str, Any] | None = None, **kwargs: Any
    ) -> StatePublication:
        """Atomically publish a new release with the updates applied.

        All the updates become visible to subsequent acquirers together,
        as a single release. Returns the newly published release.
        """
        with self.prepare() as staged:
            if updates is not None:
                staged.update(updates)
            staged.update(kwargs)
        return self.acquire()

    @contextmanager
    def prepare(self) -> Iterator[dict[str, Any]]:
        """Stage the next release, committing it on clean exit only.

        The staged changes are published (made visible to subsequent
        acquirers) only if the with-block exits without an exception. If
        the body raises, i.e. the release fails, the staged changes are
        discarded and the most recent successful release remains
        current.
        """
        staged = dict(self._values)
        yield staged
        self._values = staged
        self._version += 1

    def __getitem__(self, key: str) -> Any:
        return self._values[key]

    def __iter__(self) -> Iterator[str]:
        return iter(self._values)

    def __len__(self) -> int:
        return len(self._values)

    def __repr__(self) -> str:
        return f"StateStore(version={self._version}, values={self._values!r})"