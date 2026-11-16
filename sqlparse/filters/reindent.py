#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

from sqlparse import sql
from sqlparse import tokens as T
from sqlparse.utils import indent, offset


class ReindentFilter:
    def __init__(self, width=2, char=' ', wrap_after=0, n='\n',
                 comma_first=False, indent_after_first=False,
                 indent_columns=False, compact=False):
        self.n = n
        self.width = width
        self.char = char
        self.indent = 1 if indent_after_first else 0
        self.offset = 0
        self.wrap_after = wrap_after
        self.comma_first = comma_first
        self.indent_columns = indent_columns
        self.compact = compact
        self._curr_stmt = None
        self._last_stmt = None
        self._last_func = None

    @property
    def leading_ws(self):
        return self.offset + self.indent * self.width

    def _current_line_len(self, token):
        """Returns the width of what's already emitted on *token*'s line.

        The tokens preceding *token* are visited last one first, so the walk
        stops at the line break that starts the current line.  Rebuilding the
        statement prefix from its start instead made every caller
        O(statement), and the callers running once per group (tuple lists,
        identifier lists) quadratic in the number of groups -- a CPU
        exhaustion vector (GHSA-cfqr-cjx5-5jcm).  The walk is inlined and
        counts characters rather than collecting them: both matter, since a
        line without any break still has to be measured token by token.
        """
        length = 0
        node = token
        while node is not self._curr_stmt and node.parent is not None:
            parent = node.parent
            # ``Token`` doesn't implement ``__eq__``, so ``index()`` is an
            # identity lookup and safe against tokens sharing a value.
            stack = parent.tokens[:parent.tokens.index(node)]
            while stack:
                prev_ = stack.pop()
                if prev_.is_group:
                    stack.extend(prev_.tokens)
                    continue

                value = prev_.value
                size = len(value)
                if not size:
                    continue
                lines = value.splitlines()
                if len(lines) == 1 and len(lines[0]) == size:
                    # No break in here.  ``splitlines()`` hands back the value
                    # itself in that case, so this costs a scan but no copy --
                    # which is what keeps a long break-free line affordable.
                    length += size
                    continue

                # ``value`` holds the break that starts the current line.  The
                # sentinel keeps a trailing break from collapsing, so ``lines``
                # always has one entry more than the number of breaks.
                lines = (value + '.').splitlines()
                tail = len(lines[-1]) - 1 + length
                if tail:
                    return tail
                # Nothing but a break to our right, and ``splitlines()`` drops
                # that empty line -- so the line to measure is the one before.
                if len(lines) > 2:
                    return len(lines[-2])
                length = len(lines[0])
            node = parent
        return length

    def _get_offset(self, token):
        # Now take current offset into account and return relative offset.
        return self._current_line_len(token) - len(self.char * self.leading_ws)

    def nl(self, offset=0):
        return sql.Token(
            T.Whitespace,
            self.n + self.char * max(0, self.leading_ws + offset))

    def _next_token(self, tlist, idx=-1):
        split_words = ('FROM', 'STRAIGHT_JOIN$', 'JOIN$', 'AND', 'OR',
                       'GROUP BY', 'ORDER BY', 'UNION', 'VALUES',
                       'SET', 'BETWEEN', 'EXCEPT', 'HAVING', 'LIMIT')
        m_split = T.Keyword, split_words, True
        tidx, token = tlist.token_next_by(m=m_split, idx=idx)

        if token and token.normalized == 'BETWEEN':
            tidx, token = self._next_token(tlist, tidx)

            if token and token.normalized == 'AND':
                tidx, token = self._next_token(tlist, tidx)

        return tidx, token

    def _split_kwds(self, tlist):
        tidx, token = self._next_token(tlist)
        while token:
            pidx, prev_ = tlist.token_prev(tidx, skip_ws=False)
            uprev = str(prev_)

            if prev_ and prev_.is_whitespace:
                del tlist.tokens[pidx]
                tidx -= 1

            if not (uprev.endswith('\n') or uprev.endswith('\r')):
                tlist.insert_before(tidx, self.nl())
                tidx += 1

            tidx, token = self._next_token(tlist, tidx)

    def _split_statements(self, tlist):
        ttypes = T.Keyword.DML, T.Keyword.DDL
        tidx, token = tlist.token_next_by(t=ttypes)
        while token:
            pidx, prev_ = tlist.token_prev(tidx, skip_ws=False)
            if prev_ and prev_.is_whitespace:
                del tlist.tokens[pidx]
                tidx -= 1
            # only break if it's not the first token
            if prev_:
                tlist.insert_before(tidx, self.nl())
                tidx += 1
            tidx, token = tlist.token_next_by(t=ttypes, idx=tidx)

    def _process(self, tlist):
        func_name = f'_process_{type(tlist).__name__}'
        func = getattr(self, func_name.lower(), self._process_default)
        func(tlist)

    def _process_where(self, tlist):
        tidx, token = tlist.token_next_by(m=(T.Keyword, 'WHERE'))
        if not token:
            return
        # issue121, errors in statement fixed??
        tlist.insert_before(tidx, self.nl())
        with indent(self):
            self._process_default(tlist)

    def _process_parenthesis(self, tlist):
        ttypes = T.Keyword.DML, T.Keyword.DDL
        _, is_dml_dll = tlist.token_next_by(t=ttypes)
        fidx, first = tlist.token_next_by(m=sql.Parenthesis.M_OPEN)
        if first is None:
            return

        with indent(self, 1 if is_dml_dll else 0):
            tlist.tokens.insert(0, self.nl()) if is_dml_dll else None
            with offset(self, self._get_offset(first) + 1):
                self._process_default(tlist, not is_dml_dll)

    def _process_function(self, tlist):
        self._last_func = tlist[0]
        self._process_default(tlist)

    def _process_identifierlist(self, tlist):
        identifiers = list(tlist.get_identifiers())
        if self.indent_columns:
            first = next(identifiers[0].flatten())
            num_offset = 1 if self.char == '\t' else self.width
        else:
            first = next(identifiers.pop(0).flatten())
            num_offset = 1 if self.char == '\t' else self._get_offset(first)

        if not tlist.within(sql.Function) and not tlist.within(sql.Values):
            with offset(self, num_offset):
                position = 0
                for token in identifiers:
                    # Add 1 for the "," separator
                    position += len(token.value) + 1
                    if position > (self.wrap_after - self.offset):
                        adjust = 0
                        if self.comma_first:
                            adjust = -2
                            _, comma = tlist.token_prev(
                                tlist.token_index(token))
                            if comma is None:
                                continue
                            token = comma
                        tlist.insert_before(token, self.nl(offset=adjust))
                        if self.comma_first:
                            _, ws = tlist.token_next(
                                tlist.token_index(token), skip_ws=False)
                            if (ws is not None
                                    and ws.ttype is not T.Text.Whitespace):
                                tlist.insert_after(
                                    token, sql.Token(T.Whitespace, ' '))
                        position = 0
        else:
            # ensure whitespace
            for token in tlist:
                _, next_ws = tlist.token_next(
                    tlist.token_index(token), skip_ws=False)
                if token.value == ',' and not next_ws.is_whitespace:
                    tlist.insert_after(
                        token, sql.Token(T.Whitespace, ' '))

            end_at = self.offset + sum(len(i.value) + 1 for i in identifiers)
            adjusted_offset = 0
            if (self.wrap_after > 0
                    and end_at > (self.wrap_after - self.offset)
                    and self._last_func):
                adjusted_offset = -len(self._last_func.value) - 1

            with offset(self, adjusted_offset), indent(self):
                if adjusted_offset < 0:
                    tlist.insert_before(identifiers[0], self.nl())
                position = 0
                for token in identifiers:
                    # Add 1 for the "," separator
                    position += len(token.value) + 1
                    if (self.wrap_after > 0
                            and position > (self.wrap_after - self.offset)):
                        adjust = 0
                        tlist.insert_before(token, self.nl(offset=adjust))
                        position = 0
        self._process_default(tlist)

    def _process_case(self, tlist):
        iterable = iter(tlist.get_cases())
        cond, _ = next(iterable)
        first = next(cond[0].flatten())

        with offset(self, self._get_offset(tlist[0])):
            with offset(self, self._get_offset(first)):
                for cond, value in iterable:
                    str_cond = ''.join(str(x) for x in cond or [])
                    str_value = ''.join(str(x) for x in value)
                    end_pos = self.offset + 1 + len(str_cond) + len(str_value)
                    if (not self.compact and end_pos > self.wrap_after):
                        token = value[0] if cond is None else cond[0]
                        tlist.insert_before(token, self.nl())

                # Line breaks on group level are done. let's add an offset of
                # len "when ", "then ", "else "
                with offset(self, len("WHEN ")):
                    self._process_default(tlist)
            end_idx, end = tlist.token_next_by(m=sql.Case.M_CLOSE)
            if end_idx is not None and not self.compact:
                tlist.insert_before(end_idx, self.nl())

    def _line_len_before(self, tokens, base):
        """Returns the width of the line in front of ``tokens[-1]``.

        This is :meth:`_current_line_len` applied to the last element of
        *tokens*: values are visited from the right and the walk stops at
        the line break that starts the current line.  The caller passes the
        children emitted so far, so no ``list.index()`` lookup is needed;
        *base* is the width in front of the first child, which the walk
        would reach through the parent chain.
        """
        length = 0
        stack = [(tokens, len(tokens) - 1)]
        while stack:
            seq, pos = stack[-1]
            if pos < 0:
                stack.pop()
                continue
            token = seq[pos]
            stack[-1] = (seq, pos - 1)

            if token.is_group:
                stack.append((token.tokens, len(token.tokens) - 1))
                continue

            value = token.value
            size = len(value)
            if not size:
                continue
            lines = value.splitlines()
            if len(lines) == 1 and len(lines[0]) == size:
                length += size
                continue

            # ``value`` holds the break that starts the current line.  The
            # sentinel keeps a trailing break from collapsing, so ``lines``
            # always has one entry more than the number of breaks.
            lines = (value + '.').splitlines()
            tail = len(lines[-1]) - 1 + length
            if tail:
                return tail
            # Nothing but a break to our right, and ``splitlines()`` drops
            # that empty line -- so the line to measure is the one before.
            if len(lines) > 2:
                return len(lines[-2])
            length = len(lines[0])
        return base + length

    def _process_values(self, tlist):
        """Lay out a ``VALUES`` list with one row per line.

        Every row but the first starts on its own line, under the row that
        shares its comma.  The width of that line used to be measured per
        row, and the measurement located the row with ``list.index()`` on
        the group's children -- ``insert_after()`` repeated that lookup for
        the comma.  Laying out n rows therefore walked the child list n
        times, so formatting an export with many thousands of ``VALUES``
        rows spent most of its time rescanning rows that were already laid
        out.

        The children are now rebuilt in a single pass.  A row usually
        starts right behind a break this filter inserted, so the width of
        its line is known and can be followed forward with one addition per
        token.  A value that carries a break of its own -- a multiline
        literal, a line comment between two rows -- changes what the
        measurement reports, so from there the width is measured with
        ``_line_len_before()`` over the children emitted so far, until an
        inserted break starts a new line and the forward count is known
        again.  That reproduces the previous output exactly without paying
        the ``list.index()`` lookups.  The comma of a row comes from a
        forward-only scan -- a row without a comma of its own shares a
        later one and the scan never moves back.
        """
        tlist.insert_before(0, self.nl())
        tokens = tlist.tokens
        size = len(tokens)
        first = next((idx for idx, token in enumerate(tokens)
                      if isinstance(token, sql.Parenthesis)), None)
        if first is None:
            return

        leading = len(self.char * self.leading_ws)
        # Only the width in front of the first row has to be measured; it
        # includes whatever the statement put there.
        first_line = self._current_line_len(tokens[first])
        first_column = first_line
        if self.comma_first:
            # Breaks go in front of the comma, in line with the first row
            # less the ", " they sit next to.
            first_column -= 2

        # The comma whose break follows each row.  A row without a comma of
        # its own shares the comma of a later row; the code inserted a
        # break for it there too, so several breaks can share a comma.
        breaks = {}
        comma_of = {}
        comma = first
        for idx in range(first, size):
            if not isinstance(tokens[idx], sql.Parenthesis):
                continue
            comma = max(comma, idx + 1)
            while comma < size and not tokens[comma].match(T.Punctuation, ','):
                comma += 1
            if comma == size:
                # No comma left, so no later row has one either.
                break
            comma_of[idx] = comma
            if self.comma_first:
                # All breaks in front of a comma are at the same column.
                breaks.setdefault(comma, []).append(first_column - leading)

        # ``width`` is the width of the line up to the children emitted so
        # far, or ``None`` when a value with a break of its own made it
        # unknown; then the line is measured as before.
        width = None
        laid_out = []
        idx = 0
        while idx < size:
            token = tokens[idx]
            if self.comma_first and idx in breaks:
                for indent in breaks[idx]:
                    token = self.nl(indent)
                    laid_out.append(token)
                    width = leading + indent
            if idx in comma_of and not self.comma_first:
                if width is None:
                    column = self._line_len_before(laid_out, first_column)
                else:
                    column = width
                breaks.setdefault(comma_of[idx], []).append(column - leading)
            laid_out.append(tokens[idx])
            if width is not None:
                value = tokens[idx].value
                if '\n' in value or '\r' in value:
                    width = None
                else:
                    width += len(value)
            idx += 1
            if not self.comma_first and idx - 1 in breaks:
                # ``insert_after(..., skip_ws=True)`` put the breaks behind
                # the whitespace following the comma; keep that order and
                # let the serializer strip the resulting trailing space.
                indents = breaks[idx - 1]
                while idx < size and tokens[idx].is_whitespace:
                    laid_out.append(tokens[idx])
                    idx += 1
                for indent in indents:
                    laid_out.append(self.nl(indent))
                    width = leading + indent

        for token in laid_out:
            token.parent = tlist
        tlist.tokens = laid_out

    def _process_default(self, tlist, stmts=True):
        self._split_statements(tlist) if stmts else None
        self._split_kwds(tlist)
        for sgroup in tlist.get_sublists():
            self._process(sgroup)

    def process(self, stmt):
        self._curr_stmt = stmt
        self._process(stmt)

        if self._last_stmt is not None:
            nl = '\n' if str(self._last_stmt).endswith('\n') else '\n\n'
            stmt.tokens.insert(0, sql.Token(T.Whitespace, nl))

        self._last_stmt = stmt
        return stmt
