import re

from fastavro import writer as fastavro_writer
from fastavro.read import SchemaResolutionError
from fastavro.utils import generate_one
import fastavro

import pytest

from io import BytesIO

schema_dict_a = {
    "namespace": "example.avro2",
    "type": "record",
    "name": "evtest",
    "fields": [{"name": "a", "type": "int"}],
}

record_a = {"a": 123}

schema_dict_a_b = {
    "namespace": "example.avro2",
    "type": "record",
    "name": "evtest",
    "fields": [
        {"name": "a", "type": "int"},
        {"name": "b", "type": ["null", "int"], "default": None},
    ],
}

record_a_b = {"a": 234, "b": 345}

schema_dict_a_c = {
    "namespace": "example.avro2",
    "type": "record",
    "name": "evtest",
    "fields": [{"name": "a", "type": "int"}, {"name": "c", "type": ["null", "int"]}],
}


def avro_to_bytes_with_schema(avro_schema, avro_dict):
    with BytesIO() as bytes_io:
        fastavro_writer(bytes_io, avro_schema, [avro_dict])
        return bytes_io.getvalue()


def bytes_with_schema_to_avro(avro_read_schema, binary):
    with BytesIO(binary) as bytes_io:
        reader = fastavro.reader(bytes_io, avro_read_schema)
        return next(reader)


def test_evolution_drop_field():
    record_bytes_a_b = avro_to_bytes_with_schema(schema_dict_a_b, record_a_b)
    record_a = bytes_with_schema_to_avro(schema_dict_a, record_bytes_a_b)
    assert "b" not in record_a


def test_evolution_add_field_with_default():
    record_bytes_a = avro_to_bytes_with_schema(schema_dict_a, record_a)
    record_b = bytes_with_schema_to_avro(schema_dict_a_b, record_bytes_a)
    assert "b" in record_b
    assert record_b.get("b") is None


def test_evolution_add_field_without_default():
    with pytest.raises(
        SchemaResolutionError,
        match="No default value for field c in example.avro2.evtest",
    ):
        record_bytes_a = avro_to_bytes_with_schema(schema_dict_a, record_a)
        bytes_with_schema_to_avro(schema_dict_a_c, record_bytes_a)


def test_enum_evolution_no_default_failure():
    original_schema = {
        "type": "enum",
        "name": "test",
        "symbols": ["FOO", "BAR"],
    }

    new_schema = {
        "type": "enum",
        "name": "test",
        "symbols": ["BAZ", "BAR"],
    }

    original_records = ["FOO"]

    bio = BytesIO()
    fastavro.writer(bio, original_schema, original_records)
    bio.seek(0)

    with pytest.raises(
        fastavro.read.SchemaResolutionError,
        match=re.escape(
            "FOO not found in reader symbol list test, known symbols: ['BAZ', 'BAR']"
        ),
    ):
        list(fastavro.reader(bio, new_schema))


def test_enum_evolution_using_default():
    original_schema = {
        "type": "enum",
        "name": "test",
        "symbols": ["A", "B"],
    }

    new_schema = {
        "type": "enum",
        "name": "test",
        "symbols": ["C", "D"],
        "default": "C",
    }

    original_records = ["A"]

    bio = BytesIO()
    fastavro.writer(bio, original_schema, original_records)
    bio.seek(0)

    new_records = list(fastavro.reader(bio, new_schema))
    assert new_records == ["C"]


