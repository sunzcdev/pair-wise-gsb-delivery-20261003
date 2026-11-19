"""Resumable large-object copy with explicit, auditable checkpoints.

This module implements an explicit resume entry point for copying large
objects within one filesystem.  Instead of guessing a resume offset from
the size of a partially written target, progress is tracked in a
checkpoint object stored next to a staging file:

* the *source identity* (path, size, mtime/etag when the backend reports
  them) recorded when the copy started;
* the *confirmed prefix length* - the number of bytes known to have been
  written to the staging file and checksummed;
* the *checksum of the confirmed prefix*, so a replaced or corrupted
  staging file is detected instead of silently appended to.

The copy never writes to the final target directly.  Bytes are appended
to a staging file and the final target is only *published* (renamed over
the destination) once the whole object has been copied and verified.
Backends that cannot safely publish (no atomic rename) still get the
data, but the result honestly reports ``atomic=False`` - such backends
must not claim whole-object atomic completion.

On failure the checkpoint is kept (status ``"failed"`` plus the error),
so an operator can audit exactly how far the copy got.  On success the
checkpoint is removed after publishing.
"""

import hashlib
import json
import logging
from datetime import datetime, timezone

from .callbacks import DEFAULT_CALLBACK

logger = logging.getLogger("fsspec.resumable")

#: Format identifier written into every checkpoint file.
CHECKPOINT_FORMAT = "fsspec-resumable-copy/1"

#: Default block size for the copy loop.
DEFAULT_BLOCKSIZE = 8 * 2**20

_EMPTY_SHA256 = hashlib.sha256().hexdigest()


class ResumableCopyError(Exception):
    """Base class for resumable-copy failures."""


class SourceChangedError(ResumableCopyError):
    """The source object no longer matches the checkpointed identity."""


class TargetConflictError(ResumableCopyError):
    """The staging target was replaced or corrupted since the checkpoint."""


class ResumeUnsupportedError(ResumableCopyError):
    """The backend lacks a capability (ranged reads, appends) needed here."""


def _utcnow():
    return datetime.now(timezone.utc).isoformat()


def _normalise_identity_value(value):
    """Make an info() value comparable across calls (datetimes etc.)."""
    if value is None:
        return None
    if isinstance(value, datetime):
        return value.isoformat()
    return str(value)


def source_identity(fs, path):
    """Return a dict identifying the current version of ``path``.

    Contains ``size`` always, plus ``mtime`` and ``etag``/``checksum``
    when the backend reports them.  Two identities for the same object
    version compare equal; any change of the object changes at least one
    field that both snapshots carry.
    """
    info = fs.info(path)
    return {
        "size": info["size"],
        "mtime": _normalise_identity_value(
            info.get("mtime", info.get("created"))
        ),
        "etag": _normalise_identity_value(
            info.get("etag", info.get("checksum"))
        ),
    }


def _identity_matches(recorded, current):
    """Compare a checkpointed identity with a fresh one.

    A field only proves a change when *both* snapshots carry it: a
    backend that newly starts reporting mtimes must not invalidate an
    in-flight copy.  ``size`` is always compared.
    """
    if recorded["size"] != current["size"]:
        return False
    for key in ("mtime", "etag"):
        old, new = recorded.get(key), current.get(key)
        if old is not None and new is not None and old != new:
            return False
    return True


