import struct

from pytest import raises
from dbfread import DBF
from dbfread import MissingMemoFile
from dbfread import (MemoError, MemoBlockOutOfRange, MemoHeaderCorrupt,
                     MemoTruncated, MemoReferenceInvalid)
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
# These write real files with the exact layout dbfread reads, so the
# tests exercise the real readers instead of mocks.
#

def make_dbf(path, dbversion, memo_indexes, memo_field_length=10,
             field_type='M'):
    """Write a DBF file with a single memo field named MEMO.

    memo_indexes is a list of memo block numbers (or raw field values),
    one per record.
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
        if isinstance(index, bytes):
            data = index.ljust(memo_field_length, b' ')
        elif memo_field_length == 4:
            data = struct.pack('<I', index & 0xffffffff)
        else:
            data = str(index).encode('ascii').ljust(memo_field_length, b' ')
        records += b' ' + data

    with open(str(path), 'wb') as f:
        f.write(header + field + b'\r' + records + b'\x1a')


def make_fpt(path, blocks, blocksize=64):
    """Write a Visual FoxPro FPT file.

    blocks is a dict mapping block number to full block content
    (memo header + data). The file header is 512 bytes.
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
# Visual FoxPro (FPT) strict mode.
#

def make_vfp_table(tmp_path, memo_indexes, blocks, blocksize=64):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x30, memo_indexes, memo_field_length=4)
    make_fpt(tmp_path / 'test.fpt', blocks, blocksize=blocksize)
    return dbf_path


def test_vfp_strict_read_and_shared_block(tmp_path):
    # Two records point at the same block. Both must be readable, and
    # an empty reference (block 0) is not an error.
    dbf_path = make_vfp_table(tmp_path, [8, 8, 0],
                              {8: vfp_block(b'hello')})
    records = list(DBF(str(dbf_path), strict_memo=True))
    assert [r['MEMO'] for r in records] == ['hello', 'hello', None]


def test_vfp_strict_binary_memo_not_truncated(tmp_path):
    # Binary memos may contain \x1a and \x1f. The length in the block
    # header is authoritative, no terminator is used.
    blob = b'\x00\x1a\x1f\xffbinary\x1adata'
    dbf_path = make_vfp_table(tmp_path, [8], {8: vfp_block(blob, memotype=0)})
    (record,) = list(DBF(str(dbf_path), strict_memo=True))
    assert isinstance(record['MEMO'], BinaryMemo)
    assert bytes(record['MEMO']) == blob


def test_vfp_strict_block_out_of_range(tmp_path):
    dbf_path = make_vfp_table(tmp_path, [8, 999], {8: vfp_block(b'hello')})
    with raises(MemoBlockOutOfRange) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))

    error = excinfo.value
    assert error.record == 2
    assert error.field == 'MEMO'
    assert error.block == 999
    # The context is also in the message.
    assert 'record 2' in str(error)
    assert "'MEMO'" in str(error)
    assert 'block 999' in str(error)


def test_vfp_strict_large_reference(tmp_path):
    # VFP memo references are 4 byte unsigned integers, so 0xffffffff
    # (the all-ones field) is a damaged reference and must be reported
    # as out of range rather than read from a bogus offset.
    dbf_path = make_vfp_table(tmp_path, [b'\xff\xff\xff\xff'],
                              {8: vfp_block(b'hello')})
    with raises(MemoBlockOutOfRange) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))
    assert excinfo.value.record == 1
    assert excinfo.value.field == 'MEMO'
    assert excinfo.value.block == 0xffffffff


def test_vfp_strict_corrupt_header(tmp_path):
    # 0x99 is not a known memo type.
    dbf_path = make_vfp_table(tmp_path, [8],
                              {8: vfp_block(b'x', memotype=0x99)})
    with raises(MemoHeaderCorrupt) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))
    assert excinfo.value.record == 1
    assert excinfo.value.field == 'MEMO'
    assert excinfo.value.block == 8


