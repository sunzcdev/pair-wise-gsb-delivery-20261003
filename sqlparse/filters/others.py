#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

import re

from sqlparse import sql
from sqlparse import tokens as T
from sqlparse.utils import split_unquoted_newlines


_HINT_TTYPES = (T.Comment.Multiline.Hint, T.Comment.Single.Hint)

#: Comment categories that can be kept individually.
KEEP_HINT = 'hint'
KEEP_ORDINARY = 'ordinary'
KEEP_CATEGORIES = (KEEP_HINT, KEEP_ORDINARY)

# Line breaks (and any trailing blanks) at the end of a removed comment.
# They mark the end of a line comment and must survive the removal, see
# issue484.
_TRAILING_BREAKS = re.compile(r'([\r\n]+) *$')


def is_comment(token):
    """Returns ``True`` for comment groups and comment tokens."""
    # TODO(andi) Comment types should be unified, see related issue38
    return isinstance(token, sql.Comment) or token.ttype in T.Comment


def is_hint(token):
    """Returns ``True`` for SQL hint tokens."""
    return token.ttype in _HINT_TTYPES


def comment_body(token):
    """Returns the text of a comment token without its delimiters.

    For line comments the line terminator is dropped as well.  For hints
    the leading ``+`` of the opener counts as a delimiter too, so ``--+x``,
    ``#+x`` and ``/*+x*/`` all have the body ``x``.  Only the lexer can
    tell a comment from a similar looking string literal, so this works on
    tokens, never on raw SQL text.
    """
    value = token.value
    if token.ttype in T.Comment.Single:
        if value.startswith('--'):
            value = value[2:]
        elif value.startswith('#'):
            value = value[1:]
        if is_hint(token) and value.startswith('+'):
            value = value[1:]
        lines = value.splitlines()
        value = lines[0] if lines else ''
    else:
        if value.startswith('/*'):
            value = value[2:]
        if is_hint(token) and value.startswith('+'):
            value = value[1:]
        if value.endswith('*/'):
            value = value[:-2]
    return value


def normalize_comment_prefix(prefix):
    """Normalizes a caller supplied marker prefix.

    Comment delimiters, surrounding whitespace and case are ignored, so
    ``'-- @etl:'``, ``'@ETL:'``, ``'/*+@etl:*/'`` all describe the same
    marker.  Returns an empty string for a prefix that carries no text.
    """
    value = prefix.strip()
    if value.startswith('--'):
        value = value[2:]
    elif value.startswith('#'):
        value = value[1:]
    elif value.startswith('/*'):
        value = value[2:]
        if value.endswith('*/'):
            value = value[:-2]
    if value.startswith('+'):
        value = value[1:]
    # Trailing parts of an explicit comment closer are not part of a marker.
    return value.strip().casefold()


def _is_blank(token):
    """Plain blanks (no line break) as a single leaf token."""
    if token is None or token.is_group:
        return False
    if not token.is_whitespace or token.is_newline:
        return False
    return not token.value.strip(' ')


def _first_leaf(token):
    """First leaf token of *token*, itself if it already is one."""
    while token is not None and token.is_group:
        if not token.tokens:
            return None
        token = token.tokens[0]
    return token


def _last_leaf(token):
    """Last leaf token of *token*, itself if it already is one."""
    while token is not None and token.is_group:
        if not token.tokens:
            return None
        token = token.tokens[-1]
    return token


def _drop_empty_whitespace(tlist):
    """Removes whitespace tokens whose value has become empty.

    A collapsed blank stays behind as an empty leaf, which would occupy a
    slot in its parent group.  Dropping those keeps group contents honest
    for the filters that run afterwards.
    """
    if not tlist.is_group:
        return
    kept = []
    changed = False
    for token in tlist.tokens:
        if token.is_group:
            _drop_empty_whitespace(token)
        elif token.is_whitespace and not token.value:
            changed = True
            continue
        kept.append(token)
    if changed:
        tlist.tokens = kept


def _refresh_values(tlist):
    """Recalculates cached group values after tokens have been removed."""
    if not tlist.is_group:
        return
    for token in tlist.tokens:
        if token.is_group:
            _refresh_values(token)
        token.parent = tlist
    tlist.value = ''.join(token.value for token in tlist.tokens)


