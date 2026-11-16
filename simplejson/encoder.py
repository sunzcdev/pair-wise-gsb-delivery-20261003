"""Implementation of JSONEncoder
"""
from __future__ import absolute_import
import re
from operator import itemgetter
# Do not import Decimal directly to avoid reload issues
import decimal
import sys
from .compat import binary_type, text_type, string_types, integer_types, PY3
from .errors import JSONEncodeError, format_path

# PEP 678 add_note() is available on Python 3.11+
_HAS_ADD_NOTE = sys.version_info >= (3, 11)


def _safe_type_name(obj):
    """Return ``type(obj).__name__`` without letting it raise."""
    try:
        return type(obj).__name__
    except BaseException:
        return '?'


def _conversion_failure(exc, kind, value, path_parts, detail=None):
    """Build the optional location-diagnostic error for a failed conversion.

    ``kind`` is ``'value'`` for ``default()``/``for_json()``/``_asdict()``
    failures and ``'key'`` for object-key conversion failures.  ``path_parts``
    names only containers the encoder had already entered, so the message
    never re-runs a conversion callback to recover position information.

    Only type names are embedded -- never ``repr(value)`` -- so a payload
    that happens to hold credentials, tokens or other sensitive data does
    not leak into the exception, its notes, or a traceback log.
    """
    if isinstance(exc, JSONEncodeError):
        # Already annotated by an inner level; keep the innermost path.
        return exc
    value_type = _safe_type_name(value)
    subject = ('key of type %s' if kind == 'key' else 'value of type %s') % (
        value_type,)
    if detail is None:
        detail = '%s raised' % (_safe_type_name(exc),)
    message = (
        'cannot serialize %s at %s: %s (encoding is not atomic; any prefix '
        'already yielded by iterencode() is an incomplete document)'
        % (subject, format_path(path_parts), detail))
    return JSONEncodeError(message, original=exc, path_parts=path_parts,
                           kind=kind, value_type=value_type)


def _annotate_exception(exc, note_format, obj, *args):
    if _HAS_ADD_NOTE:
        try:
            exc.add_note(note_format % ((type(obj).__name__,) + args))
        except BaseException:
            # Diagnostic failures must not replace the serialization error.
            pass


def _checked_iterencode(chunks):
    """Track whether any output was emitted before a diagnostic failure.

    ``JSONEncodeError.complete`` is always false; this wrapper lets a
    consumer that was streaming through ``iterencode()`` tell a truncated,
    never-complete document (``prefix_emitted``) from a failure that
    happened before the first chunk.
    """
    emitted = False
    try:
        for chunk in chunks:
            emitted = True
            yield chunk
    except JSONEncodeError as exc:
        exc.prefix_emitted = emitted
        raise


def _import_speedups():
    try:
        from . import _speedups
        return (_speedups.encode_basestring_ascii,
                _speedups.encode_basestring,
                _speedups.make_encoder)
    except ImportError:
        return None, None, None
c_encode_basestring_ascii, c_encode_basestring, c_make_encoder = (
    _import_speedups())

from .decoder import PosInf
from .raw_json import RawJSON

ESCAPE = re.compile(r'[\x00-\x1f\\"]')
ESCAPE_ASCII = re.compile(r'([\\"]|[^\ -~])')
HAS_UTF8 = re.compile(r'[\x80-\xff]')
ESCAPE_DCT = {
    '\\': '\\\\',
    '"': '\\"',
    '\b': '\\b',
    '\f': '\\f',
    '\n': '\\n',
    '\r': '\\r',
    '\t': '\\t',
}
for i in range(0x20):
    ESCAPE_DCT.setdefault(chr(i), '\\u%04x' % (i,))
del i

FLOAT_REPR = repr

_NAN = float('nan')
_INFINITY = float('inf')


def _encode_decimal(d, _floatstr):
    """Return the JSON representation of a ``Decimal``.

    Finite Decimals are stringified as-is, preserving precision and trailing
    zeros.  Non-finite Decimals (``NaN``, ``sNaN``, ``Infinity``) are routed
    through the same ``allow_nan``/``ignore_nan`` handling as floats so they
    never emit invalid JSON (see #149).

    ``str(Decimal)`` yields a leading ``-`` sign at most (never ``+``); a
    finite value's first significant character is always a digit, while a
    non-finite value's is a letter (``N``, ``s`` or ``I``).  So a single
    ``str()`` plus a one/two character check distinguishes the two.
    """
    s = str(d)
    c0 = s[:1]
    c = s[1:2] if c0 == '-' else c0
    if '0' <= c <= '9':
        # First significant character is a digit: finite Decimal.
        return s
    # Non-finite Decimal. ``N``/``s`` -> NaN (float(Decimal('sNaN')) raises,
    # so map both quiet and signaling NaN to a plain NaN); ``I`` -> Infinity.
    if c == 'I':
        return _floatstr(-_INFINITY if c0 == '-' else _INFINITY)
    return _floatstr(_NAN)