def test_schema_matching_with_records_in_arrays():
    """https://github.com/fastavro/fastavro/issues/363"""
    original_schema = {
        "type": "record",
        "name": "DataRecord",
        "fields": [
            {
                "name": "string1",
                "type": "string",
            },
            {
                "name": "subrecord",
                "type": {
                    "type": "array",
                    "items": {
                        "type": "record",
                        "name": "SubRecord",
                        "fields": [
                            {
                                "name": "string2",
                                "type": "string",
                            }
                        ],
                    },
                },
            },
        ],
    }

    new_schema = {
        "type": "record",
        "name": "DataRecord",
        "fields": [
            {
                "name": "string1",
                "type": "string",
            },
            {
                "name": "subrecord",
                "type": {
                    "type": "array",
                    "items": {
                        "type": "record",
                        "name": "SubRecord",
                        "fields": [
                            {
                                "name": "string2",
                                "type": "string",
                            },
                            {
                                "name": "logs",
                                "default": None,
                                "type": [
                                    "null",
                                    {
                                        "type": "array",
                                        "items": {
                                            "type": "record",
                                            "name": "LogRecord",
                                            "fields": [
                                                {
                                                    "name": "msg",
                                                    "type": "string",
                                                    "default": "",
                                                }
                                            ],
                                        },
                                    },
                                ],
                            },
                        ],
                    },
                },
            },
        ],
    }

    record = {
        "string1": "test",
        "subrecord": [{"string2": "foo"}],
    }

    binary = avro_to_bytes_with_schema(original_schema, record)

    output_using_original_schema = bytes_with_schema_to_avro(original_schema, binary)
    assert output_using_original_schema == record

    output_using_new_schema = bytes_with_schema_to_avro(new_schema, binary)
    assert output_using_new_schema == {
        "string1": "test",
        "subrecord": [{"string2": "foo", "logs": None}],
    }


def test_schema_migrate_record_to_union():
    """https://github.com/fastavro/fastavro/issues/406"""
    original_schema = {
        "name": "Item",
        "type": "record",
        "fields": [
            {
                "name": "category",
                "type": {
                    "type": "record",
                    "name": "Category",
                    "fields": [{"name": "name", "type": "string"}],
                },
            }
        ],
    }

    new_schema_record_first = {
        "name": "Item",
        "type": "record",
        "fields": [
            {
                "name": "category",
                "type": [
                    {
                        "type": "record",
                        "name": "Category",
                        "fields": [{"name": "name", "type": "string"}],
                    },
                    "null",
                ],
            }
        ],
    }

    new_schema_null_first = {
        "name": "Item",
        "type": "record",
        "fields": [
            {
                "name": "category",
                "type": [
                    "null",
                    {
                        "type": "record",
                        "name": "Category",
                        "fields": [{"name": "name", "type": "string"}],
                    },
                ],
            }
        ],
    }

    record = {"category": {"name": "my-category"}}

    binary = avro_to_bytes_with_schema(original_schema, record)

    output_using_original_schema = bytes_with_schema_to_avro(original_schema, binary)
    assert output_using_original_schema == record

    output_using_new_schema_record_first = bytes_with_schema_to_avro(
        new_schema_record_first, binary
    )
    assert output_using_new_schema_record_first == record

    output_using_new_schema_null_first = bytes_with_schema_to_avro(
        new_schema_null_first, binary
    )
    assert output_using_new_schema_null_first == record


def test_union_of_lists_evolution_with_doc():
    """https://github.com/fastavro/fastavro/issues/486"""
    original_schema = {
        "name": "test_union_of_lists_evolution_with_doc",
        "type": "record",
        "fields": [
            {
                "name": "id",
                "type": [
                    "null",
                    {
                        "name": "some_record",
                        "type": "record",
                        "fields": [{"name": "field", "type": "string"}],
                    },
                ],
            }
        ],
    }

    new_schema = {
        "name": "test_union_of_lists_evolution_with_doc",
        "type": "record",
        "fields": [
            {
                "name": "id",
                "type": [
                    "null",
                    {
                        "name": "some_record",
                        "type": "record",
                        "doc": "some documentation",
                        "fields": [{"name": "field", "type": "string"}],
                    },
                ],
            }
        ],
    }

    record = {"id": {"field": "foo"}}

    binary = avro_to_bytes_with_schema(original_schema, record)

    output_using_new_schema = bytes_with_schema_to_avro(new_schema, binary)
    assert output_using_new_schema == record


