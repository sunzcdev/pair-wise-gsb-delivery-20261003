import struct

from pytest import raises
from dbfread import DBF
from dbfread import MissingMemoFile
from dbfread import (MemoError, MemoBlockOutOfRange, MemoHeaderCorrupt,
                     MemoTruncated)
from dbfread.memo import (VFPMemoFile, DB3MemoFile, DB4MemoFile,
                          BinaryMemo)


def test_missing_memofile():
    with raises(MissingMemoFile):
        DBF('tests/cases/no_memofile.dbf')

    # This should succeed.
    table = DBF('tests/cases/no_memofile.dbf', ignore_missing_memofile=True)

    # Memo fields should be returned as None.
    record = next(iter(table))
    assert record['MEMO'] is None


#
# Helpers to build small DBF and memo files.
#

def make_dbf(path, dbversion, memo_indexes, memo_field_length=10,
             field_type='M'):
    """Write a DBF file with a single memo field named MEMO.

    memo_indexes is a list of memo block numbers, one per record.
    """
    numrecords = len(memo_indexes)
    headerlen = 32 + 32 + 1
    recordlen = 1 + memo_field_length

    header = struct.pack(
        '<BBBBLHHHBBLLLBBH',
        dbversion,      # dbversion
        126, 11, 14,    # date (2026-11-14)
        numrecords,
        headerlen,
        recordlen,
        0,              # reserved1
        0,              # incomplete_transaction
        0,              # encryption_flag
        0,              # free_record_thread
        0,              # reserved2
        0,              # reserved3
        0,              # mdx_flag
        0,              # language_driver
        0,              # reserved4
    )
    field = struct.pack(
        '<11scLBBHBBBB7sB',
        b'MEMO',        # name
        field_type.encode('ascii'),
        0,              # address
        memo_field_length,
        0,              # decimal_count
        0,              # reserved1
        0,              # workarea_id
        0,              # reserved2
        0,              # reserved3
        0,              # set_fields_flag
        b'\0' * 7,      # reserved4
        0,              # index_field_flag
    )

    records = b''
    for index in memo_indexes:
        if memo_field_length == 4:
            data = struct.pack('<I', index)
        else:
            data = str(index).encode('ascii').ljust(memo_field_length, b' ')
        records += b' ' + data

    with open(str(path), 'wb') as f:
        f.write(header + field + b'\r' + records + b'\x1a')


def make_fpt(path, blocks, blocksize=64):
    """Write a Visual FoxPro FPT file.

    blocks is a dict mapping block number to full block content
    (memo header + data).
    """
    nextblock = max(blocks) + 1 if blocks else 1
    size = max(512, nextblock * blocksize)
    data = bytearray(size)
    data[:8] = struct.pack('>LHH', nextblock, 0, blocksize)
    for index, content in blocks.items():
        start = index * blocksize
        data[start:start + len(content)] = content
    with open(str(path), 'wb') as f:
        f.write(bytes(data))


def vfp_block(data, memotype=1, declared_length=None):
    if declared_length is None:
        declared_length = len(data)
    return struct.pack('>LL', memotype, declared_length) + data


def make_db3_dbt(path, blocks, blocksize=512):
    """Write a dBase III DBT file (no header, fixed 512 byte blocks)."""
    size = (max(blocks) + 1) * blocksize if blocks else 0
    data = bytearray(size)
    for index, content in blocks.items():
        start = index * blocksize
        data[start:start + len(content)] = content
    with open(str(path), 'wb') as f:
        f.write(bytes(data))


def make_db4_dbt(path, blocks, blocksize=512):
    """Write a dBase IV DBT file with the block size in the header."""
    nextblock = max(blocks) + 1 if blocks else 1
    size = max(512, nextblock * blocksize)
    data = bytearray(size)
    data[:22] = struct.pack('<L16sH', nextblock, b'\0' * 16, blocksize)
    for index, content in blocks.items():
        start = index * blocksize
        data[start:start + len(content)] = content
    with open(str(path), 'wb') as f:
        f.write(bytes(data))


