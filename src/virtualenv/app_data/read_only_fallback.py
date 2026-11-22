from __future__ import annotations

__lazy_modules__ = ["contextlib", "logging"]

import logging
from contextlib import contextmanager
from typing import TYPE_CHECKING, Final

from .base import AppData, ContentStore
from .read_only import ReadOnlyAppData
from .via_tempdir import TempAppData

if TYPE_CHECKING:
    from collections.abc import Callable, Generator
    from pathlib import Path
    from typing import Any, NoReturn

    from virtualenv.util.lock import PathLockBase
_LOGGER: Final[logging.Logger] = logging.getLogger(__name__)


class ReadOnlyFallbackAppData(AppData):
    """Read-only app data that materializes missing wheel images in a private temporary folder.

    A read-only cache mounted on a build machine may hold wheel images for only some of the seed packages. This store
    serves every image already present in the read-only folder as-is, and builds the missing ones from the bundled
    wheels inside a temporary app data folder, which is removed on close. Seed packages are always copied into the
    environment, so an environment never references the temporary folder once the run ends.

    :param folder: the existing read-only folder to read cached content from

    :var can_update: always ``False``, neither store is updated
    :var transient: always ``True``, the fallback folder is removed on close
    :var lock: the no-op lock of the read-only folder, used for paths inside it

    """

    can_update = False
    transient = True

    def __init__(self, folder: str) -> None:
        self._read_only = ReadOnlyAppData(folder)
        self.lock = self._read_only.lock
        self._fallback: TempAppData | None = None

    @property
    def _temp(self) -> TempAppData:
        """The temporary fallback store, created lazily so a fully populated cache creates nothing."""
        if self._fallback is None:
            self._fallback = TempAppData()
            _LOGGER.debug("missing wheel images are built in temporary app data folder %s", self._fallback)
        return self._fallback

    def __repr__(self) -> str:
        return f"{type(self).__name__}({self._read_only!r}, fallback={self._fallback!r})"

    def __str__(self) -> str:
        return str(self._read_only)

    @property
    def house(self) -> Path:
        """The wheel house of the temporary store, since the read-only folder can never receive a wheel."""
        return self._temp.house

    def close(self) -> None:
        """Close the read-only store and remove the temporary store, if one was created."""
        self._read_only.close()
        if self._fallback is not None:
            self._fallback.close()
            self._fallback = None

    def reset(self) -> None:
        """Refuse to reset, since the primary folder is read-only.

        :raises RuntimeError: always

        """
        msg = "read-only app data does not support reset"
        raise RuntimeError(msg)

    def py_info(self, path: Path) -> ContentStore:
        """Return a store that reads the read-only python info and writes misses to the temporary store."""
        return _FallbackContentStore(self._read_only.py_info(path), lambda: self._temp.py_info(path))

    def py_info_clear(self) -> None:
        """Refuse to clear the python info, since the folder is read-only.

        :raises NotImplementedError: always

        """
        raise NotImplementedError

    def embed_update_log(self, distribution: str, for_py_version: str) -> NoReturn:
        """Refuse to provide an embed update log, which neither a read-only nor a temporary store keeps."""
        return self._read_only.embed_update_log(distribution, for_py_version)

    def wheel_image(self, for_py_version: str, name: str) -> Path:
        """Return the cached image path in the read-only folder when it holds content, otherwise a temporary path.

        :param for_py_version: the target Python version string
        :param name: the image name, an installer class joined with the wheel stem

        :returns: the read-only image path when a populated image exists there, otherwise the matching temporary path

        """
        image: Final = self._read_only.wheel_image(for_py_version, name)
        if image.exists() and any(image.iterdir()):
            _LOGGER.debug("reuse read-only wheel image %s", image)
            return image
        fallback_image: Final = self._temp.wheel_image(for_py_version, name)
        _LOGGER.debug("wheel image %s missing from read-only cache, building at %s", image, fallback_image)
        return fallback_image

    @contextmanager
    def extract(self, path: Path, to_folder: Path | None) -> Generator[Path]:
        """Extract into the temporary store, since the read-only folder cannot be written to."""
        with self._temp.extract(path, to_folder) as extracted:
            yield extracted

    @contextmanager
    def locked(self, path: Path) -> Generator[PathLockBase]:
        """Lock the store that owns the path: the temporary store for fallback paths, the read-only store otherwise."""
        with self._store_for(path).locked(path) as locked_path:
            yield locked_path

    def _store_for(self, path: Path) -> AppData:
        if self._fallback is not None and self._is_within(self._fallback.lock.path, path):
            return self._fallback
        return self._read_only

    @staticmethod
    def _is_within(root: Path, path: Path) -> bool:
        try:
            path.relative_to(root)
        except ValueError:
            return False
        return True


class _FallbackContentStore(ContentStore):
    """Content store that reads from a read-only store and writes to a writable fallback store.

    A hit in the read-only store is served as-is. A miss reads as absent and every write or removal lands in the
    fallback store, leaving the read-only folder untouched.

    :param read_only_store: the store backed by the read-only app data folder
    :param fallback_store: returns the store backed by the temporary app data folder, created lazily

    """

    def __init__(self, read_only_store: ContentStore, fallback_store: Callable[[], ContentStore]) -> None:
        self._read_only_store = read_only_store
        self._fallback_store = fallback_store

    def exists(self) -> bool:
        """``True`` when either store holds the content, preferring the read-only copy."""
        return self._read_only_store.exists() or self._fallback_store().exists()

    def read(self) -> Any:  # ruff:ignore[any-type]  # the stored JSON document may be any type
        """Read the read-only content when present, otherwise the content written to the fallback store."""
        if self._read_only_store.exists():
            return self._read_only_store.read()
        return self._fallback_store().read()

    def write(self, content: Any) -> None:  # ruff:ignore[any-type]  # stores any JSON-serializable content
        """Write to the fallback store, since the read-only folder cannot be modified."""
        self._fallback_store().write(content)

    def remove(self) -> None:
        """Remove the content from the fallback store; read-only content cannot be removed."""
        self._fallback_store().remove()

    @contextmanager
    def locked(self) -> Generator[None]:
        """Hold the fallback store's lock, since only that store can change."""
        with self._fallback_store().locked():
            yield


__all__ = ["ReadOnlyFallbackAppData"]
