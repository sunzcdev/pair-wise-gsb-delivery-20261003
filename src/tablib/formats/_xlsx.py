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

INVALID_TITLE_REGEX = re.compile(r'[\\*?:/\[\]]')

#: Formula handling policies available on export.
#:
#: ``FORMULA_AUTO``
#:     Legacy behaviour: any string starting with ``=`` is stored as a
#:     formula (this is openpyxl's default).  This is the fallback policy
#:     used for columns without an explicit policy.
#:
#: ``FORMULA_TEXT``
#:     Strings are always stored as literal text, even when they start with
#:     ``=``.  The cell text is preserved verbatim (no character is stripped).
#:
#: ``FORMULA_FORMULA``
#:     Strings starting with ``=`` are explicitly stored as formulae.
#:     Useful to opt a single column back into formula handling when the
#:     default policy is ``FORMULA_TEXT``.
FORMULA_AUTO = 'auto'
FORMULA_TEXT = 'text'
FORMULA_FORMULA = 'formula'

_FORMULA_POLICIES = (FORMULA_AUTO, FORMULA_TEXT, FORMULA_FORMULA)

#: Suggested marker for formula cells that have no cached value on import.
#: Pass it as ``missing_formula_cache`` to ``import_set``/``import_book``.
FORMULA_CACHE_MISSING = '#TABLIB_FORMULA_CACHE_MISSING#'


def safe_xlsx_sheet_title(s, replace="-"):
    return re.sub(INVALID_TITLE_REGEX, replace, s)[:31]


