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
    """Raised when the lexer reaches the end of the input while a delimited
    construct (a string literal or a multiline comment) is still open.

    No complete statement boundary exists in the remaining input, so the
    splitter must stop instead of resuming at an arbitrary semicolon.
    """

    def __init__(self, message, remainder=None):
        super().__init__(message)
        #: The input that could no longer be split into complete
        #: statements, starting at the opening delimiter.
        self.remainder = remainder
