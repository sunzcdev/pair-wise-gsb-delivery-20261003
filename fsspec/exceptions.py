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


class FileChangedError(RuntimeError):
    """
    Raised when a file pinned in an ``OpenFilesPlan`` no longer matches the
    identity recorded when the plan was created
    """