def _resolve_formula_policies(formula_policy, width, headers):
    """Normalizes ``formula_policy`` into a per-column list of policies.

    ``formula_policy`` may be:

    - ``None``: every column uses the legacy ``FORMULA_AUTO`` policy.
    - one of the ``FORMULA_*`` strings: applied to every column.
    - a sequence of ``FORMULA_*`` strings: applied by column position.
    - a mapping of column index (int) or header name (str) to a
      ``FORMULA_*`` string.  The special key ``'*'`` sets the default
      policy for columns not listed explicitly.
    """
    default = FORMULA_AUTO
    per_column = {}

    if formula_policy is None:
        pass
    elif isinstance(formula_policy, str):
        if formula_policy not in _FORMULA_POLICIES:
            raise ValueError(
                f"Invalid formula policy: {formula_policy!r}. "
                f"Must be one of {_FORMULA_POLICIES}."
            )
        default = formula_policy
    elif isinstance(formula_policy, dict):
        for key, policy in formula_policy.items():
            if policy not in _FORMULA_POLICIES:
                raise ValueError(
                    f"Invalid formula policy for column {key!r}: {policy!r}. "
                    f"Must be one of {_FORMULA_POLICIES}."
                )
            if key == '*':
                default = policy
            else:
                per_column[key] = policy
    elif isinstance(formula_policy, (list, tuple)):
        for index, policy in enumerate(formula_policy):
            if policy not in _FORMULA_POLICIES:
                raise ValueError(
                    f"Invalid formula policy for column {index}: {policy!r}. "
                    f"Must be one of {_FORMULA_POLICIES}."
                )
            per_column[index] = policy
    else:
        raise TypeError(
            "formula_policy must be None, a string, a sequence or a mapping, "
            f"got {type(formula_policy).__name__}."
        )

    policies = []
    for j in range(width):
        policy = per_column.get(j)
        if policy is None and headers:
            policy = per_column.get(headers[j])
        policies.append(policy if policy is not None else default)
    return policies


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
                   escape=False, column_width="adaptive", formula_policy=None):
        """Returns XLSX representation of Dataset.

        If ``freeze_panes`` is True, Export will freeze panes only after first line.

        If ``dataset.title`` contains characters which are
        considered invalid for an XLSX file sheet name
        (https://web.archive.org/web/20230323081941/https://www.excelcodex.com/2012/06/worksheets-naming-conventions/),
        they will be replaced with ``invalid_char_subst``.

        If ``escape`` is True, formulae will have the leading '=' character removed.
        This is a security measure to prevent formulae from executing by default
        in exported XLSX files.

        ``formula_policy`` controls how strings starting with '=' are stored.
        It accepts one of the ``FORMULA_*`` constants (applied to every
        column), a sequence of constants (applied by column position) or a
        mapping of column index / header name to a constant, using the
        special key ``'*'`` as the default for unlisted columns.  Use
        ``FORMULA_TEXT`` to store strings verbatim as literal text and
        ``FORMULA_FORMULA`` to store '='-prefixed strings as formulae.
        When given, ``formula_policy`` takes precedence over ``escape`` for
        the columns it covers.  Numbers, dates and other non-string values
        are always written with their native type, regardless of the policy.

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

        cls.dset_sheet(dataset, ws, freeze_panes=freeze_panes, escape=escape,
                       formula_policy=formula_policy)

        cls._adapt_column_width(ws, column_width)

        stream = BytesIO()
        wb.save(stream)
        return stream.getvalue()

    @classmethod
    def export_book(cls, databook, freeze_panes=True, invalid_char_subst="-",
                    escape=False, column_width=None, formula_policy=None):
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

            cls.dset_sheet(dset, ws, freeze_panes=freeze_panes, escape=escape,
                           formula_policy=formula_policy)

            cls._adapt_column_width(ws, column_width)

        stream = BytesIO()
        wb.save(stream)
        return stream.getvalue()

    @classmethod
    def import_sheet(cls, dset, sheet, headers=True, skip_lines=0,
                     formula_cells=None, missing_formula_cache=None):
        """Populates dataset with sheet.

        If ``formula_cells`` is given, it must be a set of ``(row, column)``
        coordinates (1-based) of the cells that contain a formula.  Cells in
        this set whose cached value is missing (read as ``None``) are
        replaced by ``missing_formula_cache``.
        """

        dset.title = sheet.title

        for i, row in enumerate(sheet.rows):
            if i < skip_lines:
                continue
            row_vals = []
            for c in row:
                val = c.value
                if (val is None and formula_cells
                        and (c.row, c.column) in formula_cells):
                    val = missing_formula_cache
                row_vals.append(val)
            if i == skip_lines and headers:
                dset.headers = row_vals
            else:
                if i > skip_lines and len(row_vals) < dset.width:
                    row_vals += [''] * (dset.width - len(row_vals))
                dset.append(row_vals)

    @classmethod
    def import_set(cls, dset, in_stream, headers=True, read_only=True,
                   skip_lines=0, formulas=False, missing_formula_cache=None):
        """Returns databook from XLS stream.

        If ``formulas`` is True, formula cells are imported as their formula
        text (e.g. ``'=SUM(A1:B1)'``).  Otherwise (the default) the cached
        value computed by the spreadsheet application is imported.  Tablib
        never evaluates formulae itself.

        When reading cached values (``formulas=False``), formula cells whose
        cached value does not exist are imported as ``None`` by default.
        Pass ``missing_formula_cache`` (e.g. the ``FORMULA_CACHE_MISSING``
        constant) to have them replaced by that marker instead.
        """

        dset.wipe()

        formula_cells = None
        if not formulas and missing_formula_cache is not None:
            in_stream, formula_cells = cls._locate_formula_cells(
                in_stream, read_only)

        xls_book = load_workbook(in_stream, read_only=read_only,
                                 data_only=not formulas)
        sheet = xls_book.active
        cls.import_sheet(dset, sheet, headers, skip_lines,
                         formula_cells=formula_cells,
                         missing_formula_cache=missing_formula_cache)

    @classmethod
    def import_book(cls, dbook, in_stream, headers=True, read_only=True,
                    formulas=False, missing_formula_cache=None):
        """Returns databook from XLS stream.
        See import_set().
        """

        dbook.wipe()

        formula_cells_by_sheet = None
        if not formulas and missing_formula_cache is not None:
            in_stream, formula_cells_by_sheet = cls._locate_formula_cells(
                in_stream, read_only, all_sheets=True)

        xls_book = load_workbook(in_stream, read_only=read_only,
                                 data_only=not formulas)

        for i, sheet in enumerate(xls_book.worksheets):
            dset = tablib.Dataset()
            formula_cells = (
                formula_cells_by_sheet[i]
                if formula_cells_by_sheet is not None else None
            )
            cls.import_sheet(dset, sheet, headers,
                             formula_cells=formula_cells,
                             missing_formula_cache=missing_formula_cache)
            dbook.add_sheet(dset)

    @classmethod
    def _locate_formula_cells(cls, in_stream, read_only, all_sheets=False):
        """Returns a seekable stream plus the coordinates of formula cells.

        The stream is materialized into a ``BytesIO`` when necessary so it
        can be read a second time by the caller.  Coordinates are ``(row,
        column)`` tuples, 1-based, matching ``cell.row``/``cell.column``.
        With ``all_sheets``, a list of coordinate sets (one per sheet) is
        returned instead of a single set.
        """
        if hasattr(in_stream, 'read'):
            in_stream = BytesIO(in_stream.read())

        book = load_workbook(in_stream, read_only=read_only, data_only=False)
        located = [
            {(c.row, c.column) for row in sheet.rows for c in row
             if c.data_type == 'f'}
            for sheet in book.worksheets
        ]
        book.close()

        if hasattr(in_stream, 'seek'):
            in_stream.seek(0)

        return in_stream, (located if all_sheets else located[0])

    @classmethod
    def dset_sheet(cls, dataset, ws, freeze_panes=True, escape=False,
                   formula_policy=None):
        """Completes given worksheet from given Dataset.

        See export_set() for the meaning of ``escape`` and
        ``formula_policy``.
        """
        _package = dataset._package(dicts=False)

        for i, sep in enumerate(dataset._separators):
            _offset = i
            _package.insert((sep[0] + _offset), (sep[1],))

        policies = _resolve_formula_policies(
            formula_policy, dataset.width, dataset.headers)

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

                policy = policies[j] if j < len(policies) else FORMULA_AUTO
                if policy == FORMULA_TEXT:
                    # store strings verbatim, even when they start with '='
                    if cell.data_type == 'f':
                        cell.data_type = 's'
                elif escape and policy == FORMULA_AUTO \
                        and cell.data_type == 'f' \
                        and cell.value.startswith('='):
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
