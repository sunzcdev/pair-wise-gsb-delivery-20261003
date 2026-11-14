from ijson import compat

try:
    from builtins import aiter, anext
except ImportError:  # Py<3.10
    _MISSING = object()

    def aiter(obj):
        return obj.__aiter__()

    async def anext(ait, default=_MISSING):
        try:
            return await ait.__anext__()
        except StopAsyncIteration:
            if default is _MISSING:
                raise
            return default


def _to_bytes(chunk, warned: bool):
    if isinstance(chunk, bytes):
        return chunk, warned
    if isinstance(chunk, str):
        if not warned:
            compat._warn_and_return(None)
            warned = True
        return chunk.encode("utf-8"), warned
    raise TypeError("from_iter expects an iterable of bytes or str")


_EOF = object()


class IterReader:
    """File-like object backed by a byte iterator."""

    def __init__(self, byte_iter):
        self._iter = byte_iter
        self._buffer = bytearray()
        self._warned = False

    def read(self, n: int) -> bytes:
        if n == 0:
            return b""
        while not self._buffer:
            chunk = next(self._iter, _EOF)
            if chunk is _EOF:
                return b""
            chunk, self._warned = _to_bytes(chunk, self._warned)
            self._buffer.extend(chunk)
        result = bytes(self._buffer[:n])
        del self._buffer[:n]
        return result


class AiterReader:
    """Async file-like object backed by an async byte iterator."""

    def __init__(self, byte_aiter):
        self._aiter = byte_aiter
        self._buffer = bytearray()
        self._warned = False

    async def read(self, n: int) -> bytes:
        if n == 0:
            return b""
        while not self._buffer:
            chunk = await anext(self._aiter, _EOF)
            if chunk is _EOF:
                return b""
            chunk, self._warned = _to_bytes(chunk, self._warned)
            self._buffer.extend(chunk)
        result = bytes(self._buffer[:n])
        del self._buffer[:n]
        return result


def from_iter(byte_iter):
    """Convert a byte iterable (sync or async) to a file-like object."""
    if hasattr(byte_iter, "__aiter__"):
        return AiterReader(aiter(byte_iter))
    return IterReader(iter(byte_iter))
