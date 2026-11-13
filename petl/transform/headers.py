from __future__ import absolute_import, print_function, division


import itertools
import operator
import unicodedata
from collections import deque
from petl.compat import next, text_type, string_types
from petl.errors import ArgumentError
from petl.errors import FieldSelectionError
from petl.errors import HeaderNormalizationError


from petl.util.base import Table, asindices, rowgetter


def rename(table, *args, **kwargs):
    """
    Replace one or more values in the table's header row. E.g.::

        >>> import petl as etl
        >>> table1 = [['sex', 'age'],
        ...           ['m', 12],
        ...           ['f', 34],
        ...           ['-', 56]]
        >>> # rename a single field
        ... table2 = etl.rename(table1, 'sex', 'gender')
        >>> table2
        +--------+-----+
        | gender | age |
        +========+=====+
        | 'm'    |  12 |
        +--------+-----+
        | 'f'    |  34 |
        +--------+-----+
        | '-'    |  56 |
        +--------+-----+

        >>> # rename multiple fields by passing dictionary as second argument
        ... table3 = etl.rename(table1, {'sex': 'gender', 'age': 'age_years'})
        >>> table3
        +--------+-----------+
        | gender | age_years |
        +========+===========+
        | 'm'    |        12 |
        +--------+-----------+
        | 'f'    |        34 |
        +--------+-----------+
        | '-'    |        56 |
        +--------+-----------+

    The field to rename can be specified as an index (i.e., integer representing
    field position).

    If any nonexistent fields are specified, the default behaviour is to raise
    a `FieldSelectionError`. However, if `strict` keyword argument is `False`, any
    nonexistent fields specified will be silently ignored.
    """

    return RenameView(table, *args, **kwargs)


Table.rename = rename


class RenameView(Table):

    def __init__(self, table, *args, **kwargs):
        self.source = table
        if len(args) == 0:
            self.spec = dict()
        elif len(args) == 1:
            self.spec = args[0]
        elif len(args) == 2:
            self.spec = {args[0]: args[1]}
        self.strict = kwargs.get('strict', True)

    def __iter__(self):
        return iterrename(self.source, self.spec, self.strict)

    def __setitem__(self, key, value):
        self.spec[key] = value


def iterrename(source, spec, strict):
    it = iter(source)
    try:
        hdr = next(it)
    except StopIteration:
        hdr = []
    flds = list(map(text_type, hdr))
    if strict:
        for x in spec:
            if isinstance(x, int):
                if x < 0 or x >= len(hdr):
                    raise FieldSelectionError(x)
            elif x not in flds:
                raise FieldSelectionError(x)
    outhdr = [spec[i] if i in spec
              else spec[f] if f in spec
              else f
              for i, f in enumerate(flds)]
    yield tuple(outhdr)
    for row in it:
        yield tuple(row)


def setheader(table, header):
    """
    Replace header row in the given table. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar'],
        ...           ['a', 1],
        ...           ['b', 2]]
        >>> table2 = etl.setheader(table1, ['foofoo', 'barbar'])
        >>> table2
        +--------+--------+
        | foofoo | barbar |
        +========+========+
        | 'a'    |      1 |
        +--------+--------+
        | 'b'    |      2 |
        +--------+--------+

    A function can also be provided to transform each field in the existing
    header. E.g.::

        >>> table3 = etl.setheader(table1, str.upper)
        >>> table3
        +-----+-----+
        | FOO | BAR |
        +=====+=====+
        | 'a' |   1 |
        +-----+-----+
        | 'b' |   2 |
        +-----+-----+

    See also :func:`petl.transform.headers.extendheader`,
    :func:`petl.transform.headers.pushheader`.

    """

    return SetHeaderView(table, header)


Table.setheader = setheader


class SetHeaderView(Table):

    def __init__(self, source, header):
        self.source = source
        self._header = header

    def __iter__(self):
        return itersetheader(self.source, self._header)


def itersetheader(source, header):
    it = iter(source)
    try:
        source_header = next(it)
    except StopIteration:
        source_header = ()
    try:
        iter(header)
    except TypeError:
        if callable(header):
            header = [header(field) for field in source_header]
    yield tuple(header)
    for row in it:
        yield tuple(row)


def extendheader(table, fields):
    """
    Extend header row in the given table. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo'],
        ...           ['a', 1, True],
        ...           ['b', 2, False]]
        >>> table2 = etl.extendheader(table1, ['bar', 'baz'])
        >>> table2
        +-----+-----+-------+
        | foo | bar | baz   |
        +=====+=====+=======+
        | 'a' |   1 | True  |
        +-----+-----+-------+
        | 'b' |   2 | False |
        +-----+-----+-------+

    See also :func:`petl.transform.headers.setheader`,
    :func:`petl.transform.headers.pushheader`.

    """

    return ExtendHeaderView(table, fields)


