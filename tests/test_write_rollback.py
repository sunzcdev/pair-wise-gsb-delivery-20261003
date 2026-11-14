"""Tests that a record which fails partway through encoding does not leave
partial data in the Writer's block buffer."""

from io import BytesIO

import pytest

import fastavro
from fastavro.write import Writer

from .conftest import is_testing_cython_modules

pytestmark = pytest.mark.skipif(
    is_testing_cython_modules(),
    reason="transactional record writes are implemented in the pure python writer",
)


def roundtrip_records(writer_schema, records_to_write):
    """Write records, ignoring any that fail, and return what reads back."""
    bio = BytesIO()
    writer = Writer(bio, writer_schema)
    for record in records_to_write:
        try:
            writer.write(record)
        except (ValueError, TypeError):
            pass
    writer.flush()
    bio.seek(0)
    return list(fastavro.reader(bio))


def test_failed_record_does_not_corrupt_block():
    schema = {
        "type": "record",
        "name": "Test",
        "fields": [
            {"name": "name", "type": "string"},
            {"name": "value", "type": "int"},
        ],
    }
    good_1 = {"name": "one", "value": 1}
    bad = {"name": "bad", "value": "not an int"}
    good_2 = {"name": "two", "value": 2}

    assert roundtrip_records(schema, [good_1, bad, good_2]) == [good_1, good_2]


def test_failed_nested_array_rolls_back():
    schema = {
        "type": "record",
        "name": "Test",
        "fields": [
            {"name": "id", "type": "int"},
            {"name": "items", "type": {"type": "array", "items": "int"}},
        ],
    }
    good_1 = {"id": 1, "items": [1, 2, 3]}
    # Fails in the middle of encoding the nested array
    bad = {"id": 2, "items": [1, "boom", 3]}
    good_2 = {"id": 3, "items": [4, 5]}

    assert roundtrip_records(schema, [good_1, bad, good_2]) == [good_1, good_2]


def test_failed_union_rolls_back():
    schema = {
        "type": "record",
        "name": "Test",
        "fields": [
            {"name": "id", "type": "int"},
            {"name": "u", "type": ["null", "int"]},
        ],
    }
    good_1 = {"id": 1, "u": 10}
    bad = {"id": 2, "u": "no matching branch"}
    good_2 = {"id": 3, "u": None}

    assert roundtrip_records(schema, [good_1, bad, good_2]) == [good_1, good_2]


def test_failed_logical_type_rolls_back():
    schema = {
        "type": "record",
        "name": "Test",
        "fields": [
            {"name": "id", "type": "int"},
            {"name": "ts", "type": {"type": "long", "logicalType": "timestamp-millis"}},
        ],
    }
    import datetime

    tz = datetime.timezone.utc
    good_1 = {"id": 1, "ts": datetime.datetime(2020, 1, 1, tzinfo=tz)}
    bad = {"id": 2, "ts": "not a datetime"}
    good_2 = {"id": 3, "ts": datetime.datetime(2020, 1, 2, tzinfo=tz)}

    assert roundtrip_records(schema, [good_1, bad, good_2]) == [good_1, good_2]


def test_block_count_and_sync_interval_after_failure():
    """Failed records must not bump the record count and must not corrupt
    blocks when a sync boundary is crossed."""
    schema = {
        "type": "record",
        "name": "Test",
        "fields": [{"name": "value", "type": "int"}],
    }
    bio = BytesIO()
    # Tiny sync interval so blocks are dumped frequently
    writer = Writer(bio, schema, sync_interval=16)

    good_records = [{"value": i} for i in range(50)]
    for i, record in enumerate(good_records):
        writer.write(record)
        if i % 5 == 0:
            with pytest.raises((ValueError, TypeError)):
                writer.write({"value": "not an int"})
    writer.flush()

    bio.seek(0)
    assert list(fastavro.reader(bio)) == good_records


def test_block_buffer_position_restored():
    schema = {
        "type": "record",
        "name": "Test",
        "fields": [{"name": "value", "type": "int"}],
    }
    bio = BytesIO()
    writer = Writer(bio, schema)

    writer.write({"value": 1})
    offset = writer.io._fo.tell()
    assert writer.block_count == 1

    with pytest.raises((ValueError, TypeError)):
        writer.write({"value": "not an int"})

    assert writer.io._fo.tell() == offset
    assert writer.block_count == 1

    writer.write({"value": 2})
    writer.flush()

    bio.seek(0)
    assert list(fastavro.reader(bio)) == [{"value": 1}, {"value": 2}]


def test_record_source_not_consumed_twice():
    """The failing record's data must not be re-read/re-prepared on retry;
    each record from the source iterator is consumed exactly once."""
    schema = {
        "type": "record",
        "name": "Test",
        "fields": [{"name": "value", "type": "int"}],
    }
    consumed = []

    def record_source():
        for i in range(5):
            record = {"value": "bad" if i == 2 else i}
            consumed.append(i)
            yield record

    bio = BytesIO()
    writer = Writer(bio, schema)
    for record in record_source():
        try:
            writer.write(record)
        except (ValueError, TypeError):
            continue
    writer.flush()

    assert consumed == [0, 1, 2, 3, 4]
    bio.seek(0)
    assert list(fastavro.reader(bio)) == [
        {"value": 0},
        {"value": 1},
        {"value": 3},
        {"value": 4},
    ]
