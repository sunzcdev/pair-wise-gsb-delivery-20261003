# -*- coding: utf-8 -*-
from __future__ import absolute_import, print_function, division


from tempfile import NamedTemporaryFile
import gzip
import os
import logging
import pytest
from petl.compat import PY2


from petl.test.helpers import ieq, eq_
from petl.io.csv import fromcsv, fromtsv, tocsv, appendcsv, totsv, appendtsv, \
    CSVRecordError, CSVStructureError, CSVDecodeError


logger = logging.getLogger(__name__)
debug = logger.debug


def test_fromcsv():

    data = [b'foo,bar',
            b'a,1',
            b'b,2',
            b'c,2']
    f = NamedTemporaryFile(mode='wb', delete=False)
    f.write(b'\n'.join(data))
    f.close()

    expect = (('foo', 'bar'),
              ('a', '1'),
              ('b', '2'),
              ('c', '2'))
    actual = fromcsv(f.name, encoding='ascii')
    debug(actual)
    ieq(expect, actual)
    ieq(expect, actual)  # verify can iterate twice


def test_fromcsv_lineterminators():

    data = [b'foo,bar',
            b'a,1',
            b'b,2',
            b'c,2']

    expect = (('foo', 'bar'),
              ('a', '1'),
              ('b', '2'),
              ('c', '2'))

    for lt in b'\r', b'\n', b'\r\n':
        debug(repr(lt))
        f = NamedTemporaryFile(mode='wb', delete=False)
        f.write(lt.join(data))
        f.close()
        with open(f.name, 'rb') as g:
            debug(repr(g.read()))
        actual = fromcsv(f.name, encoding='ascii')
        debug(actual)
        ieq(expect, actual)


def test_fromcsv_quoted():
    import csv
    data = [b'"foo","bar"',
            b'"a",1',
            b'"b",2',
            b'"c",2']
    f = NamedTemporaryFile(mode='wb', delete=False)
    f.write(b'\n'.join(data))
    f.close()

    expect = (('foo', 'bar'),
              ('a', 1),
              ('b', 2),
              ('c', 2))
    actual = fromcsv(f.name, quoting=csv.QUOTE_NONNUMERIC)
    debug(actual)
    ieq(expect, actual)
    ieq(expect, actual)  # verify can iterate twice


def test_fromtsv():

    data = [b'foo\tbar',
            b'a\t1',
            b'b\t2',
            b'c\t2']
    f = NamedTemporaryFile(mode='wb', delete=False)
    f.write(b'\n'.join(data))
    f.close()

    expect = (('foo', 'bar'),
              ('a', '1'),
              ('b', '2'),
              ('c', '2'))
    actual = fromtsv(f.name, encoding='ascii')
    ieq(expect, actual)
    ieq(expect, actual)  # verify can iterate twice


def test_fromcsv_gz():

    data = [b'foo,bar',
            b'a,1',
            b'b,2',
            b'c,2']

    expect = (('foo', 'bar'),
              ('a', '1'),
              ('b', '2'),
              ('c', '2'))

    # '\r' not supported in PY2 because universal newline mode is
    # not supported by gzip module
    if PY2:
        lts = b'\n', b'\r\n'
    else:
        lts = b'\r', b'\n', b'\r\n'
    for lt in lts:
        f = NamedTemporaryFile(delete=False)
        f.close()
        fn = f.name + '.gz'
        os.rename(f.name, fn)
        fz = gzip.open(fn, 'wb')
        fz.write(lt.join(data))
        fz.close()
        actual = fromcsv(fn, encoding='ascii')
        ieq(expect, actual)
        ieq(expect, actual)  # verify can iterate twice


