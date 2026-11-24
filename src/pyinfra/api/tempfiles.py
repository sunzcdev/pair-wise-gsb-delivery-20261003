"""
Per-host registry of temporary files used by connectors during uploads.

A plain upload owns its temporary file exclusively: the connector creates it and
removes it as soon as the upload finishes, so the file keeps its original,
short-lived lifecycle.

A *shared* temporary file is one several operation plans deliberately point at
(they pass the same ``remote_temp_filename``). Each plan becomes an explicit
*owner* of the file; the file must survive any individual plan finishing - even
when that plan fails before it ever reads the file - and is removed exactly
once, when the last owner releases it. This mirrors a reference count keyed on
the real users rather than on individual upload calls.
"""

from __future__ import annotations

from collections.abc import Callable, Hashable, Iterator
from contextlib import contextmanager
from dataclasses import dataclass, field
from typing import TYPE_CHECKING
from uuid import uuid4

from pyinfra import logger

if TYPE_CHECKING:
    from pyinfra.api.host import Host

TempFileRemover = Callable[[str], object]


@dataclass
class TempFileLease:
    """An owner's hold on one temporary file."""

    registry: TempFileRegistry
    temp_filename: str
    owner: Hashable
    host: Host | None = None

    def release(self) -> None:
        self.registry.release(self.host, self.temp_filename, self.owner)


@dataclass
class _TempFileEntry:
    # Operation plans currently relying on this file. The file is removed only
    # when the set empties, so a plan that fails while another still holds the
    # file never breaks the other plan's later read.
    owners: set[Hashable] = field(default_factory=set)
    # Whether any owner ever held the file. Unmanaged uploads are recorded for
    # bookkeeping but keep their connector's own immediate cleanup.
    managed: bool = False
    # Whether the file's content has actually been staged on the target. Owners
    # can register before any upload happens, so this - not owner count - decides
    # whether the next caller must upload.
    uploaded: bool = False
    removed: bool = False


class TempFileRegistry:
    """
    Tracks the owners of temporary files on a single host.

    Files without owners are *unmanaged*: connectors keep handling their cleanup
    themselves and the registry simply records their path for bookkeeping. Files
    acquired with an owner are *managed*: cleanup is deferred until every owner
    has released the file, at which point it is removed a single time.
    """

    def __init__(self, remover: TempFileRemover | None = None) -> None:
        self._entries: dict[str, _TempFileEntry] = {}
        # Optional local cleanup hook. Production use removes remote paths via
        # the host; tests/local simulations can pass ``os.remove`` or a mock.
        self._remover = remover

    def is_managed(self, temp_filename: str) -> bool:
        """Whether callers explicitly registered shared ownership of this path."""

        entry = self._entries.get(temp_filename)
        return entry is not None and entry.managed and not entry.removed

    def acquire(self, temp_filename: str, owner: Hashable | None = None) -> bool:
        """
        Record that ``owner`` relies on ``temp_filename`` and report whether the
        content still needs uploading.

        Returns ``True`` while no caller has staged the file's content (so the
        caller must upload it) and ``False`` once a previous acquisition uploaded
        it. Owners may register ahead of the upload - e.g. two operation plans
        sharing one fragment - so this reflects upload state, not owner count.
        """

        entry = self._entries.get(temp_filename)
        if entry is None:
            entry = _TempFileEntry()
            self._entries[temp_filename] = entry

        if owner is not None:
            entry.managed = True
            entry.owners.add(owner)

        return not entry.uploaded

    @contextmanager
    def hold(
        self,
        host: Host | None,
        temp_filename: str,
        owner: Hashable | None = None,
    ) -> Iterator[TempFileLease]:
        """
        Acquire one owner hold for the duration of a block and release it on exit.

        Failure inside the block still releases this owner; other owners keep the
        file alive until their holds end. A unique anonymous owner is used when
        ``owner`` is omitted.
        """

        lease_owner = owner if owner is not None else uuid4().hex
        self.acquire(temp_filename, lease_owner)
        lease = TempFileLease(
            registry=self,
            temp_filename=temp_filename,
            owner=lease_owner,
            host=host,
        )
        try:
            yield lease
        finally:
            lease.release()

    def mark_uploaded(self, temp_filename: str) -> None:
        """
        Record that the file's content is now staged on the target. Later
        acquisitions will reuse it instead of uploading again.
        """

        entry = self._entries.get(temp_filename)
        if entry is not None:
            entry.uploaded = True

    def release(
        self,
        host: Host | None,
        temp_filename: str,
        owner: Hashable | None = None,
    ) -> None:
        """
        Release an owner's hold on a temporary file.

        Managed files are removed once their last owner releases them. Releasing
        an unknown or already removed file is a no-op, so cleanup is safe to run
        from failure paths and host disconnect alike.
        """

        entry = self._entries.get(temp_filename)
        if entry is None or entry.removed:
            return

        if owner is not None:
            entry.owners.discard(owner)

        # An unmanaged file (no owners ever attached) is left to the connector's
        # own cleanup, preserving the historical single-upload lifecycle.
        if not entry.managed or entry.owners:
            return

        entry.removed = True
        self._remove(host, temp_filename)
        self._entries.pop(temp_filename, None)

    def release_owner(self, host: Host | None, owner: Hashable) -> None:
        """
        Release every file held by a single owner (typically a finishing operation).
        """

        for temp_filename in [
            temp_filename
            for temp_filename, entry in self._entries.items()
            if owner in entry.owners
        ]:
            self.release(host, temp_filename, owner)

    def release_all(self, host: Host | None) -> None:
        """
        Best-effort cleanup of every managed temporary file, used when a host is
        disconnected. Unmanaged files are skipped: their owning connector is
        responsible for removing them.
        """

        for temp_filename in list(self._entries.keys()):
            entry = self._entries[temp_filename]
            if entry.removed or not entry.managed:
                continue
            entry.removed = True
            self._remove(host, temp_filename)

        # Drop unmanaged bookkeeping entries too; the host is going away.
        self._entries.clear()

    def _remove(self, host: Host | None, temp_filename: str) -> None:
        if self._remover is not None:
            try:
                self._remover(temp_filename)
            except Exception as e:
                logger.debug("Could not remove temporary file %s: %s", temp_filename, e)
            return

        if host is None:
            logger.debug("No host/remover available to remove temporary file %s", temp_filename)
            return

        # Local import avoids an import cycle with api.command re-exporting these types.
        from pyinfra.api.command import QuoteString, StringCommand

        try:
            status, output = host.run_shell_command(
                StringCommand("rm", "-f", QuoteString(temp_filename)),
            )
        except Exception as e:
            logger.debug("Could not remove temporary file %s: %s", temp_filename, e)
            return

        if not status:
            stderr = getattr(output, "stderr", "")
            logger.debug("Could not remove temporary file %s: %s", temp_filename, stderr)
