#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Exceptions used in this package."""


class SQLParseError(Exception):
    """Base class for exceptions in this module."""


class RangeEditError(SQLParseError):
    """Raised when a restricted range edit cannot be applied.

    The edit is rejected before the token tree is modified, so the
    tree remains in its previous, consistent state.
    """