# dict-like types that should be encoded as JSON objects.
# frozendict is a builtin added in CPython 3.15 (PEP 814).
if sys.version_info >= (3, 15):
    _dict_types = (dict, frozendict)
else:
    _dict_types = dict

def py_encode_basestring(s, _PY3=PY3, _q=u'"'):
    """Return a JSON representation of a Python string

    """
    if _PY3:
        if isinstance(s, bytes):
            s = str(s, 'utf-8')
        elif type(s) is not str:
            # convert an str subclass instance to exact str
            # raise a TypeError otherwise
            s = str.__str__(s)
    else:
        if isinstance(s, str) and HAS_UTF8.search(s) is not None:
            s = unicode(s, 'utf-8')
        elif type(s) not in (str, unicode):
            # convert an str subclass instance to exact str
            # convert a unicode subclass instance to exact unicode
            # raise a TypeError otherwise
            if isinstance(s, str):
                s = str.__str__(s)
            else:
                s = unicode.__getnewargs__(s)[0]
    def replace(match):
        return ESCAPE_DCT[match.group(0)]
    return _q + ESCAPE.sub(replace, s) + _q


def py_encode_basestring_ascii(s, _PY3=PY3):
    """Return an ASCII-only JSON representation of a Python string

    """
    if _PY3:
        if isinstance(s, bytes):
            s = str(s, 'utf-8')
        elif type(s) is not str:
            # convert an str subclass instance to exact str
            # raise a TypeError otherwise
            s = str.__str__(s)
    else:
        if isinstance(s, str) and HAS_UTF8.search(s) is not None:
            s = unicode(s, 'utf-8')
        elif type(s) not in (str, unicode):
            # convert an str subclass instance to exact str
            # convert a unicode subclass instance to exact unicode
            # raise a TypeError otherwise
            if isinstance(s, str):
                s = str.__str__(s)
            else:
                s = unicode.__getnewargs__(s)[0]
    def replace(match):
        s = match.group(0)
        try:
            return ESCAPE_DCT[s]
        except KeyError:
            n = ord(s)
            if n < 0x10000:
                return '\\u%04x' % (n,)
            else:
                # surrogate pair
                n -= 0x10000
                s1 = 0xd800 | ((n >> 10) & 0x3ff)
                s2 = 0xdc00 | (n & 0x3ff)
                return '\\u%04x\\u%04x' % (s1, s2)
    return '"' + str(ESCAPE_ASCII.sub(replace, s)) + '"'


encode_basestring_ascii = (
    c_encode_basestring_ascii or py_encode_basestring_ascii)

encode_basestring = (
    c_encode_basestring or py_encode_basestring)