class CopyCheckpoint:
    """Persistent, JSON-serialisable state of one resumable copy.

    Attributes
    ----------
    source, target, staging: str
        Paths of the source object, the final target and the staging
        file the copy is actually written to.
    source_identity: dict
        Identity of the source version being copied (see
        :func:`source_identity`).
    blocksize: int
        Block size of the copy loop.
    confirmed: int
        Length of the confirmed (written and checksummed) prefix of the
        staging file.
    prefix_sha256: str
        Hex SHA-256 of the confirmed prefix.
    status: str
        One of ``"in_progress"``, ``"failed"``, ``"complete"``.
    error: str or None
        The last failure, for audit, when ``status == "failed"``.
    resume_supported: bool
        False when the backend cannot perform ranged reads; an
        interrupted copy then cannot be resumed, only restarted.
    updated: str
        ISO timestamp of the last checkpoint write.
    """

    def __init__(
        self,
        source,
        target,
        staging,
        source_identity,
        blocksize,
        confirmed=0,
        prefix_sha256=_EMPTY_SHA256,
        status="in_progress",
        error=None,
        resume_supported=True,
        updated=None,
    ):
        self.source = source
        self.target = target
        self.staging = staging
        self.source_identity = dict(source_identity)
        self.blocksize = blocksize
        self.confirmed = confirmed
        self.prefix_sha256 = prefix_sha256
        self.status = status
        self.error = error
        self.resume_supported = resume_supported
        self.updated = updated or _utcnow()

    def to_dict(self):
        return {
            "format": CHECKPOINT_FORMAT,
            "source": self.source,
            "target": self.target,
            "staging": self.staging,
            "source_identity": dict(self.source_identity),
            "blocksize": self.blocksize,
            "confirmed": self.confirmed,
            "prefix_sha256": self.prefix_sha256,
            "status": self.status,
            "error": self.error,
            "resume_supported": self.resume_supported,
            "updated": self.updated,
        }

    @classmethod
    def from_dict(cls, data):
        if data.get("format") != CHECKPOINT_FORMAT:
            raise ResumableCopyError(
                f"unrecognised checkpoint format: {data.get('format')!r}"
            )
        return cls(
            source=data["source"],
            target=data["target"],
            staging=data["staging"],
            source_identity=data["source_identity"],
            blocksize=data["blocksize"],
            confirmed=data["confirmed"],
            prefix_sha256=data["prefix_sha256"],
            status=data.get("status", "in_progress"),
            error=data.get("error"),
            resume_supported=data.get("resume_supported", True),
            updated=data.get("updated"),
        )

    def save(self, fs, checkpoint_path):
        """Persist the checkpoint (best effort atomicity not required:
        a torn checkpoint fails loudly on load instead of resuming
        wrongly)."""
        self.updated = _utcnow()
        payload = json.dumps(self.to_dict(), indent=2, sort_keys=True).encode()
        fs.pipe_file(checkpoint_path, payload)

    @classmethod
    def load(cls, fs, checkpoint_path):
        """Load and validate a checkpoint; raises ResumableCopyError if
        the file is missing, unreadable or malformed."""
        try:
            raw = fs.cat_file(checkpoint_path)
        except FileNotFoundError:
            raise
        except Exception as e:  # pragma: no cover - backend specific
            raise ResumableCopyError(
                f"could not read checkpoint {checkpoint_path}: {e}"
            ) from e
        try:
            data = json.loads(raw.decode("utf-8"))
        except Exception as e:
            raise ResumableCopyError(
                f"corrupt checkpoint {checkpoint_path}: {e}"
            ) from e
        return cls.from_dict(data)


def load_checkpoint(fs, checkpoint_path):
    """Audit helper: load the checkpoint of an in-progress/failed copy."""
    return CopyCheckpoint.load(fs, checkpoint_path)


def _supports_ranged_read(fs, path):
    """Probe whether the backend honours ranged (conditional) reads."""
    try:
        fs.cat_file(path, start=0, end=1)
        return True
    except FileNotFoundError:
        raise
    except Exception:
        return False


def _open_append(fs, path):
    """Open ``path`` for appending, or raise ResumeUnsupportedError."""
    try:
        return fs.open(path, "ab")
    except (ValueError, NotImplementedError, AttributeError) as e:
        raise ResumeUnsupportedError(
            f"{type(fs).__name__} does not support append-mode writes, "
            "which resumable copy requires for its staging file"
        ) from e