Table.extendheader = extendheader


class ExtendHeaderView(Table):

    def __init__(self, source, fields):
        self.source = source
        self.fields = fields

    def __iter__(self):
        return iterextendheader(self.source, self.fields)


def iterextendheader(source, fields):
    it = iter(source)
    try:
        hdr = next(it)
    except StopIteration:
        hdr = []
    outhdr = list(hdr)
    outhdr.extend(fields)
    yield tuple(outhdr)
    for row in it:
        yield tuple(row)


def pushheader(table, header, *args):
    """
    Push rows down and prepend a header row. E.g.::

        >>> import petl as etl
        >>> table1 = [['a', 1],
        ...           ['b', 2]]
        >>> table2 = etl.pushheader(table1, ['foo', 'bar'])
        >>> table2
        +-----+-----+
        | foo | bar |
        +=====+=====+
        | 'a' |   1 |
        +-----+-----+
        | 'b' |   2 |
        +-----+-----+

    The header row can either be a list or positional arguments.

    """

    return PushHeaderView(table, header, *args)


Table.pushheader = pushheader


class PushHeaderView(Table):

    def __init__(self, source, header, *args):
        self.source = source
        self.args = args
        # if user passes header as a list, just use this and ignore args
        if isinstance(header, (list, tuple)):
            self._header = header
        # otherwise,
        elif len(args) > 0:
            self._header = []
            self._header.append(header)  # first argument is named header
            self._header.extend(args)  # add the other positional arguments
        else:
            assert False, 'bad parameters'

    def __iter__(self):
        return iterpushheader(self.source, self._header)


def iterpushheader(source, header):
    it = iter(source)
    yield tuple(header)
    for row in it:
        yield tuple(row)


def skip(table, n):
    """
    Skip `n` rows, including the header row. E.g.::

        >>> import petl as etl
        >>> table1 = [['#aaa', 'bbb', 'ccc'],
        ...           ['#mmm'],
        ...           ['foo', 'bar'],
        ...           ['a', 1],
        ...           ['b', 2]]
        >>> table2 = etl.skip(table1, 2)
        >>> table2
        +-----+-----+
        | foo | bar |
        +=====+=====+
        | 'a' |   1 |
        +-----+-----+
        | 'b' |   2 |
        +-----+-----+

    See also :func:`petl.transform.basics.skipcomments`.

    """

    return SkipView(table, n)


Table.skip = skip


class SkipView(Table):

    def __init__(self, source, n):
        self.source = source
        self.n = n

    def __iter__(self):
        return iterskip(self.source, self.n)


def iterskip(source, n):
    return itertools.islice(source, n, None)


def skiplast(table, n):
    """
    Skip the last `n` rows of a table. E.g.::

        >>> import petl as etl
        >>> table = [('name', 'value'), ('a', 1), ('b', 2), ('Total', 3)]
        >>> list(etl.skiplast(table, 1))
        [('name', 'value'), ('a', 1), ('b', 2)]

    Like :func:`skip`, this operates on all rows, including the header.
    If `n` is at least the number of rows, the result is empty. A zero
    count leaves the table unchanged; negative counts are invalid.

    The source is read once, buffering at most `n` pending rows, so it
    need not fit in memory. The returned view can be iterated again if
    the source can be iterated again.

    """

    return SkipLastView(table, n)


Table.skiplast = skiplast


class SkipLastView(Table):

    def __init__(self, source, n):
        self.source = source
        self.n = operator.index(n)
        if self.n < 0:
            raise ValueError('number of rows to skip must be non-negative')

    def __iter__(self):
        return iterskiplast(self.source, self.n)


def iterskiplast(source, n):
    it = iter(source)
    pending = deque(itertools.islice(it, n))
    for row in it:
        pending.append(row)
        yield pending.popleft()


def prefixheader(table, prefix):
    """Prefix all fields in the table header."""

    return PrefixHeaderView(table, prefix)


Table.prefixheader = prefixheader


class PrefixHeaderView(Table):

    def __init__(self, table, prefix):
        self.table = table
        self.prefix = prefix

    def __iter__(self):
        it = iter(self.table)
        try:
            hdr = next(it)
        except StopIteration:
            return
        outhdr = tuple((text_type(self.prefix) + text_type(f)) for f in hdr)
        yield outhdr
        for row in it:
            yield row


def suffixheader(table, suffix):
    """Suffix all fields in the table header."""

    return SuffixHeaderView(table, suffix)


Table.suffixheader = suffixheader