def test_vfp_strict_partial_block_header(tmp_path):
    # The block starts inside the file but is cut short: that is a
    # corrupt header, not an out of range block number.
    dbf_path = make_vfp_table(tmp_path, [8], {8: vfp_block(b'hello')})
    with open(str(tmp_path / 'test.fpt'), 'r+b') as f:
        f.truncate(8 * 64 + 4)

    with raises(MemoHeaderCorrupt) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))
    assert excinfo.value.record == 1
    assert excinfo.value.field == 'MEMO'
    assert excinfo.value.block == 8


def test_vfp_strict_truncated_memo(tmp_path):
    # The header declares 1000 bytes but the file ends long before.
    dbf_path = make_vfp_table(tmp_path, [8],
                              {8: vfp_block(b'short', declared_length=1000)})
    with raises(MemoTruncated) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))
    assert excinfo.value.record == 1
    assert excinfo.value.field == 'MEMO'
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
    assert [r['MEMO'] for r in DBF(str(dbf_path))] == ['hello', None]


#
# dBase III (DBT) strict mode.
#

def test_db3_strict(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x83, [1, 1, 0])
    make_db3_dbt(tmp_path / 'test.dbt', {1: b'alice memo\x1a'})

    records = list(DBF(str(dbf_path), strict_memo=True))
    assert [r['MEMO'] for r in records] == ['alice memo', 'alice memo', None]


def test_db3_strict_block_out_of_range(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x83, [50])
    make_db3_dbt(tmp_path / 'test.dbt', {1: b'alice memo\x1a'})

    with raises(MemoBlockOutOfRange) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))
    assert excinfo.value.record == 1
    assert excinfo.value.field == 'MEMO'
    assert excinfo.value.block == 50


def test_db3_strict_negative_reference(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x83, [-1])
    make_db3_dbt(tmp_path / 'test.dbt', {1: b'alice memo\x1a'})

    with raises(MemoBlockOutOfRange) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))
    assert excinfo.value.record == 1
    assert excinfo.value.field == 'MEMO'
    assert excinfo.value.block == -1

    # In compatible mode the historic behaviour is kept: negative
    # references are treated as empty.
    record = next(iter(DBF(str(dbf_path))))
    assert record['MEMO'] is None


def test_db3_strict_missing_terminator(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x83, [1])
    # No \x1a terminator before end of file.
    make_db3_dbt(tmp_path / 'test.dbt', {1: b'unterminated memo'})

    with raises(MemoTruncated) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))
    assert excinfo.value.record == 1
    assert excinfo.value.field == 'MEMO'
    assert excinfo.value.block == 1


def test_db3_compatible_mode_unchanged(tmp_path):
    # In compatible mode an unterminated memo returns the data read
    # so far instead of raising.
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x83, [1])
    make_db3_dbt(tmp_path / 'test.dbt', {1: b'unterminated memo'})

    (record,) = list(DBF(str(dbf_path)))
    assert record['MEMO'].startswith('unterminated memo')


#
# dBase IV (DBT) strict mode.
#

def test_db4_strict(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x8b, [1, 1, 0])
    make_db4_dbt(tmp_path / 'test.dbt', {1: db4_block(b'bob memo')})

    records = list(DBF(str(dbf_path), strict_memo=True))
    assert [r['MEMO'] for r in records] == ['bob memo', 'bob memo', None]


def test_db4_block_size_from_header(tmp_path):
    # The block size is read from the DBT file header (1024 here),
    # not assumed to be 512.
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x8b, [1])
    make_db4_dbt(tmp_path / 'test.dbt', {1: db4_block(b'big blocks')},
                 blocksize=1024)

    (record,) = list(DBF(str(dbf_path), strict_memo=True))
    assert record['MEMO'] == 'big blocks'


def test_db4_strict_binary_memo_not_truncated(tmp_path):
    # A binary memo may contain \x1f and \x1a. In strict mode the
    # length in the block header is authoritative.
    blob = b'bin\x1fary da\x1ata'
    dbt_path = tmp_path / 'test.dbt'
    make_db4_dbt(dbt_path, {1: db4_block(blob)})

    memo = DB4MemoFile(str(dbt_path), strict=True)
    assert memo[1] == blob
    memo._close()


