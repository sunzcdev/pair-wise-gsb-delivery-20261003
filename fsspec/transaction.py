"""Deferred writes and their per-file outcome.

A transaction gathers files whose writes were deferred, then commits them one
at a time when the transaction ends. Each backend commits independently, so a
transaction is *not* an atomic publish across objects: some files may already
be visible at their final destination while others are not. This module
therefore reports what actually happened for every file instead of pretending
that the batch either fully happened or fully did not:

- files whose ``commit()`` returned are reported as committed, and their
  targets are never removed afterwards (that would destroy data that the
  backend did publish);
- files whose ``commit()`` raised, and files that were never attempted, are
  reported as not committed, keeping the original exception and the identity
  (final path, temporary path/object) of each one;
- an attempt is made to remove the temporary artifacts of the not-committed
  files, and the result of every such attempt is reported separately, so a
  failed cleanup is visible rather than silently swallowed;
- a batch is never reported as successful while any file is not committed or
  any temporary artifact could not be cleaned up.

A failed transaction still leaves the filesystem: ``_intrans`` is cleared, so
subsequent writes are direct again, and callers can inspect the report and
retry the legitimate leftovers themselves.
"""

import logging
from collections import deque
from dataclasses import dataclass, field
from typing import Any

logger = logging.getLogger("fsspec")

#: The file was committed to its final destination.
COMMITTED = "committed"
#: The file was not committed: its own commit failed, or it was never attempted
#: because an earlier commit failed.
NOT_COMMITTED = "not-committed"
#: The transaction was asked to discard, and this file was discarded.
DISCARDED = "discarded"
#: A batch operation was attempted but the backend gave no per-file outcome, so
#: it is unknown whether any particular file was published.
UNKNOWN = "unknown"

#: Cleanup of the temporary artifact succeeded.
CLEANED = "cleaned"
#: Cleanup of the temporary artifact was attempted and failed.
CLEANUP_FAILED = "cleanup-failed"
#: Cleanup was not attempted (yet).
NOT_ATTEMPTED = "not-attempted"
#: No cleanup was required, e.g. the file was committed and its temporary
#: artifact was consumed by the commit.
NOT_REQUIRED = "not-required"


def _getattr_safe(obj, *names):
    """First non-None attribute of ``obj`` among ``names``, never raising."""
    for name in names:
        try:
            value = getattr(obj, name)
        except Exception:
            continue
        if value is not None:
            return value
    return None


def _describe(f):
    """Final path and temporary artifact of a deferred file.

    Backends name these attributes differently: local files use ``path`` and
    ``temp``, FTP/WebHDFS buffered files use ``target`` and ``path``, SFTP uses
    ``targetpath``/``temppath`` and cached writers use ``path``/``fn``. Values
    are passed through unchanged so they keep the backend's own identity.
    """
    path = _getattr_safe(f, "target", "targetpath", "path")
    temp = _getattr_safe(f, "temppath", "temp", "fn")
    if temp is None:
        # e.g. FTPFile/WebHDFile rename ``path`` to the temporary location and
        # keep the final location in ``target``.
        maybe = _getattr_safe(f, "path")
        if maybe is not None and maybe != path:
            temp = maybe
    return path, temp


