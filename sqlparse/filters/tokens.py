#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

from sqlparse import tokens as T
from sqlparse.keywords import COMPOSITE_KEYWORDS, is_keyword_filler


class _CaseFilter:
    ttype = None

    def __init__(self, case=None):
        case = case or 'upper'
        self.convert = getattr(str, case)

    def process(self, stream):
        for ttype, value in stream:
            if ttype in self.ttype:
                value = self.convert(value)
            yield ttype, value


class KeywordCaseFilter(_CaseFilter):
    ttype = T.Keyword

    def __init__(self, case=None):
        super().__init__(case)
        self.case = case or 'upper'

    def _convert_continuation(self, value):
        """Convert a word that continues a composite keyword.

        The words of e.g. "ORDER BY" are converted as a single keyword:
        for ``capitalize`` only the first word keeps its capital letter,
        the rest is lower-cased.
        """
        if self.case == 'capitalize':
            return value.lower()
        return self.convert(value)

    def process(self, stream):
        # The word that has to follow to continue a composite keyword like
        # "ORDER BY". The lexer already joins the words when just
        # whitespace separates them; a comment in between leaves them as
        # separate tokens, which are converted as one keyword here.
        continuation = None
        for ttype, value in stream:
            if ttype not in self.ttype:
                if continuation is not None and not is_keyword_filler(ttype):
                    continuation = None
                yield ttype, value
                continue

            if continuation is not None and value.upper() == continuation:
                continuation = None
                yield ttype, self._convert_continuation(value)
                continue

            continuation = COMPOSITE_KEYWORDS.get(value.upper())
            yield ttype, self.convert(value)


class IdentifierCaseFilter(_CaseFilter):
    ttype = T.Name, T.String.Symbol

    def process(self, stream):
        for ttype, value in stream:
            if ttype in self.ttype and value.strip()[0] != '"':
                value = self.convert(value)
            yield ttype, value


class TruncateStringFilter:
    def __init__(self, width, char):
        self.width = width
        self.char = char

    def process(self, stream):
        for ttype, value in stream:
            if ttype != T.Literal.String.Single:
                yield ttype, value
                continue

            if value[:2] == "''":
                inner = value[2:-2]
                quote = "''"
            else:
                inner = value[1:-1]
                quote = "'"

            if len(inner) > self.width:
                value = ''.join((quote, inner[:self.width], self.char, quote))
            yield ttype, value
