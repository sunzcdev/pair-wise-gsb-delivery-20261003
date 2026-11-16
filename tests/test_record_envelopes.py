"""
Tests for iterating over records together with their physical position.
"""
import builtins
import datetime
import struct
import warnings

from pytest import fixture, raises

from dbfread import DBF, FieldParser, InvalidValue
from dbfread.dbf import RecordEnvelope
from dbfread.exceptions import (RecordCountMismatchWarning,
                                TruncatedRecordWarning)


def make_dbf(path, records, numrecords=None, eof=True):
    """Write a minimal DBF file with NAME (C, 10) and BIRTHDATE (D, 8).

    ``records`` is a list of ``(deleted, name, date)`` tuples where
    ``date`` is a ``datetime.date`` or ``None``. ``numrecords`` can be
    used to store a wrong count in the header.
    """
    name_length = 10
    fields = [('NAME', b'C', name_length), ('BIRTHDATE', b'D', 8)]
    recordlen = 1 + sum(field[2] for field in fields)
    headerlen = 32 + 32 * len(fields) + 1

    header = struct.pack(
        '<BBBBLHHHBBLLLBBH',
        0x03, 114, 8, 2,
        len(records) if numrecords is None else numrecords,
        headerlen, recordlen,
        0, 0, 0, 0, 0, 0, 0, 0, 0)

    out = bytearray(header)
    for (name, field_type, length) in fields:
        out += struct.pack(
            '<11scLBBHBBBB7sB',
            name.encode('ascii'), field_type, 0, length,
            0, 0, 0, 0, 0, 0, b'\0' * 7, 0)
    out += b'\r'

    for (deleted, name, date) in records:
        out += b'*' if deleted else b' '
        out += name.encode('ascii').ljust(name_length, b' ')
        out += (date.strftime('%Y%m%d').encode('ascii')
                if date is not None else b'        ')

    if eof:
        out += b'\x1a'

    with open(str(path), 'wb') as outfile:
        outfile.write(bytes(out))

    return str(path)


@fixture
def sample(tmpdir):
    # The deleted record sits in the middle, so skipping it would
    # renumber the record that follows.
    return make_dbf(tmpdir.join('sample.dbf'), [
        (False, 'Alice', datetime.date(1987, 3, 1)),
        (True, 'Deleted', datetime.date(1979, 12, 22)),
        (False, 'Bob', datetime.date(1980, 11, 12)),
    ])


def names(envelopes):
    return [envelope.record['NAME'] for envelope in envelopes]


def test_public_imports():
    from dbfread import (RecordCountMismatchWarning, RecordCountReport,
                         RecordEnvelope, TruncatedRecordWarning)
    assert RecordEnvelope._fields == ('index', 'deleted', 'record')
    assert RecordCountReport._fields == ('header_count', 'actual_count',
                                         'difference', 'truncated')
    assert issubclass(RecordCountMismatchWarning, UserWarning)
    assert issubclass(TruncatedRecordWarning, UserWarning)


def test_file_order_and_physical_numbers(sample):
    envelopes = list(DBF(sample).iter_records())

    assert names(envelopes) == ['Alice', 'Deleted', 'Bob']
    assert [envelope.index for envelope in envelopes] == [0, 1, 2]
    assert [envelope.record_number for envelope in envelopes] == [1, 2, 3]
    assert [envelope.deleted for envelope in envelopes] == [False, True,
                                                            False]

    # The default iteration is unchanged and returns active records.
    assert [record['NAME'] for record in DBF(sample)] == ['Alice', 'Bob']


def test_range_uses_physical_numbers(sample):
    table = DBF(sample)

    assert names(table.iter_records(first=1)) == ['Deleted', 'Bob']
    assert names(table.iter_records(last=1)) == ['Alice', 'Deleted']
    assert names(table.iter_records(first=1, last=1)) == ['Deleted']
    assert names(table.iter_records(first=0, last=0)) == ['Alice']

    # Reading past the end yields nothing.
    assert names(table.iter_records(first=10)) == []

    # The numbers do not change when deleted records are skipped.
    assert [envelope.index
            for envelope in table.iter_records(first=2)] == [2]