@dataclass(eq=False)
class TransactionEntry:
    """Outcome of a single deferred file.

    Attributes
    ----------
    path: object
        Final destination the file was meant to be published at.
    temp: object or None
        Temporary artifact that holds the data before commit, when the backend
        exposes one.
    status: str
        One of ``"committed"``, ``"not-committed"``, ``"discarded"`` or
        ``"unknown"``.
    error: BaseException or None
        Exception raised by ``commit()`` for this file, if any. The exception
        object is preserved as-is.
    cleaned: bool or None
        ``True`` if the temporary artifact was removed, ``False`` if removing
        it failed, ``None`` if cleanup was not attempted or not required.
    cleanup_error: BaseException or None
        Exception raised while removing the temporary artifact, if any.
    file: object
        The deferred file object, so a caller can inspect or retry it.
    """

    path: Any = None
    temp: Any = None
    status: str = NOT_COMMITTED
    error: BaseException | None = None
    cleaned: bool | None = None
    cleanup_error: BaseException | None = None
    file: Any = field(default=None, repr=False, compare=False)

    @classmethod
    def from_file(cls, f):
        path, temp = _describe(f)
        return cls(path=path, temp=temp, file=f)

    @property
    def committed(self):
        """Whether this file reached its final destination."""
        return self.status == COMMITTED

    @property
    def attempted(self):
        """Whether ``commit()``/``discard()`` was called for this file."""
        return (
            self.status in (COMMITTED, DISCARDED)
            or self.error is not None
            or self.cleanup_error is not None
            or self.cleaned is True
        )

    @property
    def cleanup_status(self):
        """Human-readable cleanup state."""
        if self.cleaned is True:
            return CLEANED
        if self.cleaned is False:
            return CLEANUP_FAILED
        if self.status in (COMMITTED, DISCARDED):
            return NOT_REQUIRED
        return NOT_ATTEMPTED

    @property
    def identity(self):
        """Full identity of the object this entry is about."""
        return {"path": self.path, "temp": self.temp}

    def __getstate__(self):
        # The deferred file object is only meaningful in the process that owns
        # it; drop it so reports can travel back from a remote actor.
        state = self.__dict__.copy()
        state["file"] = None
        return state

    def __repr__(self):
        extra = ""
        if self.error is not None:
            extra += f", error={self.error!r}"
        if self.cleanup_error is not None:
            extra += f", cleanup_error={self.cleanup_error!r}"
        return (
            f"TransactionEntry(path={self.path!r}, temp={self.temp!r}, "
            f"status={self.status!r}, cleanup={self.cleanup_status!r}{extra})"
        )


@dataclass
class TransactionReport:
    """What happened to each file of a finished transaction.

    A report never claims more than the backend delivered: committed files
    really did return from ``commit()`` (but the transaction as a whole was
    still not atomic, and the targets are not rolled back), not-committed files
    may or may not have reached the backend if a commit raised, and cleanup
    failures mean the temporary artifact may still exist.

    Attributes
    ----------
    requested: str
        ``"commit"`` or ``"discard"``, i.e. what the caller asked for.
    entries: list of TransactionEntry
        One entry per file, in processing order.
    error: BaseException or None
        The primary exception that stopped the transaction, unchanged.
    """

    requested: str = "commit"
    entries: list = field(default_factory=list)
    error: BaseException | None = None

    def add(self, f):
        """Create and register an entry for the deferred file ``f``."""
        entry = TransactionEntry.from_file(f)
        self.entries.append(entry)
        return entry

    @property
    def committed(self):
        """Entries that were committed to their final destination."""
        return [e for e in self.entries if e.status == COMMITTED]

    @property
    def not_committed(self):
        """Entries that were not committed (failed or never attempted)."""
        return [e for e in self.entries if e.status == NOT_COMMITTED]

    #: Alias for :attr:`not_committed`.
    @property
    def pending(self):
        return self.not_committed

    @property
    def discarded(self):
        """Entries that were discarded."""
        return [e for e in self.entries if e.status == DISCARDED]

    @property
    def failed(self):
        """Entries whose ``commit()`` raised."""
        return [e for e in self.entries if e.error is not None]

    @property
    def skipped(self):
        """Entries that were never attempted because an earlier one failed."""
        return [
            e
            for e in self.entries
            if e.status == NOT_COMMITTED
            and e.error is None
            and e.cleanup_error is None
        ]

    @property
    def indeterminate(self):
        """Entries with no per-file outcome from the backend."""
        return [e for e in self.entries if e.status == UNKNOWN]

    #: Alias for :attr:`indeterminate`.
    @property
    def unknown(self):
        return self.indeterminate

    @property
    def cleanup_succeeded(self):
        """Entries whose temporary artifact was cleaned up."""
        return [e for e in self.entries if e.cleaned is True]

    #: Alias for :attr:`cleanup_succeeded`.
    @property
    def cleaned(self):
        return self.cleanup_succeeded

    @property
    def cleanup_failed(self):
        """Entries whose temporary artifact could not be cleaned up."""
        return [e for e in self.entries if e.cleaned is False]

    @property
    def leftovers(self):
        """Entries that may still have a temporary artifact on the backend."""
        return [e for e in self.entries if e.cleaned is False]

    @property
    def committed_paths(self):
        return [e.path for e in self.committed]

    @property
    def not_committed_paths(self):
        return [e.path for e in self.not_committed]

    @property
    def cleanup_failed_paths(self):
        return [e.path for e in self.cleanup_failed]

    @property
    def ok(self):
        """Whether the transaction did everything it was asked to do.

        A commit is only ``ok`` if every file was committed; a discard is only
        ``ok`` if nothing raised and every file was discarded. A failed cleanup
        or an indeterminate outcome always makes it ``False``.
        """
        if self.error is not None:
            return False
        if self.failed or self.indeterminate or self.cleanup_failed:
            return False
        if self.requested == "commit":
            return all(e.status == COMMITTED for e in self.entries)
        return all(e.status == DISCARDED for e in self.entries)

    #: Alias for :attr:`ok`.
    @property
    def success(self):
        return self.ok

    #: Alias for :attr:`ok`.
    @property
    def successful(self):
        return self.ok

    def __bool__(self):
        return self.ok

    @property
    def summary(self):
        """One-line human-readable summary of the outcome."""
        parts = [
            f"{len(self.committed)} committed",
            f"{len(self.not_committed)} not committed",
            f"{len(self.cleanup_succeeded)} cleaned up",
            f"{len(self.cleanup_failed)} cleanup failed",
        ]
        if self.discarded:
            parts.insert(1, f"{len(self.discarded)} discarded")
        if self.indeterminate:
            parts.append(f"{len(self.indeterminate)} indeterminate")
        return ", ".join(parts)

    def __str__(self):
        return f"<TransactionReport {self.requested}: {self.summary}>"


