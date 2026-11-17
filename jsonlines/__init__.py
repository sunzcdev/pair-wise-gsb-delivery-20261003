"""
Module for the jsonlines data format.
"""

# expose only public api
from .jsonlines import (
    Backend,
    CodecCapabilities,
    CodecConfig,
    CodecConfigError,
    Error,
    InvalidLineError,
    Reader,
    Writer,
    get_default_backend,
    open,
    set_default_backend,
)

__all__ = [
    "Backend",
    "CodecCapabilities",
    "CodecConfig",
    "CodecConfigError",
    "Error",
    "InvalidLineError",
    "Reader",
    "Writer",
    "get_default_backend",
    "open",
    "set_default_backend",
]
