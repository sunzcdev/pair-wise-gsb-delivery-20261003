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


class CopyIncompleteError(IOError):
    """A copy operation ended with the state of some targets unknown.

    The generic layer never retries a copy once the backend may have started
    writing, because resending the request could duplicate or corrupt data.
    Instead, the destinations that were already attempted are reported here.
    """

    def __init__(self, message, completed=None, incomplete=None, failures=None):
        super().__init__(message)
        #: destinations confirmed to have received the bytes
        self.completed = list(completed or [])
        #: destinations whose final state could not be determined
        self.incomplete = list(incomplete or [])
        #: mapping of destination to the exception recorded for a failed attempt
        self.failures = dict(failures or {})