class JSONEncoder(object):
    """Extensible JSON <http://json.org> encoder for Python data structures.

    Supports the following objects and types by default:

    +-------------------+---------------+
    | Python            | JSON          |
    +===================+===============+
    | dict, namedtuple  | object        |
    +-------------------+---------------+
    | list, tuple       | array         |
    +-------------------+---------------+
    | str, unicode      | string        |
    +-------------------+---------------+
    | int, long, float  | number        |
    +-------------------+---------------+
    | True              | true          |
    +-------------------+---------------+
    | False             | false         |
    +-------------------+---------------+
    | None              | null          |
    +-------------------+---------------+

    To extend this to recognize other objects, subclass and implement a
    ``.default()`` method with another method that returns a serializable
    object for ``o`` if possible, otherwise it should call the superclass
    implementation (to raise ``TypeError``).

    """
    item_separator = ', '
    key_separator = ': '

    def __init__(self, skipkeys=False, ensure_ascii=True,
                 check_circular=True, allow_nan=False, sort_keys=False,
                 indent=None, separators=None, encoding='utf-8', default=None,
                 use_decimal=True, namedtuple_as_object=True,
                 tuple_as_array=True, bigint_as_string=False,
                 item_sort_key=None, for_json=False, ignore_nan=False,
                 int_as_string_bitcount=None, iterable_as_array=False,
                 diagnose_errors=False):
        """Constructor for JSONEncoder, with sensible defaults.

        If skipkeys is false, then it is a TypeError to attempt
        encoding of keys that are not str, int, long, float or None.  If
        skipkeys is True, such items are simply skipped.

        If ensure_ascii is true, the output is guaranteed to be str
        objects with all incoming unicode characters escaped.  If
        ensure_ascii is false, the output will be unicode object.

        If check_circular is true, then lists, dicts, iterables encoded with
        iterable_as_array, and custom encoded objects will be checked for
        circular references during encoding to prevent an infinite recursion
        (which would cause a RecursionError, or RuntimeError on Python 2).
        Otherwise, no such check takes place.

        If allow_nan is true (default: False), then out of range float
        values (nan, inf, -inf) will be serialized to
        their JavaScript equivalents (NaN, Infinity, -Infinity)
        instead of raising a ValueError. See
        ignore_nan for ECMA-262 compliant behavior.

        If sort_keys is true, then the output of dictionaries will be
        sorted by key; this is useful for regression tests to ensure
        that JSON serializations can be compared on a day-to-day basis.

        If indent is a string, then JSON array elements and object members
        will be pretty-printed with a newline followed by that string repeated
        for each level of nesting. ``None`` (the default) selects the most compact
        representation without any newlines. For backwards compatibility with
        versions of simplejson earlier than 2.1.0, an integer is also accepted
        and is converted to a string with that many spaces.

        If specified, separators should be an (item_separator, key_separator)
        tuple.  The default is (', ', ': ') if *indent* is ``None`` and
        (',', ': ') otherwise.  To get the most compact JSON representation,
        you should specify (',', ':') to eliminate whitespace.

        If specified, default is a function that gets called for objects
        that can't otherwise be serialized.  It should return a JSON encodable
        version of the object or raise a ``TypeError``.

        If encoding is not None, then all input strings will be
        transformed into unicode using that encoding prior to JSON-encoding.
        The default is UTF-8.

        If use_decimal is true (default: ``True``), ``decimal.Decimal`` will
        be supported directly by the encoder. For the inverse, decode JSON
        with ``parse_float=decimal.Decimal``.

        If namedtuple_as_object is true (the default), objects with
        ``_asdict()`` methods will be encoded as JSON objects.

        If tuple_as_array is true (the default), tuple (and subclasses) will
        be encoded as JSON arrays.

        If *iterable_as_array* is true (default: ``False``),
        any object not in the above table that implements ``__iter__()``
        will be encoded as a JSON array.

        If bigint_as_string is true (not the default), ints 2**53 and higher
        or lower than -2**53 will be encoded as strings. This is to avoid the
        rounding that happens in Javascript otherwise.

        If int_as_string_bitcount is a positive number (n), then int of size
        greater than or equal to 2**n or lower than or equal to -2**n will be
        encoded as strings.

        If specified, item_sort_key is a callable used to sort the items in
        each dictionary. This is useful if you want to sort items other than
        in alphabetical order by key.

        If for_json is true (not the default), objects with a ``for_json()``
        method will use the return value of that method for encoding as JSON
        instead of the object.

        If *ignore_nan* is true (default: ``False``), then out of range
        :class:`float` values (``nan``, ``inf``, ``-inf``) will be serialized
        as ``null`` in compliance with the ECMA-262 specification. If true,
        this will override *allow_nan*.

        If *diagnose_errors* is true (default: ``False``), a failure raised
        by ``default()``, ``for_json()``, ``_asdict()`` or object-key
        conversion is reported as a :exc:`~simplejson.errors.JSONEncodeError`
        that names the position in the structure (for example
        ``$.records[3].payload``) and keeps the original exception as its
        ``__cause__``. This is a pure-Python encoder feature; the C speedups
        do not implement it, so the encoder falls back to the Python
        implementation when it is requested. Only the path and the type
        names of the offending value are recorded -- never the value itself.
        Because ``iterencode()`` streams, the exception does not imply the
        output was all-or-nothing: see :attr:`JSONEncodeError.complete`.

        """

        self.skipkeys = skipkeys
        self.ensure_ascii = ensure_ascii
        self.check_circular = check_circular
        self.allow_nan = allow_nan
        self.sort_keys = sort_keys
        self.use_decimal = use_decimal
        self.namedtuple_as_object = namedtuple_as_object
        self.tuple_as_array = tuple_as_array
        self.iterable_as_array = iterable_as_array
        self.bigint_as_string = bigint_as_string
        self.item_sort_key = item_sort_key
        self.for_json = for_json
        self.ignore_nan = ignore_nan
        self.int_as_string_bitcount = int_as_string_bitcount
        self.diagnose_errors = diagnose_errors
        if indent is not None and not isinstance(indent, string_types):
            indent = indent * ' '
        self.indent = indent
        if separators is not None:
            self.item_separator, self.key_separator = separators
        elif indent is not None:
            self.item_separator = ','
        if default is not None:
            self.default = default
        self.encoding = encoding

    def default(self, o):
        """Implement this method in a subclass such that it returns
        a serializable object for ``o``, or calls the base implementation
        (to raise a ``TypeError``).

        For example, to support arbitrary iterators, you could
        implement default like this::

            def default(self, o):
                try:
                    iterable = iter(o)
                except TypeError:
                    pass
                else:
                    return list(iterable)
                return JSONEncoder.default(self, o)

        """
        raise TypeError('Object of type %s is not JSON serializable' %
                        o.__class__.__name__)

    def encode(self, o):
        """Return a JSON string representation of a Python data structure.

        >>> from simplejson import JSONEncoder
        >>> JSONEncoder().encode({"foo": ["bar", "baz"]})
        '{"foo": ["bar", "baz"]}'

        """
        # This is for extremely simple cases and benchmarks.
        if isinstance(o, binary_type):
            _encoding = self.encoding
            if (_encoding is not None and not (_encoding == 'utf-8')):
                o = text_type(o, _encoding)
        if isinstance(o, string_types):
            if self.ensure_ascii:
                return encode_basestring_ascii(o)
            else:
                return encode_basestring(o)
        # This doesn't pass the iterator directly to ''.join() because the
        # exceptions aren't as detailed.  The list call should be roughly
        # equivalent to the PySequence_Fast that ''.join() would do.
        chunks = self.iterencode(o)
        if not isinstance(chunks, (list, tuple)):
            try:
                chunks = list(chunks)
            except JSONEncodeError as exc:
                # Nothing was returned to the caller, so no partial document
                # escaped even though the generator had emitted chunks.
                exc.prefix_emitted = False
                raise
        if self.ensure_ascii:
            return ''.join(chunks)
        else:
            return u''.join(chunks)

    def iterencode(self, o):
        """Encode the given object and yield each string
        representation as available.

        For example::

            for chunk in JSONEncoder().iterencode(bigobject):
                mysocket.write(chunk)

        """
        if self.check_circular:
            markers = {}
        else:
            markers = None
        if self.ensure_ascii:
            _encoder = encode_basestring_ascii
        else:
            _encoder = encode_basestring
        if self.encoding != 'utf-8' and self.encoding is not None:
            def _encoder(o, _orig_encoder=_encoder, _encoding=self.encoding):
                if isinstance(o, binary_type):
                    o = text_type(o, _encoding)
                return _orig_encoder(o)

        def floatstr(o, allow_nan=self.allow_nan, ignore_nan=self.ignore_nan,
                _repr=FLOAT_REPR, _inf=PosInf, _neginf=-PosInf):
            # Check for specials. Note that this type of test is processor
            # and/or platform-specific, so do tests which don't depend on
            # the internals.

            if o != o:
                text = 'NaN'
            elif o == _inf:
                text = 'Infinity'
            elif o == _neginf:
                text = '-Infinity'
            else:
                if type(o) != float:
                    # See #118, do not trust custom str/repr
                    o = float(o)
                return _repr(o)

            if ignore_nan:
                text = 'null'
            elif not allow_nan:
                raise ValueError(
                    "Out of range float values are not JSON compliant: " +
                    repr(o))

            return text

        key_memo = {}
        int_as_string_bitcount = (
            53 if self.bigint_as_string else self.int_as_string_bitcount)
        # The C encoder has no location tracking, so requesting diagnostics
        # must not silently lose them by taking the accelerated path.
        if c_make_encoder is not None and not self.diagnose_errors:
            _iterencode = c_make_encoder(
                markers, self.default, _encoder, self.indent,
                self.key_separator, self.item_separator, self.sort_keys,
                self.skipkeys, self.allow_nan, key_memo, self.use_decimal,
                self.namedtuple_as_object, self.tuple_as_array,
                int_as_string_bitcount,
                self.item_sort_key, self.encoding, self.for_json,
                self.ignore_nan, decimal.Decimal, self.iterable_as_array)
        else:
            _iterencode = _make_iterencode(
                markers, self.default, _encoder, self.indent, floatstr,
                self.key_separator, self.item_separator, self.sort_keys,
                self.skipkeys, self.use_decimal,
                self.namedtuple_as_object, self.tuple_as_array,
                int_as_string_bitcount,
                self.item_sort_key, self.encoding, self.for_json,
                self.iterable_as_array, Decimal=decimal.Decimal,
                _diagnose_errors=self.diagnose_errors)
        try:
            if self.diagnose_errors:
                return _checked_iterencode(_iterencode(o, 0, _path=()))
            return _iterencode(o, 0)
        finally:
            key_memo.clear()