def test_range_validation(sample):
    table = DBF(sample)

    with raises(ValueError):
        list(table.iter_records(first=-1))
    with raises(ValueError):
        list(table.iter_records(last=-1))
    with raises(ValueError):
        list(table.iter_records(first=2, last=1))
    with raises(TypeError):
        list(table.iter_records(first='1'))


def test_loaded_matches_streaming(sample):
    streamed = list(DBF(sample).iter_records())
    loaded_table = DBF(sample, load=True)
    loaded = list(loaded_table.iter_records())

    assert loaded == streamed
    assert loaded_table.envelopes == streamed
    assert len(loaded_table.envelopes) == 3

    for (first, last) in [(None, None), (1, None), (None, 1), (1, 2),
                          (2, 2)]:
        assert list(loaded_table.iter_records(first, last)) == \
            list(DBF(sample).iter_records(first, last))

    # Loading still splits the records the same way.
    assert [record['NAME'] for record in loaded_table] == ['Alice', 'Bob']
    assert [record['NAME'] for record in loaded_table.deleted] == ['Deleted']
    assert len(loaded_table) == 2
    assert len(loaded_table.deleted) == 1

    # Unloading drops the cached envelopes.
    loaded_table.unload()
    assert not loaded_table.loaded
    assert list(loaded_table.iter_records()) == streamed


def test_envelopes_property_streams(sample):
    table = DBF(sample)

    assert not table.loaded
    assert len(table.envelopes) == 3
    assert names(table.envelopes) == ['Alice', 'Deleted', 'Bob']


def test_envelope_access(sample):
    envelope = list(DBF(sample).iter_records(first=1))[0]

    assert isinstance(envelope, RecordEnvelope)
    assert envelope[0] == 1
    assert envelope['index'] == 1
    assert envelope['deleted'] is True
    assert envelope['record']['NAME'] == 'Deleted'
    assert envelope.get('record')['NAME'] == 'Deleted'
    assert envelope.get('missing', 'default') == 'default'

    index, deleted, record = envelope
    assert (index, deleted) == (1, True)
    assert record['NAME'] == 'Deleted'


def test_header_count_mismatch_too_few(tmpdir):
    path = make_dbf(tmpdir.join('few.dbf'), [
        (False, 'Alice', None),
        (False, 'Bob', None),
    ], numrecords=5)

    table = DBF(path)
    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter('always')
        assert len(list(table.iter_records())) == 2

    assert any(issubclass(w.category, RecordCountMismatchWarning)
               for w in caught)

    report = table.check_record_count()
    assert report.header_count == 5
    assert report.actual_count == 2
    assert report.difference == 3
    assert report.missing == 3
    assert not report.truncated
    assert not report.ok


def test_header_count_mismatch_too_many(tmpdir):
    path = make_dbf(tmpdir.join('many.dbf'), [
        (False, 'Alice', None),
        (True, 'Bob', None),
    ], numrecords=1)

    table = DBF(path)
    report = table.check_record_count()
    assert (report.header_count, report.actual_count) == (1, 2)
    assert report.difference == -1
    assert report.extra == 1

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter('always')
        assert len(list(table.iter_records())) == 2

    assert any(issubclass(w.category, RecordCountMismatchWarning)
               for w in caught)


def test_matching_count_does_not_warn(sample):
    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter('always')
        list(DBF(sample).iter_records())

    assert not any(issubclass(w.category, RecordCountMismatchWarning)
                   for w in caught)


