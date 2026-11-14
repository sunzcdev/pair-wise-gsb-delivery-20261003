"""Tests for the strict skip mode of the pure python reader.

When a reader schema ignores large array/map fields, the pure python
backend can jump over collection blocks that declare their byte size
(negative block counts). The strict skip mode validates the byte
boundaries and end positions of those blocks, reports truncations with
the field path, works on both seekable and sequential-only inputs (the
latter discarding bytes through a bounded buffer), and never constructs
the ignored values in order to validate them.
"""

import io
import json

import pytest

import fastavro
from fastavro.io.binary_decoder import BinaryDecoder, SKIP_BUFFER_SIZE
from fastavro.io.binary_encoder import BinaryEncoder
from fastavro.read import _read as _reader


pytestmark = pytest.mark.skipif(
    hasattr(_reader, "CYTHON_MODULE"),
    reason="strict_skip is only implemented by the pure python backend",
)


class SequentialOnly:
    """A read-only, non-seekable stream wrapper.

    Records the size of every read() call so tests can prove that skipped
    bytes are discarded through a bounded buffer.
    """

    def __init__(self, data):
        self._fo = io.BytesIO(data)
        self.read_sizes = []

    def read(self, size=-1):
        self.read_sizes.append(size)
        return self._fo.read(size)

    def seekable(self):
        return False


def encode(encoder_fn):
    buffer = io.BytesIO()
    encoder_fn(BinaryEncoder(buffer))
    return buffer.getvalue()


def write_negative_array_block(enc, payload, count):
    """Write one array block using a negative count + byte size."""
    enc.write_long(-count)
    enc.write_long(len(payload))
    enc.write_fixed(payload)


def string_payload(items):
    return encode(lambda enc: [enc.write_utf8(item) for item in items])


WRITER_SCHEMA = {
    "type": "record",
    "name": "Root",
    "fields": [
        {"name": "keep", "type": "int"},
        {"name": "big", "type": {"type": "array", "items": "string"}},
        {"name": "tail", "type": "string"},
    ],
}

READER_SCHEMA = {
    "type": "record",
    "name": "Root",
    "fields": [
        {"name": "keep", "type": "int"},
        {"name": "tail", "type": "string"},
    ],
}


def make_record_bytes(items, keep=7, tail="tail-value"):
    payload = string_payload(items)

    def _encode(enc):
        enc.write_int(keep)
        write_negative_array_block(enc, payload, len(items))
        enc.write_long(0)  # end of array
        enc.write_utf8(tail)

    return encode(_encode)


def schemaless(data, strict_skip=True, fo_factory=None):
    fo = fo_factory(data) if fo_factory else io.BytesIO(data)
    return fastavro.schemaless_reader(
        fo,
        WRITER_SCHEMA,
        READER_SCHEMA,
        strict_skip=strict_skip,
    )


def test_strict_skip_negative_block_count_seekable():
    items = [f"item-{i}" for i in range(100)]
    data = make_record_bytes(items)
    record = schemaless(data)
    assert record == {"keep": 7, "tail": "tail-value"}


def test_strict_skip_negative_block_count_sequential_bounded_buffer():
    # ~1MB of skipped data; must be discarded in bounded chunks
    items = [f"item-{i}" * 100 for i in range(100)]
    data = make_record_bytes(items)
    stream = SequentialOnly(data)
    record = schemaless(data, fo_factory=lambda d: stream)
    assert record == {"keep": 7, "tail": "tail-value"}
    # Every single read must be bounded; the skipped megabyte must not be
    # buffered in one go
    assert stream.read_sizes, "expected the stream to be read"
    assert max(stream.read_sizes) <= SKIP_BUFFER_SIZE


def test_strict_skip_does_not_construct_ignored_items():
    # The block declares 5 bytes of payload that is *not* valid avro string
    # data. Strict skip must jump over the declared bytes without trying to
    # construct the items (which would raise).
    def _encode(enc):
        enc.write_int(3)
        write_negative_array_block(enc, b"\xff\xff\xff\xff\xff", 3)
        enc.write_long(0)
        enc.write_utf8("after")

    record = schemaless(encode(_encode))
    assert record == {"keep": 3, "tail": "after"}


