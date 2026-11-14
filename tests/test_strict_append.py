import pytest

from fastavro import writer, reader, parse_schema
from fastavro.read import MAGIC

from .conftest import is_testing_cython_modules

pytestmark = pytest.mark.skipif(
    is_testing_cython_modules(),
    reason="strict append is only implemented in the pure python writer",
)

SCHEMA = {
    "name": "test_strict_append",
    "namespace": "test",
    "type": "record",
    "fields": [
        {"name": "station", "type": "string"},
        {"name": "time", "type": "long"},
        {"name": "temp", "type": "int"},
    ],
}

RECORDS = [
    {"station": "011990-99999", "temp": 0, "time": 1433269388},
    {"station": "011990-99999", "temp": 22, "time": 1433270389},
]

MORE_RECORDS = [
    {"station": "012650-99999", "temp": 111, "time": 1433275478},
]


def read_file(path):
    with open(path, "rb") as fo:
        return list(reader(fo))


def test_strict_append_matching_schema(tmpdir):
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS)

    with open(test_file, "a+b") as out:
        writer(out, SCHEMA, MORE_RECORDS, strict_append=True)

    assert read_file(test_file) == RECORDS + MORE_RECORDS


def test_strict_append_does_not_write_second_header(tmpdir):
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS)

    with open(test_file, "a+b") as out:
        writer(out, SCHEMA, MORE_RECORDS, strict_append=True)

    with open(test_file, "rb") as fo:
        contents = fo.read()
    assert contents.count(MAGIC) == 1


def test_strict_append_with_none_schema(tmpdir):
    """A schema of None defers to the file schema, as in lenient mode."""
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS)

    with open(test_file, "a+b") as out:
        writer(out, None, MORE_RECORDS, strict_append=True)

    assert read_file(test_file) == RECORDS + MORE_RECORDS


def test_strict_append_json_key_order_is_irrelevant(tmpdir):
    """Structurally identical schemas must match regardless of key order."""
    reordered_schema = {
        "fields": [
            {"type": "string", "name": "station"},
            {"type": "long", "name": "time"},
            {"type": "int", "name": "temp"},
        ],
        "type": "record",
        "namespace": "test",
        "name": "test_strict_append",
    }
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS)

    with open(test_file, "a+b") as out:
        writer(out, reordered_schema, MORE_RECORDS, strict_append=True)

    assert read_file(test_file) == RECORDS + MORE_RECORDS


def test_strict_append_type_shorthand_is_irrelevant(tmpdir):
    """{'type': 'string'} and 'string' encode identically and must match."""
    shorthand_schema = {
        "name": "test_strict_append",
        "namespace": "test",
        "type": "record",
        "fields": [
            {"name": "station", "type": {"type": "string"}},
            {"name": "time", "type": "long"},
            {"name": "temp", "type": "int"},
        ],
    }
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS)

    with open(test_file, "a+b") as out:
        writer(out, shorthand_schema, MORE_RECORDS, strict_append=True)

    assert read_file(test_file) == RECORDS + MORE_RECORDS


def test_strict_append_doc_and_aliases_are_irrelevant(tmpdir):
    schema_with_doc = dict(SCHEMA, doc="a weather reading")
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, schema_with_doc, RECORDS)

    with open(test_file, "a+b") as out:
        writer(out, SCHEMA, MORE_RECORDS, strict_append=True)

    assert read_file(test_file) == RECORDS + MORE_RECORDS


def test_strict_append_schema_mismatch_fails_before_writing(tmpdir):
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS)

    other_schema = {
        "name": "test_strict_append",
        "namespace": "test",
        "type": "record",
        "fields": [
            {"name": "station", "type": "string"},
            {"name": "time", "type": "long"},
            {"name": "temp", "type": "long"},  # different type
        ],
    }

    with open(test_file, "rb") as fo:
        before = fo.read()

    with open(test_file, "a+b") as out:
        with pytest.raises(ValueError, match="schema does not match"):
            writer(out, other_schema, MORE_RECORDS, strict_append=True)

    with open(test_file, "rb") as fo:
        assert fo.read() == before

    assert read_file(test_file) == RECORDS


def test_strict_append_read_compatible_schema_is_rejected(tmpdir):
    """A schema that is compatible for schema resolution (an added field with
    a default) is still not identical for append encoding and must fail."""
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS)

    evolved_schema = {
        "name": "test_strict_append",
        "namespace": "test",
        "type": "record",
        "fields": [
            {"name": "station", "type": "string"},
            {"name": "time", "type": "long"},
            {"name": "temp", "type": "int"},
            {"name": "extra", "type": "string", "default": "n/a"},
        ],
    }

    with open(test_file, "rb") as fo:
        before = fo.read()

    with open(test_file, "a+b") as out:
        with pytest.raises(ValueError, match="schema does not match"):
            writer(out, evolved_schema, MORE_RECORDS, strict_append=True)

    with open(test_file, "rb") as fo:
        assert fo.read() == before


def test_strict_append_field_default_mismatch_is_rejected(tmpdir):
    """Defaults influence the encoded bytes and therefore the identity."""
    test_file = str(tmpdir.join("test.avro"))
    schema_with_default = {
        "name": "test_strict_append",
        "namespace": "test",
        "type": "record",
        "fields": [
            {"name": "station", "type": "string"},
            {"name": "time", "type": "long"},
            {"name": "temp", "type": "int", "default": 0},
        ],
    }
    with open(test_file, "wb") as out:
        writer(out, schema_with_default, RECORDS)

    with open(test_file, "a+b") as out:
        with pytest.raises(ValueError, match="schema does not match"):
            writer(out, SCHEMA, MORE_RECORDS, strict_append=True)