def test_tocsv_appendcsv():

    # exercise function
    table = (('foo', 'bar'),
             ('a', 1),
             ('b', 2),
             ('c', 2))
    f = NamedTemporaryFile(delete=False)
    f.close()
    tocsv(table, f.name, encoding='ascii', lineterminator='\n')

    # check what it did
    with open(f.name, 'rb') as o:
        data = [b'foo,bar',
                b'a,1',
                b'b,2',
                b'c,2']
        # don't forget final terminator
        expect = b'\n'.join(data) + b'\n'
        actual = o.read()
        eq_(expect, actual)

    # check appending
    table2 = (('foo', 'bar'),
              ('d', 7),
              ('e', 9),
              ('f', 1))
    appendcsv(table2, f.name, encoding='ascii', lineterminator='\n')

    # check what it did
    with open(f.name, 'rb') as o:
        data = [b'foo,bar',
                b'a,1',
                b'b,2',
                b'c,2',
                b'd,7',
                b'e,9',
                b'f,1']
        # don't forget final terminator
        expect = b'\n'.join(data) + b'\n'
        actual = o.read()
        eq_(expect, actual)


def test_tocsv_noheader():

    # check explicit no header
    table = (('foo', 'bar'),
             ('a', 1),
             ('b', 2),
             ('c', 2))
    f = NamedTemporaryFile(delete=False)
    tocsv(table, f.name, encoding='ascii', lineterminator='\n',
          write_header=False)

    # check what it did
    with open(f.name, 'rb') as o:
        data = [b'a,1',
                b'b,2',
                b'c,2']
        # don't forget final terminator
        expect = b'\n'.join(data) + b'\n'
        actual = o.read()
        eq_(expect, actual)


def test_totsv_appendtsv():

    # exercise function
    table = (('foo', 'bar'),
             ('a', 1),
             ('b', 2),
             ('c', 2))
    f = NamedTemporaryFile(delete=False)
    f.close()
    totsv(table, f.name, encoding='ascii', lineterminator='\n')

    # check what it did
    with open(f.name, 'rb') as o:
        data = [b'foo\tbar',
                b'a\t1',
                b'b\t2',
                b'c\t2']
        # don't forget final terminator
        expect = b'\n'.join(data) + b'\n'
        actual = o.read()
        eq_(expect, actual)

    # check appending
    table2 = (('foo', 'bar'),
              ('d', 7),
              ('e', 9),
              ('f', 1))
    appendtsv(table2, f.name, encoding='ascii', lineterminator='\n')

    # check what it did
    with open(f.name, 'rb') as o:
        data = [b'foo\tbar',
                b'a\t1',
                b'b\t2',
                b'c\t2',
                b'd\t7',
                b'e\t9',
                b'f\t1']
        # don't forget final terminator
        expect = b'\n'.join(data) + b'\n'
        actual = o.read()
        eq_(expect, actual)


def test_tocsv_appendcsv_gz():

    # exercise function
    table = (('foo', 'bar'),
             ('a', 1),
             ('b', 2),
             ('c', 2))
    f = NamedTemporaryFile(delete=False)
    fn = f.name + '.gz'
    f.close()
    tocsv(table, fn, encoding='ascii', lineterminator='\n')

    # check what it did
    o = gzip.open(fn, 'rb')
    try:
        data = [b'foo,bar',
                b'a,1',
                b'b,2',
                b'c,2']
        # don't forget final terminator
        expect = b'\n'.join(data) + b'\n'
        actual = o.read()
        eq_(expect, actual)
    finally:
        o.close()

    # check appending
    table2 = (('foo', 'bar'),
              ('d', 7),
              ('e', 9),
              ('f', 1))
    appendcsv(table2, fn, encoding='ascii', lineterminator='\n')

    # check what it did
    o = gzip.open(fn, 'rb')
    try:
        data = [b'foo,bar',
                b'a,1',
                b'b,2',
                b'c,2',
                b'd,7',
                b'e,9',
                b'f,1']
        # don't forget final terminator
        expect = b'\n'.join(data) + b'\n'
        actual = o.read()
        eq_(expect, actual)
    finally:
        o.close()
        
def test_fromcsv_header():

    header = ['foo', 'bar']
    data = [b'a,1',
            b'b,2',
            b'c,2']
    f = NamedTemporaryFile(mode='wb', delete=False)
    f.write(b'\n'.join(data))
    f.close()

    expect = (('foo', 'bar'),
              ('a', '1'),
              ('b', '2'),
              ('c', '2'))
    actual = fromcsv(f.name, encoding='ascii', header=header)
    debug(actual)
    ieq(expect, actual)
    ieq(expect, actual)  # verify can iterate twice