class StripCommentsFilter:
    """Removes comments, optionally keeping selected categories.

    :param keep: comment categories that are kept.  ``'hint'`` keeps SQL
        hints (``/*+ ... */`` and ``--+ ...``), ``'ordinary'`` keeps plain
        comments.  Both can be combined.  Defaults to hints only, which
        matches the historical behaviour (issue262).
    :param prefixes: marker prefixes declared by the caller.  Comments
        whose body starts with one of them are always kept, in place.
    """

    def __init__(self, keep=(KEEP_HINT,), prefixes=()):
        self.keep = frozenset(keep)
        self.prefixes = tuple(
            prefix for prefix in map(normalize_comment_prefix, prefixes)
            if prefix)

    def _keep(self, token):
        # An explicitly declared marker wins over the category selection:
        # the caller asked for this comment to survive, whatever it is.
        if self.prefixes:
            body = comment_body(token).lstrip().casefold()
            if body.startswith(self.prefixes):
                return True
        if is_hint(token):
            return KEEP_HINT in self.keep
        return KEEP_ORDINARY in self.keep

    def _replacement(self, value, prev_leaf, next_leaf):
        """Token that replaces a removed comment, ``None`` if it can go.

        Everything in the comment value is dropped except a trailing line
        terminator: a line comment owns the newline that ends it, and the
        SQL after the comment must stay on its own line (issue484).

        Otherwise a single blank is inserted -- and only if the two
        neighbouring leaves would otherwise be glued together, which is
        what turned ``1/*c*/AS`` into ``1AS`` (issue425).
        """
        if prev_leaf is None:
            # Nothing precedes the comment, so removal cannot merge tokens.
            return None
        match = _TRAILING_BREAKS.search(value)
        if match is not None:
            return sql.Token(T.Whitespace.Newline, match.group(1))
        if next_leaf is None:
            return None
        if prev_leaf.is_whitespace or next_leaf.is_whitespace:
            return None
        if (prev_leaf.match(T.Punctuation, '(')
                or next_leaf.match(T.Punctuation, ')')):
            # A parenthesis already separates the neighbours.
            return None
        return sql.Token(T.Whitespace, ' ')

    def _strip_comment_group(self, group, prev_leaf, next_leaf):
        """Rewrites one ``sql.Comment`` group in place.

        Returns ``True`` when a comment survives, ``False`` when the group
        dissolved because everything in it was removed.  Hints and ordinary
        comments may share a single group (``/*a*/ /*+b*/``); filtering on
        the leaf level keeps a surviving hint exactly where it was instead
        of dropping the whole group and its position.

        Whitespace between the comments is kept, since a newline here is
        the terminator of a removed line comment and has to stay; blanks
        left over at the group border are trimmed because the surrounding
        whitespace already provides the separation.
        """
        if not self._keep_group_parts(group, prev_leaf, next_leaf):
            return False
        group.value = ''.join(token.value for token in group.tokens)
        for token in group.tokens:
            token.parent = group
        return True

    def _keep_group_parts(self, group, prev_leaf, next_leaf):
        """Filters the leaves of a comment group, ``False`` if none remain."""
        kept = []
        has_comment = False
        removed = False
        for token in group.tokens:
            if is_comment(token) and not isinstance(token, sql.Comment):
                if self._keep(token):
                    kept.append(token)
                    has_comment = True
                    continue
                removed = True
                # The comment's own line break is not part of its text: a
                # line comment consumed the terminator, and the SQL behind
                # it has to stay on the next line.
                match = _TRAILING_BREAKS.search(token.value)
                if match is not None:
                    kept.append(
                        sql.Token(T.Whitespace.Newline, match.group(1)))
                continue
            if token.is_group and is_comment(token):
                if self._keep_group_parts(token, prev_leaf, next_leaf):
                    token.value = ''.join(t.value for t in token.tokens)
                    kept.append(token)
                    has_comment = True
                else:
                    removed = True
                continue
            if token.is_whitespace:
                kept.append(token)
            # Anything else only occurs in malformed input; drop it.

        if not has_comment:
            group.tokens = kept
            return False

        if not removed:
            # Nothing was dropped, so the group is left exactly as it was.
            return True

        # Trim blanks at the borders: the whitespace around the group
        # already separates it from its neighbours.  A blank can only be
        # dropped if the neighbour is itself whitespace or absent.
        while kept and _is_blank(kept[-1]) and (
                next_leaf is None or next_leaf.is_whitespace):
            kept.pop()
        while kept and _is_blank(kept[0]) and (
                prev_leaf is None or prev_leaf.is_whitespace):
            kept.pop(0)

        # Collapse blanks that ended up next to each other.
        result = []
        for token in kept:
            if result and _is_blank(token) and _is_blank(result[-1]):
                continue
            result.append(token)
        group.tokens = result
        return True

    def _strip_list(self, tlist, prev_leaf=None, next_leaf=None):
        """Removes unwanted comments from *tlist*, in document order.

        *prev_leaf* and *next_leaf* are the leaf tokens surrounding the
        list; they are threaded through the recursion so a comment that
        borders a group still sees its real neighbours.  Walking up the
        parent chain instead would be quadratic on wide lists.

        The new child list is built in one pass.  Deleting from the middle
        of the old one would shift the tail once per removed comment, which
        is quadratic for input made up mostly of comments.

        Returns whether the list still carries a comment.  Only comment
        groups are ever dropped entirely, and only when nothing in them
        survived.
        """
        tokens = tlist.tokens
        count = len(tokens)

        # Leaf that follows each position, resolved in a single reverse
        # pass instead of rescanning the tail per comment.
        following = [None] * count
        upcoming = next_leaf
        for idx in range(count - 1, -1, -1):
            following[idx] = upcoming
            upcoming = _first_leaf(tokens[idx])

        filtered = []
        has_comment = False
        prev = prev_leaf
        for idx, token in enumerate(tokens):
            if is_comment(token):
                nxt = following[idx]
                if isinstance(token, sql.Comment):
                    if self._strip_comment_group(token, prev, nxt):
                        token.parent = tlist
                        filtered.append(token)
                        has_comment = True
                        prev = _last_leaf(token)
                        continue
                    # Nothing in the group survives, so it is replaced like
                    # a single removed comment: a line comment still hands
                    # its terminator to the next statement, and everything
                    # else disappears.
                    value = token.value
                elif self._keep(token):
                    filtered.append(token)
                    has_comment = True
                    prev = token
                    continue
                else:
                    value = token.value

                replacement = self._replacement(value, prev, nxt)
                if replacement is None:
                    if _is_blank(prev) and _is_blank(nxt):
                        # The comment stood between two blanks, so the two
                        # runs form a single gap now.  Clearing the trailing
                        # one keeps the gap at the position the comment had.
                        # The token can belong to a neighbouring group;
                        # emptiness is cleaned up before the next filter.
                        nxt.value = ''
                    continue
                replacement.parent = tlist
                filtered.append(replacement)
                prev = replacement
                continue

            if token.is_group:
                self._strip_list(token, prev, following[idx])
            token.parent = tlist
            filtered.append(token)
            prev = _last_leaf(token)

        tlist.tokens = filtered
        return has_comment

    def process(self, stmt):
        self._strip_list(stmt)
        _drop_empty_whitespace(stmt)
        _refresh_values(stmt)
        return stmt


