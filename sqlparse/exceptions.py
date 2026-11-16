#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Exceptions used in this package."""


class SQLParseError(Exception):
    """Base class for exceptions in this module."""


class OutputLimitExceeded(SQLParseError):
    """Raised when formatting would return more characters than allowed.

    Separate from the other :class:`SQLParseError` uses so callers can tell
    an over-budget result (their own limit, raise it or narrow the SQL) from
    invalid input or a grouping limit.
    """

    def __init__(self, limit, length):
        self.limit = limit
        self.length = length
        super().__init__(
            f'Formatted output of {length} characters exceeds the '
            f'output_limit of {limit} characters')