def test_union_of_lists_evolution_with_extra_type():
    """https://github.com/fastavro/fastavro/issues/486"""
    original_schema = {
        "name": "test_union_of_lists_evolution_with_extra_type",
        "type": "record",
        "fields": [
            {
                "name": "id",
                "type": [
                    "null",
                    {
                        "name": "some_record",
                        "type": "record",
                        "fields": [{"name": "field", "type": "string"}],
                    },
                ],
            }
        ],
    }

    new_schema = {
        "name": "test_union_of_lists_evolution_with_extra_type",
        "type": "record",
        "fields": [
            {
                "name": "id",
                "type": [
                    "null",
                    {
                        "name": "some_record",
                        "type": "record",
                        "fields": [{"name": "field", "type": "string"}],
                    },
                    "string",
                ],
            }
        ],
    }

    record = {"id": {"field": "foo"}}

    binary = avro_to_bytes_with_schema(original_schema, record)

    output_using_new_schema = bytes_with_schema_to_avro(new_schema, binary)
    assert output_using_new_schema == record


INT_ARRAY = {"type": "array", "items": "int"}
LONG_ARRAY = {"type": "array", "items": "long"}
INT_MAP = {"type": "map", "values": "int"}
LONG_MAP = {"type": "map", "values": "long"}
ENUM_AB = {"type": "enum", "name": "enum", "symbols": ["A", "B"]}
ENUM2_AB = {"type": "enum", "name": "enum2", "symbols": ["A", "B"]}
ENUM_ABC = {"type": "enum", "name": "enum", "symbols": ["A", "B", "C"]}
INT_UNION = ["int"]
LONG_UNION = ["long"]
FLOAT_UNION = ["float"]
DOUBLE_UNION = ["double"]
STRING_UNION = ["string"]
BYTES_UNION = ["bytes"]
INT_LONG_UNION = ["int", "long"]
INT_FLOAT_UNION = ["int", "float"]
INT_LONG_FLOAT_DOUBLE_UNION = ["int", "long", "float", "double"]
INT_STRING_UNION = ["int", "string"]
STRING_INT_UNION = ["string", "int"]
FIXED_4_BYTES = {"type": "fixed", "name": "fixed", "size": 4}
FIXED_8_BYTES = {"type": "fixed", "name": "fixed", "size": 8}
EMPTY_RECORD = {"type": "record", "name": "record", "fields": []}
A_INT_RECORD = {
    "type": "record",
    "name": "record",
    "fields": [{"name": "a", "type": "int"}],
}
A_DINT_RECORD = {
    "type": "record",
    "name": "record",
    "fields": [{"name": "a", "type": "int", "default": 0}],
}
A_LONG_RECORD = {
    "type": "record",
    "name": "record",
    "fields": [{"name": "a", "type": "long"}],
}
A_INT_B_INT_RECORD = {
    "type": "record",
    "name": "record",
    "fields": [{"name": "a", "type": "int"}, {"name": "b", "type": "int"}],
}
A_INT_B_DINT_RECORD = {
    "type": "record",
    "name": "record",
    "fields": [
        {"name": "a", "type": "int"},
        {"name": "b", "type": "int", "default": 0},
    ],
}
A_DINT_B_DINT_RECORD = {
    "type": "record",
    "name": "record",
    "fields": [
        {"name": "a", "type": "int", "default": 0},
        {"name": "b", "type": "int", "default": 0},
    ],
}
ENUM_ABC_ENUM_DEFAULT_A_RECORD = {
    "type": "record",
    "name": "record",
    "fields": [
        {
            "name": "Field",
            "type": {
                "type": "enum",
                "name": "enum",
                "symbols": ["A", "B", "C"],
                "default": "A",
            },
        },
    ],
}
ENUM_AB_ENUM_DEFAULT_A_RECORD = {
    "type": "record",
    "name": "record",
    "fields": [
        {
            "name": "Field",
            "type": {
                "type": "enum",
                "name": "enum",
                "symbols": ["A", "B"],
                "default": "A",
            },
        },
    ],
}
ENUM_ABC_FIELD_DEFAULT_B_ENUM_DEFAULT_A_RECORD = {
    "type": "record",
    "name": "record",
    "fields": [
        {
            "name": "Field",
            "type": {
                "type": "enum",
                "name": "enum",
                "symbols": ["A", "B", "C"],
                "default": "A",
            },
            "default": "B",
        },
    ],
}
ENUM_AB_FIELD_DEFAULT_A_ENUM_DEFAULT_B_RECORD = {
    "type": "record",
    "name": "record",
    "fields": [
        {
            "name": "Field",
            "type": {
                "type": "enum",
                "name": "enum",
                "symbols": ["A", "B"],
                "default": "B",
            },
            "default": "A",
        }
    ],
}
NS_RECORD1 = {
    "type": "record",
    "name": "record",
    "fields": [
        {
            "name": "f1",
            "type": [
                "null",
                {
                    "type": "array",
                    "items": {
                        "type": "record",
                        "name": "inner_record",
                        "namespace": "ns1",
                        "fields": [{"name": "a", "type": "int"}],
                    },
                },
            ],
        }
    ],
}
NS_RECORD2 = {
    "type": "record",
    "name": "record",
    "fields": [
        {
            "name": "f1",
            "type": [
                "null",
                {
                    "type": "array",
                    "items": {
                        "type": "record",
                        "name": "inner_record",
                        "namespace": "ns2",
                        "fields": [{"name": "a", "type": "int"}],
                    },
                },
            ],
        }
    ],
}