class JSONEncoderForHTML(JSONEncoder):
    """An encoder that produces JSON safe to embed in HTML.

    To embed JSON content in, say, a script tag on a web page, the
    characters &, < and > should be escaped. They cannot be escaped
    with the usual entities (e.g. &amp;) because they are not expanded
    within <script> tags.

    This class also escapes the line separator and paragraph separator
    characters U+2028 and U+2029, irrespective of the ensure_ascii setting,
    as these characters are not valid in JavaScript strings (see
    http://timelessrepo.com/json-isnt-a-javascript-subset).
    """

    def encode(self, o):
        # Override JSONEncoder.encode because it has hacks for
        # performance that make things more complicated.
        chunks = self.iterencode(o)
        try:
            if self.ensure_ascii:
                return ''.join(chunks)
            else:
                return u''.join(chunks)
        except JSONEncodeError as exc:
            # The joined result is never returned, so nothing partial
            # reached the caller.
            exc.prefix_emitted = False
            raise

    def iterencode(self, o):
        chunks = super(JSONEncoderForHTML, self).iterencode(o)
        for chunk in chunks:
            chunk = chunk.replace('&', '\\u0026')
            chunk = chunk.replace('<', '\\u003c')
            chunk = chunk.replace('>', '\\u003e')

            if not self.ensure_ascii:
                chunk = chunk.replace(u'\u2028', '\\u2028')
                chunk = chunk.replace(u'\u2029', '\\u2029')

            yield chunk


