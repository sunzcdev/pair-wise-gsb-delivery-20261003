from agate import fixed, utils


@classmethod
def from_fixed(cls, path, schema_path, column_names=utils.default, column_types=None, row_names=None, encoding='utf-8',
               schema_encoding='utf-8', layout='character', short_records='pad', trailing_bytes='ignore', **kwargs):
    """
    Create a new table from a fixed-width file and a CSV schema.

    Schemas must be in the "ffs" format. There is a repository of such schemas
    maintained at `wireservice/ffs <https://github.com/wireservice/ffs>`_.

    :param path:
        File path or file-like object from which to read fixed-width data.
    :param schema_path:
        File path or file-like object from which to read schema (CSV) data.
    :param column_names:
        By default, these will be parsed from the schema. For alternatives, see
        :meth:`.Table.__init__`.
    :param column_types:
        See :meth:`.Table.__init__`.
    :param row_names:
        See :meth:`.Table.__init__`.
    :param encoding:
        Character encoding of the fixed-width file. Note: if passing in a file
        handle it is assumed you have already opened it with the correct
        encoding specified.
    :param schema_encoding:
        Character encoding of the schema file. Note: if passing in a file
        handle it is assumed you have already opened it with the correct
        encoding specified.
    :param layout:
        Whether the schema's :code:`start` and :code:`length` values count
        :code:`"character"` (the default) or :code:`"byte"`. With the byte
        layout, the data file is read as raw bytes, each field range is
        sliced before decoding, and multibyte characters can no longer shift
        field boundaries. See :class:`.fixed.Reader`.
    :param short_records:
        Passed through to :class:`.fixed.Reader`.
    :param trailing_bytes:
        Passed through to :class:`.fixed.Reader`.
    :param kwargs:
        Additional keyword arguments, such as :code:`byte_layout` or
        :code:`on_short_record`, are passed through to :class:`.fixed.Reader`.
    """
    from agate.table import Table

    close_f = False

    close_schema_f = False

    layout = fixed._resolve(layout, fixed._LAYOUT_ALIASES, 'layout')
    byte_layout = layout == fixed.LAYOUT_BYTE

    try:
        if not hasattr(path, 'read'):
            if byte_layout:
                f = open(path, 'rb')
            else:
                f = open(path, encoding=encoding)
            close_f = True
        else:
            f = path

        if not hasattr(schema_path, 'read'):
            schema_f = open(schema_path, encoding=schema_encoding)
            close_schema_f = True
        else:
            schema_f = schema_path

        reader = fixed.reader(f, schema_f, layout=layout, encoding=encoding,
                              short_records=short_records, trailing_bytes=trailing_bytes, **kwargs)
        rows = list(reader)

    finally:
        if close_f:
            f.close()

        if close_schema_f:
            schema_f.close()

    if column_names == utils.default:
        column_names = reader.fieldnames

    return Table(rows, column_names, column_types, row_names=row_names)
