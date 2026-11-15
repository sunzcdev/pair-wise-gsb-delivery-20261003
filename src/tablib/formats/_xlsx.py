""" Tablib - XLSX Support.
"""

__lazy_modules__ = {
    "io",
    "openpyxl",
    "openpyxl.reader",
    "openpyxl.reader.excel",
    "openpyxl.styles",
    "openpyxl.utils",
    "openpyxl.workbook",
}

import re
from io import BytesIO

from openpyxl.reader.excel import ExcelReader, load_workbook
from openpyxl.styles import Alignment, Font
from openpyxl.utils import get_column_letter
from openpyxl.workbook import Workbook

import tablib
from tablib.exceptions import ImportBoundExceeded

INVALID_TITLE_REGEX = re.compile(r'[\\*?:/\[\]]')


def safe_xlsx_sheet_title(s, replace="-"):
    return re.sub(INVALID_TITLE_REGEX, replace, s)[:31]


def _validate_bound(value, name):
    """Validate an optional import boundary.

    ``None`` means "no limit"; otherwise the value must be a positive
    integer (booleans are rejected even though they are ``int`` subclasses).
    """
    if value is None:
        return None
    if isinstance(value, bool) or not isinstance(value, int) or value < 1:
        raise ValueError(
            f"{name} must be a positive integer or None, got {value!r}"
        )
    return value


def _trim_row(row_vals):
    """Drop trailing empty cells.

    openpyxl pads rows up to the declared (sometimes forged) sheet
    dimension; the trailing padding carries no data and must not drive the
    dataset width or force a huge allocation.
    """
    trimmed = list(row_vals)
    while trimmed and trimmed[-1] is None:
        trimmed.pop()
    return trimmed


def _select_sheet(workbook, sheet):
    """Return the worksheet selected by name (str) or zero-based index (int)."""
    if isinstance(sheet, str):
        try:
            return workbook[sheet]
        except KeyError:
            available = ", ".join(workbook.sheetnames)
            raise ValueError(
                f"Worksheet {sheet!r} not found. Available worksheets: {available}"
            ) from None
    if isinstance(sheet, int) and not isinstance(sheet, bool):
        try:
            return workbook.worksheets[sheet]
        except IndexError:
            raise ValueError(
                f"Worksheet index {sheet} is out of range; the workbook "
                f"has {len(workbook.worksheets)} worksheet(s)."
            ) from None
    raise TypeError(
        "sheet must be a worksheet name (str), a zero-based index (int), "
        f"or None for the active worksheet, got {type(sheet).__name__}"
    )