def test_strict_append_parsed_schema(tmpdir):
    test_file = str(tmpdir.join("test.avro"))
    parsed = parse_schema(SCHEMA)
    with open(test_file, "wb") as out:
        writer(out, parsed, RECORDS)

    with open(test_file, "a+b") as out:
        writer(out, parsed, MORE_RECORDS, strict_append=True)

    assert read_file(test_file) == RECORDS + MORE_RECORDS


def test_strict_append_codec_mismatch_fails(tmpdir):
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS, codec="deflate")

    with open(test_file, "rb") as fo:
        before = fo.read()

    with open(test_file, "a+b") as out:
        with pytest.raises(ValueError, match="codec"):
            writer(out, SCHEMA, MORE_RECORDS, strict_append=True)

    with open(test_file, "rb") as fo:
        assert fo.read() == before


def test_strict_append_matching_codec(tmpdir):
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS, codec="deflate")

    with open(test_file, "a+b") as out:
        writer(out, SCHEMA, MORE_RECORDS, codec="deflate", strict_append=True)

    assert read_file(test_file) == RECORDS + MORE_RECORDS


def test_strict_append_sync_marker_mismatch_fails(tmpdir):
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS)

    with open(test_file, "a+b") as out:
        with pytest.raises(ValueError, match="sync marker"):
            writer(
                out,
                SCHEMA,
                MORE_RECORDS,
                sync_marker=b"0" * 16,
                strict_append=True,
            )


def test_strict_append_empty_file_fails(tmpdir):
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb"):
        pass

    with open(test_file, "a+b") as out:
        with pytest.raises(ValueError, match="empty"):
            writer(out, SCHEMA, RECORDS, strict_append=True)

    # Nothing should have been written, not even a header
    with open(test_file, "rb") as fo:
        assert fo.read() == b""


def test_strict_append_truncated_file_fails(tmpdir):
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS)

    with open(test_file, "rb") as fo:
        contents = fo.read()

    # Truncate the tail of the last block
    with open(test_file, "wb") as out:
        out.write(contents[:-5])

    with open(test_file, "a+b") as out:
        with pytest.raises(ValueError, match="truncated"):
            writer(out, SCHEMA, MORE_RECORDS, strict_append=True)


def test_strict_append_truncated_block_data_fails(tmpdir):
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS)

    with open(test_file, "rb") as fo:
        contents = fo.read()

    # Remove the sync marker and part of the block data
    with open(test_file, "wb") as out:
        out.write(contents[:-20])

    with open(test_file, "a+b") as out:
        with pytest.raises(ValueError, match="truncated"):
            writer(out, SCHEMA, MORE_RECORDS, strict_append=True)


def test_strict_append_unreadable_file_fails(tmpdir):
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS)

    with open(test_file, "ab") as out:
        with pytest.raises(ValueError, match="not readable"):
            writer(out, SCHEMA, MORE_RECORDS, strict_append=True)


def test_strict_append_unseekable_stream_fails(tmpdir):
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS)

    class UnseekableStream:
        def __init__(self, path):
            self._fo = open(path, "ab")

        def seekable(self):
            return False

        def readable(self):
            return False

        def write(self, data):
            return self._fo.write(data)

        def flush(self):
            self._fo.flush()

        def close(self):
            self._fo.close()

    stream = UnseekableStream(test_file)
    try:
        with pytest.raises(ValueError, match="not seekable"):
            writer(stream, SCHEMA, MORE_RECORDS, strict_append=True)
    finally:
        stream.close()


def test_strict_append_not_an_avro_file_fails(tmpdir):
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        out.write(b"this is not an avro container file at all")

    with open(test_file, "a+b") as out:
        with pytest.raises(ValueError):
            writer(out, SCHEMA, MORE_RECORDS, strict_append=True)


def test_strict_append_multiple_times(tmpdir):
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS)

    with open(test_file, "a+b") as out:
        writer(out, SCHEMA, MORE_RECORDS, strict_append=True)

    with open(test_file, "a+b") as out:
        writer(out, SCHEMA, RECORDS, strict_append=True)

    assert read_file(test_file) == RECORDS + MORE_RECORDS + RECORDS


def test_lenient_append_still_ignores_schema(tmpdir):
    """Without strict_append, the existing lenient behavior is preserved."""
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb") as out:
        writer(out, SCHEMA, RECORDS)

    other_schema = {
        "name": "completely_different",
        "type": "record",
        "fields": [{"name": "other", "type": "string"}],
    }

    with open(test_file, "a+b") as out:
        writer(out, other_schema, MORE_RECORDS)

    assert read_file(test_file) == RECORDS + MORE_RECORDS


def test_lenient_append_empty_file_writes_header(tmpdir):
    """Without strict_append, an empty file is treated as a new file."""
    test_file = str(tmpdir.join("test.avro"))
    with open(test_file, "wb"):
        pass

    with open(test_file, "a+b") as out:
        writer(out, SCHEMA, RECORDS)

    assert read_file(test_file) == RECORDS
