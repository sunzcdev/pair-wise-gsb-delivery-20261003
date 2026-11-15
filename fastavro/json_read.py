from typing import IO, Optional

from ._read_py import reader
from .io.json_decoder import AvroJSONDecoder
from .schema import parse_schema
from .types import Schema


def json_reader(
    fo: IO,
    schema: Schema,
    reader_schema: Optional[Schema] = None,
    *,
    decoder=AvroJSONDecoder,
    explain_callback=None,
    on_explain_error: str = "raise",
) -> reader:
    """Iterator over records in an avro json file.

    Parameters
    ----------
    fo
        File-like object to read from
    schema
        Original schema used when writing the JSON data
    reader_schema
        If the schema has changed since being written then the new schema can
        be given to allow for schema migration
    decoder
        By default the standard AvroJSONDecoder will be used, but a custom one
        could be passed here
    explain_callback
        Optional callable invoked with an :class:`ExplanationEvent` for each
        real parse decision, using the same path / record-index semantics as
        the binary readers. When ``None`` (the default) no events are
        collected.
    on_explain_error
        What to do when ``explain_callback`` raises: ``"raise"`` (default;
        propagate) or ``"ignore"`` (swallow and keep reading).

    Example::

        from fastavro import json_reader

        schema = {
            'doc': 'A weather reading.',
            'name': 'Weather',
            'namespace': 'test',
            'type': 'record',
            'fields': [
                {'name': 'station', 'type': 'string'},
                {'name': 'time', 'type': 'long'},
                {'name': 'temp', 'type': 'int'},
            ]
        }

        with open('some-file', 'r') as fo:
            avro_reader = json_reader(fo, schema)
            for record in avro_reader:
                print(record)
    """
    reader_instance = reader(
        decoder(fo),
        schema,
        explain_callback=explain_callback,
        on_explain_error=on_explain_error,
    )
    if reader_schema:
        reader_instance.reader_schema = parse_schema(
            reader_schema, reader_instance._named_schemas["reader"], _write_hint=False
        )
    return reader_instance