def _attach_report(exc, report):
    """Make the report available from the exception, without replacing it."""
    try:
        exc.transaction_report = report
    except Exception:
        pass
    try:
        exc.add_note(f"fsspec transaction: {report.summary}")
    except Exception:
        pass


def _discard(entry, f):
    """Try to remove the temporary artifact of ``f`` and record the result."""
    try:
        f.discard()
    except FileNotFoundError:
        # The artifact is already absent: cleanup is idempotent, so this is a
        # success, not a leftover. This also covers a commit that published the
        # target and then raised, leaving nothing left to remove.
        entry.cleaned = True
    except BaseException as e:
        entry.cleaned = False
        entry.cleanup_error = e
        logger.debug("Discarding deferred file %s failed", entry.path, exc_info=True)
    else:
        entry.cleaned = True


def _process(files, commit):
    """Commit or discard every deferred file, collecting a report.

    The queue is drained. The first exception (the primary cause) is kept on
    the report; every file that was not committed is cleaned up afterwards,
    including the file that failed. Files that were already committed are
    never touched again.
    """
    report = TransactionReport(requested="commit" if commit else "discard")
    remaining = deque(files)
    files.clear()
    primary = None
    try:
        while remaining:
            f = remaining.popleft()
            entry = report.add(f)
            if not commit:
                _discard(entry, f)
                if entry.cleanup_error is not None:
                    entry.status = NOT_COMMITTED
                    if primary is None:
                        primary = entry.cleanup_error
                else:
                    entry.status = DISCARDED
                continue
            try:
                f.commit()
            except BaseException as e:
                entry.status = NOT_COMMITTED
                entry.error = e
                primary = e
                # Do not roll back anything that already committed: for these
                # backends a published target cannot be withdrawn reliably, and
                # deleting it would destroy data. Only clean temporary
                # artifacts, including the one whose commit just failed.
                _discard(entry, f)
                while remaining:
                    f2 = remaining.popleft()
                    _discard(report.add(f2), f2)
                break
            else:
                entry.status = COMMITTED
    finally:
        # Safety net for unexpected errors: never drop a temporary artifact
        # silently. In the normal paths ``remaining`` is already empty.
        while remaining:
            f2 = remaining.popleft()
            _discard(report.add(f2), f2)
        report.error = primary
    return report