def db4_block(data, declared_length=None, signature=b'\xff\xff\x08\x08'):
    if declared_length is None:
        declared_length = len(data)
    return signature + struct.pack('<L', declared_length) + data


#
# Visual FoxPro (FPT) strict mode tests.
#

def make_vfp_table(tmp_path, memo_indexes, blocks, blocksize=64):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x30, memo_indexes, memo_field_length=4)
    make_fpt(tmp_path / 'test.fpt', blocks, blocksize=blocksize)
    return dbf_path


def test_vfp_strict_read_and_shared_block(tmp_path):
    # Two records point at the same block. Both must be readable.
    dbf_path = make_vfp_table(tmp_path, [8, 8, 0],
                              {8: vfp_block(b'hello')})
    table = DBF(str(dbf_path), strict_memo=True)
    records = list(table)
    assert records[0]['MEMO'] == 'hello'
    assert records[1]['MEMO'] == 'hello'
    # Empty reference (block 0) is not an error.
    assert records[2]['MEMO'] is None


def test_vfp_strict_binary_memo_not_truncated(tmp_path):
    # Binary memos may contain \x1a and \x1f. The length in the block
    # header is authoritative, no terminator is used.
    blob = b'\x00\x1a\x1f\xffbinary\x1adata'
    dbf_path = make_vfp_table(tmp_path, [8], {8: vfp_block(blob, memotype=0)})
    table = DBF(str(dbf_path), strict_memo=True)
    (record,) = list(table)
    assert isinstance(record['MEMO'], BinaryMemo)
    assert bytes(record['MEMO']) == blob


def test_vfp_strict_block_out_of_range(tmp_path):
    dbf_path = make_vfp_table(tmp_path, [8, 999], {8: vfp_block(b'hello')})
    table = DBF(str(dbf_path), strict_memo=True)
    with raises(MemoBlockOutOfRange) as excinfo:
        list(table)
    error = excinfo.value
    assert error.record == 2
    assert error.field == 'MEMO'
    assert error.block == 999
    assert 'record 2' in str(error)
    assert "'MEMO'" in str(error)
    assert 'block 999' in str(error)


def test_vfp_strict_corrupt_header(tmp_path):
    # 0x99 is not a known memo type.
    dbf_path = make_vfp_table(tmp_path, [8], {8: vfp_block(b'x', memotype=0x99)})
    table = DBF(str(dbf_path), strict_memo=True)
    with raises(MemoHeaderCorrupt) as excinfo:
        list(table)
    assert excinfo.value.record == 1
    assert excinfo.value.field == 'MEMO'
    assert excinfo.value.block == 8


def test_vfp_strict_truncated_memo(tmp_path):
    # The header declares 1000 bytes but the file ends long before.
    dbf_path = make_vfp_table(tmp_path, [8],
                              {8: vfp_block(b'short', declared_length=1000)})
    table = DBF(str(dbf_path), strict_memo=True)
    with raises(MemoTruncated) as excinfo:
        list(table)
    assert excinfo.value.record == 1
    assert excinfo.value.block == 8


def test_vfp_failed_read_does_not_move_cursor(tmp_path):
    # After a failed read the next, independent read must still seek
    # to the right block.
    fpt_path = tmp_path / 'test.fpt'
    make_fpt(fpt_path, {8: vfp_block(b'hello')})
    memo = VFPMemoFile(str(fpt_path), strict=True)
    with raises(MemoBlockOutOfRange):
        memo[999]
    assert memo[8] == b'hello'
    memo._close()


def test_vfp_compatible_mode_unchanged(tmp_path):
    # Default (compatible) mode reads valid files as before and does
    # not raise the new strict errors.
    dbf_path = make_vfp_table(tmp_path, [8, 0], {8: vfp_block(b'hello')})
    table = DBF(str(dbf_path))
    records = list(table)
    assert records[0]['MEMO'] == 'hello'
    assert records[1]['MEMO'] is None


#
# dBase III (DBT) strict mode tests.
#

def test_db3_strict(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x83, [1, 1, 0])
    make_db3_dbt(tmp_path / 'test.dbt', {1: b'alice memo\x1a'})

    table = DBF(str(dbf_path), strict_memo=True)
    records = list(table)
    assert records[0]['MEMO'] == 'alice memo'
    # Two records can share one block.
    assert records[1]['MEMO'] == 'alice memo'
    assert records[2]['MEMO'] is None