def _verify_staging_prefix(fs, checkpoint):
    """Re-read and checksum the confirmed prefix of the staging file.

    Returns a hasher primed with the confirmed prefix, so the copy loop
    can continue the running checksum incrementally.

    Raises TargetConflictError if the staging file was replaced or
    corrupted, ResumeUnsupportedError if the backend cannot read ranges
    and therefore the prefix cannot be verified at all.
    """
    try:
        info = fs.info(checkpoint.staging)
    except FileNotFoundError as e:
        if not checkpoint.confirmed:
            # Nothing was ever confirmed, so a missing staging file is
            # the expected state (e.g. the previous run stopped right
            # after writing the checkpoint); it is recreated on append.
            return hashlib.sha256()
        raise TargetConflictError(
            f"staging file {checkpoint.staging} is gone although the "
            f"checkpoint confirms {checkpoint.confirmed} bytes"
        ) from e
    if info["size"] != checkpoint.confirmed:
        raise TargetConflictError(
            f"staging file {checkpoint.staging} has size {info['size']} "
            f"but the checkpoint confirms a prefix of "
            f"{checkpoint.confirmed} bytes; it was replaced or truncated"
        )
    if not checkpoint.confirmed:
        return hashlib.sha256()
    if not _supports_ranged_read(fs, checkpoint.staging):
        raise ResumeUnsupportedError(
            f"{type(fs).__name__} cannot read byte ranges, so the "
            "confirmed prefix of the staging file cannot be verified; "
            "refusing to resume on unverified state"
        )
    hasher = hashlib.sha256()
    pos = 0
    while pos < checkpoint.confirmed:
        end = min(pos + checkpoint.blocksize, checkpoint.confirmed)
        data = fs.cat_file(checkpoint.staging, start=pos, end=end)
        if not data:
            raise TargetConflictError(
                f"staging file {checkpoint.staging} ended at {pos} bytes "
                f"while verifying a confirmed prefix of "
                f"{checkpoint.confirmed} bytes"
            )
        hasher.update(data)
        pos += len(data)
    if hasher.hexdigest() != checkpoint.prefix_sha256:
        raise TargetConflictError(
            f"confirmed prefix of {checkpoint.staging} does not match "
            "the checkpoint checksum; the staging file was replaced"
        )
    return hasher


def _discard_state(fs, checkpoint_path, staging_path):
    for path in (checkpoint_path, staging_path):
        try:
            fs.rm(path)
        except FileNotFoundError:
            pass
        except Exception:  # pragma: no cover - backend specific
            logger.warning("could not remove %s", path, exc_info=True)


