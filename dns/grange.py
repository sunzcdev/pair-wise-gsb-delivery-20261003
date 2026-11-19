# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

# Copyright (C) 2012-2017 Nominum, Inc.
#
# Permission to use, copy, modify, and distribute this software and its
# documentation for any purpose with or without fee is hereby granted,
# provided that the above copyright notice and this permission notice
# appear in all copies.
#
# THE SOFTWARE IS PROVIDED "AS IS" AND NOMINUM DISCLAIMS ALL WARRANTIES
# WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
# MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL NOMINUM BE LIABLE FOR
# ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
# WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
# ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT
# OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

"""DNS GENERATE range conversion."""

import dns._text_util
import dns.exception


def from_text(text: str) -> tuple[int, int, int]:
    """Convert the text form of a range in a ``$GENERATE`` statement to an
    integer.

    :param text: The textual range in ``$GENERATE`` form.
    :type text: str
    :rtype: tuple[int, int, int]
    """

    start = -1
    stop = -1
    step = 1
    cur = ""
    state = 0
    # state   0   1   2
    #         x - y / z

    if text and text[0] == "-":
        raise dns.exception.SyntaxError("Start cannot be a negative number")

    def as_int() -> int:
        if cur == "":
            raise dns.exception.SyntaxError("missing $GENERATE range value")
        try:
            return int(cur)
        except ValueError:
            raise dns.exception.SyntaxError(f"invalid $GENERATE range value {cur!r}")

    for c in text:
        if c == "-" and state == 0:
            start = as_int()
            cur = ""
            state = 1
        elif c == "/":
            stop = as_int()
            cur = ""
            state = 2
        elif dns._text_util.is_ascii_digit(c):
            cur += c
        else:
            raise dns.exception.SyntaxError(f"Could not parse {c}")

    if state == 0:
        raise dns.exception.SyntaxError("no stop value specified")
    elif state == 1:
        stop = as_int()
    else:
        assert state == 2
        step = as_int()

    if step < 1:
        raise dns.exception.SyntaxError("$GENERATE step must be a positive number")
    if start < 0:
        raise dns.exception.SyntaxError("Start cannot be a negative number")
    if start > stop:
        raise dns.exception.SyntaxError("start must be <= stop")

    return (start, stop, step)


def count(start: int, stop: int, step: int) -> int:
    """Return the number of iterations a ``$GENERATE`` range will perform.

    The range is inclusive of *stop*, as it is in BIND and in
    :py:func:`dns.grange.from_text`.

    :param start: The first value of the range.
    :type start: int
    :param stop: The inclusive last value of the range.
    :type stop: int
    :param step: The increment between values; must be positive.
    :type step: int
    :rtype: int
    """
    if step < 1:
        raise dns.exception.SyntaxError("$GENERATE step must be a positive number")
    if stop < start:
        return 0
    return (stop - start) // step + 1