def test_db3_strict_block_out_of_range(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x83, [50])
    make_db3_dbt(tmp_path / 'test.dbt', {1: b'alice memo\x1a'})

    table = DBF(str(dbf_path), strict_memo=True)
    with raises(MemoBlockOutOfRange) as excinfo:
        list(table)
    assert excinfo.value.record == 1
    assert excinfo.value.field == 'MEMO'
    assert excinfo.value.block == 50


def test_db3_strict_missing_terminator(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x83, [1])
    # No \x1a terminator before end of file.
    make_db3_dbt(tmp_path / 'test.dbt', {1: b'unterminated memo'})

    table = DBF(str(dbf_path), strict_memo=True)
    with raises(MemoTruncated):
        list(table)


def test_db3_compatible_mode_unchanged(tmp_path):
    # In compatible mode an unterminated memo returns the data read
    # so far instead of raising.
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x83, [1])
    make_db3_dbt(tmp_path / 'test.dbt', {1: b'unterminated memo'})

    table = DBF(str(dbf_path))
    (record,) = list(table)
    assert record['MEMO'].startswith('unterminated memo')


#
# dBase IV (DBT) strict mode tests.
#

def test_db4_strict(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x8b, [1, 0])
    make_db4_dbt(tmp_path / 'test.dbt', {1: db4_block(b'bob memo')})

    table = DBF(str(dbf_path), strict_memo=True)
    records = list(table)
    assert records[0]['MEMO'] == 'bob memo'
    assert records[1]['MEMO'] is None


def test_db4_block_size_from_header(tmp_path):
    # The block size is read from the DBT file header (1024 here),
    # not assumed to be 512.
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x8b, [1])
    make_db4_dbt(tmp_path / 'test.dbt', {1: db4_block(b'big blocks')},
                 blocksize=1024)

    table = DBF(str(dbf_path), strict_memo=True)
    (record,) = list(table)
    assert record['MEMO'] == 'big blocks'


def test_db4_strict_binary_memo_not_truncated(tmp_path):
    # A binary memo may contain \x1f. In strict mode the length in
    # the block header is authoritative and the memo is not cut at
    # the terminator byte.
    blob = b'bin\x1fary da\x1ata'
    dbt_path = tmp_path / 'test.dbt'
    make_db4_dbt(dbt_path, {1: db4_block(blob)})

    memo = DB4MemoFile(str(dbt_path), strict=True)
    assert memo[1] == blob
    memo._close()


def test_db4_strict_corrupt_header(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x8b, [1])
    make_db4_dbt(tmp_path / 'test.dbt',
                 {1: db4_block(b'data', signature=b'\xde\xad\xbe\xef')})

    table = DBF(str(dbf_path), strict_memo=True)
    with raises(MemoHeaderCorrupt) as excinfo:
        list(table)
    assert excinfo.value.record == 1
    assert excinfo.value.field == 'MEMO'
    assert excinfo.value.block == 1


def test_db4_strict_truncated_memo(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x8b, [1])
    make_db4_dbt(tmp_path / 'test.dbt',
                 {1: db4_block(b'short', declared_length=1000)})

    table = DBF(str(dbf_path), strict_memo=True)
    with raises(MemoTruncated):
        list(table)


def test_db4_strict_block_out_of_range(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x8b, [7])
    make_db4_dbt(tmp_path / 'test.dbt', {1: db4_block(b'bob memo')})

    table = DBF(str(dbf_path), strict_memo=True)
    with raises(MemoBlockOutOfRange) as excinfo:
        list(table)
    assert excinfo.value.block == 7


def test_memo_errors_are_ioerrors(tmp_path):
    # MemoError subclasses IOError so existing except clauses keep
    # working.
    dbf_path = make_vfp_table(tmp_path, [999], {})
    table = DBF(str(dbf_path), strict_memo=True)
    with raises(IOError):
        list(table)
    with raises(MemoError):
        list(DBF(str(dbf_path), strict_memo=True))
