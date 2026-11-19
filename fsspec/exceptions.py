"""
fsspec user-defined exception classes
"""

import asyncio


class BlocksizeMismatchError(ValueError):
    """
    Raised when a cached file is opened with a different blocksize than it was
    written with
    """


class FSTimeoutError(asyncio.TimeoutError):
    """
    Raised when a fsspec function timed out occurs
    """


class ChecksumUnsupported(NotImplementedError):
    """
    Raised when a filesystem cannot provide the requested level of
    checksum guarantee for a path

    This is deliberately *not* a success: callers asking for a guarantee
    the filesystem cannot give (for example a backend-provided content
    digest from a backend that only exposes version identity) get this
    error instead of a value that would silently mean less than asked for.
    """
