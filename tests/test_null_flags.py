"""
Tests for Visual FoxPro null flags (_NullFlags) support.

The test files are constructed locally in the DBF file format, so no
FoxPro installation is needed to run these tests.
"""
import datetime
import struct
from pytest import fixture, raises
from dbfread import DBF

DBFHeaderStruct = struct.Struct('<BBBBLHHHBBLLLBBH')
DBFFieldStruct = struct.Struct('<11scLBBHBBBB7sB')

# Visual FoxPro field flags (byte 18 of the field descriptor, stored
# in the low byte of the 'reserved1' header field).
FLAG_SYSTEM = 0x01
FLAG_NULLABLE = 0x02


def pack_field(name, type_, length, decimal=0, flags=0):
    return DBFFieldStruct.pack(
        name.encode('ascii'),
        type_.encode('ascii'),
        0,          # address
        length,
        decimal,
        flags,      # reserved1: low byte holds the VFP field flags
        0, 0, 0, 0,
        b'\0' * 7,
        0)


def write_dbf(path, fields, records, dbversion=0x30):
    """Write a minimal DBF file.

    fields is a list of (name, type, length, decimal, flags) tuples
    and records is a list of (deletion_flag, [field_data, ...]).
    """
    headerlen = 32 + 32 * len(fields) + 1
    recordlen = 1 + sum(field[2] for field in fields)

    header = DBFHeaderStruct.pack(
        dbversion,
        126, 11, 14,            # date: 2026-11-14
        len(records),
        headerlen,
        recordlen,
        0, 0, 0, 0, 0, 0, 0, 0, 0)

    with open(path, 'wb') as outfile:
        outfile.write(header)
        for field in fields:
            outfile.write(pack_field(*field))
        outfile.write(b'\r')
        for deletion_flag, data in records:
            outfile.write(deletion_flag + b''.join(data))
        outfile.write(b'\x1a')


# A table with four nullable fields, one non-nullable field and the
# hidden _NullFlags field. Bit n of the null bit map belongs to field
# n, so NAME=bit 0, AGE=bit 1, BIRTH=bit 2, OK=bit 3, CODE=bit 4.
FIELDS = [
    ('NAME', 'C', 10, 0, FLAG_NULLABLE),
    ('AGE', 'N', 3, 0, FLAG_NULLABLE),
    ('BIRTH', 'D', 8, 0, FLAG_NULLABLE),
    ('OK', 'L', 1, 0, FLAG_NULLABLE),
    ('CODE', 'C', 4, 0, 0),
    ('_NullFlags', '0', 1, 0, FLAG_SYSTEM),
]

RECORDS = [
    # No nulls.
    (b' ', [b'Alice     ', b' 30', b'19870301', b'T', b'AB  ', b'\x00']),
    # NAME and AGE are null even though the fields contain data.
    (b' ', [b'Bob       ', b'  0', b'19800101', b'F', b'    ', b'\x03']),
    # Deleted record where BIRTH and OK are null.
    (b'*', [b'Deleted   ', b'  0', b'        ', b' ', b'XY  ', b'\x0c']),
    # Empty values that are NOT flagged as null keep their normal
    # meaning ('' for text, 0 for numeric, None for empty date).
    (b' ', [b'          ', b'  0', b'        ', b' ', b'Z   ', b'\x00']),
]

expected_records = [
    {'NAME': u'Alice', 'AGE': 30, 'BIRTH': datetime.date(1987, 3, 1),
     'OK': True, 'CODE': u'AB'},
    {'NAME': None, 'AGE': None, 'BIRTH': datetime.date(1980, 1, 1),
     'OK': False, 'CODE': u''},
    {'NAME': u'', 'AGE': 0, 'BIRTH': None, 'OK': None, 'CODE': u'Z'},
]

expected_deleted = [
    {'NAME': u'Deleted', 'AGE': 0, 'BIRTH': None, 'OK': None,
     'CODE': u'XY'},
]


@fixture
def dbf_path(tmpdir):
    path = str(tmpdir.join('nullflags.dbf'))
    write_dbf(path, FIELDS, RECORDS)
    return path


def test_nullflags_field_is_hidden(dbf_path):
    table = DBF(dbf_path, encoding='ascii')

    # The _NullFlags field should not leak into the public field list.
    assert table.field_names == ['NAME', 'AGE', 'BIRTH', 'OK', 'CODE']
    assert [field.type for field in table.fields] == \
        ['C', 'N', 'D', 'L', 'C']

    for record in table:
        assert '_NullFlags' not in record


def test_null_values(dbf_path):
    table = DBF(dbf_path, encoding='ascii')
    assert list(table) == expected_records


def test_deleted_records(dbf_path):
    table = DBF(dbf_path, encoding='ascii')
    assert list(table.deleted) == expected_deleted


def test_load_and_unload(dbf_path):
    table = DBF(dbf_path, encoding='ascii', load=True)
    assert list(table) == expected_records
    assert list(table.deleted) == expected_deleted

    table.unload()
    assert list(table) == expected_records
    assert list(table.deleted) == expected_deleted


def test_recfactory(dbf_path):
    # A custom recfactory should get the same items: no _NullFlags
    # field and None for null values.
    table = DBF(dbf_path, encoding='ascii', recfactory=lambda items: items)
    records = list(table)

    assert records[0] == [('NAME', u'Alice'), ('AGE', 30),
                          ('BIRTH', datetime.date(1987, 3, 1)),
                          ('OK', True), ('CODE', u'AB')]
    assert ('NAME', None) in records[1]
    assert ('AGE', None) in records[1]
    assert all(name != '_NullFlags' for name, value in records[1])


def test_raw_mode_keeps_nullflags(dbf_path):
    # In raw mode the record is returned exactly as stored, including
    # the _NullFlags field, so the caller can verify the bit map.
    table = DBF(dbf_path, encoding='ascii', raw=True)
    records = list(table)

    assert records[0]['_NullFlags'] == b'\x00'
    assert records[1]['_NullFlags'] == b'\x03'
    assert records[1]['NAME'] == b'Bob       '
    assert records[1]['AGE'] == b'  0'


def test_nullable_fields_without_nullflags_field(tmpdir):
    # Nullable fields but no _NullFlags field is an invalid layout.
    fields = [field for field in FIELDS if field[0] != '_NullFlags']
    records = [(flag, data[:-1]) for flag, data in RECORDS]
    path = str(tmpdir.join('no_nullflags.dbf'))
    write_dbf(path, fields, records)

    table = DBF(path, encoding='ascii')
    with raises(ValueError) as excinfo:
        list(table)
    assert 'record 0' in str(excinfo.value)
    assert 'byte offset' in str(excinfo.value)

    # The error should point out the position of the deleted record
    # as well. (The deleted record is the third record in the file.)
    with raises(ValueError) as excinfo:
        list(table.deleted)
    assert 'record 2' in str(excinfo.value)


def test_short_nullflags_bitmap(tmpdir):
    # Nine nullable fields need a two byte bit map (one bit per field
    # including _NullFlags itself), but the field is only one byte.
    fields = ([('FIELD%d' % i, 'C', 1, 0, FLAG_NULLABLE)
               for i in range(9)] +
              [('_NullFlags', '0', 1, 0, FLAG_SYSTEM)])
    records = [(b' ', [b'x'] * 9 + [b'\x00'])]
    path = str(tmpdir.join('short_bitmap.dbf'))
    write_dbf(path, fields, records)

    table = DBF(path, encoding='ascii')
    with raises(ValueError) as excinfo:
        list(table)
    message = str(excinfo.value)
    assert 'record 0' in message
    assert '_NullFlags' in message