def resumable_copy(
    fs,
    source,
    target,
    *,
    blocksize=None,
    staging_path=None,
    checkpoint_path=None,
    on_source_change="error",
    on_staging_conflict="error",
    callback=DEFAULT_CALLBACK,
):
    """Copy ``source`` to ``target`` on ``fs`` with explicit resume support.

    This is the explicit resume entry point: call it once to start a
    copy, and call it again with the same arguments to resume an
    interrupted one.  Progress is checkpointed after every block; the
    resume offset is taken from the *confirmed* prefix recorded in the
    checkpoint and verified against the staging file, never guessed from
    the target's size.

    Parameters
    ----------
    fs: AbstractFileSystem
        The filesystem both paths live on.
    source, target: str
        Source object and final destination.  The destination is only
        published after the copy completes; while copying, data lives in
        the staging file.
    blocksize: int, optional
        Copy block size (defaults to ``fs.blocksize``).  The final block
        may be short; it is handled like any other.
    staging_path, checkpoint_path: str, optional
        Locations of the staging file and the checkpoint.  Default to
        ``target + ".fsspec-part"`` and ``target + ".fsspec-resume.json"``.
    on_source_change: "error" or "restart"
        What to do when the source no longer matches the checkpointed
        identity: raise SourceChangedError (default) or discard the
        state and copy the new version from scratch.
    on_staging_conflict: "error" or "restart"
        What to do when the staging file was replaced/corrupted, or its
        confirmed prefix cannot be verified: raise (default) or discard
        the state and start over.
    callback: fsspec.callbacks.Callback
        Progress callback, updated with each confirmed block.

    Returns
    -------
    dict with keys ``source``, ``target``, ``staging``, ``size``,
    ``sha256`` (whole-object checksum), ``resumed`` (whether an existing
    checkpoint was continued), ``atomic`` (whether the backend
    guarantees atomic publish of the final object) and ``checkpoint``
    (the checkpoint path, kept only on failure).
    """
    if on_source_change not in ("error", "restart"):
        raise ValueError("on_source_change must be 'error' or 'restart'")
    if on_staging_conflict not in ("error", "restart"):
        raise ValueError("on_staging_conflict must be 'error' or 'restart'")

    source = fs._strip_protocol(source)
    target = fs._strip_protocol(target)
    staging_path = (
        fs._strip_protocol(staging_path)
        if staging_path is not None
        else target + ".fsspec-part"
    )
    checkpoint_path = (
        fs._strip_protocol(checkpoint_path)
        if checkpoint_path is not None
        else target + ".fsspec-resume.json"
    )
    blocksize = blocksize or fs.blocksize
    if blocksize <= 0:
        raise ValueError("blocksize must be positive")

    identity = source_identity(fs, source)
    size = identity["size"]
    resumed = False

    checkpoint = None
    if fs.exists(checkpoint_path):
        checkpoint = CopyCheckpoint.load(fs, checkpoint_path)
        if checkpoint.source != source or checkpoint.target != target:
            raise ResumableCopyError(
                f"checkpoint {checkpoint_path} belongs to copy "
                f"{checkpoint.source} -> {checkpoint.target}, not "
                f"{source} -> {target}"
            )
        # Verify the source is still the version we started copying.
        if not _identity_matches(checkpoint.source_identity, identity):
            if on_source_change == "restart":
                logger.info(
                    "source %s changed since the checkpoint; restarting",
                    source,
                )
                _discard_state(fs, checkpoint_path, checkpoint.staging)
                checkpoint = None
            else:
                raise SourceChangedError(
                    f"source {source} changed since the checkpoint was "
                    f"written (recorded {checkpoint.source_identity}, now "
                    f"{identity}); pass on_source_change='restart' to "
                    "copy the new version from scratch"
                )
    hasher = None
    if (
        checkpoint is not None
        and checkpoint.status == "complete"
        and not fs.exists(checkpoint.staging)
    ):
        # A previous run published the target but stopped before
        # removing the checkpoint.  Only accept this when the published
        # object provably is the checkpointed one.
        if fs.exists(target) and _supports_ranged_read(fs, target):
            digest = hashlib.sha256()
            pos = 0
            while pos < checkpoint.confirmed:
                end = min(pos + checkpoint.blocksize, checkpoint.confirmed)
                data = fs.cat_file(target, start=pos, end=end)
                if not data:
                    break
                digest.update(data)
                pos += len(data)
            if (
                pos == checkpoint.confirmed
                and digest.hexdigest() == checkpoint.prefix_sha256
            ):
                try:
                    fs.rm(checkpoint_path)
                except Exception:  # pragma: no cover - backend specific
                    logger.warning(
                        "could not remove checkpoint %s",
                        checkpoint_path,
                        exc_info=True,
                    )
                return {
                    "source": source,
                    "target": target,
                    "staging": checkpoint.staging,
                    "size": checkpoint.confirmed,
                    "sha256": checkpoint.prefix_sha256,
                    "resumed": True,
                    "atomic": bool(getattr(fs, "atomic_publish", False)),
                    "checkpoint": None,
                }
        raise TargetConflictError(
            f"checkpoint {checkpoint_path} says the copy completed but "
            f"staging file {checkpoint.staging} is gone and target "
            f"{target} does not match the checkpointed object; inspect "
            "and remove the checkpoint manually"
        )
    if checkpoint is not None:
        # Verify the staging file is still exactly the confirmed prefix;
        # the returned hasher is primed with that prefix so the running
        # checksum can continue incrementally.
        try:
            hasher = _verify_staging_prefix(fs, checkpoint)
        except (TargetConflictError, ResumeUnsupportedError):
            if on_staging_conflict == "restart":
                logger.info(
                    "staging state for %s is unusable; restarting", target
                )
                _discard_state(fs, checkpoint_path, checkpoint.staging)
                checkpoint = None
            else:
                raise
    if checkpoint is not None and checkpoint.confirmed > size:
        if on_staging_conflict == "restart":
            _discard_state(fs, checkpoint_path, checkpoint.staging)
            checkpoint = None
        else:
            raise TargetConflictError(
                f"checkpoint confirms {checkpoint.confirmed} bytes but "
                f"the source is only {size} bytes"
            )

    if checkpoint is None:
        # Fresh start: truncate any stale staging file and record the
        # source version we are about to copy.
        resumed = False
        try:
            fs.rm(staging_path)
        except FileNotFoundError:
            pass
        checkpoint = CopyCheckpoint(
            source=source,
            target=target,
            staging=staging_path,
            source_identity=identity,
            blocksize=blocksize,
            resume_supported=_supports_ranged_read(fs, source),
        )
        checkpoint.save(fs, checkpoint_path)
    else:
        resumed = True
        if not checkpoint.resume_supported and checkpoint.confirmed:
            raise ResumeUnsupportedError(
                f"{type(fs).__name__} cannot read byte ranges, so the "
                f"interrupted copy of {source} cannot be resumed from "
                f"{checkpoint.confirmed} bytes; delete {checkpoint_path} "
                "to start over"
            )

    hasher = hasher or hashlib.sha256()
    confirmed = checkpoint.confirmed

    try:
        with _open_append(fs, staging_path) as out:
            if checkpoint.resume_supported:
                while confirmed < size:
                    end = min(confirmed + blocksize, size)
                    data = fs.cat_file(source, start=confirmed, end=end)
                    if not data:
                        raise SourceChangedError(
                            f"source {source} ended after {confirmed} "
                            f"bytes but {size} were expected; it changed "
                            "during the copy"
                        )
                    written = out.write(data)
                    if written is not None and written != len(data):
                        raise ResumableCopyError(
                            f"short write to staging file {staging_path}: "
                            f"{written} of {len(data)} bytes"
                        )
                    # A short trailing block (size not a multiple of
                    # blocksize) is just a short ``data`` here; only the
                    # bytes actually read are confirmed.
                    confirmed += len(data)
                    checkpoint.confirmed = confirmed
                    hasher.update(data)
                    checkpoint.prefix_sha256 = hasher.hexdigest()
                    checkpoint.save(fs, checkpoint_path)
                    callback.relative_update(len(data))
            else:
                # Fallback backend without ranged reads: stream
                # sequentially from the start.  Progress is still
                # checkpointed for audit, but an interruption cannot be
                # resumed (enforced above and on the next call).
                if confirmed:
                    raise ResumeUnsupportedError(
                        f"{type(fs).__name__} cannot read byte ranges; "
                        "cannot resume a partial copy"
                    )
                with fs.open(source, "rb") as src:
                    while confirmed < size:
                        data = src.read(min(blocksize, size - confirmed))
                        if not data:
                            raise SourceChangedError(
                                f"source {source} ended after {confirmed} "
                                f"bytes but {size} were expected; it "
                                "changed during the copy"
                            )
                        written = out.write(data)
                        if written is not None and written != len(data):
                            raise ResumableCopyError(
                                "short write to staging file "
                                f"{staging_path}: {written} of "
                                f"{len(data)} bytes"
                            )
                        confirmed += len(data)
                        checkpoint.confirmed = confirmed
                        hasher.update(data)
                        checkpoint.prefix_sha256 = hasher.hexdigest()
                        checkpoint.save(fs, checkpoint_path)
                        callback.relative_update(len(data))
    except Exception as e:
        # Keep auditable progress: the checkpoint records how far the
        # copy got and why it stopped.
        checkpoint.status = "failed"
        checkpoint.error = f"{type(e).__name__}: {e}"
        try:
            checkpoint.save(fs, checkpoint_path)
        except Exception:  # pragma: no cover - backend specific
            logger.warning(
                "could not persist failure checkpoint %s",
                checkpoint_path,
                exc_info=True,
            )
        raise

    # The whole object is staged; verify before publishing.
    staged_size = fs.info(checkpoint.staging)["size"]
    if staged_size != size:
        checkpoint.status = "failed"
        checkpoint.error = (
            f"staged size {staged_size} != source size {size}"
        )
        checkpoint.save(fs, checkpoint_path)
        raise TargetConflictError(
            f"staging file {checkpoint.staging} has {staged_size} bytes "
            f"after the copy but {size} were expected"
        )
    final_sha256 = hasher.hexdigest()
    checkpoint.status = "complete"
    checkpoint.error = None
    checkpoint.prefix_sha256 = final_sha256
    checkpoint.save(fs, checkpoint_path)

    # Publish: the final target only appears now.  Only backends that
    # declare a safe (atomic) rename may claim whole-object atomic
    # completion.
    atomic = bool(getattr(fs, "atomic_publish", False))
    if not atomic:
        logger.warning(
            "%s does not guarantee atomic rename; publishing %s to %s "
            "is not a whole-object atomic operation",
            type(fs).__name__,
            staging_path,
            target,
        )
    fs.mv(staging_path, target)

    # Success: the checkpoint has served its purpose.
    try:
        fs.rm(checkpoint_path)
    except Exception:  # pragma: no cover - backend specific
        logger.warning(
            "could not remove checkpoint %s", checkpoint_path, exc_info=True
        )

    return {
        "source": source,
        "target": target,
        "staging": staging_path,
        "size": size,
        "sha256": final_sha256,
        "resumed": resumed,
        "atomic": atomic,
        "checkpoint": None,
    }