def test_db4_compatible_mode_still_splits_on_terminator(tmp_path):
    # The historic lenient behaviour cuts the memo at \x1f.
    dbt_path = tmp_path / 'test.dbt'
    make_db4_dbt(dbt_path, {1: db4_block(b'before\x1fafter')})

    memo = DB4MemoFile(str(dbt_path))
    assert memo[1] == b'before'
    memo._close()


def test_db4_strict_corrupt_header(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x8b, [1])
    make_db4_dbt(tmp_path / 'test.dbt',
                 {1: db4_block(b'data', signature=b'\xde\xad\xbe\xef')})

    with raises(MemoHeaderCorrupt) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))
    assert excinfo.value.record == 1
    assert excinfo.value.field == 'MEMO'
    assert excinfo.value.block == 1


def test_db4_strict_partial_block_header(tmp_path):
    # The block starts inside the file but is cut short.
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x8b, [1])
    make_db4_dbt(tmp_path / 'test.dbt', {1: db4_block(b'valid')})
    with open(str(tmp_path / 'test.dbt'), 'r+b') as f:
        f.truncate(512 + 4)

    with raises(MemoHeaderCorrupt) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))
    assert excinfo.value.record == 1
    assert excinfo.value.field == 'MEMO'
    assert excinfo.value.block == 1


def test_db4_strict_truncated_memo(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x8b, [1])
    make_db4_dbt(tmp_path / 'test.dbt',
                 {1: db4_block(b'short', declared_length=1000)})

    with raises(MemoTruncated) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))
    assert excinfo.value.record == 1
    assert excinfo.value.field == 'MEMO'
    assert excinfo.value.block == 1


def test_db4_strict_block_out_of_range(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x8b, [7])
    make_db4_dbt(tmp_path / 'test.dbt', {1: db4_block(b'bob memo')})

    with raises(MemoBlockOutOfRange) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))
    assert excinfo.value.record == 1
    assert excinfo.value.field == 'MEMO'
    assert excinfo.value.block == 7


def test_db4_strict_negative_reference(tmp_path):
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x8b, [-1])
    make_db4_dbt(tmp_path / 'test.dbt', {1: db4_block(b'bob memo')})

    with raises(MemoBlockOutOfRange) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))
    assert excinfo.value.block == -1


#
# Malformed references (not a number) get context too.
#

def test_malformed_reference_has_context(tmp_path):
    # The second record's memo reference is not a number.
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x83, [b'1', b'notanindex'])
    make_db3_dbt(tmp_path / 'test.dbt', {1: b'valid\x1a'})

    with raises(MemoReferenceInvalid) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))

    error = excinfo.value
    assert error.record == 2
    assert error.field == 'MEMO'
    # Still a ValueError for backwards compatibility, and it carries
    # the same context as the other memo errors.
    assert isinstance(error, ValueError)
    assert 'record 2' in str(error)


def test_malformed_reference_without_strict_memo(tmp_path):
    # The context is attached whenever the reference cannot be parsed,
    # even in the default compatible mode.
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x83, [b'1', b'notanindex'])
    make_db3_dbt(tmp_path / 'test.dbt', {1: b'valid\x1a'})

    with raises(MemoReferenceInvalid) as excinfo:
        list(DBF(str(dbf_path)))
    assert excinfo.value.record == 2
    assert excinfo.value.field == 'MEMO'


#
# Cursor independence after a failed read.
#

def test_db3_cursor_after_failed_read(tmp_path):
    dbt_path = tmp_path / 'test.dbt'
    make_db3_dbt(dbt_path, {1: b'valid\x1a', 2: b'unterminated'})
    memo = DB3MemoFile(str(dbt_path), strict=True)
    with raises(MemoTruncated):
        memo[2]
    assert memo[1] == b'valid'
    memo._close()