def _make_iterencode(markers, _default, _encoder, _indent, _floatstr,
        _key_separator, _item_separator, _sort_keys, _skipkeys,
        _use_decimal, _namedtuple_as_object, _tuple_as_array,
        _int_as_string_bitcount, _item_sort_key,
        _encoding,_for_json,
        _iterable_as_array,
        _diagnose_errors=False,
        ## HACK: hand-optimized bytecode; turn globals into locals
        _PY3=PY3,
        ValueError=ValueError,
        string_types=string_types,
        Decimal=None,
        dict=dict,
        _dict_types=_dict_types,
        float=float,
        id=id,
        integer_types=integer_types,
        isinstance=isinstance,
        list=list,
        str=str,
        tuple=tuple,
        iter=iter,
    ):
    if _use_decimal and Decimal is None:
        Decimal = decimal.Decimal
    if _item_sort_key is not None and not callable(_item_sort_key):
        raise TypeError("item_sort_key must be None or callable")
    elif _sort_keys and _item_sort_key is None:
        _item_sort_key = itemgetter(0)

    if (_int_as_string_bitcount is not None and
        (_int_as_string_bitcount <= 0 or
         not isinstance(_int_as_string_bitcount, integer_types))):
        raise TypeError("int_as_string_bitcount must be a positive integer")

    # Interpreter-level exceptions are never converted into a diagnostic;
    # wrapping them would hide Ctrl-C / shutdown from the caller.
    _unwrappable = (KeyboardInterrupt, SystemExit, GeneratorExit)

    def _diagnose(exc, kind, value, path):
        # Return a JSONEncodeError to raise instead of ``exc``, or None to
        # let ``exc`` propagate untouched.  ``path`` is None when location
        # tracking is off, which also keeps the hot path tuple-free.
        if not _diagnose_errors or isinstance(exc, _unwrappable):
            return None
        return _conversion_failure(exc, kind, value, path or ())

    def _asdict_error(value, returned, path):
        # _asdict() returned something that is not a dict.  The callback
        # itself succeeded, so the error is built here rather than caught.
        message = '_asdict() must return a dict, not %s' % (
            _safe_type_name(returned),)
        if path is None or not _diagnose_errors:
            return TypeError(message)
        return _conversion_failure(
            TypeError(message), 'value', value, path, detail=message)

    def call_method(obj, method_name, path=None):
        method = getattr(obj, method_name, None)
        if callable(method):
            try:
                return (method(),)
            except TypeError as exc:
                # JSONEncodeError subclasses TypeError (so existing
                # handlers keep working) but means "a nested encode failed",
                # not "this protocol is unsupported".  Re-raise it instead
                # of falling through to _asdict()/default().
                if _diagnose_errors and isinstance(exc, JSONEncodeError):
                    raise
            except BaseException as exc:
                replacement = _diagnose(exc, 'value', obj, path)
                if replacement is None:
                    raise
                raise replacement
        return None

    def _encode_int(value):
        skip_quoting = (
            _int_as_string_bitcount is None
            or
            _int_as_string_bitcount < 1
        )
        if type(value) not in integer_types:
            # See #118, do not trust custom str/repr
            value = int(value)
        if (
            skip_quoting or
            value.bit_length() <= _int_as_string_bitcount
        ):
            return str(value)
        return '"' + str(value) + '"'

    def _iterencode_method(obj, result, _current_indent_level, encode,
                           _path=None):
        if markers is not None:
            markerid = id(obj)
            if markerid in markers:
                raise ValueError("Circular reference detected")
            markers[markerid] = obj
        try:
            for chunk in encode(result, _current_indent_level, _path=_path):
                yield chunk
        except BaseException:
            # Like the C encoder's encoder_markers_discard: release the
            # marker so a reused markers dict reports no false cycle,
            # without letting a marker that has already gone replace
            # the in-flight exception.
            if markers is not None:
                markers.pop(markerid, None)
            raise
        if markers is not None:
            del markers[markerid]

    def _iterencode_list(lst, _current_indent_level, _origin=None, _path=None):
        # _origin is the object being encoded: lst itself, or under
        # iterable_as_array the iterable that produced the iterator lst.
        # It is the circular reference marker (a fresh iterator would
        # never match) and is named in error notes. An iterator's truth
        # value says nothing about emptiness, so only lists and tuples
        # take the fast path.
        # _path is the location of this container, extended per item for
        # diagnostics; None when diagnose_errors is off.
        if _origin is None:
            if not lst:
                yield '[]'
                return
            _origin = lst
        if markers is not None:
            markerid = id(_origin)
            if markerid in markers:
                raise ValueError("Circular reference detected")
            markers[markerid] = _origin
        try:
            buf = '['
            if _indent is not None:
                _current_indent_level += 1
                newline_indent = '\n' + (_indent * _current_indent_level)
                separator = _item_separator + newline_indent
                buf += newline_indent
            else:
                newline_indent = None
                separator = _item_separator
            first = True
            for i, value in enumerate(lst):
                if first:
                    first = False
                else:
                    buf = separator
                try:
                    if isinstance(value, string_types):
                        yield buf + _encoder(value)
                    elif _PY3 and isinstance(value, bytes) and _encoding is not None:
                        yield buf + _encoder(value)
                    elif isinstance(value, RawJSON):
                        yield buf + value.encoded_json
                    elif value is None:
                        yield buf + 'null'
                    elif value is True:
                        yield buf + 'true'
                    elif value is False:
                        yield buf + 'false'
                    elif isinstance(value, integer_types):
                        yield buf + _encode_int(value)
                    elif isinstance(value, float):
                        yield buf + _floatstr(value)
                    elif _use_decimal and isinstance(value, Decimal):
                        yield buf + _encode_decimal(value, _floatstr)
                    else:
                        yield buf
                        # Built only on the conversion path: scalars never
                        # need a diagnostic, so this keeps the hot loop free.
                        item_path = (_path + (i,)
                                     if _path is not None else None)
                        for_json = _for_json and call_method(
                            value, 'for_json', item_path)
                        if for_json:
                            chunks = _iterencode_method(
                                value, for_json[0], _current_indent_level,
                                _iterencode, item_path)
                        else:
                            _asdict = _namedtuple_as_object and call_method(
                                value, '_asdict', item_path)
                            if _asdict:
                                dct = _asdict[0]
                                if not isinstance(dct, dict):
                                    raise _asdict_error(value, dct, item_path)
                                chunks = _iterencode_method(
                                    value, dct, _current_indent_level,
                                    _iterencode_dict, item_path)
                            elif isinstance(value, list):
                                chunks = _iterencode_list(
                                    value, _current_indent_level,
                                    _path=item_path)
                            elif _tuple_as_array and isinstance(value, tuple):
                                chunks = _iterencode_list(
                                    value, _current_indent_level,
                                    _path=item_path)
                            elif isinstance(value, _dict_types):
                                chunks = _iterencode_dict(
                                    value, _current_indent_level,
                                    _path=item_path)
                            else:
                                # for_json and _asdict were already tried
                                chunks = _iterencode(
                                    value, _current_indent_level, True,
                                    item_path)
                        for chunk in chunks:
                            yield chunk
                except BaseException as exc:
                    _annotate_exception(
                        exc, 'when serializing %s item %d',
                        _origin, i)
                    raise
            if first:
                # iterable_as_array misses the fast path at the top
                yield '[]'
            else:
                if newline_indent is not None:
                    _current_indent_level -= 1
                    yield '\n' + (_indent * _current_indent_level)
                yield ']'
        except BaseException:
            if markers is not None:
                markers.pop(markerid, None)
            raise
        if markers is not None:
            del markers[markerid]

    def _stringify_key(key, _path=None):
        if isinstance(key, string_types): # pragma: no cover
            pass
        elif _PY3 and isinstance(key, bytes) and _encoding is not None:
            key = str(key, _encoding)
        elif isinstance(key, float):
            key = _floatstr(key)
        elif key is True:
            key = 'true'
        elif key is False:
            key = 'false'
        elif key is None:
            key = 'null'
        elif isinstance(key, integer_types):
            if type(key) not in integer_types:
                # See #118, do not trust custom str/repr
                key = int(key)
            key = str(key)
        elif _use_decimal and isinstance(key, Decimal):
            key = _encode_decimal(key, _floatstr)
        elif _skipkeys:
            key = None
        else:
            # A key that cannot be converted is reported as kind='key', so
            # callers can tell it apart from a failing value conversion.
            # _safe_type_name avoids letting a hostile metaclass __name__
            # mask the conversion error.
            error = TypeError('keys must be str, int, float, bool or None, '
                              'not %s' % (_safe_type_name(key),))
            replacement = _diagnose(error, 'key', key, _path)
            if replacement is not None:
                raise replacement
            raise error
        return key

    def _iterencode_dict(dct, _current_indent_level, _path=None):
        if not dct:
            yield '{}'
            return
        if markers is not None:
            markerid = id(dct)
            if markerid in markers:
                raise ValueError("Circular reference detected")
            markers[markerid] = dct
        try:
            yield '{'
            if _indent is not None:
                _current_indent_level += 1
                newline_indent = '\n' + (_indent * _current_indent_level)
                item_separator = _item_separator + newline_indent
                yield newline_indent
            else:
                newline_indent = None
                item_separator = _item_separator
            first = True
            if _PY3:
                iteritems = dct.items()
            else:
                iteritems = dct.iteritems()
            if _item_sort_key is not None:
                items = []
                for k, v in dct.items():
                    if not isinstance(k, string_types):
                        k = _stringify_key(k, _path)
                        if k is None:
                            continue
                    items.append((k, v))
                items.sort(key=_item_sort_key)
            else:
                items = iteritems
            for key, value in items:
                if _item_sort_key is None and not isinstance(key, string_types):
                    key = _stringify_key(key, _path)
                    if key is None:
                        # _skipkeys must be True
                        continue
                if first:
                    first = False
                else:
                    yield item_separator
                yield _encoder(key)
                yield _key_separator
                try:
                    if isinstance(value, string_types):
                        yield _encoder(value)
                    elif _PY3 and isinstance(value, bytes) and _encoding is not None:
                        yield _encoder(value)
                    elif isinstance(value, RawJSON):
                        yield value.encoded_json
                    elif value is None:
                        yield 'null'
                    elif value is True:
                        yield 'true'
                    elif value is False:
                        yield 'false'
                    elif isinstance(value, integer_types):
                        yield _encode_int(value)
                    elif isinstance(value, float):
                        yield _floatstr(value)
                    elif _use_decimal and isinstance(value, Decimal):
                        yield _encode_decimal(value, _floatstr)
                    else:
                        # The diagnostic path names the source member key;
                        # keys reaching this point are already text.  Built
                        # only on the conversion path so scalars stay cheap.
                        member_path = (
                            _path + (key,) if _path is not None else None)
                        for_json = _for_json and call_method(
                            value, 'for_json', member_path)
                        if for_json:
                            chunks = _iterencode_method(
                                value, for_json[0], _current_indent_level,
                                _iterencode, member_path)
                        else:
                            _asdict = _namedtuple_as_object and call_method(
                                value, '_asdict', member_path)
                            if _asdict:
                                # Not `dct`: the note below names dct's type.
                                asdict_dct = _asdict[0]
                                if not isinstance(asdict_dct, dict):
                                    raise _asdict_error(
                                        value, asdict_dct, member_path)
                                chunks = _iterencode_method(
                                    value, asdict_dct, _current_indent_level,
                                    _iterencode_dict, member_path)
                            elif isinstance(value, list):
                                chunks = _iterencode_list(
                                    value, _current_indent_level,
                                    _path=member_path)
                            elif _tuple_as_array and isinstance(value, tuple):
                                chunks = _iterencode_list(
                                    value, _current_indent_level,
                                    _path=member_path)
                            elif isinstance(value, _dict_types):
                                chunks = _iterencode_dict(
                                    value, _current_indent_level,
                                    _path=member_path)
                            else:
                                # for_json and _asdict were already tried
                                chunks = _iterencode(
                                    value, _current_indent_level, True,
                                    member_path)
                        for chunk in chunks:
                            yield chunk
                except BaseException as exc:
                    _annotate_exception(
                        exc, 'when serializing %s item %r',
                        dct, key)
                    raise
            if newline_indent is not None:
                _current_indent_level -= 1
                yield '\n' + (_indent * _current_indent_level)
            yield '}'
        except BaseException:
            if markers is not None:
                markers.pop(markerid, None)
            raise
        if markers is not None:
            del markers[markerid]

    def _iterencode(o, _current_indent_level, _methods_tried=False,
                    _path=None):
        # _methods_tried: the list and dict item paths already called
        # for_json() and _asdict() on o, so calling them again would run
        # them (and any side effects) twice, unlike the C encoder.
        # _path: location of o, tracked only for containers already
        # entered; None when diagnose_errors is off.
        if isinstance(o, string_types):
            yield _encoder(o)
        elif _PY3 and isinstance(o, bytes) and _encoding is not None:
            yield _encoder(o)
        elif isinstance(o, RawJSON):
            yield o.encoded_json
        elif o is None:
            yield 'null'
        elif o is True:
            yield 'true'
        elif o is False:
            yield 'false'
        elif isinstance(o, integer_types):
            yield _encode_int(o)
        elif isinstance(o, float):
            yield _floatstr(o)
        else:
            for_json = (_for_json and not _methods_tried
                        and call_method(o, 'for_json', _path))
            if for_json:
                for chunk in _iterencode_method(
                        o, for_json[0], _current_indent_level, _iterencode,
                        _path):
                    yield chunk
            else:
                _asdict = (_namedtuple_as_object and not _methods_tried
                           and call_method(o, '_asdict', _path))
                if _asdict:
                    dct = _asdict[0]
                    if not isinstance(dct, dict):
                        raise _asdict_error(o, dct, _path)
                    for chunk in _iterencode_method(
                            o, dct, _current_indent_level, _iterencode_dict,
                            _path):
                        yield chunk
                elif isinstance(o, list):
                    for chunk in _iterencode_list(
                            o, _current_indent_level, _path=_path):
                        yield chunk
                elif (_tuple_as_array and isinstance(o, tuple)):
                    for chunk in _iterencode_list(
                            o, _current_indent_level, _path=_path):
                        yield chunk
                elif isinstance(o, _dict_types):
                    for chunk in _iterencode_dict(
                            o, _current_indent_level, _path=_path):
                        yield chunk
                elif _use_decimal and isinstance(o, Decimal):
                    yield _encode_decimal(o, _floatstr)
                else:
                    while _iterable_as_array:
                        # Reject a revisited iterable before its __iter__
                        # runs again; _iterencode_list marks o itself.
                        if markers is not None and id(o) in markers:
                            raise ValueError("Circular reference detected")
                        try:
                            iterator = iter(o)
                        except TypeError:
                            break
                        for chunk in _iterencode_list(iterator,
                                                      _current_indent_level,
                                                      o, _path):
                            yield chunk
                        return
                    if markers is not None:
                        markerid = id(o)
                        if markerid in markers:
                            raise ValueError("Circular reference detected")
                        markers[markerid] = o
                    try:
                        converted = _default(o)
                    except BaseException as exc:
                        if markers is not None:
                            markers.pop(markerid, None)
                        replacement = _diagnose(exc, 'value', o, _path)
                        if replacement is not None:
                            _annotate_exception(
                                replacement, 'when serializing %s object', o)
                            raise replacement
                        _annotate_exception(
                            exc, 'when serializing %s object', o)
                        raise
                    try:
                        for chunk in _iterencode(converted,
                                                 _current_indent_level,
                                                 _path=_path):
                            yield chunk
                    except BaseException as exc:
                        if markers is not None:
                            markers.pop(markerid, None)
                        # default() succeeded, so this note reflects the
                        # returned object's type, matching the C encoder.
                        _annotate_exception(
                            exc, 'when serializing %s object', converted)
                        raise
                    if markers is not None:
                        del markers[markerid]

    return _iterencode
