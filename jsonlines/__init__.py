"""
Module for the jsonlines data format.
"""

# expose only public api
from .jsonlines import (
    Error,
    InvalidArchiveError,
    InvalidLineError,
    Reader,
    Writer,
    open,
    open_gzip_archive,
)

__all__ = [
    "Error",
    "InvalidArchiveError",
    "InvalidLineError",
    "Reader",
    "Writer",
    "open",
    "open_gzip_archive",
]
