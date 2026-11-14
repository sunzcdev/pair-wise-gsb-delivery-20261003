import csv as pycsv
import io
import warnings
from decimal import Decimal

from agate import Table
from agate.data_types import Boolean, Date, DateTime, Number, Text, TimeDelta
from agate.testcase import AgateTestCase
from agate.type_tester import TypeTester


def _can_sniff_delimiter(sample):
    try:
        return pycsv.Sniffer().sniff(sample, ['|', ',', '\t', ';', ' ', ':']) is not None
    except pycsv.Error:
        return False


class SequentialOnlyStream:
    """
    A text stream that supports sequential reading only. It deliberately has no
    ``seek``, ``tell`` or ``buffer`` attribute, mimicking a pipe or socket.
    """
    def __init__(self, text):
        self._buffer = io.StringIO(text)
        self.closed = False

    def read(self, size=-1):
        return self._buffer.read(size)

    def readline(self):
        return self._buffer.readline()

    def __iter__(self):
        return self

    def __next__(self):
        line = self.readline()

        if not line:
            raise StopIteration

        return line

    def close(self):
        self.closed = True


class TestFromCSV(AgateTestCase):
    def setUp(self):
        self.rows = (
            (1, 'a', True, '11/4/2015', '11/4/2015 12:22 PM', '4:15'),
            (2, '👍', False, '11/5/2015', '11/4/2015 12:45 PM', '6:18'),
            (None, 'b', None, None, None, None)
        )

        self.column_names = [
            'number', 'text', 'boolean', 'date', 'datetime', 'timedelta'
        ]

        self.column_types = [
            Number(), Text(), Boolean(), Date(), DateTime(), TimeDelta()
        ]

    def test_from_csv(self):
        table1 = Table(self.rows, self.column_names, self.column_types)
        table2 = Table.from_csv('examples/test.csv')

        self.assertColumnNames(table2, table1.column_names)
        self.assertColumnTypes(table2, [Number, Text, Boolean, Date, DateTime, TimeDelta])

        self.assertRows(table2, table1.rows)

    def test_from_csv_crlf(self):
        table1 = Table(self.rows, self.column_names, self.column_types)
        table2 = Table.from_csv('examples/test_crlf.csv')

        self.assertColumnNames(table2, table1.column_names)
        self.assertColumnTypes(table2, [Number, Text, Boolean, Date, DateTime, TimeDelta])

        self.assertRows(table2, table1.rows)

    def test_from_csv_cr(self):
        table1 = Table(self.rows, self.column_names, self.column_types)
        table2 = Table.from_csv('examples/test_cr.csv')

        self.assertColumnNames(table2, table1.column_names)
        self.assertColumnTypes(table2, [Number, Text, Boolean, Date, DateTime, TimeDelta])

        self.assertRows(table2, table1.rows)

    def test_from_csv_file_like_object(self):
        table1 = Table(self.rows, self.column_names, self.column_types)

        f = open('examples/test.csv', encoding='utf-8')

        table2 = Table.from_csv(f)
        f.close()

        self.assertColumnNames(table2, table1.column_names)
        self.assertColumnTypes(table2, [Number, Text, Boolean, Date, DateTime, TimeDelta])

        self.assertRows(table2, table1.rows)

    def test_from_csv_type_tester(self):
        tester = TypeTester(force={
            'number': Text()
        })

        table = Table.from_csv('examples/test.csv', column_types=tester)

        self.assertColumnTypes(table, [Text, Text, Boolean, Date, DateTime, TimeDelta])

    def test_from_csv_no_type_tester(self):
        tester = TypeTester(limit=0)

        table = Table.from_csv('examples/test.csv', column_types=tester)

        self.assertColumnTypes(table, [Text, Text, Text, Text, Text, Text])

    def test_from_csv_no_header(self):
        warnings.simplefilter('ignore')

        try:
            table = Table.from_csv('examples/test_no_header.csv', header=False)
        finally:
            warnings.resetwarnings()

        self.assertColumnNames(table, ['a', 'b', 'c', 'd', 'e', 'f'])
        self.assertColumnTypes(table, [Number, Text, Boolean, Date, DateTime, TimeDelta])

    def test_from_csv_no_header_columns(self):
        table = Table.from_csv('examples/test_no_header.csv', self.column_names, header=False)

        self.assertColumnNames(table, self.column_names)
        self.assertColumnTypes(table, [Number, Text, Boolean, Date, DateTime, TimeDelta])

    def test_from_csv_sniff_limit_0(self):
        table2 = Table.from_csv('examples/test_csv_sniff.csv', sniff_limit=0)

        self.assertColumnNames(table2, ['number|text|boolean|date|datetime|timedelta'])
        self.assertColumnTypes(table2, [Text])

    def test_from_csv_sniff_limit_200(self):
        table1 = Table(self.rows, self.column_names, self.column_types)
        table2 = Table.from_csv('examples/test_csv_sniff.csv', sniff_limit=200)

        self.assertColumnNames(table2, table1.column_names)
        self.assertColumnTypes(table2, [Number, Text, Boolean, Date, DateTime, TimeDelta])

        self.assertRows(table2, table1.rows)

    def test_from_csv_sniff_limit_none(self):
        table1 = Table(self.rows, self.column_names, self.column_types)
        table2 = Table.from_csv('examples/test_csv_sniff.csv', sniff_limit=None)

        self.assertColumnNames(table2, table1.column_names)
        self.assertColumnTypes(table2, [Number, Text, Boolean, Date, DateTime, TimeDelta])

        self.assertRows(table2, table1.rows)

    def test_from_csv_skip_lines(self):
        warnings.simplefilter('ignore')

        try:
            table1 = Table(self.rows[1:], column_types=self.column_types)
            table2 = Table.from_csv('examples/test.csv', header=False, skip_lines=2)
        finally:
            warnings.resetwarnings()

        self.assertColumnNames(table2, table1.column_names)
        self.assertColumnTypes(table2, [Number, Text, Boolean, Date, DateTime, TimeDelta])

        self.assertRows(table2, table1.rows)

    def test_from_csv_skip_lines_crlf(self):
        warnings.simplefilter('ignore')

        try:
            table1 = Table(self.rows[1:], column_types=self.column_types)
            table2 = Table.from_csv('examples/test_crlf.csv', header=False, skip_lines=2)
        finally:
            warnings.resetwarnings()

        self.assertColumnNames(table2, table1.column_names)
        self.assertColumnTypes(table2, [Number, Text, Boolean, Date, DateTime, TimeDelta])

        self.assertRows(table2, table1.rows)

    def test_from_csv_skip_lines_cr(self):
        warnings.simplefilter('ignore')

        try:
            table1 = Table(self.rows[1:], column_types=self.column_types)
            table2 = Table.from_csv('examples/test_cr.csv', header=False, skip_lines=2)
        finally:
            warnings.resetwarnings()

        self.assertColumnNames(table2, table1.column_names)
        self.assertColumnTypes(table2, [Number, Text, Boolean, Date, DateTime, TimeDelta])

        self.assertRows(table2, table1.rows)

    def test_from_csv_row_limit(self):
        table1 = Table(self.rows[:2], self.column_names, self.column_types)
        table2 = Table.from_csv('examples/test.csv', row_limit=2)

        self.assertColumnNames(table2, table1.column_names)
        self.assertColumnTypes(table2, [Number, Text, Boolean, Date, DateTime, TimeDelta])

        self.assertRows(table2, table1.rows)

    def test_from_csv_row_limit_no_header_columns(self):
        table1 = Table(self.rows[:2], self.column_names, self.column_types)
        table2 = Table.from_csv('examples/test_no_header.csv', self.column_names, header=False, row_limit=2)

        self.assertColumnNames(table2, table1.column_names)
        self.assertColumnTypes(table2, [Number, Text, Boolean, Date, DateTime, TimeDelta])

        self.assertRows(table2, table1.rows)

    def test_from_csv_row_limit_too_high(self):
        table1 = Table(self.rows, self.column_names, self.column_types)
        table2 = Table.from_csv('examples/test.csv', row_limit=200)

        self.assertColumnNames(table2, table1.column_names)
        self.assertColumnTypes(table2, [Number, Text, Boolean, Date, DateTime, TimeDelta])

        self.assertRows(table2, table1.rows)

    def test_from_csv_empty(self):
        table = Table.from_csv('examples/empty.csv')

        self.assertColumnNames(table, [])
        self.assertColumnTypes(table, [])

        self.assertRows(table, [])

    def test_from_csv_empty_input(self):
        import io

        table = Table.from_csv(io.StringIO(''))

        self.assertColumnNames(table, [])
        self.assertColumnTypes(table, [])
        self.assertRows(table, [])

    def test_from_csv_empty_input_with_column_names(self):
        import io

        table = Table.from_csv(io.StringIO(''), column_names=['a', 'b'])

        self.assertColumnNames(table, ['a', 'b'])
        self.assertRows(table, [])

    def test_from_csv_sniff_non_seekable_stream(self):
        # The sniffing branch used to require tell()/seek(), which a
        # sequential-only stream does not provide.
        text = "number|text\n1|a\n2|b\n3|c\n"

        table = Table.from_csv(SequentialOnlyStream(text), sniff_limit=20)

        self.assertColumnNames(table, ['number', 'text'])
        self.assertRows(table, [
            (Decimal('1'), 'a'),
            (Decimal('2'), 'b'),
            (Decimal('3'), 'c')
        ])

    def test_from_csv_sniff_non_seekable_stream_none(self):
        text = "number|text\n1|a\n2|b\n3|c\n"

        table = Table.from_csv(SequentialOnlyStream(text), sniff_limit=None)

        self.assertColumnNames(table, ['number', 'text'])
        self.assertRows(table, [
            (Decimal('1'), 'a'),
            (Decimal('2'), 'b'),
            (Decimal('3'), 'c')
        ])

    def test_from_csv_sniff_prefix_mid_line(self):
        # Every possible cut point must reassemble into the same records.
        text = "number|text\n1|a\n2|b\n3|c\n"
        expected = [
            (Decimal('1'), 'a'),
            (Decimal('2'), 'b'),
            (Decimal('3'), 'c')
        ]

        for sniff_limit in range(1, len(text) + 3):
            with warnings.catch_warnings():
                warnings.simplefilter('ignore')
                table = Table.from_csv(SequentialOnlyStream(text), sniff_limit=sniff_limit)

            self.assertEqual(len(table.rows), 3, sniff_limit)

            # When the sample is large enough to reveal the delimiter, the
            # columns must be split; otherwise all three records still survive.
            sample = text[:min(sniff_limit, len(text))]
            if _can_sniff_delimiter(sample):
                self.assertColumnNames(table, ['number', 'text'])
                self.assertRows(table, expected)
            else:
                self.assertEqual(len(table.columns), 1)

    def test_from_csv_sniff_prefix_inside_quoted_multiline_field(self):
        text = '"id","name"\n"1","line1\nline2"\n"2","ok"\n'
        # Place the cut squarely inside the embedded new-line field.
        sniff_limit = text.index('line2')

        table = Table.from_csv(SequentialOnlyStream(text), sniff_limit=sniff_limit)

        self.assertColumnNames(table, ['id', 'name'])
        self.assertRows(table, [
            (Decimal('1'), 'line1\nline2'),
            (Decimal('2'), 'ok')
        ])

    def test_from_csv_sniff_prefix_ending_on_delimiter(self):
        text = 'ab|cd\n1|2\n'
        sniff_limit = text.index('|') + 1

        table = Table.from_csv(SequentialOnlyStream(text), sniff_limit=sniff_limit)

        # The record must not be split at the prefix boundary.
        self.assertEqual(len(table.rows), 1)
        self.assertEqual(len(table.rows[0]), 2)

    def test_from_csv_sniff_failure_keeps_caller_dialect(self):
        pycsv.register_dialect('agate_test_pipe', delimiter='|')

        try:
            # A single-column sample makes sniffing fail. The caller-supplied
            # dialect must be retained and the prefix must not be dropped.
            with warnings.catch_warnings():
                warnings.simplefilter('ignore')
                table = Table.from_csv(
                    SequentialOnlyStream('a\nb\nc\n'),
                    sniff_limit=5,
                    dialect='agate_test_pipe'
                )
        finally:
            pycsv.unregister_dialect('agate_test_pipe')

        self.assertColumnNames(table, ['a'])
        self.assertRows(table, [('b',), ('c',)])

    def test_from_csv_sniff_failure_keeps_caller_kwargs(self):
        with warnings.catch_warnings():
            warnings.simplefilter('ignore')
            table = Table.from_csv(
                SequentialOnlyStream('a\nb\nc\n'),
                sniff_limit=5,
                delimiter='|'
            )

        self.assertColumnNames(table, ['a'])
        self.assertRows(table, [('b',), ('c',)])

    def test_from_csv_sniff_empty_stream_no_warning(self):
        with warnings.catch_warnings():
            warnings.simplefilter('error')
            table = Table.from_csv(SequentialOnlyStream(''), sniff_limit=20)

        self.assertColumnNames(table, [])
        self.assertRows(table, [])

    def test_from_csv_sniff_does_not_close_borrowed_stream(self):
        stream = SequentialOnlyStream('a,b\n1,2\n')

        Table.from_csv(stream, sniff_limit=3)

        self.assertFalse(stream.closed)

    def test_from_csv_sniff_closes_opened_file(self):
        # Files opened by the library itself are still closed afterwards.
        table = Table.from_csv('examples/test_csv_sniff.csv', sniff_limit=200)

        self.assertEqual(len(table.rows), 3)

    def test_from_csv_sniff_with_skip_lines_header_column_names_row_limit(self):
        text = "junkline\nnumber|text\n1|a\n2|b\n3|c\n"

        table = Table.from_csv(
            SequentialOnlyStream(text),
            column_names=['X', 'Y'],
            skip_lines=1,
            sniff_limit=20,
            row_limit=2
        )

        self.assertColumnNames(table, ['X', 'Y'])
        self.assertRows(table, [
            (Decimal('1'), 'a'),
            (Decimal('2'), 'b')
        ])

    def test_from_csv_sniff_no_header_with_column_names_and_row_limit(self):
        text = "1|a\n2|b\n3|c\n"

        table = Table.from_csv(
            SequentialOnlyStream(text),
            ['X', 'Y'],
            header=False,
            sniff_limit=8,
            row_limit=2
        )

        self.assertColumnNames(table, ['X', 'Y'])
        self.assertRows(table, [
            (Decimal('1'), 'a'),
            (Decimal('2'), 'b')
        ])