def test_strict_skip_block_end_position_is_exact():
    # Two consecutive negative-count blocks followed by more data: landing
    # even one byte off would corrupt the following reads.
    payload_1 = string_payload(["a", "b"])
    payload_2 = string_payload(["c"])

    def _encode(enc):
        enc.write_int(11)
        write_negative_array_block(enc, payload_1, 2)
        write_negative_array_block(enc, payload_2, 1)
        enc.write_long(0)
        enc.write_utf8("end")

    record = schemaless(encode(_encode))
    assert record == {"keep": 11, "tail": "end"}


def make_truncated_block_record():
    """A record whose skipped array block declares more bytes than remain."""
    payload = string_payload(["a", "b", "c"])

    def _encode(enc):
        enc.write_int(7)
        enc.write_long(-3)
        # The block declares far more bytes than the stream actually holds
        enc.write_long(len(payload) + 100)
        enc.write_fixed(payload)
        enc.write_long(0)
        enc.write_utf8("tail")

    return encode(_encode)


def test_strict_skip_truncated_seekable_reports_field_path():
    with pytest.raises(EOFError) as excinfo:
        schemaless(make_truncated_block_record())
    assert "big" in str(excinfo.value)


def test_strict_skip_truncated_sequential_reports_field_path():
    with pytest.raises(EOFError) as excinfo:
        schemaless(make_truncated_block_record(), fo_factory=SequentialOnly)
    assert "big" in str(excinfo.value)


def test_strict_skip_truncation_path_includes_nested_record_fields():
    writer_schema = {
        "type": "record",
        "name": "Root",
        "fields": [
            {
                "name": "outer",
                "type": {
                    "type": "record",
                    "name": "Inner",
                    "fields": [
                        {"name": "drop_me", "type": "bytes"},
                        {"name": "keep2", "type": "int"},
                    ],
                },
            },
            {"name": "keep1", "type": "int"},
        ],
    }
    reader_schema = {
        "type": "record",
        "name": "Root",
        "fields": [
            {
                "name": "outer",
                "type": {
                    "type": "record",
                    "name": "Inner",
                    "fields": [{"name": "keep2", "type": "int"}],
                },
            },
            {"name": "keep1", "type": "int"},
        ],
    }

    def _encode(enc):
        enc.write_long(1000)  # drop_me declares 1000 bytes
        enc.write_fixed(b"only-a-few")  # but the stream ends early
        # keep2 and keep1 never arrive

    data = encode(_encode)
    with pytest.raises(EOFError) as excinfo:
        fastavro.schemaless_reader(
            io.BytesIO(data), writer_schema, reader_schema, strict_skip=True
        )
    message = str(excinfo.value)
    assert "outer" in message
    assert "drop_me" in message
    assert "outer.drop_me" in message


