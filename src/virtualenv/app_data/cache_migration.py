from __future__ import annotations

__lazy_modules__ = [
    "contextlib",
    "pathlib",
    "platformdirs",
    "shutil",
    "typing",
]

import logging
import os
import pathlib
import shutil
from contextlib import suppress
from typing import Final

from platformdirs import user_cache_dir, user_data_dir

_LOGGER: Final[logging.Logger] = logging.getLogger(__name__)
_MIGRATION_MARKER: Final[str] = ".virtualenv-migration-incomplete"


def cache_dir_with_migration() -> str:
    """Return the cache directory after migrating legacy app data when needed.

    The new cache directory may be nested inside the old data directory, in which case the old entries are moved
    into it one by one. That migration is resumable: an interrupted run is picked up where it left off instead of
    assuming the presence of the new directory means every entry was already moved.

    :returns: the cache directory path

    """
    new_dir: Final = user_cache_dir(appname="virtualenv", appauthor="pypa")
    old_dir: Final = user_data_dir(appname="virtualenv", appauthor="pypa")
    if new_dir == old_dir or not os.path.isdir(old_dir):
        return new_dir
    if not os.path.isdir(new_dir) and not _is_nested_dir(new_dir, old_dir):
        _LOGGER.info("migrating app data from %s to %s", old_dir, new_dir)
        try:
            shutil.move(old_dir, new_dir)
        except OSError as exception:
            _LOGGER.warning(
                "could not migrate app data from %s to %s: %r, using old location", old_dir, new_dir, exception
            )
            return old_dir
        return new_dir
    try:
        return _migrate_entries(old_dir, new_dir)
    except OSError as exception:  # e.g. the old directory cannot be listed
        _LOGGER.warning("could not migrate app data from %s to %s: %r, using old location", old_dir, new_dir, exception)
        return old_dir


def _is_nested_dir(inner: str, outer: str) -> bool:
    try:
        return os.path.normcase(os.path.commonpath((outer, inner))) == os.path.normcase(outer)
    except ValueError:
        return False


def _migrate_entries(old_dir: str, new_dir: str) -> str:
    """Move what is left of the old layout into the new one, entry by entry, resuming after an interruption.

    :param old_dir: the old application data directory
    :param new_dir: the cache directory, possibly nested inside the old one

    :returns: the directory to use

    """
    nested: Final = _is_nested_dir(new_dir, old_dir)
    marker: Final = pathlib.Path(old_dir, _MIGRATION_MARKER)
    skipped_entries: Final = {_MIGRATION_MARKER}
    if nested:
        # the new directory lives inside the old one, so its top-level part must not be moved into itself
        skipped_entries.add(os.path.relpath(new_dir, old_dir).split(os.sep, 1)[0])
    pending: Final = [entry for entry in os.listdir(old_dir) if entry not in skipped_entries]
    if not pending:
        # an interrupted run may have finished every move before stopping, leaving only the marker behind
        with suppress(OSError):
            marker.unlink()
        return new_dir
    resumed: Final = marker.is_file()
    _LOGGER.info(
        "%s app data migration from %s to %s with %d entr%s to move",
        "resuming" if resumed else "starting",
        old_dir,
        new_dir,
        len(pending),
        "y" if len(pending) == 1 else "ies",
    )
    marker.touch(exist_ok=True)
    moved: Final[list[str]] = []
    try:
        os.makedirs(new_dir, exist_ok=True)
        for entry in pending:
            source = os.path.join(old_dir, entry)
            target = os.path.join(new_dir, entry)
            if os.path.lexists(target):
                # the new layout already holds this entry, moved by an earlier run or generated there since; that
                # copy is the one in use, so keep it and leave the old copy untouched
                _LOGGER.warning(
                    "keeping in-use app data entry %s of the new layout (moved earlier or generated there since);"
                    " the old copy at %s was left in place",
                    target,
                    source,
                )
                continue
            shutil.move(source, target)
            moved.append(entry)
    except OSError as exception:
        # put back what this run moved, so the old location stays whole while it remains in use for this run
        if not sum(not _restore_entry(entry, old_dir, new_dir) for entry in moved):
            with suppress(OSError):
                os.rmdir(new_dir)
        _LOGGER.warning(
            "could not finish app data migration from %s to %s: %r, using old location",
            old_dir,
            new_dir,
            exception,
        )
        return old_dir
    with suppress(OSError):
        marker.unlink()
    if not nested:
        with suppress(OSError):  # entries kept for the new layout keep the old folder around
            os.rmdir(old_dir)
    return new_dir


def _restore_entry(entry: str, old_dir: str, new_dir: str) -> bool:
    try:
        shutil.move(os.path.join(new_dir, entry), old_dir)
    except OSError:
        return False
    return True


__all__ = [
    "cache_dir_with_migration",
]