class Transaction:
    """Filesystem transaction write context

    Gathers files for deferred commit or discard, so that several write
    operations can be finalized semi-atomically. This works by having this
    instance as the ``.transaction`` attribute of the given filesystem

    Commits happen one file at a time and are not atomic across files. When the
    transaction finishes, :attr:`report` (also returned by :meth:`complete`)
    describes the outcome of every file: committed, not committed, cleaned up
    or cleanup failed. If a commit fails, the original exception is raised
    unchanged, files that already committed are left in place, and the
    filesystem leaves deferred-write mode.
    """

    def __init__(self, fs, **kwargs):
        """
        Parameters
        ----------
        fs: FileSystem instance
        """
        self.fs = fs
        self.files = deque()
        self.report = None

    def __enter__(self):
        self.start()
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        """End transaction and commit, if exit is not due to exception"""
        # only commit if there was no exception
        try:
            self.complete(commit=exc_type is None)
        except BaseException as e:
            if exc_type is None:
                raise
            if e is not exc_val:
                # A cleanup failure during rollback must not replace the
                # exception that made us roll back; keep it on the report and
                # in the log instead.
                logger.debug("Cleaning up an aborted transaction failed", exc_info=True)
        finally:
            if self.fs is not None:
                self.fs._intrans = False
                self.fs._transaction = None
                self.fs = None
        if exc_val is not None and self.report is not None:
            _attach_report(exc_val, self.report)
        return False

    def start(self):
        """Start a transaction on this FileSystem"""
        self.files = deque()  # clean up after previous failed completions
        self.report = None
        self.fs._intrans = True

    def complete(self, commit=True):
        """Finish transaction: commit or discard all deferred files

        Returns
        -------
        TransactionReport
            Per-file outcome, including which files were committed, which were
            not, and whether their temporary artifacts were cleaned up.

        Raises
        ------
        BaseException
            The original exception raised by a failing ``commit()`` or
            ``discard()``, unchanged, after all remaining temporary artifacts
            have been dealt with.
        """
        report = None
        try:
            report = _process(self.files, commit)
            self.report = report
            if report.error is not None:
                _attach_report(report.error, report)
                raise report.error
            return report
        finally:
            # A failed commit or discard must still end the transaction.
            # Leaving _intrans set would defer every later write on this
            # filesystem into a temporary file that nothing ever commits,
            # and instances are cached, so that would persist process-wide.
            if self.fs is not None:
                self.fs._intrans = False
                self.fs._transaction = None
                self.fs = None


class FileActor:
    def __init__(self):
        self.files = []

    def commit(self):
        return _process(self.files, commit=True)

    def discard(self):
        return _process(self.files, commit=False)

    def append(self, f):
        self.files.append(f)


class DaskTransaction(Transaction):
    def __init__(self, fs):
        """
        Parameters
        ----------
        fs: FileSystem instance
        """
        import distributed

        super().__init__(fs)
        client = distributed.default_client()
        self.files = client.submit(FileActor, actor=True).result()

    def start(self):
        """Start a transaction, keeping the remote file actor"""
        self.report = None
        self.fs._intrans = True

    def complete(self, commit=True):
        """Finish transaction: commit or discard all deferred files"""
        report = None
        try:
            if commit:
                report = self.files.commit().result()
            else:
                report = self.files.discard().result()
            self.report = report
            if report.error is not None:
                _attach_report(report.error, report)
                raise report.error
            return report
        finally:
            if self.fs is not None:
                self.fs._intrans = False
                self.fs._transaction = None
                self.fs = None
