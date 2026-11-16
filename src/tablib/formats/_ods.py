""" Tablib - ODF Support.
"""

__lazy_modules__ = {"datetime", "io", "numbers"}

import datetime as dt
import numbers
from io import BytesIO

from odf import number, opendocument, style, table, text

import tablib
from tablib.exceptions import ODSValidationError

# Explicit budgets for the number of rows/columns a sheet is allowed to
# expand to once ``table:number-rows-repeated`` and
# ``table:number-columns-repeated`` attributes are taken into account.
# ODS files encode large (often empty) areas with very short, highly
# compressed XML, so the limits must be checked against the *expanded*
# counts rather than the number of elements actually present in the file.
# These match the maximum sheet dimensions supported by common
# spreadsheet applications.
MAX_ROWS = 1_048_576
MAX_COLUMNS = 16_384

ROW_TAG = 'table:table-row'
CELL_TAG = 'table:table-cell'
COVERED_CELL_TAG = 'table:covered-table-cell'

bold = style.Style(name="bold", family="paragraph")
bold.addElement(style.TextProperties(
    fontweight="bold",
    fontweightasian="bold",
    fontweightcomplex="bold",
))


def set_date_style(style):
    style.addElement(number.Year(style="long"))
    style.addElement(number.Text(text="-"))
    style.addElement(number.Month(style="long"))
    style.addElement(number.Text(text="-"))
    style.addElement(number.Day(style="long"))


def set_time_style(style):
    style.addElement(number.Hours(style="long"))
    style.addElement(number.Text(text=":"))
    style.addElement(number.Minutes(style="long"))
    style.addElement(number.Text(text=":"))
    style.addElement(number.Seconds(style="long", decimalplaces="0"))


date_style = number.DateStyle(name="date-style1")
set_date_style(date_style)
ds = style.Style(
    name="ds1",
    datastylename="date-style1",
    parentstylename="Default",
    family="table-cell",
)

time_style = number.DateStyle(name="time-style1")
set_time_style(time_style)
ts = style.Style(
    name="ts1",
    datastylename="time-style1",
    parentstylename="Default",
    family="table-cell",
)

datetime_style = number.DateStyle(name="datetime-style1")
set_date_style(datetime_style)
datetime_style.addElement(number.Text(text=" "))
set_time_style(datetime_style)
dts = style.Style(
    name="dts1",
    datastylename="datetime-style1",
    parentstylename="Default",
    family="table-cell",
)


