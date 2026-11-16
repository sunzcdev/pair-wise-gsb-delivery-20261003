"""Error classes used by simplejson
"""
import re

from .compat import PY3, binary_type, integer_types, text_type

__all__ = [
    'JSONDecodeError', 'JSONEncodeError', 'format_path',
]

# A component is rendered with dot notation only when it looks like a plain
# ASCII identifier; everything else uses bracket notation so the path stays
# unambiguous.  Non-ASCII identifiers deliberately take the bracket form.
_IDENTIFIER = re.compile(r'[A-Za-z_][A-Za-z0-9_]*\Z')


def _safe_text(value):
    """Return ``value`` as text, or None if it has no usable text form.

    Never raises, and never calls an overridden ``__str__`` on a string
    subclass: the path is built while an unrelated serialization error is in
    flight, so hostile user code must not replace it.  Path components come
    from object keys, which are structural names rather than payload values,
    so they are safe to include.
    """
    try:
        if type(value) is text_type:
            return value
        if not PY3 and isinstance(value, binary_type):
            # Python 2 byte strings: decode ASCII where possible, otherwise
            # fall back to a lossless latin-1 view rather than dropping the
            # key entirely.
            try:
                return text_type(value, 'ascii')
            except UnicodeDecodeError:
                return text_type(value, 'latin-1')
        if isinstance(value, text_type):
            # str subclass: take the underlying buffer, not __str__.
            return text_type.__str__(value)
        return text_type(value)
    except BaseException:
        return None


def _safe_repr(value):
    try:
        return repr(value)
    except BaseException:
        return '<unrepresentable>'


def format_path(parts):
    """Format path components as a JSONPath-like string.

    ``parts`` is a sequence of ``int`` array indices and string object
    keys, in outermost-first order, describing only containers that the
    encoder had already entered.  The root value is ``'$'``; a list item
    renders as ``'[0]'``; an object member renders as ``'.name'`` when the
    key is a plain identifier and ``"['odd key']"`` otherwise.
    """
    chunks = ['$']
    for part in parts:
        if isinstance(part, integer_types) and not isinstance(part, bool):
            chunks.append('[%d]' % (part,))
            continue
        text = _safe_text(part)
        if text is None:
            chunks.append('[?]')
        elif _IDENTIFIER.match(text):
            chunks.append('.' + text)
        else:
            chunks.append('[' + _safe_repr(text) + ']')
    return ''.join(chunks)


class JSONEncodeError(TypeError, ValueError):
    """Diagnostic wrapper for a failed ``default()``/``for_json()`` call.

    The Python encoder raises this only when *diagnose_errors* is enabled
    (see :class:`JSONEncoder`).  It subclasses both :exc:`TypeError` and
    :exc:`ValueError` so that ``except TypeError``/``except ValueError``
    code written against the original failure keeps working.

    The original exception is available as :attr:`original` and as the
    implicit ``__cause__`` (a normal exception chain; it is *not* folded
    into this exception's message).

    Attributes:

    path: JSONPath-like string naming the position, e.g.
        ``'$.records[3].payload'``.
    path_parts: tuple of the raw components (``int`` indices and ``str``
        keys) that make up :attr:`path`.
    kind: ``'value'`` when converting a value failed, ``'key'`` when
        converting an object key failed.
    value_type: ``type(value).__name__`` of the object whose conversion
        failed.  Only the type name is recorded; the value itself is never
        embedded in the message.
    original: the exception raised by the conversion callback.
    complete: always ``False``.  Encoding is not atomic: when
        :meth:`JSONEncoder.iterencode` is used, chunks yielded before the
        failure are an incomplete prefix and must not be treated as a
        valid JSON document.
    prefix_emitted: ``True`` when at least one chunk had already been
        yielded by :meth:`JSONEncoder.iterencode` before the failure, so
        the consumer is holding a truncated document.  ``False`` when
        nothing was emitted (for example when the very first value fails),
        or when the exception was not raised through ``iterencode``.

    """

    def __init__(self, message, original=None, path_parts=(), kind='value',
                 value_type=None):
        TypeError.__init__(self, message)
        self.original = original
        self.path_parts = tuple(path_parts)
        self.path = format_path(self.path_parts)
        self.kind = kind
        self.value_type = value_type
        self.complete = False
        self.prefix_emitted = False
        if original is not None:
            # Written explicitly rather than with ``raise ... from`` so
            # this module still parses on Python 2.7.
            self.__cause__ = original
            self.__suppress_context__ = True

    def __reduce__(self):
        # Exception pickling drops the ``__cause__``/``__suppress_context__``
        # link, so restore them explicitly along with the diagnostic fields.
        return (_rebuild_encode_error,
                (self.args[0] if self.args else '',
                 self.original, self.path_parts, self.kind,
                 self.value_type, self.prefix_emitted))


def _rebuild_encode_error(message, original, path_parts, kind, value_type,
                          prefix_emitted):
    exc = JSONEncodeError(message, original=original, path_parts=path_parts,
                          kind=kind, value_type=value_type)
    exc.prefix_emitted = prefix_emitted
    return exc


def linecol(doc, pos):
    lineno = doc.count('\n', 0, pos) + 1
    if lineno == 1:
        colno = pos + 1
    else:
        colno = pos - doc.rindex('\n', 0, pos)
    return lineno, colno


def errmsg(msg, doc, pos, end=None):
    lineno, colno = linecol(doc, pos)
    msg = msg.replace('%r', repr(doc[pos:pos + 1]))
    if end is None:
        fmt = '%s: line %d column %d (char %d)'
        return fmt % (msg, lineno, colno, pos)
    endlineno, endcolno = linecol(doc, end)
    fmt = '%s: line %d column %d - line %d column %d (char %d - %d)'
    return fmt % (msg, lineno, colno, endlineno, endcolno, pos, end)


class JSONDecodeError(ValueError):
    """Subclass of ValueError with the following additional properties:

    msg: The unformatted error message
    doc: The JSON document being parsed
    pos: The start index of doc where parsing failed
    end: The end index of doc where parsing failed (may be None)
    lineno: The line corresponding to pos
    colno: The column corresponding to pos
    endlineno: The line corresponding to end (may be None)
    endcolno: The column corresponding to end (may be None)

    """
    # Note that this exception is used from _speedups
    def __init__(self, msg, doc, pos, end=None):
        ValueError.__init__(self, errmsg(msg, doc, pos, end=end))
        self.msg = msg
        self.doc = doc
        self.pos = pos
        self.end = end
        self.lineno, self.colno = linecol(doc, pos)
        if end is not None:
            self.endlineno, self.endcolno = linecol(doc, end)
        else:
            self.endlineno, self.endcolno = None, None

    def __reduce__(self):
        return self.__class__, (self.msg, self.doc, self.pos, self.end)