def test_truncated_tail_is_not_data(tmpdir):
    path = tmpdir.join('truncated.dbf')
    make_dbf(path, [
        (False, 'Alice', None),
        (False, 'Bob', None),
    ])
    # Drop the end-of-file marker and half of the last record.
    data = path.read_binary()
    path.write_binary(data[:-1 - 7])

    table = DBF(str(path))

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter('always')
        envelopes = list(table.iter_records())

    assert names(envelopes) == ['Alice']
    assert [envelope.index for envelope in envelopes] == [0]
    assert any(issubclass(w.category, TruncatedRecordWarning)
               for w in caught)

    report = table.check_record_count()
    assert report.header_count == 2
    assert report.actual_count == 1
    assert report.difference == 1
    assert report.truncated
    assert not report.ok

    # The partial record must not appear as active or deleted data.
    with warnings.catch_warnings(record=True):
        warnings.simplefilter('always')
        loaded = DBF(str(path), load=True)
    assert [record['NAME'] for record in loaded] == ['Alice']
    assert len(loaded) == 1

    # The normal streaming iteration skips it as well.
    with warnings.catch_warnings(record=True):
        warnings.simplefilter('always')
        assert [record['NAME'] for record in table] == ['Alice']
        assert [record['NAME'] for record in table.deleted] == []
        assert list(table.envelopes)[-1].index == 0


def test_custom_parser_and_recfactory(sample):
    class UpperParser(FieldParser):
        def parseC(self, field, data):
            return FieldParser.parseC(self, field, data).upper()

    def recfactory(items):
        return dict(items)

    table = DBF(sample, parserclass=UpperParser, recfactory=recfactory)
    envelope = list(table.iter_records(first=0))[0]

    assert isinstance(envelope.record, dict)
    assert envelope.record['NAME'] == 'ALICE'

    # The custom parser is used by the normal iteration as well.
    assert [record['NAME'] for record in table] == ['ALICE', 'BOB']


def test_invalid_values_use_custom_parser(tmpdir):
    class LenientParser(FieldParser):
        def parse(self, field, data):
            try:
                return FieldParser.parse(self, field, data)
            except ValueError:
                return InvalidValue(data)

    path = tmpdir.join('invalid.dbf')
    make_dbf(path, [(False, 'Alice', None)])
    data = bytearray(path.read_binary())
    # Corrupt the birth date of the only record.
    offset = 32 + 32 * 2 + 1 + 1 + 10
    data[offset:offset + 8] = b'NotAYear'
    path.write_binary(bytes(data))

    table = DBF(str(path), parserclass=LenientParser)
    envelope = list(table.iter_records())[0]
    assert isinstance(envelope.record['BIRTHDATE'], InvalidValue)


def test_files_are_opened_once(tmpdir, monkeypatch):
    path = make_dbf(tmpdir.join('counted.dbf'), [
        (False, 'Alice', None),
        (True, 'Bob', None),
        (False, 'Carol', None),
    ])

    opened = []
    real_open = builtins.open

    def counting_open(file, *args, **kwargs):
        opened.append(str(file))
        return real_open(file, *args, **kwargs)

    table = DBF(path)
    monkeypatch.setattr(builtins, 'open', counting_open)
    opened.clear()

    list(table.iter_records())

    assert len([name for name in opened if name.endswith('.dbf')]) == 1


def test_memo_file_opened_once(monkeypatch):
    opened = []
    real_open = builtins.open

    def counting_open(file, *args, **kwargs):
        opened.append(str(file))
        return real_open(file, *args, **kwargs)

    table = DBF('tests/cases/memotest.dbf')
    monkeypatch.setattr(builtins, 'open', counting_open)
    opened.clear()

    envelopes = list(table.iter_records())

    assert [envelope.record['MEMO'] for envelope in envelopes] == \
        ['Alice memo', 'Bob memo', 'Deleted Guy memo']
    assert len([name for name in opened if name.endswith('.dbf')]) == 1
    memo_opens = [name for name in opened if name.lower().endswith('.fpt')]
    assert len(memo_opens) == 1


def test_load_opens_file_once(tmpdir, monkeypatch):
    path = make_dbf(tmpdir.join('loadonce.dbf'), [
        (False, 'Alice', None),
        (True, 'Bob', None),
        (False, 'Carol', None),
    ])

    table = DBF(path)

    opened = []
    real_open = builtins.open

    def counting_open(file, *args, **kwargs):
        opened.append(str(file))
        return real_open(file, *args, **kwargs)

    monkeypatch.setattr(builtins, 'open', counting_open)
    opened.clear()

    table.load()

    assert len([name for name in opened if name.endswith('.dbf')]) == 1
    assert [envelope.index for envelope in table.envelopes] == [0, 1, 2]