@pytest.mark.parametrize(
    "writer_schema,reader_schema",
    [
        ("boolean", "boolean"),
        ("int", "int"),
        ("int", "long"),
        ("long", "long"),
        ("int", "float"),
        ("long", "float"),
        ("int", "double"),
        ("long", "double"),
        ("float", "double"),
        ("string", "string"),
        ("bytes", "bytes"),
        ("string", "bytes"),
        # ("bytes", "string"),
        (INT_ARRAY, INT_ARRAY),
        (INT_ARRAY, LONG_ARRAY),
        (INT_MAP, INT_MAP),
        (INT_MAP, LONG_MAP),
        (ENUM_AB, ENUM_AB),
        (ENUM_AB, ENUM_ABC),
        (INT_UNION, FLOAT_UNION),
        (LONG_UNION, FLOAT_UNION),
        (INT_LONG_UNION, FLOAT_UNION),
        (INT_UNION, INT_UNION),
        (STRING_INT_UNION, INT_STRING_UNION),
        (INT_UNION, LONG_UNION),
        (INT_UNION, FLOAT_UNION),
        (INT_UNION, DOUBLE_UNION),
        (LONG_UNION, FLOAT_UNION),
        (LONG_UNION, DOUBLE_UNION),
        (FLOAT_UNION, DOUBLE_UNION),
        (STRING_UNION, BYTES_UNION),
        # (BYTES_UNION, STRING_UNION),
        (INT_FLOAT_UNION, DOUBLE_UNION),
        (INT_FLOAT_UNION, "float"),
        (INT_LONG_UNION, "long"),
        (INT_FLOAT_UNION, "double"),
        (INT_LONG_FLOAT_DOUBLE_UNION, "double"),
        (FIXED_4_BYTES, FIXED_4_BYTES),
        (EMPTY_RECORD, EMPTY_RECORD),
        (A_INT_RECORD, EMPTY_RECORD),
        (A_INT_RECORD, A_INT_RECORD),
        (A_INT_RECORD, A_DINT_RECORD),
        (A_DINT_RECORD, A_DINT_RECORD),
        (A_DINT_RECORD, A_INT_RECORD),
        (A_INT_RECORD, A_LONG_RECORD),
        (A_INT_B_INT_RECORD, A_INT_RECORD),
        (A_INT_B_INT_RECORD, A_DINT_RECORD),
        (A_INT_RECORD, A_INT_B_DINT_RECORD),
        (EMPTY_RECORD, A_DINT_B_DINT_RECORD),
        (A_INT_RECORD, A_DINT_B_DINT_RECORD),
        (A_DINT_B_DINT_RECORD, A_INT_B_INT_RECORD),
        ({"type": "null"}, {"type": "null"}),
        ("null", "null"),
        (ENUM_ABC_ENUM_DEFAULT_A_RECORD, ENUM_AB_ENUM_DEFAULT_A_RECORD),
        (
            ENUM_ABC_FIELD_DEFAULT_B_ENUM_DEFAULT_A_RECORD,
            ENUM_AB_FIELD_DEFAULT_A_ENUM_DEFAULT_B_RECORD,
        ),
        (A_DINT_B_DINT_RECORD, A_INT_B_INT_RECORD),
        (NS_RECORD2, NS_RECORD1),
    ],
)
def test_schema_compatibility(writer_schema, reader_schema):
    bio = BytesIO()
    writer_data = generate_one(writer_schema)
    fastavro.writer(bio, writer_schema, [writer_data])
    bio.seek(0)

    # This should not throw an exception if the schemas are compatible
    list(fastavro.reader(bio, reader_schema))


