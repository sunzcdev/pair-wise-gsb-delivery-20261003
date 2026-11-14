from io import BytesIO

import pytest

import fastavro
from fastavro import _read_py
from fastavro._read_common import SchemaResolutionError

# Largest consecutive integer exactly representable as an avro (32-bit) float
FLOAT32_MAX_EXACT = 2**24
# Largest consecutive integer exactly representable as an avro (64-bit) double
FLOAT64_MAX_EXACT = 2**53


def roundtrip(value, writer_schema, reader_schema, **kwargs):
    bio = BytesIO()
    fastavro.schemaless_writer(bio, writer_schema, value)
    bio.seek(0)
    return _read_py.schemaless_reader(bio, writer_schema, reader_schema, **kwargs)


def strict_roundtrip(value, writer_schema, reader_schema):
    return roundtrip(
        value, writer_schema, reader_schema, strict_numeric_promotion=True
    )


@pytest.mark.parametrize(
    "value",
    [FLOAT32_MAX_EXACT, -FLOAT32_MAX_EXACT, 0, 1, -1],
)
def test_long_to_float_exact_values_accepted(value):
    assert strict_roundtrip(value, "long", "float") == float(value)


@pytest.mark.parametrize(
    "value",
    [FLOAT32_MAX_EXACT + 1, -FLOAT32_MAX_EXACT - 1],
)
def test_long_to_float_lossy_values_rejected(value):
    with pytest.raises(SchemaResolutionError) as exc:
        strict_roundtrip(value, "long", "float")
    assert "long" in str(exc.value)
    assert "float" in str(exc.value)
    assert str(value) in str(exc.value)


@pytest.mark.parametrize(
    "value",
    [FLOAT64_MAX_EXACT, -FLOAT64_MAX_EXACT, FLOAT32_MAX_EXACT + 1],
)
def test_long_to_double_exact_values_accepted(value):
    assert strict_roundtrip(value, "long", "double") == float(value)


@pytest.mark.parametrize(
    "value",
    [FLOAT64_MAX_EXACT + 1, -FLOAT64_MAX_EXACT - 1],
)
def test_long_to_double_lossy_values_rejected(value):
    with pytest.raises(SchemaResolutionError) as exc:
        strict_roundtrip(value, "long", "double")
    assert "long" in str(exc.value)
    assert "double" in str(exc.value)


def test_int_to_float_uses_float32_precision_not_python_float():
    # 2**24 + 1 is exactly representable as a Python float (double) but not
    # as an avro (32-bit) float
    value = FLOAT32_MAX_EXACT + 1
    assert strict_roundtrip(value, "int", "double") == float(value)
    with pytest.raises(SchemaResolutionError):
        strict_roundtrip(value, "int", "float")


def test_float_to_double_always_lossless():
    # every avro float (32-bit) is exactly representable as a double
    assert strict_roundtrip(1.5, "float", "double") == 1.5
    assert strict_roundtrip(-0.75, "float", "double") == -0.75


def test_unpromoted_float_data_unchanged():
    # 1.1 cannot be represented exactly as a 32-bit float; the strict option
    # must not alter the original float data when no promotion happens
    expected = roundtrip(1.1, "float", "float")
    assert strict_roundtrip(1.1, "float", "float") == expected
    assert strict_roundtrip(1.1, "float", "float") != 1.1


def test_default_behavior_still_allows_lossy_promotion():
    # without the strict option, the default avro-compatible (lossy)
    # promotion is preserved
    value = FLOAT64_MAX_EXACT + 1
    result = roundtrip(value, "long", "double")
    assert result == float(value)
    assert result != value


def test_nested_record_field_path_in_error():
    writer_schema = {
        "type": "record",
        "name": "Outer",
        "fields": [
            {
                "name": "outer",
                "type": {
                    "type": "record",
                    "name": "Inner",
                    "fields": [{"name": "inner", "type": "long"}],
                },
            }
        ],
    }
    reader_schema = {
        "type": "record",
        "name": "Outer",
        "fields": [
            {
                "name": "outer",
                "type": {
                    "type": "record",
                    "name": "Inner",
                    "fields": [{"name": "inner", "type": "double"}],
                },
            }
        ],
    }
    record = {"outer": {"inner": FLOAT64_MAX_EXACT + 1}}
    with pytest.raises(SchemaResolutionError) as exc:
        strict_roundtrip(record, writer_schema, reader_schema)
    assert "outer.inner" in str(exc.value)
    assert "long" in str(exc.value)
    assert "double" in str(exc.value)


def test_array_field_path_in_error():
    values = [1, FLOAT64_MAX_EXACT + 1, 3]
    with pytest.raises(SchemaResolutionError) as exc:
        strict_roundtrip(
            values,
            {"type": "array", "items": "long"},
            {"type": "array", "items": "double"},
        )
    assert "[1]" in str(exc.value)


def test_array_exact_values_accepted():
    values = [1, -2, FLOAT64_MAX_EXACT]
    assert strict_roundtrip(
        values,
        {"type": "array", "items": "long"},
        {"type": "array", "items": "double"},
    ) == [1.0, -2.0, float(FLOAT64_MAX_EXACT)]


def test_map_field_path_in_error():
    value = {"bad_key": FLOAT64_MAX_EXACT + 1}
    with pytest.raises(SchemaResolutionError) as exc:
        strict_roundtrip(
            value,
            {"type": "map", "values": "long"},
            {"type": "map", "values": "double"},
        )
    assert "bad_key" in str(exc.value)


def test_union_promotion_checked():
    value = FLOAT64_MAX_EXACT + 1
    with pytest.raises(SchemaResolutionError):
        strict_roundtrip(value, ["null", "long"], ["null", "double"])
    # exact values in the union are still promoted
    assert strict_roundtrip(5, ["null", "long"], ["null", "double"]) == 5.0


def test_reader_union_branch_order_determines_promotion():
    value = FLOAT64_MAX_EXACT + 1
    # the long branch matches first, so no promotion happens and the original
    # integer is returned untouched
    assert strict_roundtrip(value, "long", ["long", "double"]) == value
    # the double branch matches first, so the promotion is checked and fails
    with pytest.raises(SchemaResolutionError):
        strict_roundtrip(value, "long", ["double", "long"])


def test_reader_option_on_file_reader():
    writer_schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "id", "type": "long"}],
    }
    reader_schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "id", "type": "float"}],
    }
    bio = BytesIO()
    fastavro.writer(bio, writer_schema, [{"id": FLOAT32_MAX_EXACT + 1}])
    bio.seek(0)
    with pytest.raises(SchemaResolutionError) as exc:
        list(_read_py.reader(bio, reader_schema, strict_numeric_promotion=True))
    assert "id" in str(exc.value)

    bio = BytesIO()
    fastavro.writer(bio, writer_schema, [{"id": FLOAT32_MAX_EXACT}])
    bio.seek(0)
    records = list(_read_py.reader(bio, reader_schema, strict_numeric_promotion=True))
    assert records == [{"id": float(FLOAT32_MAX_EXACT)}]


def test_reader_option_on_block_reader():
    writer_schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "id", "type": "long"}],
    }
    reader_schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "id", "type": "double"}],
    }
    bio = BytesIO()
    fastavro.writer(bio, writer_schema, [{"id": FLOAT64_MAX_EXACT + 1}])
    bio.seek(0)
    blocks = _read_py.block_reader(
        bio, reader_schema, strict_numeric_promotion=True
    )
    with pytest.raises(SchemaResolutionError):
        for block in blocks:
            list(block)