def test_db4_cursor_after_failed_read(tmp_path):
    dbt_path = tmp_path / 'test.dbt'
    make_db4_dbt(dbt_path, {1: db4_block(b'valid'),
                            2: db4_block(b'bad', signature=b'BAD!')})
    memo = DB4MemoFile(str(dbt_path), strict=True)
    with raises(MemoHeaderCorrupt):
        memo[2]
    assert memo[1] == b'valid'
    memo._close()


def test_vfp_cursor_after_failed_read_strict(tmp_path):
    fpt_path = tmp_path / 'test.fpt'
    make_fpt(fpt_path, {8: vfp_block(b'valid'),
                        9: vfp_block(b'bad', declared_length=9999)})
    memo = VFPMemoFile(str(fpt_path), strict=True)
    with raises(MemoTruncated):
        memo[9]
    assert memo[8] == b'valid'
    memo._close()


def test_vfp_invalid_file_header_block_size(tmp_path):
    # A block size that cannot describe the file is a damaged FPT
    # header. It is reported before any record is read.
    fpt_path = tmp_path / 'test.fpt'
    make_fpt(fpt_path, {8: vfp_block(b'hello')})
    with open(str(fpt_path), 'r+b') as f:
        # blocksize is the third field of the FPT header (offset 6).
        f.seek(6)
        f.write(struct.pack('>H', 60000))

    with raises(MemoHeaderCorrupt):
        VFPMemoFile(str(fpt_path), strict=True)

    # The compatible reader falls back to the default and still works.
    memo = VFPMemoFile(str(fpt_path))
    assert memo.blocksize == 512
    memo._close()


def test_compatible_mode_short_vfp_body_still_raises_ioerror(tmp_path):
    # The historic behaviour for a Visual FoxPro memo whose body is
    # shorter than its declared length is an IOError.
    fpt_path = tmp_path / 'test.fpt'
    make_fpt(fpt_path, {8: vfp_block(b'short', declared_length=5000)})

    memo = VFPMemoFile(str(fpt_path))
    with raises(IOError):
        memo[8]
    memo._close()


def test_db4_strict_block_size_larger_than_file(tmp_path):
    # A DBT header block size larger than the file cannot describe it.
    dbt_path = tmp_path / 'test.dbt'
    data = bytearray(512)
    data[:22] = struct.pack('<L16sH', 2, b'\0' * 16, 4096)
    with open(str(dbt_path), 'wb') as f:
        f.write(bytes(data))

    with raises(MemoHeaderCorrupt):
        DB4MemoFile(str(dbt_path), strict=True)


#
# Compatibility with the existing error classes and options.
#

def test_memo_errors_are_ioerrors(tmp_path):
    dbf_path = make_vfp_table(tmp_path, [999], {})
    with raises(IOError):
        list(DBF(str(dbf_path), strict_memo=True))
    with raises(MemoError):
        list(DBF(str(dbf_path), strict_memo=True))


def test_error_record_number_counts_deleted_records(tmp_path):
    # The record number is the physical DBF record number, so deleted
    # records (which are skipped) still advance it.
    dbf_path = tmp_path / 'test.dbf'
    make_dbf(dbf_path, 0x83, [1, 999])
    make_db3_dbt(tmp_path / 'test.dbt', {1: b'valid\x1a'})

    # Mark the first record as deleted by flipping its separator.
    with open(str(dbf_path), 'r+b') as f:
        f.seek(65)
        f.write(b'*')

    with raises(MemoBlockOutOfRange) as excinfo:
        list(DBF(str(dbf_path), strict_memo=True))
    # Record 1 is deleted, so the bad reference is on record 2.
    assert excinfo.value.record == 2


def test_strict_memo_combined_with_ignore_missing_memofile(tmp_path):
    dbf_path = make_vfp_table(tmp_path, [8], {8: vfp_block(b'hello')})
    (tmp_path / 'test.fpt').unlink()

    for strict in (False, True):
        table = DBF(str(dbf_path), strict_memo=strict,
                    ignore_missing_memofile=True)
        assert next(iter(table))['MEMO'] is None

        with raises(MissingMemoFile):
            DBF(str(dbf_path), strict_memo=strict,
                ignore_missing_memofile=False)
