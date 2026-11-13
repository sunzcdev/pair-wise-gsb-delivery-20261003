from __future__ import absolute_import, print_function, division


import itertools
from petl.compat import next, text_type, string_types, binary_type


from petl.errors import ArgumentError
from petl.util.base import Table

try:
    from collections.abc import Mapping, Iterable
except ImportError:  # pragma: no cover - Python 2 fallback
    from collections import Mapping, Iterable


def unpack(table, field, newfields=None, include_original=False, missing=None):
    """
    Unpack data values that are lists or tuples. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar'],
        ...           [1, ['a', 'b']],
        ...           [2, ['c', 'd']],
        ...           [3, ['e', 'f']]]
        >>> table2 = etl.unpack(table1, 'bar', ['baz', 'quux'])
        >>> table2
        +-----+-----+------+
        | foo | baz | quux |
        +=====+=====+======+
        |   1 | 'a' | 'b'  |
        +-----+-----+------+
        |   2 | 'c' | 'd'  |
        +-----+-----+------+
        |   3 | 'e' | 'f'  |
        +-----+-----+------+

    This function will attempt to unpack exactly the number of values as
    given by the number of new fields specified. If there are more values
    than new fields, remaining values will not be unpacked. If there are less
    values than new fields, `missing` values will be added.

    Negative field indices count from the end of the header.
    When field names repeat, an index selects that position rather than the
    first occurrence of the name.

    See also :func:`petl.transform.unpacks.unpackdict`.

    """

    return UnpackView(table, field, newfields=newfields,
                      include_original=include_original, missing=missing)


Table.unpack = unpack


class UnpackView(Table):

    def __init__(self, source, field, newfields=None, include_original=False,
                 missing=None):
        self.source = source
        self.field = field
        self.newfields = newfields
        self.include_original = include_original
        self.missing = missing

    def __iter__(self):
        return iterunpack(self.source, self.field, self.newfields,
                          self.include_original, self.missing)


def iterunpack(source, field, newfields, include_original, missing):
    it = iter(source)

    try:
        hdr = next(it)
    except StopIteration:
        hdr = []
    flds = list(map(text_type, hdr))
    if field in flds:
        field_index = flds.index(field)
    elif isinstance(field, int) and -len(flds) <= field < len(flds):
        field_index = field + len(flds) if field < 0 else field
        field = flds[field_index]
    else:
        raise ArgumentError('field invalid: must be either field name or index')

    # determine output fields
    outhdr = list(flds)
    if not include_original:
        del outhdr[field_index]
    if isinstance(newfields, (list, tuple)):
        outhdr.extend(newfields)
        nunpack = len(newfields)
    elif isinstance(newfields, int):
        nunpack = newfields
        newfields = [text_type(field) + text_type(i+1) for i in range(newfields)]
        outhdr.extend(newfields)
    elif newfields is None:
        nunpack = 0
    else:
        raise ArgumentError('newfields argument must be list or tuple of field '
                            'names, or int (number of values to unpack)')
    yield tuple(outhdr)

    # construct the output data
    for row in it:
        value = row[field_index]
        if include_original:
            out_row = list(row)
        else:
            out_row = [v for i, v in enumerate(row) if i != field_index]
        nvals = len(value)
        if nunpack > 0:
            if nvals >= nunpack:
                newvals = value[:nunpack]
            else:
                newvals = list(value) + ([missing] * (nunpack - nvals))
            out_row.extend(newvals)
        yield tuple(out_row)


def unpackdict(table, field, keys=None, includeoriginal=False,
               samplesize=1000, missing=None):
    """
    Unpack dictionary values into separate fields. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar'],
        ...           [1, {'baz': 'a', 'quux': 'b'}],
        ...           [2, {'baz': 'c', 'quux': 'd'}],
        ...           [3, {'baz': 'e', 'quux': 'f'}]]
        >>> table2 = etl.unpackdict(table1, 'bar')
        >>> table2
        +-----+-----+------+
        | foo | baz | quux |
        +=====+=====+======+
        |   1 | 'a' | 'b'  |
        +-----+-----+------+
        |   2 | 'c' | 'd'  |
        +-----+-----+------+
        |   3 | 'e' | 'f'  |
        +-----+-----+------+

    See also :func:`petl.transform.unpacks.unpack`.

    """

    return UnpackDictView(table, field, keys=keys,
                          includeoriginal=includeoriginal,
                          samplesize=samplesize, missing=missing)


Table.unpackdict = unpackdict


class UnpackDictView(Table):

    def __init__(self, table, field, keys=None, includeoriginal=False,
                 samplesize=1000, missing=None):
        self.table = table
        self.field = field
        self.keys = keys
        self.includeoriginal = includeoriginal
        self.samplesize = samplesize
        self.missing = missing

    def __iter__(self):
        return iterunpackdict(self.table, self.field, self.keys,
                              self.includeoriginal, self.samplesize,
                              self.missing)


def iterunpackdict(table, field, keys, includeoriginal, samplesize, missing):

    # set up
    it = iter(table)
    try:
        hdr = next(it)
    except StopIteration:
        hdr = []
    flds = list(map(text_type, hdr))
    fidx = flds.index(field)
    outhdr = list(flds)
    if not includeoriginal:
        del outhdr[fidx]

    # are keys specified?
    if not keys:
        # need to sample to find keys
        sample = list(itertools.islice(it, samplesize))
        keys = set()
        for row in sample:
            try:
                keys |= set(row[fidx].keys())
            except AttributeError:
                pass
        it = itertools.chain(sample, it)
        keys = sorted(keys)
    outhdr.extend(keys)
    yield tuple(outhdr)

    # generate the data rows
    for row in it:
        outrow = list(row)
        if not includeoriginal:
            del outrow[fidx]
        for key in keys:
            try:
                outrow.append(row[fidx][key])
            except (IndexError, KeyError, TypeError):
                outrow.append(missing)
        yield tuple(outrow)