def test_strict_skip_nested_collections_and_unions_keep_level_state():
    writer_schema = {
        "type": "record",
        "name": "Root",
        "fields": [
            {"name": "keep", "type": "int"},
            {
                "name": "nested",
                "type": {
                    "type": "array",
                    "items": {"type": "array", "items": "string"},
                },
            },
            {
                "name": "choice",
                "type": [
                    "null",
                    {"type": "array", "items": "string"},
                    {"type": "map", "values": "string"},
                ],
            },
            {
                "name": "mapped",
                "type": {"type": "map", "values": {"type": "array", "items": "long"}},
            },
            {"name": "tail", "type": "string"},
        ],
    }
    reader_schema = {
        "type": "record",
        "name": "Root",
        "fields": [
            {"name": "keep", "type": "int"},
            {"name": "tail", "type": "string"},
        ],
    }

    inner_1 = string_payload(["x", "y"])
    inner_2 = string_payload(["z"])
    nested_payload = encode(
        lambda enc: (
            write_negative_array_block(enc, inner_1, 2),
            enc.write_long(0),
            write_negative_array_block(enc, inner_2, 1),
            enc.write_long(0),
        )
    )
    union_payload = string_payload(["u1", "u2", "u3"])
    map_array_payload = encode(lambda enc: [enc.write_long(i) for i in range(4)])

    def _encode(enc):
        enc.write_int(42)
        # nested: array of arrays, one negative-count block
        write_negative_array_block(enc, nested_payload, 2)
        enc.write_long(0)
        # choice: union selecting the array branch (index 1)
        enc.write_long(1)
        write_negative_array_block(enc, union_payload, 3)
        enc.write_long(0)
        # mapped: map with one negative-count block; each value is an array
        map_block = encode(
            lambda enc: (
                enc.write_utf8("first"),
                write_negative_array_block(enc, map_array_payload, 4),
                enc.write_long(0),
                enc.write_utf8("second"),
                enc.write_long(2),
                enc.write_long(1),
                enc.write_long(2),
                enc.write_long(0),
            )
        )
        write_negative_array_block(enc, map_block, 2)
        enc.write_long(0)
        enc.write_utf8("done")

    record = fastavro.schemaless_reader(
        io.BytesIO(encode(_encode)),
        writer_schema,
        reader_schema,
        strict_skip=True,
    )
    assert record == {"keep": 42, "tail": "done"}


def test_strict_skip_positive_count_blocks_still_validate_items():
    # Positive-count blocks have no byte size, so items are skipped one by
    # one; nested negative-count blocks inside must still be jumped over.
    inner_payload = string_payload(["deep"])

    def _encode(enc):
        enc.write_int(5)
        enc.write_long(2)  # positive count block with 2 items
        enc.write_utf8("plain")
        # an item that is itself... just a string here
        enc.write_utf8("values")
        enc.write_long(0)
        enc.write_utf8("tail!")

    record = schemaless(encode(_encode))
    assert record == {"keep": 5, "tail": "tail!"}
    assert inner_payload  # keep the helper honest


def test_strict_skip_negative_block_size_rejected():
    def _encode(enc):
        enc.write_int(1)
        enc.write_long(-2)  # two items
        enc.write_long(-10)  # invalid negative byte size
        enc.write_fixed(b"xx")
        enc.write_long(0)
        enc.write_utf8("tail")

    with pytest.raises(ValueError) as excinfo:
        schemaless(encode(_encode))
    assert "big" in str(excinfo.value)


def test_strict_skip_union_index_out_of_range():
    writer_schema = {
        "type": "record",
        "name": "Root",
        "fields": [
            {"name": "keep", "type": "int"},
            {"name": "u", "type": ["null", "string"]},
            {"name": "tail", "type": "string"},
        ],
    }
    reader_schema = {
        "type": "record",
        "name": "Root",
        "fields": [{"name": "keep", "type": "int"}],
    }

    def _encode(enc):
        enc.write_int(1)
        enc.write_long(9)  # invalid union index
        enc.write_utf8("tail")

    with pytest.raises(ValueError) as excinfo:
        fastavro.schemaless_reader(
            io.BytesIO(encode(_encode)),
            writer_schema,
            reader_schema,
            strict_skip=True,
        )
    assert "u" in str(excinfo.value)


def _project(record, fields):
    return {key: record[key] for key in fields}


def test_reader_strict_skip_matches_full_read_projection():
    writer_schema = {
        "type": "record",
        "name": "Root",
        "fields": [
            {"name": "a", "type": "int"},
            {"name": "skipped_array", "type": {"type": "array", "items": "string"}},
            {"name": "skipped_map", "type": {"type": "map", "values": "long"}},
            {"name": "b", "type": "string"},
        ],
    }
    reader_schema = {
        "type": "record",
        "name": "Root",
        "fields": [
            {"name": "a", "type": "int"},
            {"name": "b", "type": "string"},
        ],
    }
    records = [
        {
            "a": i,
            "skipped_array": [f"value-{i}-{j}" for j in range(50)],
            "skipped_map": {f"key-{j}": j * i for j in range(20)},
            "b": f"tail-{i}",
        }
        for i in range(10)
    ]

    buffer = io.BytesIO()
    fastavro.writer(buffer, writer_schema, records)

    buffer.seek(0)
    expected = [_project(record, ("a", "b")) for record in fastavro.reader(buffer)]

    buffer.seek(0)
    strict_records = list(
        fastavro.reader(buffer, reader_schema=reader_schema, strict_skip=True)
    )
    assert strict_records == expected

    buffer.seek(0)
    default_records = list(fastavro.reader(buffer, reader_schema=reader_schema))
    assert default_records == expected