def test_fromcsv_diagnostics_column_counts():
    data = (b'preamble\n'
            b'\n'
            b'foo,bar\n'
            b'a,1\n'
            b'\n'
            b'b,2,3\n'
            b'c,"multi\n'
            b'line",extra\n'
            b'd,4\n')
    f = NamedTemporaryFile(mode='wb', delete=False)
    f.write(data)
    f.close()

    result = fromcsv(f.name, encoding='ascii', diagnostics=True, skip=1)
    expect = (('foo', 'bar'),
              ('a', '1'),
              ('d', '4'))
    ieq(expect, result)
    ieq(expect, result)  # cached, does not reread the source

    diagnostics = result.diagnostics
    eq_(2, len(diagnostics))

    first = diagnostics[0]
    eq_('column_count', first.kind)
    eq_('b,2,3\n', first.raw_text)
    eq_(b'b,2,3\n', first.raw_bytes)
    eq_(6, first.start_line)
    eq_(6, first.end_line)
    eq_(2, first.expected_columns)
    eq_(3, first.actual_columns)

    second = diagnostics[1]
    eq_('column_count', second.kind)
    eq_('c,"multi\nline",extra\n', second.raw_text)
    eq_(7, second.start_line)
    eq_(8, second.end_line)
    eq_(2, second.expected_columns)
    eq_(3, second.actual_columns)

    os.unlink(f.name)


def test_fromcsv_diagnostics_first_error():
    data = (b'foo,bar\n'
            b'a,1\n'
            b'b,2,3\n'
            b'c,4\n')
    f = NamedTemporaryFile(mode='wb', delete=False)
    f.write(data)
    f.close()

    with pytest.raises(CSVRecordError) as excinfo:
        list(fromcsv(f.name, encoding='ascii', diagnostics=True,
                     on_error='raise'))
    error = excinfo.value
    eq_('column_count', error.kind)
    eq_('b,2,3\n', error.raw_text)
    eq_(3, error.start_line)
    eq_(3, error.end_line)
    eq_(2, error.expected_columns)
    eq_(3, error.actual_columns)
    os.unlink(f.name)


def test_fromcsv_diagnostics_structure_error_stops():
    data = (b'foo,bar\n'
            b'a,1\n'
            b'b,"unclosed\n')
    f = NamedTemporaryFile(mode='wb', delete=False)
    f.write(data)
    f.close()

    result = fromcsv(f.name, encoding='ascii', diagnostics=True,
                     strict=True)
    ieq((('foo', 'bar'), ('a', '1')), result)
    eq_(1, len(result.diagnostics))
    diagnostic = result.diagnostics[0]
    eq_('csv', diagnostic.kind)
    eq_(3, diagnostic.start_line)
    eq_(3, diagnostic.end_line)
    eq_(True, diagnostic.fatal)
    eq_([], result.decode_errors)
    eq_(1, len(result.structure_errors))

    with pytest.raises(CSVStructureError):
        list(fromcsv(f.name, encoding='ascii', diagnostics=True,
                     strict=True, raise_on_fatal=True))
    os.unlink(f.name)


def test_fromcsv_diagnostics_decode_error_is_separate():
    data = (b'foo,bar\n'
            b'a,1\n'
            b'b,\xff\n'
            b'c,4\n')
    f = NamedTemporaryFile(mode='wb', delete=False)
    f.write(data)
    f.close()

    result = fromcsv(f.name, encoding='ascii', diagnostics=True)
    ieq((('foo', 'bar'), ('a', '1')), result)
    eq_(1, len(result.diagnostics))
    diagnostic = result.diagnostics[0]
    eq_('decode', diagnostic.kind)
    eq_(3, diagnostic.start_line)
    eq_(3, diagnostic.end_line)
    eq_(b'b,\xff\n', diagnostic.raw_bytes)
    eq_([], result.structure_errors)
    eq_(1, len(result.decode_errors))

    with pytest.raises(CSVDecodeError):
        list(fromcsv(f.name, encoding='ascii', diagnostics=True,
                     raise_on_fatal=True))
    os.unlink(f.name)
