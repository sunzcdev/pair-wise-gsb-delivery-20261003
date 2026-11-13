import os
import shutil
from io import BytesIO, StringIO

from agate import Table
from agate.testcase import AgateTestCase


SCHEMA = 'column,start,length\ntext,0,3\nvalue,3,4\n'
DATA = '名ab12'.encode('utf-8')


class TestFromFixed(AgateTestCase):
    def test_from_fixed(self):
        table1 = Table.from_csv('examples/testfixed_converted.csv')
        table2 = Table.from_fixed('examples/testfixed', 'examples/testfixed_schema.csv')

        self.assertColumnNames(table2, table1.column_names)
        self.assertColumnTypes(table2, [type(c) for c in table1.column_types])

        self.assertRows(table2, table1.rows)

    def test_from_fixed_file_like_objects(self):
        table1 = Table.from_csv('examples/testfixed_converted.csv')

        with open('examples/testfixed', encoding='utf-8') as f, \
                open('examples/testfixed_schema.csv', encoding='utf-8') as schema_f:
            table2 = Table.from_fixed(f, schema_f)

        self.assertColumnNames(table2, table1.column_names)
        self.assertColumnTypes(table2, [type(c) for c in table1.column_types])

        self.assertRows(table2, table1.rows)

    def test_from_fixed_byte_layout_file_like(self):
        f = BytesIO(DATA)
        schema_f = StringIO(SCHEMA)

        table = Table.from_fixed(f, schema_f, layout='byte')

        self.assertColumnNames(table, ('text', 'value'))
        self.assertRows(table, [('名', 'ab12')])
        self.assertFalse(f.closed)
        self.assertFalse(schema_f.closed)

    def test_from_fixed_byte_layout_paths(self):
        directory = 'test_fixed_tmp'

        if os.path.exists(directory):
            self.skipTest('temporary directory %r already exists' % directory)

        os.mkdir(directory)

        try:
            data_path = os.path.join(directory, 'data.bin')
            schema_path = os.path.join(directory, 'schema.csv')

            with open(data_path, 'wb') as f:
                f.write(DATA)

            with open(schema_path, 'w', encoding='utf-8') as f:
                f.write(SCHEMA)

            table = Table.from_fixed(data_path, schema_path, layout='byte')

            self.assertRows(table, [('名', 'ab12')])
        finally:
            shutil.rmtree(directory, ignore_errors=True)

    def test_from_fixed_options_passthrough(self):
        f = BytesIO(DATA)
        schema_f = StringIO(SCHEMA)

        table = Table.from_fixed(f, schema_f, byte_layout=True, short_records='error',
                                 trailing_bytes='error')

        self.assertRows(table, [('名', 'ab12')])
        self.assertFalse(f.closed)

    def test_from_fixed_byte_layout_multibyte_split(self):
        schema_f = StringIO(SCHEMA)

        with self.assertRaises(ValueError):
            Table.from_fixed(BytesIO(b'\xe5\x90'), schema_f, layout='byte')

    def test_from_fixed_short_records_error(self):
        schema = 'column,start,length\na,0,3\nb,3,3\n'

        with self.assertRaises(ValueError):
            Table.from_fixed(BytesIO(b'ab'), StringIO(schema), layout='byte', short_records='error')

    def test_from_fixed_trailing_bytes_error(self):
        schema = 'column,start,length\na,0,3\nb,3,3\n'

        with self.assertRaises(ValueError):
            Table.from_fixed(BytesIO(b'abcdefXX'), StringIO(schema), layout='byte', trailing_bytes='error')