def test_bytes_writer_string_reader():
    writer_schema = "bytes"
    reader_schema = "string"

    # If the bytes are utf-8 encoded, reader should work
    bio = BytesIO()
    fastavro.writer(bio, writer_schema, [b"123"])
    bio.seek(0)
    list(fastavro.reader(bio, reader_schema))

    # If the bytes are not utf-8, decoding will fail
    bio = BytesIO()
    fastavro.writer(bio, writer_schema, ["かわいい".encode("cp932")])
    bio.seek(0)
    with pytest.raises(UnicodeDecodeError):
        list(fastavro.reader(bio, reader_schema))


@pytest.mark.parametrize(
    "writer_schema,reader_schema",
    [
        ("int", "null"),
        ("long", "null"),
        ("int", "boolean"),
        ("null", "int"),
        ("boolean", "int"),
        ("long", "int"),
        ("float", "int"),
        ("double", "int"),
        ("float", "long"),
        ("double", "long"),
        ("double", "float"),
        ("string", "double"),
        ("string", FIXED_4_BYTES),
        ("boolean", "string"),
        ("int", "string"),
        ("null", "bytes"),
        ("int", "bytes"),
        ("int", A_INT_RECORD),
        (LONG_ARRAY, INT_ARRAY),
        (INT_ARRAY, INT_MAP),
        (INT_MAP, INT_ARRAY),
        (LONG_MAP, INT_MAP),
        (ENUM2_AB, "int"),
        ("int", ENUM2_AB),
        (FIXED_4_BYTES, FIXED_8_BYTES),
        (FIXED_8_BYTES, FIXED_4_BYTES),
    ],
)
def test_schema_incompatibility(writer_schema, reader_schema):
    bio = BytesIO()
    writer_data = generate_one(writer_schema)
    fastavro.writer(bio, writer_schema, [writer_data])
    bio.seek(0)

    with pytest.raises(SchemaResolutionError):
        list(fastavro.reader(bio, reader_schema))


def test_union_writer_simple_reader():
    writer_schema = INT_LONG_FLOAT_DOUBLE_UNION
    writer_data = ("float", generate_one("float"))
    for reader_schema in INT_LONG_FLOAT_DOUBLE_UNION:
        bio = BytesIO()
        fastavro.writer(bio, writer_schema, [writer_data])
        bio.seek(0)

        # If we are reading as an int or long, it should fail. If we are reading
        # as a float or double, it should pass
        if reader_schema in ("int", "long"):
            with pytest.raises(SchemaResolutionError):
                list(fastavro.reader(bio, reader_schema))
        else:
            list(fastavro.reader(bio, reader_schema))


def test_writer_enum_more_symbols_than_reader_enum():
    writer_schema = ENUM_ABC
    reader_schema = ENUM_AB

    for symbol in writer_schema["symbols"]:
        bio = BytesIO()
        fastavro.writer(bio, writer_schema, [symbol])
        bio.seek(0)

        # If the symbol is in the reader symbols, it should work. Otherwise it
        # should fail
        if symbol in reader_schema["symbols"]:
            list(fastavro.reader(bio, reader_schema))
        else:
            with pytest.raises(
                SchemaResolutionError,
                match=re.escape(
                    "C not found in reader symbol list enum, known symbols: ['A', 'B']"
                ),
            ):
                list(fastavro.reader(bio, reader_schema))


