#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

from sqlparse import tokens as T
from sqlparse.exceptions import SQLParseError


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


class ProtectedRegionFilter:
    """Merges the tokens covered by protected regions into single tokens.

    *regions* is a sorted sequence of ``(start, end)`` character offsets
    into the original SQL text.  Both offsets must coincide with token
    boundaries, otherwise :class:`~sqlparse.exceptions.SQLParseError` is
    raised before any formatting takes place.  The merged tokens are typed
    ``T.Protected`` so that all later filters (case conversion, comment
    stripping, reindenting, output escaping) leave their content untouched.
    """

    def __init__(self, regions):
        self.regions = tuple(regions)

    def process(self, stream):
        regions = self.regions
        idx = 0
        pos = 0
        pending = None
        pending_end = 0
        for ttype, value in stream:
            tok_start = pos
            tok_end = pos + len(value)
            pos = tok_end

            if pending is not None:
                # Inside a protected region: accumulate until its end.
                if tok_end > pending_end:
                    raise SQLParseError(
                        'Protected region must end on a token boundary')
                pending += value
                if tok_end == pending_end:
                    yield T.Protected, pending
                    pending = None
                continue

            if idx < len(regions) and tok_end > regions[idx][0]:
                start, end = regions[idx]
                idx += 1
                if tok_start != start:
                    raise SQLParseError(
                        'Protected region must start on a token boundary')
                if tok_end > end:
                    raise SQLParseError(
                        'Protected region must end on a token boundary')
                if tok_end == end:
                    yield T.Protected, value
                else:
                    pending = value
                    pending_end = end
                continue

            yield ttype, value

        if pending is not None or idx < len(regions):
            raise SQLParseError(
                'Protected region exceeds the length of the statement')