class SuffixHeaderView(Table):

    def __init__(self, table, suffix):
        self.table = table
        self.suffix = suffix

    def __iter__(self):
        it = iter(self.table)
        try:
            hdr = next(it)
        except StopIteration:
            return
        outhdr = tuple((text_type(f) + text_type(self.suffix)) for f in hdr)
        yield outhdr
        for row in it:
            yield row


def sortheader(table, reverse=False, missing=None):
    """Re-order columns so the header is sorted.

    .. versionadded:: 1.1.0

    """

    return SortHeaderView(table, reverse, missing)


Table.sortheader = sortheader


class SortHeaderView(Table):

    def __init__(self, table, reverse, missing):
        self.table = table
        self.reverse = reverse
        self.missing = missing

    def __iter__(self):
        it = iter(self.table)
        try:
            hdr = next(it)
        except StopIteration:
            return
        shdr = sorted(hdr, reverse=self.reverse)
        indices = asindices(hdr, shdr)
        transform = rowgetter(*indices)

        # yield the transformed header
        yield tuple(shdr)

        # construct the transformed data
        missing = self.missing
        for row in it:
            try:
                yield transform(row)
            except IndexError:
                # row is short, let's be kind and fill in any missing fields
                yield tuple(row[i] if i < len(row) else missing
                            for i in indices)


##############################
# composable header normalizing
##############################

# valid unicodedata.normalize forms
_UNICODE_FORMS = ('NFC', 'NFD', 'NFKC', 'NFKD')


def _casefold(value):
    # str.casefold() only exists from Python 3.3
    casefold = getattr(value, 'casefold', None)
    if casefold is not None:
        return casefold()
    return value.lower()


class HeaderMapping(tuple):
    """
    Position-indexed mapping produced by :class:`HeaderNormalizer`.

    Each element is an ``(index, original, normalized)`` triple, kept in
    column order, so original headers are always recoverable by position even
    when several columns shared the same original value or collapsed onto the
    same normalized value, e.g.::

        >>> mapping = HeaderMapping([
        ...     (0, ' Foo ', 'foo'),
        ...     (1, 'FOO', 'foo_1'),
        ... ])
        >>> mapping.original_at(1)
        'FOO'
        >>> mapping.normalized_at(0)
        'foo'
        >>> list(mapping.pairs())
        [(' Foo ', 'foo'), ('FOO', 'foo_1')]

    """

    def original_at(self, index):
        """Return the original header at column position `index`."""
        return self[index][1]

    def normalized_at(self, index):
        """Return the normalized header at column position `index`."""
        return self[index][2]

    def pairs(self):
        """Yield ``(original, normalized)`` pairs in column order."""
        for _, original, normalized in self:
            yield original, normalized

    def normalized_names(self):
        """Return the normalized names as a tuple, in column order."""
        return tuple(normalized for _, _, normalized in self)

    def original_names(self):
        """Return the original names as a tuple, in column order."""
        return tuple(original for _, original, _ in self)

    def asdict(self):
        """
        Return an ``{original: normalized}`` dictionary.

        Raises a `ValueError` if the same original header appears more than
        once, because a plain dictionary cannot represent the ambiguity; use
        the position-based accessors in that case.
        """
        result = {}
        for _, original, normalized in self:
            if original in result:
                raise ValueError(
                    'duplicate original header %r cannot be represented in a '
                    'plain dict; use position-based accessors instead'
                    % (original,)
                )
            result[original] = normalized
        return result