def test_union_of_schemas_evolution():
    """https://github.com/fastavro/fastavro/issues/553"""
    original_schema = [
        {
            "name": "other_record",
            "type": "record",
            "fields": [
                {"name": "other_one", "type": "boolean"},
                {"name": "other_two", "type": "float"},
            ],
        },
        {
            "name": "root_record",
            "type": "record",
            "fields": [
                {"name": "one", "type": "boolean"},
                {"name": "two", "type": "float"},
                {"name": "three", "type": "other_record"},
            ],
        },
    ]

    new_schema = [
        {
            "name": "other_record",
            "type": "record",
            "fields": [
                {"name": "other_two", "type": "float"},
                {"name": "new_three", "type": "float", "default": 0.0},
            ],
        },
        {
            "name": "root_record",
            "type": "record",
            "fields": [{"name": "three", "type": "other_record"}],
        },
    ]

    record = {"one": True, "two": 0.0, "three": {"other_one": False, "other_two": 1.0}}

    binary = avro_to_bytes_with_schema(original_schema, record)

    output_using_new_schema = bytes_with_schema_to_avro(new_schema, binary)
    assert output_using_new_schema == {"three": {"other_two": 1.0, "new_three": 0.0}}


def test_records_match_by_unnamespaced_name():
    """https://issues.apache.org/jira/browse/AVRO-3561"""
    original_schema = {
        "name": "ns.test_record",
        "type": "record",
        "fields": [{"name": "f1", "type": "int"}],
    }

    new_schema = {
        "name": "ns.foo.test_record",
        "type": "record",
        "fields": [
            {"name": "f1", "type": "int"},
            {"name": "f2", "type": "int", "default": 3},
        ],
    }

    record = {"f1": 0}

    binary = avro_to_bytes_with_schema(original_schema, record)

    output_using_new_schema = bytes_with_schema_to_avro(new_schema, binary)
    assert output_using_new_schema == {"f1": 0, "f2": 3}


def test_named_type_inline_writer_named_reference_reader():
    """https://github.com/fastavro/fastavro/issues/896"""
    writer_schema = {
        "type": "record",
        "name": "Envelope",
        "fields": [
            {
                "name": "event",
                "type": [
                    {
                        "type": "record",
                        "name": "A",
                        "fields": [
                            {
                                "name": "c",
                                "type": {
                                    "type": "enum",
                                    "name": "Color",
                                    "symbols": ["RED", "GREEN"],
                                },
                            },
                        ],
                    },
                    {
                        "type": "record",
                        "name": "B",
                        "fields": [
                            {"name": "c", "type": "Color"},
                        ],
                    },
                ],
            },
        ],
    }

    reader_schema = {
        "type": "record",
        "name": "Envelope",
        "fields": [
            {
                "name": "event",
                "type": [
                    {
                        "type": "record",
                        "name": "B",
                        "fields": [
                            {
                                "name": "c",
                                "type": {
                                    "type": "enum",
                                    "name": "Color",
                                    "symbols": ["RED", "GREEN"],
                                },
                            },
                        ],
                    },
                    {
                        "type": "record",
                        "name": "A",
                        "fields": [
                            {"name": "c", "type": "Color"},
                        ],
                    },
                ],
            },
        ],
    }

    parsed_writer = fastavro.parse_schema(writer_schema)
    parsed_reader = fastavro.parse_schema(reader_schema)

    for record_name in ("A", "B"):
        record = {"event": (record_name, {"c": "GREEN"})}
        bio = BytesIO()
        fastavro.schemaless_writer(bio, parsed_writer, record)
        bio.seek(0)
        decoded = fastavro.schemaless_reader(
            bio, parsed_writer, parsed_reader, return_record_name=True
        )
        assert decoded == record


WRITER_SCHEMA = {
    "type": "record",
    "name": "default_evolution",
    "fields": [{"name": "a", "type": "int"}],
}


def _avro_file(schema=WRITER_SCHEMA, records=None, num_records=2):
    if records is None:
        records = [{"a": index} for index in range(num_records)]
    bio = BytesIO()
    fastavro_writer(bio, schema, records)
    bio.seek(0)
    return bio


def test_mutable_array_default_is_not_shared_between_records():
    reader_schema = {
        "type": "record",
        "name": "default_evolution",
        "fields": [
            {"name": "a", "type": "int"},
            {
                "name": "arr",
                "type": {"type": "array", "items": "int"},
                "default": [1, 2],
            },
        ],
    }

    records = list(fastavro.reader(_avro_file(), reader_schema))

    records[0]["arr"].append(99)
    assert records[1]["arr"] == [1, 2]