def test_existing_files_have_matching_counts():
    for filename in ['tests/cases/memotest.dbf',
                     'tests/cases/no_memofile.dbf',
                     'examples/files/people.dbf']:
        table = DBF(filename, ignore_missing_memofile=True)
        report = table.check_record_count()
        assert report.ok, (filename, report)
        assert report.truncated is False
        assert len(list(table.iter_records())) == report.actual_count


def test_trailing_padding_is_not_truncation(tmpdir):
    for tail, name in [(b'    ', 'spaces'), (b'\x00' * 4, 'nuls')]:
        path = tmpdir.join('padded_{}.dbf'.format(name))
        make_dbf(path, [
            (False, 'Alice', None),
            (False, 'Bob', None),
        ], eof=False)
        path.write_binary(path.read_binary() + tail)

        table = DBF(str(path))

        with warnings.catch_warnings(record=True) as caught:
            warnings.simplefilter('always')
            assert names(table.iter_records()) == ['Alice', 'Bob']

        assert not any(issubclass(w.category, TruncatedRecordWarning)
                       for w in caught)
        assert not any(issubclass(w.category, RecordCountMismatchWarning)
                       for w in caught)

        report = table.check_record_count()
        assert report.ok
        assert not report.truncated


def test_truncated_report_and_lengths_agree(tmpdir):
    path = tmpdir.join('short.dbf')
    make_dbf(path, [
        (False, 'Alice', None),
        (False, 'Bob', None),
    ])
    # Remove the end-of-file marker and part of the last record.
    path.write_binary(path.read_binary()[:-6])

    table = DBF(str(path))

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter('always')
        assert names(table.iter_records()) == ['Alice']

    assert any(issubclass(w.category, TruncatedRecordWarning)
               for w in caught)

    # len() must agree with what iteration returns.
    assert len(table) == 1
    assert len(table.deleted) == 0
    assert len(table.envelopes) == 1

    report = table.check_record_count()
    assert report.truncated
    assert report.difference == 1
    assert not report.ok


def test_unknown_record_flag_keeps_position(tmpdir):
    path = tmpdir.join('unknown.dbf')
    make_dbf(path, [(False, 'Alice', None)], eof=False)
    data = bytearray(path.read_binary())
    # Add a slot that starts with a byte that is not a record flag,
    # then another normal record after it.
    data += b'X' + b'\0' * (1 + 10 + 8 - 1)
    data += b' ' + b'Carol'.ljust(10, b' ') + b'        '
    path.write_binary(bytes(data) + b'\x1a')

    with warnings.catch_warnings(record=True):
        warnings.simplefilter('always')
        envelopes = list(DBF(str(path)).iter_records())

    # The slot with the unknown flag is skipped, but the record after
    # it keeps physical position 2.
    assert names(envelopes) == ['Alice', 'Carol']
    assert [envelope.index for envelope in envelopes] == [0, 2]


def test_all_deleted_and_all_active(tmpdir):
    path = make_dbf(tmpdir.join('all_deleted.dbf'), [
        (True, 'One', None),
        (True, 'Two', None),
    ])

    table = DBF(path)
    assert names(table.iter_records()) == ['One', 'Two']
    assert [envelope.deleted for envelope in table.iter_records()] == \
        [True, True]

    # Normal iteration returns no active records, while deleted and
    # the lengths still work.
    assert list(table) == []
    assert len(table) == 0
    assert len(table.deleted) == 2
    assert len(table.envelopes) == 2


def test_load_keeps_file_order(tmpdir):
    path = make_dbf(tmpdir.join('order.dbf'), [
        (True, 'Deleted1', None),
        (False, 'Active', None),
        (True, 'Deleted2', None),
    ])

    table = DBF(path, load=True)
    assert [record['NAME'] for record in table] == ['Active']
    assert [record['NAME'] for record in table.deleted] == \
        ['Deleted1', 'Deleted2']

    assert [(envelope.index, envelope.deleted,
             envelope.record['NAME'])
            for envelope in table.envelopes] == [
        (0, True, 'Deleted1'),
        (1, False, 'Active'),
        (2, True, 'Deleted2'),
    ]
