"""Implementation of JSONEncoder
"""
from __future__ import absolute_import
import re
from operator import itemgetter
# Do not import Decimal directly to avoid reload issues
import decimal
import sys
from .compat import binary_type, text_type, string_types, integer_types, PY3

# PEP 678 add_note() is available on Python 3.11+
_HAS_ADD_NOTE = sys.version_info >= (3, 11)


def _annotate_exception(exc, note_format, obj, *args):
    if _HAS_ADD_NOTE:
        try:
            exc.add_note(note_format % ((type(obj).__name__,) + args))
        except BaseException:
            # Diagnostic failures must not replace the serialization error.
            pass


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
# Deterministic mode additionally escapes the surrogate code points:
# an unpaired surrogate has no UTF-8 representation, so it is emitted
# as a lowercase \uXXXX escape to keep the output well-formed UTF-8.
ESCAPE_DETERMINISTIC = re.compile(u'[\\x00-\\x1f\\\\"\\ud800-\\udfff]')
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


def py_encode_basestring_deterministic(s, _PY3=PY3):
    """Return the deterministic JSON representation of a string.

    Basic Multilingual Plane characters are emitted literally (the
    output of deterministic mode is UTF-8 text); only the escapes that
    JSON mandates are used.  Unpaired surrogates, which have no UTF-8
    representation, are emitted as lowercase ``\\uXXXX`` escapes so
    that the output stays well-formed.
    """
    if _PY3:
        if type(s) is not str:
            # convert an str subclass instance to exact str
            # raise a TypeError otherwise
            s = str.__str__(s)
    else:
        if isinstance(s, str):
            # bytes must be UTF-8 in deterministic mode
            s = unicode(s, 'utf-8')
        elif type(s) is not unicode:
            # convert a unicode subclass instance to exact unicode
            # raise a TypeError otherwise
            s = unicode.__getnewargs__(s)[0]
    def replace(match):
        c = match.group(0)
        try:
            return ESCAPE_DCT[c]
        except KeyError:
            # an unpaired surrogate
            return '\\u%04x' % (ord(c),)
    return u'"' + ESCAPE_DETERMINISTIC.sub(replace, s) + u'"'


if PY3:
    def _utf16_sort_key(s):
        # Big-endian UTF-16 code units compare in UTF-16 code unit
        # order; surrogatepass maps each unpaired surrogate to its own
        # code unit so lone surrogates sort by code unit value too.
        return s.encode('utf-16-be', 'surrogatepass')
else:
    def _utf16_sort_key(s):
        # List of UTF-16 code units; on narrow builds non-BMP
        # characters are already stored as a surrogate pair.
        units = []
        for c in s:
            n = ord(c)
            if n < 0x10000:
                units.append(n)
            else:
                n -= 0x10000
                units.append(0xd800 | (n >> 10))
                units.append(0xdc00 | (n & 0x3ff))
        return units