class ODSFormat:
    title = 'ods'
    extensions = ('ods',)

    @classmethod
    def export_set(cls, dataset):
        """Returns ODF representation of Dataset."""

        wb = opendocument.OpenDocumentSpreadsheet()
        wb.automaticstyles.addElement(bold)
        wb.styles.addElement(date_style)
        wb.automaticstyles.addElement(ds)
        wb.styles.addElement(time_style)
        wb.automaticstyles.addElement(ts)
        wb.styles.addElement(datetime_style)
        wb.automaticstyles.addElement(dts)

        ws = table.Table(name=dataset.title if dataset.title else 'Tablib Dataset')
        wb.spreadsheet.addElement(ws)
        cls.dset_sheet(dataset, ws)

        stream = BytesIO()
        wb.save(stream)
        return stream.getvalue()

    @classmethod
    def export_book(cls, databook):
        """Returns ODF representation of DataBook."""

        wb = opendocument.OpenDocumentSpreadsheet()
        wb.automaticstyles.addElement(bold)

        for i, dset in enumerate(databook._datasets):
            ws = table.Table(name=dset.title if dset.title else f"Sheet{i}")
            wb.spreadsheet.addElement(ws)
            cls.dset_sheet(dset, ws)

        stream = BytesIO()
        wb.save(stream)
        return stream.getvalue()

    @staticmethod
    def _repeated(element, attribute, budget):
        """Return the validated value of an ODS repetition attribute.

        ``table:number-rows-repeated`` and
        ``table:number-columns-repeated`` are positive integers according to
        the ODF specification. Documents produced in the wild may carry
        missing, non-numeric, zero or negative values, or values that would
        expand past the configured budget; each of those cases is reported
        with a diagnostic instead of silently producing wrong dimensions or
        triggering an integer overflow / memory exhaustion.
        """
        raw = element.getAttribute(attribute)
        if raw is None:
            return 1
        try:
            repeat = int(raw)
        except (TypeError, ValueError):
            raise ODSValidationError(
                f"Invalid {attribute!r} value {raw!r}: expected a positive integer"
            )
        if repeat < 1:
            raise ODSValidationError(
                f"Invalid {attribute!r} value {raw!r}: expected a positive integer"
            )
        if repeat > budget:
            raise ODSValidationError(
                f"{attribute!r} value {repeat} exceeds the maximum of {budget}"
            )
        return repeat

    @classmethod
    def _row_repeat(cls, row):
        return cls._repeated(row, 'numberrowsrepeated', MAX_ROWS)

    @classmethod
    def _cell_repeat(cls, cell):
        return cls._repeated(cell, 'numbercolumnsrepeated', MAX_COLUMNS)

    @staticmethod
    def _is_blank_tail(cell):
        """Tell a repeated blank tail cell apart from a real data cell.

        An empty cell carrying a ``table:number-columns-repeated`` attribute
        encodes the blank tail of a sheet (often tens of thousands of
        columns) and must never be materialized. A plain empty cell without
        a repetition attribute is genuine data and is read as an empty
        string instead.
        """
        return (
            not cell.hasChildNodes()
            and cell.getAttribute('numbercolumnsrepeated') is not None
        )

    @classmethod
    def _check_dimensions(cls, sheet):
        """Validate repeat counts and expanded dimensions before expansion.

        The whole sheet is inspected up front so that an over-budget or
        malformed document is rejected before any large list is built, no
        matter how short its compressed representation looks.
        """
        total_rows = 0
        for row in sheet.childNodes:
            if row.tagName != ROW_TAG:
                continue
            row_repeat = cls._row_repeat(row)
            total_rows += row_repeat
            if total_rows > MAX_ROWS:
                raise ODSValidationError(
                    f"Sheet expands to {total_rows} rows, exceeding the maximum "
                    f"of {MAX_ROWS}"
                )
            total_columns = 0
            for cell in row.childNodes:
                if cell.tagName == COVERED_CELL_TAG:
                    # Merged cells are represented by a leading cell plus
                    # covered placeholders; tablib has no representation for
                    # them, so fail loudly instead of silently misaligning
                    # the following columns.
                    raise ODSValidationError(
                        "Covered table cells (table:covered-table-cell) from merged "
                        "ranges are not supported"
                    )
                if cell.tagName != CELL_TAG:
                    continue
                cell_repeat = cls._cell_repeat(cell)
                total_columns += cell_repeat
                if total_columns > MAX_COLUMNS:
                    raise ODSValidationError(
                        f"Row expands to {total_columns} columns, exceeding the "
                        f"maximum of {MAX_COLUMNS}"
                    )

    @classmethod
    def _read_row(cls, row):
        """Read one physical row, honoring repeated cells but trimming blank tails."""
        values = []
        for cell in row.childNodes:
            if cell.tagName == COVERED_CELL_TAG:
                raise ODSValidationError(
                    "Covered table cells (table:covered-table-cell) from merged "
                    "ranges are not supported"
                )
            if cell.tagName != CELL_TAG:
                continue
            repeat = cls._cell_repeat(cell)
            if cls._is_blank_tail(cell):
                # Repeated empty cells encode the blank tail of a sheet
                # (see e.g. ragged.ods); do not materialize them.
                continue
            value = cls.read_cell(cell)
            values.extend([value] * repeat)
        return values

    @classmethod
    def _iter_rows(cls, sheet, skip_lines=0):
        """Yield expanded rows, honoring ``table:number-rows-repeated``.

        Every physical row is yielded once per repetition (including blank
        rows); the expansion budget is enforced up front by
        :meth:`_check_dimensions`, so iteration can never run past it.
        Distinguishing the blank sheet tail from the real data range is the
        caller's responsibility.
        """
        row_index = 0
        for row in sheet.childNodes:
            if row.tagName != ROW_TAG:
                continue
            repeat = cls._row_repeat(row)
            values = cls._read_row(row)
            for _ in range(repeat):
                if row_index >= skip_lines:
                    yield values
                row_index += 1

    @classmethod
    def import_sheet(cls, dset, sheet, headers=True, skip_lines=0):
        """Populate dataset `dset` with sheet data."""

        dset.title = sheet.getAttribute('name')

        # Reject malformed/over-budget documents before expanding anything.
        cls._check_dimensions(sheet)

        pending_blanks = 0
        first_row = True
        for row_vals in cls._iter_rows(sheet, skip_lines):
            if not row_vals:
                # Blank rows inside the data range are kept as empty rows,
                # but a trailing run of blank rows (the compressed empty
                # sheet tail) must not be materialized. Hold them back until
                # a non-empty row proves they were interior.
                pending_blanks += 1
                continue
            if first_row:
                # Leading blank rows appear before any real content and have
                # no width to pad to, so they are simply discarded.
                pending_blanks = 0
                if headers:
                    dset.headers = row_vals
                else:
                    dset.append(row_vals)
                first_row = False
                continue
            for _ in range(pending_blanks):
                dset.append([''] * dset.width)
            pending_blanks = 0
            if len(row_vals) < dset.width:
                row_vals += [''] * (dset.width - len(row_vals))
            dset.append(row_vals)

    @classmethod
    def read_cell(cls, cell, value_type=None):
        def convert_date(val):
            if 'T' in val:
                return dt.datetime.strptime(val, "%Y-%m-%dT%H:%M:%S")
            else:
                return dt.datetime.strptime(val, "%Y-%m-%d").date()

        if value_type is None:
            value_type = cell.getAttribute('valuetype')
        if value_type == 'date':
            date_value = cell.getAttribute('datevalue')
            if date_value:
                return convert_date(date_value)
        if value_type == 'time':
            time_value = cell.getAttribute('timevalue')
            try:
                return dt.datetime.strptime(time_value, "PT%HH%MM%SS").time()
            except ValueError:
                # backwards compatibility for times exported with older tablib versions
                return dt.datetime.strptime(time_value, "%H:%M:%S").time()
        if value_type == 'boolean':
            bool_value = cell.getAttribute('booleanvalue')
            return bool_value == 'true'
        if not cell.childNodes:
            value = getattr(cell, 'data', None)
            if value is None:
                try:
                    value = cell.getAttribute('value')
                except ValueError:
                    pass
            if value is None:
                return ''
            if value_type == 'float':
                return float(value)
            if value_type == 'date':
                return convert_date(value)
            return value  # Any other type default to 'string'

        for subnode in cell.childNodes:
            return cls.read_cell(subnode, value_type)

    @classmethod
    def import_set(cls, dset, in_stream, headers=True, skip_lines=0):
        """Populate dataset `dset` from ODS stream."""

        ods_book = opendocument.load(in_stream)

        # Parse into a fresh dataset first and only replace the existing
        # contents once the whole document has been parsed successfully, so
        # a failure cannot leave behind a half-imported (or wiped) dataset.
        new_dset = tablib.Dataset()
        for sheet in ods_book.spreadsheet.childNodes:
            if sheet.qname[1] == 'table':
                cls.import_sheet(new_dset, sheet, headers, skip_lines)

        dset.wipe()
        dset.title = new_dset.title
        dset.headers = new_dset.headers
        for row in new_dset._data:
            dset.append(row.tuple)

    @classmethod
    def import_book(cls, dbook, in_stream, headers=True):
        """Populate databook `dbook` from ODS stream."""

        ods_book = opendocument.load(in_stream)

        # Build the new sheets first; the existing databook is only replaced
        # after every sheet has been parsed successfully.
        new_sheets = []
        for sheet in ods_book.spreadsheet.childNodes:
            if sheet.qname[1] != 'table':
                continue
            dset = tablib.Dataset()
            cls.import_sheet(dset, sheet, headers)
            new_sheets.append(dset)

        dbook.wipe()
        for dset in new_sheets:
            dbook.add_sheet(dset)

    @classmethod
    def dset_sheet(cls, dataset, ws):
        """Completes given worksheet from given Dataset."""
        _package = dataset._package(dicts=False)

        for i, sep in enumerate(dataset._separators):
            _offset = i
            _package.insert((sep[0] + _offset), (sep[1],))

        for row_number, row in enumerate(_package, start=1):
            is_header = row_number == 1 and dataset.headers
            style = bold if is_header else None
            odf_row = table.TableRow(stylename=style)
            ws.addElement(odf_row)
            for j, col in enumerate(row):
                if isinstance(col, bool):
                    cell = table.TableCell(
                        valuetype="boolean",
                        booleanvalue="true" if col else "false",
                    )
                elif isinstance(col, numbers.Number):
                    cell = table.TableCell(valuetype="float", value=col)
                elif isinstance(col, dt.datetime):
                    cell = table.TableCell(
                        valuetype="date",
                        datevalue=col.strftime('%Y-%m-%dT%H:%M:%S'),
                        stylename=dts,
                    )
                    cell.addElement(text.P(text=col.strftime('%Y-%m-%d %H:%M:%S')))
                elif isinstance(col, dt.date):
                    date_value = col.strftime('%Y-%m-%d')
                    cell = table.TableCell(valuetype="date", datevalue=date_value, stylename=ds)
                    cell.addElement(text.P(text=date_value))
                elif isinstance(col, dt.time):
                    cell = table.TableCell(
                        valuetype="time",
                        timevalue=col.strftime('PT%HH%MM%SS'),
                        stylename=ts,
                    )
                    cell.addElement(text.P(text=col.strftime('%H:%M:%S')))
                elif col is None:
                    cell = table.TableCell(valuetype="void")
                else:
                    cell = table.TableCell(valuetype="string")
                    cell.addElement(text.P(text=str(col), stylename=style))
                odf_row.addElement(cell)

    @classmethod
    def detect(cls, stream):
        if isinstance(stream, bytes):
            # load expects a file-like object.
            stream = BytesIO(stream)
        try:
            opendocument.load(stream)
            return True
        except Exception:
            return False