class HeaderNormalizer(object):
    """
    Composable header normalization.

    Three independent steps can be switched on or off and are applied in a
    fixed, predictable order:

    1. Unicode normalization (`unicode_form`, any form accepted by
       :func:`unicodedata.normalize`: ``'NFC'``, ``'NFD'``, ``'NFKC'`` or
       ``'NFKD'``; ``None`` skips the step)
    2. stripping leading and trailing whitespace (`strip`)
    3. case folding (`casefold`)

    Each step is optional and they can be combined freely. The normalizer
    never coerces non-string headers into strings: encountering one raises a
    `TypeError` describing the offending column.

    By default, if the rules collapse two or more columns onto the same name,
    a :class:`petl.errors.HeaderNormalizationError` is raised when the header
    is read, before any data row is yielded. Pass ``disambiguate=True`` to
    instead append a suffix (default ``'_1'``, ``'_2'``, ...) to the later
    columns, in original column order. Suffix numbers are chosen so the
    generated names neither collide with each other nor with names that
    already existed with a suffix, e.g.::

        >>> normalizer = HeaderNormalizer(strip=True, casefold=True,
        ...                               disambiguate=True)
        >>> normalized, mapping = normalizer.normalize(('Foo', 'foo', 'foo_1'))
        >>> normalized
        ('foo', 'foo_2', 'foo_1')
        >>> mapping.original_at(1)
        'foo'

    Repeated empty headers are handled by the same rule::

        >>> normalized, mapping = normalizer.normalize(('', ' ', ''))
        >>> normalized
        ('', '_1', '_2')

    """

    def __init__(self, unicode_form=None, strip=False, casefold=False,
                 disambiguate=False, suffix='_'):
        if unicode_form is not None and unicode_form not in _UNICODE_FORMS:
            raise ArgumentError(
                'unicode_form must be one of %s or None, got %r'
                % (', '.join(_UNICODE_FORMS), unicode_form)
            )
        if not isinstance(suffix, string_types) or not suffix:
            raise ArgumentError(
                'suffix must be a non-empty string, got %r' % (suffix,)
            )
        self.unicode_form = unicode_form
        self.strip = strip
        self.casefold = casefold
        self.disambiguate = disambiguate
        self.suffix = suffix

    def transform(self, name):
        """Apply the configured normalization steps to a single string."""
        if self.unicode_form is not None:
            name = unicodedata.normalize(self.unicode_form, name)
        if self.strip:
            name = name.strip()
        if self.casefold:
            name = _casefold(name)
        return name

    def normalize(self, header):
        """
        Normalize `header`.

        Returns a ``(normalized_names, mapping)`` pair where `mapping` is a
        :class:`HeaderMapping` allowing the original header to be recovered by
        column position. Raises `TypeError` for non-string headers and
        :class:`petl.errors.HeaderNormalizationError` on unresolved
        collisions.
        """
        originals = tuple(header)

        # non-string headers must fail loudly rather than be coerced
        for index, name in enumerate(originals):
            if not isinstance(name, string_types):
                raise TypeError(
                    'header at column %d is not a string: %r'
                    % (index, name)
                )

        transformed = [self.transform(name) for name in originals]

        if not self.disambiguate:
            collisions = self._find_collisions(transformed)
            if collisions:
                raise HeaderNormalizationError(collisions)
            normalized = tuple(transformed)
        else:
            normalized = self._disambiguate(transformed)

        mapping = HeaderMapping(
            (index, originals[index], normalized[index])
            for index in range(len(originals))
        )
        return normalized, mapping

    @staticmethod
    def _find_collisions(names):
        positions = {}
        for index, name in enumerate(names):
            positions.setdefault(name, []).append(index)
        return {name: idxs for name, idxs in positions.items()
                if len(idxs) > 1}

    def _disambiguate(self, names):
        # reserve every candidate name up front, including names that already
        # carry the suffix, so generated names can never collide with them
        used = set(names)
        first_use = {}
        result = []
        for index, name in enumerate(names):
            if name not in first_use:
                first_use[name] = index
                result.append(name)
                continue
            candidate = None
            counter = 1
            while candidate is None or candidate in used:
                candidate = '%s%s%d' % (name, self.suffix, counter)
                counter += 1
            used.add(candidate)
            result.append(candidate)
        return tuple(result)


def normalizeheader(table, unicode_form=None, strip=False, casefold=False,
                    disambiguate=False, suffix='_'):
    """
    Normalize the table's header row using composable rules, leaving every
    data row untouched. E.g.::

        >>> import petl as etl
        >>> table1 = [[' Foo ', 'BAR', 'bar'],
        ...           ['a', 1, 2]]
        >>> table2 = etl.normalizeheader(table1, strip=True, casefold=True,
        ...                               disambiguate=True)
        >>> table2
        +-----+-----+-------+
        | foo | bar | bar_1 |
        +=====+=====+=======+
        | 'a' |   1 |     2 |
        +-----+-----+-------+

    The steps are optional and composable: `unicode_form` selects a Unicode
    normalization form (``'NFC'``, ``'NFD'``, ``'NFKC'`` or ``'NFKD'``),
    `strip` removes leading and trailing whitespace and `casefold` applies
    case folding. They run in that order.

    By default a collision raises a
    :class:`petl.errors.HeaderNormalizationError` before any data are read;
    pass ``disambiguate=True`` to append `suffix`-based numbered names in
    column order. Non-string headers raise `TypeError` instead of being
    silently stringified.

    To obtain the original-to-normalized mapping without wrapping a table,
    use :class:`HeaderNormalizer` directly.

    """

    return NormalizeHeaderView(
        table,
        HeaderNormalizer(unicode_form=unicode_form, strip=strip,
                         casefold=casefold, disambiguate=disambiguate,
                         suffix=suffix)
    )


Table.normalizeheader = normalizeheader


class NormalizeHeaderView(Table):

    def __init__(self, table, normalizer):
        self.table = table
        self.normalizer = normalizer

    def __iter__(self):
        it = iter(self.table)
        try:
            hdr = next(it)
        except StopIteration:
            return
        normalized, _ = self.normalizer.normalize(hdr)
        yield normalized
        for row in it:
            yield tuple(row)
