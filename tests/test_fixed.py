import unittest
from io import BytesIO, StringIO

from agate import csv, fixed


SCHEMA = 'column,start,length\ntext,0,3\nvalue,3,4\n'


def schema_stream(schema=SCHEMA):
    return StringIO(schema)


class TestFixed(unittest.TestCase):
    def test_reader(self):
        with open('examples/testfixed_converted.csv') as f:
            csv_reader = csv.Reader(f)
            csv_header = next(csv_reader)
            csv_data = list(csv_reader)

        with open('examples/testfixed') as f:
            with open('examples/testfixed_schema.csv') as schema_f:
                fixed_reader = fixed.Reader(f, schema_f)
                fixed_data = list(fixed_reader)

        self.assertEqual(csv_header, fixed_reader.fieldnames)
        self.assertEqual(csv_data, fixed_data)

    def test_reader_func(self):
        with open('examples/testfixed_converted.csv') as f:
            csv_reader = csv.reader(f)
            csv_header = next(csv_reader)
            csv_data = list(csv_reader)

        with open('examples/testfixed') as f:
            with open('examples/testfixed_schema.csv') as schema_f:
                fixed_reader = fixed.reader(f, schema_f)
                fixed_data = list(fixed_reader)

        self.assertEqual(csv_header, fixed_reader.fieldnames)
        self.assertEqual(csv_data, fixed_data)

    def test_dict_reader(self):
        with open('examples/testfixed_converted.csv') as f:
            csv_reader = csv.DictReader(f)
            csv_data = list(csv_reader)

        with open('examples/testfixed') as f:
            with open('examples/testfixed_schema.csv') as schema_f:
                fixed_reader = fixed.DictReader(f, schema_f)
                fixed_data = list(fixed_reader)

        self.assertEqual(csv_reader.fieldnames, fixed_reader.fieldnames)
        self.assertEqual(csv_data, fixed_data)

    def test_character_layout_unchanged_with_multibyte(self):
        reader = fixed.Reader(StringIO('名ab1234\n'), schema_stream(), layout='character')

        self.assertEqual(next(reader), ['名ab', '1234'])

    def test_byte_layout_slices_bytes_before_decoding(self):
        line = '名ab1234'.encode('utf-8')

        reader = fixed.Reader(BytesIO(line), schema_stream(), layout='byte')

        self.assertEqual(next(reader), ['名', 'ab12'])

    def test_byte_layout_strips_line_endings(self):
        line = '名ab12'.encode('utf-8')

        for ending in (b'\n', b'\r\n', b'\r'):
            reader = fixed.Reader(BytesIO(line + ending), schema_stream(), layout='byte')

            self.assertEqual(next(reader), ['名', 'ab12'])

    def test_byte_layout_dict_reader(self):
        line = '名ab12'.encode('utf-8')

        reader = fixed.DictReader(BytesIO(line), schema_stream(), layout='byte')

        self.assertEqual(next(reader), {'text': '名', 'value': 'ab12'})

    def test_byte_layout_uses_encoding(self):
        line = '名ab12'.encode('utf-16-le')
        schema = 'column,start,length\ntext,0,2\nvalue,2,8\n'

        reader = fixed.Reader(BytesIO(line), schema_stream(schema), layout='byte', encoding='utf-16-le')

        self.assertEqual(next(reader), ['名', 'ab12'])

    def test_byte_layout_alias_and_boolean_flag(self):
        line = '名ab12'.encode('utf-8')

        for kwargs in ({'layout': 'bytes'}, {'byte_mode': True}, {'layout_mode': 'binary'}):
            reader = fixed.Reader(BytesIO(line), schema_stream(), **kwargs)

            self.assertEqual(reader.layout, 'byte')
            self.assertEqual(next(reader), ['名', 'ab12'])

    def test_multibyte_split_reports_record_and_field(self):
        stream = BytesIO(b'ok1ok2\n' + b'\xe5\x90')

        reader = fixed.Reader(stream, schema_stream(), layout='byte')
        next(reader)

        with self.assertRaises(ValueError) as ctx:
            next(reader)

        message = str(ctx.exception).lower()
        self.assertIn('record 2', message)
        self.assertIn('text', message)

    def test_byte_layout_requires_binary_stream(self):
        with self.assertRaises(TypeError):
            next(fixed.Reader(StringIO('名ab1234'), schema_stream(), layout='byte'))

    def test_character_layout_requires_text_stream(self):
        with self.assertRaises(TypeError):
            next(fixed.Reader(BytesIO(b'abc'), schema_stream(), layout='character'))

    def test_short_records_pad_by_default(self):
        schema = 'column,start,length\na,0,3\nb,3,3\n'

        reader = fixed.Reader(BytesIO(b'ab'), schema_stream(schema), layout='byte')

        self.assertEqual(next(reader), ['ab', ''])

    def test_short_records_error(self):
        schema = 'column,start,length\na,0,3\nb,3,3\n'

        reader = fixed.Reader(BytesIO(b'ab\nabcdef'), schema_stream(schema),
                              layout='byte', short_records='error')

        with self.assertRaises(ValueError) as ctx:
            next(reader)

        message = str(ctx.exception).lower()
        self.assertIn('record 1', message)
        self.assertIn('a', message)

    def test_trailing_bytes_ignored_by_default(self):
        schema = 'column,start,length\na,0,3\nb,3,3\n'

        reader = fixed.Reader(BytesIO(b'abcdefXX'), schema_stream(schema), layout='byte')

        self.assertEqual(next(reader), ['abc', 'def'])

    def test_trailing_bytes_error(self):
        schema = 'column,start,length\na,0,3\nb,3,3\n'

        reader = fixed.Reader(BytesIO(b'abcdef\nabcdefXX'), schema_stream(schema),
                              layout='byte', trailing_bytes='error')
        next(reader)

        with self.assertRaises(ValueError) as ctx:
            next(reader)

        message = str(ctx.exception).lower()
        self.assertIn('record 2', message)
        self.assertIn('2 extra', message)

    def test_character_layout_trailing_error(self):
        schema = 'column,start,length\na,0,3\nb,3,3\n'

        reader = fixed.Reader(StringIO('abcdefXX'), schema_stream(schema), trailing_bytes='error')

        with self.assertRaises(ValueError) as ctx:
            next(reader)

        self.assertIn('characters', str(ctx.exception).lower())

    def test_schema_rejects_negative_length(self):
        schema = 'column,start,length\na,0,-1\n'

        with self.assertRaises(ValueError) as ctx:
            fixed.Reader(BytesIO(b'abc'), schema_stream(schema))

        self.assertIn('negative length', str(ctx.exception))

    def test_schema_rejects_overlapping_ranges(self):
        schema = 'column,start,length\na,0,3\nb,2,3\n'

        with self.assertRaises(ValueError) as ctx:
            fixed.Reader(BytesIO(b'abcde'), schema_stream(schema))

        message = str(ctx.exception)
        self.assertIn('overlapping', message)
        self.assertIn('a', message)
        self.assertIn('b', message)

    def test_schema_rejects_duplicate_names(self):
        schema = 'column,start,length\na,0,2\na,2,2\n'

        with self.assertRaises(ValueError) as ctx:
            fixed.Reader(BytesIO(b'abcd'), schema_stream(schema))

        self.assertIn('duplicate column name', str(ctx.exception))

    def test_invalid_layout_option(self):
        with self.assertRaises(ValueError):
            fixed.Reader(BytesIO(b'abc'), schema_stream(), layout='nope')

    def test_unexpected_keyword_argument(self):
        with self.assertRaises(TypeError):
            fixed.Reader(BytesIO(b'abc'), schema_stream(), bogus=True)

    def test_line_num_counts_records(self):
        reader = fixed.Reader(BytesIO(b'a\nb\nc\n'),
                              schema_stream('column,start,length\nx,0,1\n'),
                              layout='byte')
        list(reader)

        self.assertEqual(reader.line_num, 3)
