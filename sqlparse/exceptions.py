#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Exceptions used in this package."""


class SQLParseError(Exception):
    """Base class for exceptions in this module."""


class IncompleteInputError(SQLParseError):
    """Raised when the lexer reaches the end of the input while a
    delimited construct (a quote-delimited literal or a multiline
    comment) is still open.

    The remaining input contains no statement boundary the lexer could
    rely on -- a semicolon inside the open construct does not start a new
    statement -- so it has to be reported as incomplete instead of
    letting the splitter guess a new start at an arbitrary semicolon.
    """

    def __init__(self, message, remainder=None):
        super().__init__(message)
        #: The input that could no longer be split into complete
        #: statements, starting at the opening delimiter.
        self.remainder = remainder