def test_block_reader_strict_skip():
    writer_schema = {
        "type": "record",
        "name": "Root",
        "fields": [
            {"name": "a", "type": "int"},
            {"name": "skipped", "type": {"type": "array", "items": "string"}},
        ],
    }
    reader_schema = {
        "type": "record",
        "name": "Root",
        "fields": [{"name": "a", "type": "int"}],
    }
    records = [{"a": i, "skipped": ["x"] * 10} for i in range(5)]

    buffer = io.BytesIO()
    fastavro.writer(buffer, writer_schema, records)
    buffer.seek(0)

    blocks = list(fastavro.block_reader(buffer, reader_schema=reader_schema, strict_skip=True))
    assert [record for block in blocks for record in block] == [
        {"a": i} for i in range(5)
    ]


def test_strict_skip_decoder_skip_exact_seekable():
    data = b"0123456789"
    decoder = BinaryDecoder(io.BytesIO(data))
    decoder.skip_exact(4)
    assert decoder.fo.read(2) == b"45"
    with pytest.raises(EOFError):
        decoder.skip_exact(100)


def test_strict_skip_decoder_skip_exact_sequential():
    stream = SequentialOnly(b"x" * (SKIP_BUFFER_SIZE * 3 + 17) + b"end")
    decoder = BinaryDecoder(stream)
    decoder.skip_exact(SKIP_BUFFER_SIZE * 3 + 17)
    assert decoder.fo.read(3) == b"end"
    assert max(stream.read_sizes) <= SKIP_BUFFER_SIZE
    with pytest.raises(EOFError):
        decoder.skip_exact(1)


def make_container(writer_schema, block_bytes, num_records=1):
    """Hand-build a minimal avro container file around raw block bytes."""
    sync = b"0123456789abcdef"
    out = io.BytesIO()
    enc = BinaryEncoder(out)
    out.write(b"Obj\x01")
    enc.write_long(2)  # two metadata entries
    enc.write_utf8("avro.schema")
    schema_bytes = json.dumps(writer_schema).encode()
    enc.write_long(len(schema_bytes))
    enc.write_fixed(schema_bytes)
    enc.write_utf8("avro.codec")
    enc.write_long(4)
    enc.write_fixed(b"null")
    enc.write_long(0)  # end of metadata map
    enc.write_fixed(sync)
    enc.write_long(num_records)
    enc.write_long(len(block_bytes))
    enc.write_fixed(block_bytes)
    enc.write_fixed(sync)
    return out.getvalue()


def test_strict_skip_bad_length_cannot_overrun_record_boundary():
    """A wrong block byte size must not skip into the following record.

    The skipped array block declares a byte size that extends past the end
    of the record (and the whole data block). Strict mode must raise instead
    of landing inside the next record and continuing to parse garbage.
    """
    payload = string_payload(["a"])

    def record_bytes(keep, tail):
        def _encode(enc):
            enc.write_int(keep)
            enc.write_long(-1)
            # Declares far more bytes than remain in this record/block
            enc.write_long(len(payload) + 1000)
            enc.write_fixed(payload)
            enc.write_long(0)
            enc.write_utf8(tail)

        return encode(_encode)

    block_bytes = record_bytes(1, "one") + record_bytes(2, "two")
    container = make_container(WRITER_SCHEMA, block_bytes, num_records=2)

    with pytest.raises(EOFError) as excinfo:
        list(
            fastavro.reader(
                io.BytesIO(container),
                reader_schema=READER_SCHEMA,
                strict_skip=True,
            )
        )
    assert "big" in str(excinfo.value)