# Integers with an absolute value up to 2**53 are exactly
# representable as IEEE 754 doubles; anything larger may round
# differently across JSON implementations, so deterministic mode
# rejects it instead of guessing.
_EXACT_INT_LIMIT = 2 ** 53


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
                 deterministic=False):
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

        If *deterministic* is true (default: ``False``), the encoder
        produces a canonical JSON digest: object keys are sorted by
        UTF-16 code unit order, the output is compact UTF-8 text with
        no insignificant whitespace, only string keys and finite JSON
        numbers are accepted, and objects that cannot satisfy this
        contract are rejected with ``TypeError`` instead of being
        passed to ``default()``. The deterministic configuration is
        fixed, so it conflicts with (and refuses to be silently
        overridden by) *sort_keys*, *item_sort_key*, *indent*,
        *separators*, *default*, *skipkeys*, *allow_nan*, *ignore_nan*,
        *for_json*, *bigint_as_string*, *int_as_string_bitcount*,
        *iterable_as_array*, and non-UTF-8 *encoding*; it also requires
        *ensure_ascii=False* to be passed explicitly. Combining
        ``deterministic=True`` with any of those raises ``TypeError``.

        """

        if deterministic:
            # The deterministic configuration fixes the key order,
            # separators, and number formatting itself. Any option that
            # would change (or appear to change) those decisions is a
            # hard error rather than a silent override, so that a
            # digest never depends on which option "won".
            conflicts = []
            if skipkeys:
                conflicts.append('skipkeys')
            if ensure_ascii:
                conflicts.append('ensure_ascii')
            if allow_nan:
                conflicts.append('allow_nan')
            if ignore_nan:
                conflicts.append('ignore_nan')
            if sort_keys:
                conflicts.append('sort_keys')
            if item_sort_key is not None:
                conflicts.append('item_sort_key')
            if indent is not None:
                conflicts.append('indent')
            if separators is not None:
                conflicts.append('separators')
            if default is not None:
                conflicts.append('default')
            if for_json:
                conflicts.append('for_json')
            if bigint_as_string:
                conflicts.append('bigint_as_string')
            if int_as_string_bitcount is not None:
                conflicts.append('int_as_string_bitcount')
            if iterable_as_array:
                conflicts.append('iterable_as_array')
            if not tuple_as_array:
                conflicts.append('tuple_as_array')
            if not namedtuple_as_object:
                conflicts.append('namedtuple_as_object')
            if encoding not in (None, 'utf-8'):
                conflicts.append('encoding')
            if conflicts:
                raise TypeError(
                    'deterministic=True does not allow the conflicting '
                    'option(s): %s. The deterministic configuration fixes '
                    'key order (UTF-16 code unit order), separators, and '
                    'number formatting itself; remove the conflicting '
                    'options instead of overriding them. Note that '
                    'deterministic output is UTF-8 text, so '
                    'ensure_ascii=False must be passed explicitly.'
                    % (', '.join(conflicts),))
            # Normalize the fixed settings so that introspection of the
            # encoder reflects what deterministic mode actually does.
            self.item_separator, self.key_separator = (',', ':')
        self.deterministic = deterministic

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
        if self.deterministic:
            return u''.join(self.iterencode(o))
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
            chunks = list(chunks)
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
        if self.deterministic:
            _iterencode = _make_deterministic_iterencode(
                markers, self.use_decimal, Decimal=decimal.Decimal)
            return _iterencode(o)
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
        if c_make_encoder is not None:
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
                self.iterable_as_array, Decimal=decimal.Decimal)
        try:
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
        if self.ensure_ascii:
            return ''.join(chunks)
        else:
            return u''.join(chunks)

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

    def call_method(obj, method_name):
        method = getattr(obj, method_name, None)
        if callable(method):
            try:
                return (method(),)
            except TypeError:
                pass
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

    def _iterencode_method(obj, result, _current_indent_level, encode):
        if markers is not None:
            markerid = id(obj)
            if markerid in markers:
                raise ValueError("Circular reference detected")
            markers[markerid] = obj
        try:
            for chunk in encode(result, _current_indent_level):
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

    def _iterencode_list(lst, _current_indent_level, _origin=None):
        # _origin is the object being encoded: lst itself, or under
        # iterable_as_array the iterable that produced the iterator lst.
        # It is the circular reference marker (a fresh iterator would
        # never match) and is named in error notes. An iterator's truth
        # value says nothing about emptiness, so only lists and tuples
        # take the fast path.
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
                        for_json = _for_json and call_method(value, 'for_json')
                        if for_json:
                            chunks = _iterencode_method(value, for_json[0],
                                                        _current_indent_level, _iterencode)
                        else:
                            _asdict = _namedtuple_as_object and call_method(value, '_asdict')
                            if _asdict:
                                dct = _asdict[0]
                                if not isinstance(dct, dict):
                                    raise TypeError("_asdict() must return a dict, not %s" % (type(dct).__name__,))
                                chunks = _iterencode_method(value, dct,
                                                            _current_indent_level, _iterencode_dict)
                            elif isinstance(value, list):
                                chunks = _iterencode_list(value, _current_indent_level)
                            elif _tuple_as_array and isinstance(value, tuple):
                                chunks = _iterencode_list(value, _current_indent_level)
                            elif isinstance(value, _dict_types):
                                chunks = _iterencode_dict(value, _current_indent_level)
                            else:
                                # for_json and _asdict were already tried
                                chunks = _iterencode(value, _current_indent_level, True)
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

    def _stringify_key(key):
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
            raise TypeError('keys must be str, int, float, bool or None, '
                            'not %s' % key.__class__.__name__)
        return key

    def _iterencode_dict(dct, _current_indent_level):
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
                        k = _stringify_key(k)
                        if k is None:
                            continue
                    items.append((k, v))
                items.sort(key=_item_sort_key)
            else:
                items = iteritems
            for key, value in items:
                if _item_sort_key is None and not isinstance(key, string_types):
                    key = _stringify_key(key)
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
                        for_json = _for_json and call_method(value, 'for_json')
                        if for_json:
                            chunks = _iterencode_method(value, for_json[0],
                                                        _current_indent_level, _iterencode)
                        else:
                            _asdict = _namedtuple_as_object and call_method(value, '_asdict')
                            if _asdict:
                                # Not `dct`: the note below names dct's type.
                                asdict_dct = _asdict[0]
                                if not isinstance(asdict_dct, dict):
                                    raise TypeError("_asdict() must return a dict, not %s" % (type(asdict_dct).__name__,))
                                chunks = _iterencode_method(value, asdict_dct,
                                                            _current_indent_level, _iterencode_dict)
                            elif isinstance(value, list):
                                chunks = _iterencode_list(value, _current_indent_level)
                            elif _tuple_as_array and isinstance(value, tuple):
                                chunks = _iterencode_list(value, _current_indent_level)
                            elif isinstance(value, _dict_types):
                                chunks = _iterencode_dict(value, _current_indent_level)
                            else:
                                # for_json and _asdict were already tried
                                chunks = _iterencode(value, _current_indent_level, True)
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

    def _iterencode(o, _current_indent_level, _methods_tried=False):
        # _methods_tried: the list and dict item paths already called
        # for_json() and _asdict() on o, so calling them again would run
        # them (and any side effects) twice, unlike the C encoder.
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
                        and call_method(o, 'for_json'))
            if for_json:
                for chunk in _iterencode_method(o, for_json[0],
                                              _current_indent_level, _iterencode):
                    yield chunk
            else:
                _asdict = (_namedtuple_as_object and not _methods_tried
                           and call_method(o, '_asdict'))
                if _asdict:
                    dct = _asdict[0]
                    if not isinstance(dct, dict):
                        raise TypeError("_asdict() must return a dict, not %s" % (type(dct).__name__,))
                    for chunk in _iterencode_method(o, dct,
                                                  _current_indent_level, _iterencode_dict):
                        yield chunk
                elif isinstance(o, list):
                    for chunk in _iterencode_list(o, _current_indent_level):
                        yield chunk
                elif (_tuple_as_array and isinstance(o, tuple)):
                    for chunk in _iterencode_list(o, _current_indent_level):
                        yield chunk
                elif isinstance(o, _dict_types):
                    for chunk in _iterencode_dict(o, _current_indent_level):
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
                                                      o):
                            yield chunk
                        return
                    if markers is not None:
                        markerid = id(o)
                        if markerid in markers:
                            raise ValueError("Circular reference detected")
                        markers[markerid] = o
                    try:
                        o = _default(o)
                        for chunk in _iterencode(o, _current_indent_level):
                            yield chunk
                    except BaseException as exc:
                        if markers is not None:
                            markers.pop(markerid, None)
                        _annotate_exception(
                            exc, 'when serializing %s object', o)
                        raise
                    if markers is not None:
                        del markers[markerid]

    return _iterencode


def _make_deterministic_iterencode(markers, _use_decimal,
        ## HACK: hand-optimized bytecode; turn globals into locals
        _PY3=PY3,
        ValueError=ValueError,
        TypeError=TypeError,
        string_types=string_types,
        integer_types=integer_types,
        _dict_types=_dict_types,
        isinstance=isinstance,
        float=float,
        id=id,
        Decimal=None,
    ):
    """Return an encoder for the restricted deterministic configuration.

    The output is compact UTF-8 JSON text: object keys are sorted by
    UTF-16 code unit order, separators are fixed to (',', ':'), only
    string keys and finite JSON numbers are accepted, and there is no
    default()/for_json/RawJSON fallback -- anything outside the
    contract raises TypeError (or ValueError for out-of-range numbers).
    """
    if _use_decimal and Decimal is None:
        Decimal = decimal.Decimal
    elif not _use_decimal:
        Decimal = None

    _encode_string = py_encode_basestring_deterministic

    def _encode_float(value):
        if type(value) is not float:
            # See #118, do not trust custom str/repr
            value = float(value)
        if value != value or value == _INFINITY or value == -_INFINITY:
            raise ValueError(
                "Out of range float values are not JSON compliant "
                "in deterministic mode: " + repr(value))
        if value == 0:
            # IEEE 754 negative zero digests identically to zero.
            return '0'
        return FLOAT_REPR(value)

    def _encode_int(value):
        if type(value) not in integer_types:
            # See #118, do not trust custom str/repr
            value = int(value)
        if not -_EXACT_INT_LIMIT <= value <= _EXACT_INT_LIMIT:
            raise ValueError(
                "Integer values outside the exactly representable "
                "IEEE 754 range (-2**53..2**53) are not deterministic: "
                + repr(value))
        return str(value)

    def _encode_decimal_value(value):
        if not value.is_finite():
            raise ValueError(
                "Out of range Decimal values are not JSON compliant "
                "in deterministic mode: " + repr(value))
        # str() of a finite Decimal is always a valid JSON number;
        # the representation (including a signed zero) is preserved.
        return str(value)

    def _iterencode_list(lst):
        if not lst:
            yield '[]'
            return
        if markers is not None:
            markerid = id(lst)
            if markerid in markers:
                raise ValueError("Circular reference detected")
            markers[markerid] = lst
        try:
            first = True
            for i, value in enumerate(lst):
                if first:
                    first = False
                    yield '['
                else:
                    yield ','
                try:
                    for chunk in _iterencode(value):
                        yield chunk
                except BaseException as exc:
                    _annotate_exception(
                        exc, 'when serializing %s item %d',
                        lst, i)
                    raise
            yield ']'
        finally:
            if markers is not None:
                markers.pop(markerid, None)

    def _iterencode_dict(dct):
        if not dct:
            yield '{}'
            return
        if markers is not None:
            markerid = id(dct)
            if markerid in markers:
                raise ValueError("Circular reference detected")
            markers[markerid] = dct
        try:
            items = []
            for key, value in dct.items():
                if not isinstance(key, string_types):
                    raise TypeError(
                        'keys must be str in deterministic mode, '
                        'not %s' % key.__class__.__name__)
                if not _PY3 and isinstance(key, str):
                    # bytes keys must be UTF-8 in deterministic mode
                    key = unicode(key, 'utf-8')
                items.append((_utf16_sort_key(key), key, value))
            # Sort by UTF-16 code units, not by code point: the two
            # differ whenever a key outside the BMP is compared with
            # a BMP key at or above U+E000.
            items.sort(key=itemgetter(0))
            first = True
            for _sort_key, key, value in items:
                if first:
                    first = False
                    yield '{'
                else:
                    yield ','
                yield _encode_string(key)
                yield ':'
                try:
                    for chunk in _iterencode(value):
                        yield chunk
                except BaseException as exc:
                    _annotate_exception(
                        exc, 'when serializing %s item %r',
                        dct, key)
                    raise
            yield '}'
        finally:
            if markers is not None:
                markers.pop(markerid, None)

    def _iterencode(o):
        if isinstance(o, string_types):
            yield _encode_string(o)
        elif o is None:
            yield 'null'
        elif o is True:
            yield 'true'
        elif o is False:
            yield 'false'
        elif isinstance(o, integer_types):
            yield _encode_int(o)
        elif isinstance(o, float):
            yield _encode_float(o)
        elif Decimal is not None and isinstance(o, Decimal):
            yield _encode_decimal_value(o)
        elif isinstance(o, list):
            for chunk in _iterencode_list(o):
                yield chunk
        elif isinstance(o, tuple):
            for chunk in _iterencode_list(o):
                yield chunk
        elif isinstance(o, _dict_types):
            for chunk in _iterencode_dict(o):
                yield chunk
        else:
            # No default(), for_json(), _asdict(), iterable, or RawJSON
            # fallback: a custom object that cannot satisfy the
            # deterministic contract is rejected outright.
            raise TypeError(
                'Object of type %s is not JSON serializable in '
                'deterministic mode' % o.__class__.__name__)

    return _iterencode