class StripWhitespaceFilter:
    def _stripws(self, tlist):
        func_name = f'_stripws_{type(tlist).__name__}'
        func = getattr(self, func_name.lower(), self._stripws_default)
        func(tlist)

    @staticmethod
    def _stripws_default(tlist):
        last_was_ws = False
        is_first_char = True
        for token in tlist.tokens:
            if token.is_whitespace:
                token.value = '' if last_was_ws or is_first_char else ' '
            last_was_ws = token.is_whitespace
            is_first_char = False

    def _stripws_identifierlist(self, tlist):
        # Removes newlines before commas, see issue140
        last_nl = None
        for token in list(tlist.tokens):
            if last_nl and token.ttype is T.Punctuation and token.value == ',':
                tlist.tokens.remove(last_nl)
            last_nl = token if token.is_whitespace else None

            # next_ = tlist.token_next(token, skip_ws=False)
            # if (next_ and not next_.is_whitespace and
            #             token.ttype is T.Punctuation and token.value == ','):
            #     tlist.insert_after(token, sql.Token(T.Whitespace, ' '))
        return self._stripws_default(tlist)

    def _stripws_parenthesis(self, tlist):
        # Grouping keeps the brackets as first and last token. Don't rely on
        # that for malformed input, the indexing below would fail otherwise.
        if not (len(tlist.tokens) >= 2
                and tlist.tokens[0].match(T.Punctuation, '(')
                and tlist.tokens[-1].match(T.Punctuation, ')')):
            return self._stripws_default(tlist)
        while tlist.tokens[1].is_whitespace:
            tlist.tokens.pop(1)
        while tlist.tokens[-2].is_whitespace:
            tlist.tokens.pop(-2)
        if tlist.tokens[-2].is_group:
            # save to remove the last whitespace
            while tlist.tokens[-2].tokens[-1].is_whitespace:
                tlist.tokens[-2].tokens.pop(-1)
        self._stripws_default(tlist)

    def process(self, stmt, depth=0):
        [self.process(sgroup, depth + 1) for sgroup in stmt.get_sublists()]
        self._stripws(stmt)
        if depth == 0 and stmt.tokens and stmt.tokens[-1].is_whitespace:
            stmt.tokens.pop(-1)
        return stmt


class SpacesAroundOperatorsFilter:
    @staticmethod
    def _process(tlist):

        ttypes = (T.Operator, T.Comparison)
        tidx, token = tlist.token_next_by(t=ttypes)
        while token:
            nidx, next_ = tlist.token_next(tidx, skip_ws=False)
            if next_ and next_.ttype != T.Whitespace:
                tlist.insert_after(tidx, sql.Token(T.Whitespace, ' '))

            pidx, prev_ = tlist.token_prev(tidx, skip_ws=False)
            if prev_ and prev_.ttype != T.Whitespace:
                tlist.insert_before(tidx, sql.Token(T.Whitespace, ' '))
                tidx += 1  # has to shift since token inserted before it

            # assert tlist.token_index(token) == tidx
            tidx, token = tlist.token_next_by(t=ttypes, idx=tidx)

    def process(self, stmt):
        [self.process(sgroup) for sgroup in stmt.get_sublists()]
        SpacesAroundOperatorsFilter._process(stmt)
        return stmt


class StripTrailingSemicolonFilter:

    def process(self, stmt):
        while stmt.tokens and (stmt.tokens[-1].is_whitespace
                               or stmt.tokens[-1].value == ';'):
            stmt.tokens.pop()
        return stmt


# ---------------------------
# postprocess

class SerializerUnicode:
    @staticmethod
    def process(stmt):
        lines = split_unquoted_newlines(stmt)
        return '\n'.join(line.rstrip() for line in lines)
