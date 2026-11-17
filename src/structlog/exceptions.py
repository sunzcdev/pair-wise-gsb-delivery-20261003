# SPDX-License-Identifier: MIT OR Apache-2.0
# This file is dual licensed under the terms of the Apache License, Version
# 2.0, and the MIT License.  See the LICENSE file in the root of this
# repository for complete details.

"""
Exceptions factored out to avoid import loops.
"""

from __future__ import annotations


class DropEvent(BaseException):
    """
    If raised by an processor, the event gets silently dropped.

    Derives from BaseException because it's technically not an error.
    """


class EventTooLargeError(ValueError):
    """
    A log event can't be squeezed into the configured byte budget.

    Raised by `structlog.processors.ByteBudget` if the required fields --
    together with the truncation facts that have to be attached -- are too
    large on their own.

    It derives from `ValueError` so that callers who merely want to detect an
    unusable event can keep catching a broad exception type.

    .. versionadded:: 26.2.0
    """


class NoConsoleRendererConfiguredError(Exception):
    """
    A user asked for the current `structlog.dev.ConsoleRenderer` but none is
    configured.

    .. versionadded:: 25.5.0
    """


class MultipleConsoleRenderersConfiguredError(Exception):
    """
    A user asked for the current `structlog.dev.ConsoleRenderer` and more than one is configured.

    .. versionadded:: 25.5.0
    """