class XLSXFormat:
    title = 'xlsx'
    extensions = ('xlsx',)

    @classmethod
    def detect(cls, stream):
        """Returns True if given stream is a readable excel file."""
        try:
            # No need to fully load the file, it should be enough to be able to
            # read the manifest.
            reader = ExcelReader(stream, read_only=False)
            reader.read_manifest()
            return True
        except Exception:
            return False

    @classmethod
    def export_set(cls, dataset, freeze_panes=True, invalid_char_subst="-",
                   escape=False, column_width="adaptive"):
        """Returns XLSX representation of Dataset.

        If ``freeze_panes`` is True, Export will freeze panes only after first line.

        If ``dataset.title`` contains characters which are
        considered invalid for an XLSX file sheet name
        (https://web.archive.org/web/20230323081941/https://www.excelcodex.com/2012/06/worksheets-naming-conventions/),
        they will be replaced with ``invalid_char_subst``.

        If ``escape`` is True, formulae will have the leading '=' character removed.
        This is a security measure to prevent formulae from executing by default
        in exported XLSX files.

        If ``column_width`` is set to "adaptive", the column width will be set to the maximum
        width of the content in each column. If it is set to an integer, the column width will be
        set to that integer value. If it is set to None, the column width will be set as the
        default openpyxl.Worksheet width value.

        """
        wb = Workbook()
        ws = wb.worksheets[0]

        ws.title = (
            safe_xlsx_sheet_title(dataset.title, invalid_char_subst)
            if dataset.title else 'Tablib Dataset'
        )

        cls.dset_sheet(dataset, ws, freeze_panes=freeze_panes, escape=escape)

        cls._adapt_column_width(ws, column_width)

        stream = BytesIO()
        wb.save(stream)
        return stream.getvalue()

    @classmethod
    def export_book(cls, databook, freeze_panes=True, invalid_char_subst="-",
                    escape=False, column_width=None):
        """Returns XLSX representation of DataBook.
        See export_set().
        """

        wb = Workbook()
        for sheet in wb.worksheets:
            wb.remove(sheet)
        for i, dset in enumerate(databook._datasets):
            ws = wb.create_sheet()
            ws.title = (
                safe_xlsx_sheet_title(dset.title, invalid_char_subst)
                if dset.title else f"Sheet{i}"
            )

            cls.dset_sheet(dset, ws, freeze_panes=freeze_panes, escape=escape)

            cls._adapt_column_width(ws, column_width)

        stream = BytesIO()
        wb.save(stream)
        return stream.getvalue()

    @classmethod
    def import_sheet(cls, dset, sheet, headers=True, skip_lines=0,
                     max_rows=None, max_columns=None):
        """Populates dataset with sheet.

        :param max_rows: maximum number of data rows (the header row, when
            ``headers`` is True, is not counted) that may be read.
        :param max_columns: maximum number of columns that may be read.

        Rows and columns past the respective limits are never silently
        dropped: :class:`tablib.ImportBoundExceeded` is raised as soon as
        out-of-bound data is encountered.

        Only the actual data region of the worksheet is read. Rows whose
        cells are all empty terminate the scan (matching the ODS importer),
        which also protects against worksheets that declare a huge dimension
        or contain millions of empty trailing rows.
        """
        max_rows = _validate_bound(max_rows, "max_rows")
        max_columns = _validate_bound(max_columns, "max_columns")

        dset.title = sheet.title

        # In read-only mode openpyxl trusts the (possibly bogus) dimension
        # declared by the file and materialises a row of that width for every
        # declared row.  Resetting it makes iteration follow the cells that
        # are actually present in the sheet XML instead.
        if getattr(sheet, "reset_dimensions", None) is not None:
            sheet.reset_dimensions()

        data_rows_read = 0
        header_seen = not headers
        for i, row in enumerate(sheet.iter_rows(values_only=True)):
            if i < skip_lines:
                continue

            row_vals = _trim_row(row)

            # A row without any value marks the end of the populated region.
            # Stopping here avoids iterating over potentially millions of
            # empty trailing rows (and never pre-allocates for them).
            if not row_vals:
                break

            if max_columns is not None and len(row_vals) > max_columns:
                raise ImportBoundExceeded(
                    f"Worksheet {sheet.title!r} contains {len(row_vals)} "
                    f"column(s), exceeding the max_columns limit of "
                    f"{max_columns}"
                )

            if not header_seen:
                dset.headers = row_vals
                header_seen = True
                continue

            if dset.width and len(row_vals) < dset.width:
                row_vals += [''] * (dset.width - len(row_vals))

            data_rows_read += 1
            if max_rows is not None and data_rows_read > max_rows:
                raise ImportBoundExceeded(
                    f"Worksheet {sheet.title!r} contains more than "
                    f"{max_rows} data row(s), exceeding the max_rows limit"
                )
            dset.append(row_vals)

    @classmethod
    def import_set(cls, dset, in_stream, headers=True, read_only=True,
                   skip_lines=0, sheet=None, max_rows=None, max_columns=None):
        """Populates ``dset`` from an XLSX stream.

        :param sheet: name (str) or zero-based index (int) of the worksheet
            to import.  ``None`` (the default) imports the active worksheet.
        :param max_rows: see :meth:`import_sheet`.
        :param max_columns: see :meth:`import_sheet`.
        """

        dset.wipe()

        max_rows = _validate_bound(max_rows, "max_rows")
        max_columns = _validate_bound(max_columns, "max_columns")

        # Take ownership of the bytes we read: the ZipFile opened by openpyxl
        # is closed in ``finally``, while the caller's stream stays open and
        # positioned wherever it was.
        workbook = load_workbook(
            BytesIO(in_stream.read()),
            read_only=read_only,
            data_only=True,
        )
        try:
            worksheet = workbook.active if sheet is None else _select_sheet(workbook, sheet)
            cls.import_sheet(
                dset,
                worksheet,
                headers=headers,
                skip_lines=skip_lines,
                max_rows=max_rows,
                max_columns=max_columns,
            )
        except Exception:
            # Never leave a partially imported dataset behind: an out-of-bound
            # worksheet aborts the import explicitly rather than truncating.
            dset.wipe()
            raise
        finally:
            workbook.close()

    @classmethod
    def import_book(cls, dbook, in_stream, headers=True, read_only=True,
                    max_rows=None, max_columns=None):
        """Populates ``dbook`` from an XLSX stream.

        Every worksheet is imported as a separate dataset. The row and
        column limits apply per worksheet.
        """

        dbook.wipe()

        max_rows = _validate_bound(max_rows, "max_rows")
        max_columns = _validate_bound(max_columns, "max_columns")

        workbook = load_workbook(
            BytesIO(in_stream.read()),
            read_only=read_only,
            data_only=True,
        )
        try:
            for worksheet in workbook.worksheets:
                dset = tablib.Dataset()
                cls.import_sheet(
                    dset,
                    worksheet,
                    headers=headers,
                    max_rows=max_rows,
                    max_columns=max_columns,
                )
                dbook.add_sheet(dset)
        except Exception:
            # A bound violation in any sheet aborts the whole book import.
            dbook.wipe()
            raise
        finally:
            workbook.close()

    @classmethod
    def dset_sheet(cls, dataset, ws, freeze_panes=True, escape=False):
        """Completes given worksheet from given Dataset."""
        _package = dataset._package(dicts=False)

        for i, sep in enumerate(dataset._separators):
            _offset = i
            _package.insert((sep[0] + _offset), (sep[1],))

        bold = Font(bold=True)
        wrap_text = Alignment(wrap_text=True)

        for i, row in enumerate(_package):
            row_number = i + 1
            for j, col in enumerate(row):
                col_idx = get_column_letter(j + 1)
                cell = ws[f'{col_idx}{row_number}']

                # bold headers
                if (row_number == 1) and dataset.headers:
                    cell.font = bold
                    if freeze_panes:
                        #  Export Freeze only after first Line
                        ws.freeze_panes = 'A2'

                # bold separators
                elif len(row) < dataset.width:
                    cell.font = bold

                # wrap the rest
                else:
                    if '\n' in str(col):
                        cell.alignment = wrap_text

                try:
                    cell.value = col
                except ValueError:
                    cell.value = str(col)

                if escape and cell.data_type == 'f' and cell.value.startswith('='):
                    cell.value = cell.value[1:]

    @classmethod
    def _adapt_column_width(cls, worksheet, width):
        if isinstance(width, str) and width != "adaptive":
            msg = (
                f"Invalid value for column_width: {width}. "
                "Must be 'adaptive' or an integer."
            )
            raise ValueError(msg)

        if width is None:
            return

        column_widths = []
        if width == "adaptive":
            for row in worksheet.values:
                for i, cell in enumerate(row):
                    cell_width = len(str(cell))
                    if len(column_widths) > i:
                        if cell_width > column_widths[i]:
                            column_widths[i] = cell_width
                    else:
                        column_widths.append(cell_width)
        else:
            column_widths = [width] * worksheet.max_column

        for i, column_width in enumerate(column_widths, 1):  # start at 1
            worksheet.column_dimensions[get_column_letter(i)].width = column_width