def test_mutable_map_default_is_not_shared_between_records():
    reader_schema = {
        "type": "record",
        "name": "default_evolution",
        "fields": [
            {"name": "a", "type": "int"},
            {
                "name": "mp",
                "type": {"type": "map", "values": "int"},
                "default": {"x": 1},
            },
        ],
    }

    records = list(fastavro.reader(_avro_file(), reader_schema))

    records[0]["mp"]["y"] = 7
    assert records[1]["mp"] == {"x": 1}


def test_mutable_default_does_not_mutate_parsed_schema():
    reader_schema = {
        "type": "record",
        "name": "default_evolution",
        "fields": [
            {"name": "a", "type": "int"},
            {
                "name": "arr",
                "type": {"type": "array", "items": "int"},
                "default": [1, 2],
            },
            {
                "name": "mp",
                "type": {"type": "map", "values": "int"},
                "default": {"x": 1},
            },
        ],
    }
    parsed_reader_schema = fastavro.parse_schema(reader_schema)

    record = next(fastavro.reader(_avro_file(num_records=1), parsed_reader_schema))
    record["arr"].append(99)
    record["mp"]["y"] = 7

    fields = {
        field["name"]: field for field in parsed_reader_schema["fields"]
    }
    assert fields["arr"]["default"] == [1, 2]
    assert fields["mp"]["default"] == {"x": 1}


def test_mutable_default_is_not_shared_between_two_readers():
    reader_schema = {
        "type": "record",
        "name": "default_evolution",
        "fields": [
            {"name": "a", "type": "int"},
            {
                "name": "arr",
                "type": {"type": "array", "items": "int"},
                "default": [1, 2],
            },
        ],
    }
    parsed_reader_schema = fastavro.parse_schema(reader_schema)

    first_record = next(
        fastavro.reader(_avro_file(num_records=1), parsed_reader_schema)
    )
    second_record = next(
        fastavro.reader(_avro_file(num_records=1), parsed_reader_schema)
    )

    first_record["arr"].append(99)
    assert second_record["arr"] == [1, 2]


def test_nested_mutable_defaults_are_independent():
    reader_schema = {
        "type": "record",
        "name": "default_evolution",
        "fields": [
            {"name": "a", "type": "int"},
            {
                "name": "nested",
                "type": {
                    "type": "record",
                    "name": "nested_default_record",
                    "fields": [
                        {"name": "x", "type": "int"},
                        {
                            "name": "arr",
                            "type": {"type": "array", "items": "int"},
                        },
                        {
                            "name": "mp",
                            "type": {"type": "map", "values": "int"},
                        },
                    ],
                },
                "default": {"x": 1, "arr": [1], "mp": {"k": 1}},
            },
            {
                "name": "array_of_records",
                "type": {
                    "type": "array",
                    "items": {
                        "type": "record",
                        "name": "array_item_record",
                        "fields": [
                            {
                                "name": "arr",
                                "type": {"type": "array", "items": "int"},
                            }
                        ],
                    },
                },
                "default": [{"arr": [1, 2]}],
            },
        ],
    }

    first_record = next(
        fastavro.reader(
            _avro_file(num_records=1), fastavro.parse_schema(reader_schema)
        )
    )
    second_record = next(
        fastavro.reader(
            _avro_file(num_records=1), fastavro.parse_schema(reader_schema)
        )
    )

    first_record["nested"]["arr"].append(99)
    first_record["nested"]["mp"]["new"] = 2
    first_record["array_of_records"][0]["arr"].append(99)

    assert second_record["nested"] == {"x": 1, "arr": [1], "mp": {"k": 1}}
    assert second_record["array_of_records"] == [{"arr": [1, 2]}]