def unpackzip(table, fields, newfields=None, include_original=False,
              strict=True, missing=None, index=None):
    """
    Unpack several sequence-valued fields together, pairing their elements by
    position rather than taking their cartesian product. E.g.::

        >>> import petl as etl
        >>> table1 = [['id', 'times', 'values'],
        ...           [1, ['t1', 't2'], [10, 20]],
        ...           [2, ['t3'], [30, 40]]]
        >>> table2 = etl.unpackzip(table1, ['times', 'values'])
        >>> table2
        +----+-------+--------+
        | id | times | values |
        +====+=======+========+
        |  1 | 't1'  |     10 |
        +----+-------+--------+
        |  1 | 't2'  |     20 |
        +----+-------+--------+

    Each input row produces one output row per aligned element, so the
    output fields keep the same names as the unpacked fields unless
    `newfields` is given.

    When `strict` is True (the default) every unpacked value must be a
    sequence of the same length. Rows whose values have differing lengths
    are dropped in full, without emitting partial results. When `strict` is
    False the longest value determines the number of output rows and shorter
    values are padded with `missing`.

    If `index` is given, it is used as the name of an extra output field
    containing the zero-based position of each aligned element.

    Only lists, tuples and other non-string, non-mapping iterables are
    treated as sequences; strings, bytes and mappings raise an
    :class:`petl.errors.ArgumentError`. ``None`` and empty sequences produce
    no output rows in strict mode, while under non-strict mode ``None`` is
    treated as an empty sequence and padded with `missing`.

    Negative field indices count from the end of the header. When field
    names repeat, an index selects that position rather than the first
    occurrence of the name.

    See also :func:`petl.transform.unpacks.unpack`.

    """

    return UnpackZipView(table, fields, newfields=newfields,
                         include_original=include_original,
                         strict=strict, missing=missing, index=index)


Table.unpackzip = unpackzip


class UnpackZipView(Table):

    def __init__(self, source, fields, newfields=None, include_original=False,
                 strict=True, missing=None, index=None):
        self.source = source
        self.fields = fields
        self.newfields = newfields
        self.include_original = include_original
        self.strict = strict
        self.missing = missing
        self.index = index

    def __iter__(self):
        return iterunpackzip(self.source, self.fields, self.newfields,
                             self.include_original, self.strict, self.missing,
                             self.index)


def _resolve_unpackzip_indices(flds, fields):
    if not isinstance(fields, (list, tuple)):
        raise ArgumentError(
            'fields argument must be a list or tuple of field names or '
            'indices')
    if len(fields) == 0:
        raise ArgumentError('at least one field must be given')
    indices = []
    for field in fields:
        if isinstance(field, int) and -len(flds) <= field < len(flds):
            indices.append(field + len(flds) if field < 0 else field)
        elif field in flds:
            indices.append(flds.index(field))
        else:
            raise ArgumentError(
                'field invalid: must be either field name or index')
    return indices


def _materialize_sequence(value, field):
    if value is None:
        return []
    if isinstance(value, (string_types, binary_type, Mapping)):
        raise ArgumentError(
            'value in field %r is not a sequence: %r' % (field, value))
    if isinstance(value, (list, tuple)):
        return list(value)
    if isinstance(value, Iterable):
        # materialise once so a single-use generator is never re-consumed
        return list(value)
    raise ArgumentError(
        'value in field %r is not a sequence: %r' % (field, value))


def iterunpackzip(source, fields, newfields, include_original, strict,
                  missing, index):
    it = iter(source)

    try:
        hdr = next(it)
    except StopIteration:
        return
    flds = list(map(text_type, hdr))
    field_indices = _resolve_unpackzip_indices(flds, fields)

    if newfields is None:
        out_newfields = [flds[i] for i in field_indices]
    elif isinstance(newfields, (list, tuple)):
        if len(newfields) != len(field_indices):
            raise ArgumentError(
                'newfields argument must have one name per unpacked field')
        out_newfields = list(newfields)
    else:
        raise ArgumentError(
            'newfields argument must be a list or tuple of field names')

    outhdr = list(flds)
    if not include_original:
        for field_index in sorted(field_indices, reverse=True):
            del outhdr[field_index]
    outhdr.extend(out_newfields)
    if index is not None:
        outhdr.append(index)
    yield tuple(outhdr)

    drop_positions = set() if include_original else set(field_indices)

    for row in it:
        sequences = []
        for field_index in field_indices:
            value = row[field_index] if field_index < len(row) else None
            sequence = _materialize_sequence(
                value, flds[field_index])
            sequences.append(sequence)

        if strict:
            expected_length = len(sequences[0])
            if any(len(sequence) != expected_length
                   for sequence in sequences[1:]):
                # drop the whole row, never emit partial results
                continue
            row_length = expected_length
        else:
            row_length = max((len(sequence) for sequence in sequences),
                             default=0)

        if row_length == 0:
            continue

        base_row = [value for position, value in enumerate(row)
                    if position not in drop_positions]
        for element_index in range(row_length):
            out_row = list(base_row)
            for sequence in sequences:
                if element_index < len(sequence):
                    out_row.append(sequence[element_index])
                else:
                    out_row.append(missing)
            if index is not None:
                out_row.append(element_index)
            yield tuple(out_row)