def test_bytes_and_fixed_defaults_are_decoded_without_json_roundtrip():
    reader_schema = {
        "type": "record",
        "name": "default_evolution",
        "fields": [
            {"name": "a", "type": "int"},
            {"name": "blob", "type": "bytes", "default": "abc"},
            {
                "name": "fixed_blob",
                "type": {"type": "fixed", "name": "fixed_default", "size": 3},
                "default": "xyz",
            },
            {
                "name": "nested",
                "type": {
                    "type": "record",
                    "name": "bytes_nested_record",
                    "fields": [
                        {"name": "blob", "type": "bytes"},
                    ],
                },
                "default": {"blob": "nested"},
            },
            {
                "name": "array_of_bytes",
                "type": {"type": "array", "items": "bytes"},
                "default": ["ab", "cd"],
            },
            {
                "name": "map_of_bytes",
                "type": {"type": "map", "values": "bytes"},
                "default": {"key": "value"},
            },
        ],
    }

    record = bytes_with_schema_to_avro(
        fastavro.parse_schema(reader_schema),
        avro_to_bytes_with_schema(WRITER_SCHEMA, {"a": 1}),
    )

    assert record["blob"] == b"abc"
    assert isinstance(record["blob"], bytes)
    assert record["fixed_blob"] == b"xyz"
    assert record["nested"]["blob"] == b"nested"
    assert record["array_of_bytes"] == [b"ab", b"cd"]
    assert record["map_of_bytes"] == {"key": b"value"}


def test_union_default_uses_matching_branch_type():
    reader_schema = {
        "type": "record",
        "name": "default_evolution",
        "fields": [
            {"name": "a", "type": "int"},
            {
                "name": "null_union",
                "type": ["null", "string", "bytes"],
                "default": None,
            },
            {
                "name": "string_union",
                "type": ["null", "string", "int"],
                "default": "text",
            },
            {
                "name": "bytes_union",
                "type": ["null", "bytes", "int"],
                "default": "blob",
            },
            {
                "name": "record_union",
                "type": [
                    "null",
                    {
                        "type": "record",
                        "name": "union_default_record",
                        "fields": [
                            {"name": "x", "type": "int"},
                            {"name": "blob", "type": "bytes"},
                        ],
                    },
                ],
                "default": {"x": 1, "blob": "ab"},
            },
        ],
    }

    record = bytes_with_schema_to_avro(
        fastavro.parse_schema(reader_schema),
        avro_to_bytes_with_schema(WRITER_SCHEMA, {"a": 1}),
    )

    assert record["null_union"] is None
    assert record["string_union"] == "text"
    assert record["bytes_union"] == b"blob"
    assert record["record_union"] == {"x": 1, "blob": b"ab"}


def test_logical_type_defaults_are_converted_to_python_types():
    import datetime
    import uuid

    reader_schema = {
        "type": "record",
        "name": "default_evolution",
        "fields": [
            {"name": "a", "type": "int"},
            {
                "name": "day",
                "type": {"type": "int", "logicalType": "date"},
                "default": 0,
            },
            {
                "name": "timestamp",
                "type": {"type": "long", "logicalType": "timestamp-micros"},
                "default": 0,
            },
            {
                "name": "identifier",
                "type": {"type": "string", "logicalType": "uuid"},
                "default": "12345678-1234-5678-1234-567812345678",
            },
        ],
    }

    record = bytes_with_schema_to_avro(
        fastavro.parse_schema(reader_schema),
        avro_to_bytes_with_schema(WRITER_SCHEMA, {"a": 1}),
    )

    assert record["day"] == datetime.date(1970, 1, 1)
    assert record["timestamp"] == datetime.datetime(
        1970, 1, 1, tzinfo=datetime.timezone.utc
    )
    assert record["identifier"] == uuid.UUID(
        "12345678-1234-5678-1234-567812345678"
    )


def test_schemaless_reader_mutable_default_isolation():
    reader_schema = {
        "type": "record",
        "name": "default_evolution",
        "fields": [
            {"name": "a", "type": "int"},
            {
                "name": "arr",
                "type": {"type": "array", "items": "bytes"},
                "default": ["ab"],
            },
        ],
    }
    parsed_reader_schema = fastavro.parse_schema(reader_schema)

    def read_one():
        bio = BytesIO()
        fastavro.schemaless_writer(bio, WRITER_SCHEMA, {"a": 1})
        bio.seek(0)
        return fastavro.schemaless_reader(
            bio, WRITER_SCHEMA, parsed_reader_schema
        )

    first_record = read_one()
    second_record = read_one()

    first_record["arr"].append(b"zz")
    assert second_record["arr"] == [b"ab"]
