""" Tablib - XLSX Support.
"""

__lazy_modules__ = {
    "datetime",
    "io",
    "openpyxl",
    "openpyxl.reader",
    "openpyxl.reader.excel",
    "openpyxl.styles",
    "openpyxl.styles.numbers",
    "openpyxl.utils",
    "openpyxl.utils.cell",
    "openpyxl.utils.datetime",
    "openpyxl.workbook",
    "posixpath",
    "tablib",
    "tablib.exceptions",
    "xml.etree",
    "xml.etree.ElementTree",
    "zipfile",
}

import datetime as dt
import posixpath
import re
import zipfile
from io import BytesIO
from xml.etree import ElementTree

from openpyxl.reader.excel import ExcelReader, load_workbook
from openpyxl.styles import Alignment, Font
from openpyxl.styles.numbers import (
    BUILTIN_FORMATS,
    is_date_format,
    is_datetime,
    is_timedelta_format,
)
from openpyxl.utils import get_column_letter
from openpyxl.utils.cell import coordinate_to_tuple
from openpyxl.utils.datetime import MAC_EPOCH, WINDOWS_EPOCH, from_excel
from openpyxl.workbook import Workbook

import tablib
from tablib.exceptions import InvalidDateSerial

INVALID_TITLE_REGEX = re.compile(r'[\\*?:/\[\]]')

# ---------------------------------------------------------------------------
# Excel date systems
#
# Date and time cells are stored as serial numbers relative to a workbook-wide
# date system: the "1900" system (serial 1 is 1900-01-01, serial 60 is the
# fictitious 1900-02-29) or the "1904" system (serial 0 is 1904-01-01).  The
# same date therefore has different serial numbers in each system, so the date
# system of an imported workbook is recorded and reused on export.
# ---------------------------------------------------------------------------

DATE_SYSTEM_1900 = 1900
DATE_SYSTEM_1904 = 1904

#: Serial value of the fictitious 1900-02-29 in the 1900 date system.
FICTITIOUS_LEAP_DAY_SERIAL = 60

#: Supported values of the ``date_serial_policy`` import parameter.
DATE_SERIAL_POLICIES = ('reject', 'keep_serial')

#: Date kinds, i.e. provenance kinds derived from the cell number format.
DATE_KINDS = ('date', 'datetime', 'time', 'timedelta')

_EPOCH_DATE_SYSTEMS = {
    WINDOWS_EPOCH: DATE_SYSTEM_1900,
    MAC_EPOCH: DATE_SYSTEM_1904,
}

#: Namespace of the ``r:id`` attribute relating worksheets to their parts.
_DOCUMENT_RELATIONSHIP_NS = (
    'http://schemas.openxmlformats.org/officeDocument/2006/relationships'
)


def safe_xlsx_sheet_title(s, replace="-"):
    return re.sub(INVALID_TITLE_REGEX, replace, s)[:31]


def _date_system_from_epoch(epoch):
    """Return the date system year (1900 or 1904) of an openpyxl epoch."""
    return _EPOCH_DATE_SYSTEMS.get(epoch)


def _cell_position(cell, row_index, column_index):
    """Return the 1-based ``(row, column)`` of *cell* in its worksheet.

    Both openpyxl worksheet flavours iterate from row 1 and column 1, filling
    the leading cells of the used range with empty ones.  Empty cells of a
    lazily read worksheet carry no coordinates of their own, so their position
    inside the iterated range is used instead.
    """
    try:
        row = cell.row
        column = cell.column
    except (AttributeError, IndexError, KeyError, TypeError, ValueError):
        row = column = None
    if row is None or column is None:
        return row_index + 1, column_index + 1
    return row, column


def _cell_coordinate(cell, position):
    """Return the cell reference used in error messages."""
    try:
        coordinate = cell.coordinate
    except (AttributeError, IndexError, KeyError, TypeError, ValueError):
        coordinate = None
    if coordinate:
        return coordinate
    return '{}{}'.format(*position)


def _time_fraction(value):
    """Return the fraction of a day held by the time part of *value*."""
    seconds = (
        value.hour * 3600
        + value.minute * 60
        + value.second
        + value.microsecond / 1_000_000
    )
    return seconds / 86400


def _serial_from_value(value, epoch):
    """Return the Excel serial number of a Python date/time *value*.

    This mirrors openpyxl's own conversion and is only used when the raw
    serial cannot be read back from the sheet XML, for example for ISO 8601
    date literals or for files whose package could not be scanned.  The
    rebasing for the 1900 system keeps a genuine 1900-02-28 from being
    reported as the fictitious 1900-02-29.
    """
    if epoch is None:
        return None
    if isinstance(value, dt.time):
        if value.tzinfo is not None:
            return None
        return _time_fraction(value)
    if isinstance(value, dt.timedelta):
        return value.total_seconds() / 86400
    if isinstance(value, (dt.datetime, dt.date)):
        # A timezone-aware value cannot be expressed as a naive Excel serial;
        # the wall-clock value is left untouched rather than shifted by the
        # local timezone.
        if getattr(value, 'tzinfo', None) is not None:
            return None
        if isinstance(value, dt.datetime):
            days = (value - epoch).days
            fraction = _time_fraction(value)
        else:
            days = (dt.datetime.combine(value, dt.time()) - epoch).days
            fraction = 0
    else:
        return None
    if epoch == WINDOWS_EPOCH and 0 < days <= FICTITIOUS_LEAP_DAY_SERIAL:
        days -= 1
    return days + fraction


def _serial_number(serial):
    """Return *serial* as an ``int`` when integral, else as a ``float``."""
    return int(serial) if float(serial).is_integer() else serial


def _number_format_kind(cell):
    """Return the date kind of *cell* according to its number format.

    Returns ``'date'``, ``'datetime'``, ``'time'``, ``'timedelta'`` or ``None``
    for cells that are not date-formatted.  Classification never looks at the
    value, so a plain number that happens to look like a date serial is not
    treated as a date.  A package whose stylesheet is broken or missing can
    still be read, just without any date formatting.
    """
    try:
        number_format = getattr(cell, 'number_format', None)
    except (IndexError, KeyError, TypeError, ValueError):
        return None
    if not number_format:
        return None
    if is_timedelta_format(number_format):
        return 'timedelta'
    return is_datetime(number_format)


def _cell_kind(cell, number_format_kind):
    """Return the kind of value a parsed *cell* holds.

    This describes the imported value, so a date-only Excel cell (whose value
    tablib imports as a ``datetime``) is ``'datetime'``; the Excel number
    format is reported separately as ``format_kind``.  A numeric or error
    placeholder cell that is date-formatted keeps the date kind, so an
    unconvertible date serial is never mistaken for a plain number, and a
    styled but empty cell stays ``'empty'``.
    """
    value = cell.value
    if value is None:
        return 'empty'
    if isinstance(value, bool):
        return 'bool'
    if isinstance(value, dt.datetime):
        return 'datetime'
    if isinstance(value, dt.date):
        return 'date'
    if isinstance(value, dt.time):
        return 'time'
    if isinstance(value, dt.timedelta):
        return 'timedelta'
    if isinstance(value, (int, float)):
        return number_format_kind or 'number'
    try:
        data_type = cell.data_type
    except (IndexError, KeyError, TypeError, ValueError):
        data_type = None
    if data_type == 'e':
        # openpyxl stores an unconvertible date serial as an error placeholder.
        return number_format_kind or 'error'
    return 'text'


def _serial_anomaly(serial, number_format_kind, date_system, epoch):
    """Return the anomaly of a date serial, or ``None`` when it is usable.

    The 1900 leap-day gap only concerns cells formatted as a date or a
    datetime; a time-only cell holds a time of day, so its serial is not
    reinterpreted as the fictitious leap day.  Timedelta cells hold durations,
    so a negative value is not anomalous for them either.
    """
    if serial is None or number_format_kind is None:
        return None
    if number_format_kind in ('date', 'datetime'):
        if date_system == DATE_SYSTEM_1900 and (
            FICTITIOUS_LEAP_DAY_SERIAL <= serial < FICTITIOUS_LEAP_DAY_SERIAL + 1
        ):
            return 'fictitious_leap_day'
    if number_format_kind != 'timedelta' and serial < 0:
        return 'negative_serial'
    if epoch is not None:
        try:
            from_excel(serial, epoch, timedelta=number_format_kind == 'timedelta')
        except (OverflowError, ValueError):
            return 'serial_out_of_range'
    return None


def _has_time_fraction(serial, value):
    """Whether a date/time cell carries a fractional (time-of-day) part."""
    if serial is not None:
        return serial % 1 != 0
    if isinstance(value, dt.timedelta):
        return bool(value.seconds or value.microseconds)
    if isinstance(value, (dt.datetime, dt.time)):
        return bool(value.hour or value.minute or value.second or value.microsecond)
    return False


def _cell_source(position, value, sheet_metadata):
    """Return where the imported value of *position* actually came from."""
    if not sheet_metadata:
        return 'empty' if value is None else 'unknown'
    formula_cells = sheet_metadata.get('formula_cells')
    if formula_cells is None:
        return 'empty' if value is None else 'unknown'
    if position in formula_cells:
        return 'formula_cache' if formula_cells[position] else 'formula_cache_missing'
    return 'empty' if value is None else 'literal'


def _check_date_serial_policy(policy):
    if policy not in DATE_SERIAL_POLICIES:
        raise ValueError(
            f'Invalid date_serial_policy: {policy!r}. '
            'Must be one of {}.'.format(', '.join(map(repr, DATE_SERIAL_POLICIES)))
        )


# ---------------------------------------------------------------------------
# Reading the raw XLSX package
#
# openpyxl converts date serials to Python dates while parsing, which loses
# the information needed here: in the 1900 date system the fictitious
# 1900-02-29 (serial 60) and the genuine 1900-02-28 (serial 59) both come out
# as ``datetime(1900, 2, 28)``.  The original serials are recovered by reading
# the worksheet XML directly; nothing is monkeypatched and both the read-only
# and the regular reader use the same metadata.
# ---------------------------------------------------------------------------

def _local_name(tag):
    """Return the local (namespace-less) name of an XML *tag*."""
    if not isinstance(tag, str):
        return None
    return tag.rsplit('}', 1)[-1]


def _read_xml(archive, part):
    """Return the parsed XML of *part*, or ``None`` when it is unavailable."""
    try:
        return ElementTree.fromstring(archive.read(part))
    except (ElementTree.ParseError, KeyError, OSError, ValueError):
        return None


def _part_relationships(archive, part):
    """Return ``(id, type, target)`` triples for internal relationships.

    Targets are resolved against the part's directory so they can be looked
    up in the archive directly.  External relationships are skipped.
    """
    directory = posixpath.dirname(part)
    rels_part = posixpath.join(directory, '_rels', posixpath.basename(part) + '.rels')
    relationships = _read_xml(archive, rels_part)
    if relationships is None:
        return []
    collected = []
    for element in relationships:
        if _local_name(element.tag) != 'Relationship':
            continue
        if element.get('TargetMode') == 'External':
            continue
        identifier = element.get('Id')
        target = element.get('Target')
        if not identifier or not target:
            continue
        if target.startswith('/'):
            resolved = target[1:]
        else:
            resolved = posixpath.normpath(posixpath.join(directory, target))
        collected.append((identifier, element.get('Type') or '', resolved))
    return collected


def _workbook_part(archive, names):
    """Return the name of the workbook part of *archive*, or ``None``."""
    root = _read_xml(archive, '_rels/.rels')
    if root is not None:
        for element in root:
            if _local_name(element.tag) != 'Relationship':
                continue
            if element.get('TargetMode') == 'External':
                continue
            if not (element.get('Type') or '').endswith('/officeDocument'):
                continue
            target = element.get('Target')
            if target:
                part = target[1:] if target.startswith('/') else target
                if part in names:
                    return part
    candidates = sorted(
        name for name in names
        if posixpath.basename(name) == 'workbook.xml'
        and posixpath.dirname(name) in ('', 'xl')
    )
    return candidates[0] if candidates else None


def _relationship_id(element):
    """Return the ``r:id`` attribute of a workbook child element."""
    for key, value in element.attrib.items():
        namespace, _, local_name = key.rpartition('}')
        if local_name != 'id':
            continue
        if namespace.strip('{') in ('', _DOCUMENT_RELATIONSHIP_NS):
            return value
    return None


def _package_parts(archive, names):
    """Locate the worksheet and stylesheet parts of *archive*.

    Returns ``(sheet_parts, styles_part, active_title)``.  ``sheet_parts`` maps
    sheet titles to worksheet part names and ``active_title`` is the title of
    the sheet openpyxl reports as active, or ``None`` when it cannot be
    determined.  A package that does not follow the expected layout yields an
    empty mapping, in which case the import falls back to openpyxl's values.
    """
    workbook_part = _workbook_part(archive, names)
    if workbook_part is None:
        return {}, None, None
    relationships = _part_relationships(archive, workbook_part)
    styles_part = next(
        (part for _id, kind, part in relationships
         if kind.endswith('/styles') and part in names),
        None,
    )
    workbook = _read_xml(archive, workbook_part)
    if workbook is None:
        return {}, styles_part, None

    targets = {
        identifier: part for identifier, _kind, part in relationships if part in names
    }
    sheet_parts = {}
    sheet_titles = []
    active_index = 0
    for element in workbook.iter():
        name = _local_name(element.tag)
        if name == 'sheet':
            part = targets.get(_relationship_id(element))
            title = element.get('name')
            if title is not None:
                sheet_titles.append(title)
                if part is not None:
                    sheet_parts[title] = part
        elif name == 'workbookView':
            try:
                active_index = int(element.get('activeTab') or 0)
            except (TypeError, ValueError):
                active_index = 0

    active_title = None
    if 0 <= active_index < len(sheet_titles):
        active_title = sheet_titles[active_index]
    return sheet_parts, styles_part, active_title


def _date_style_ids(archive, styles_part):
    """Return the cell style indexes whose number format is a date format."""
    if styles_part is None:
        return set()
    stylesheet = _read_xml(archive, styles_part)
    if stylesheet is None:
        return set()

    custom_formats = {}
    for element in stylesheet.iter():
        if _local_name(element.tag) != 'numFmt':
            continue
        try:
            custom_formats[int(element.get('numFmtId'))] = element.get('formatCode')
        except (TypeError, ValueError):
            continue

    date_styles = set()
    for element in stylesheet.iter():
        if _local_name(element.tag) != 'cellXfs':
            continue
        for index, cell_format in enumerate(element):
            try:
                num_format_id = int(cell_format.get('numFmtId') or 0)
            except (TypeError, ValueError):
                continue
            format_code = custom_formats.get(num_format_id)
            if format_code is None:
                format_code = BUILTIN_FORMATS.get(num_format_id)
            if is_date_format(format_code):
                date_styles.add(index)
        break
    return date_styles


def _scan_sheet_cells(archive, part, date_styles, want_formulas):
    """Read raw cell metadata from one worksheet part.

    ``serials`` maps 1-based sheet coordinates to the raw numeric ``<v>`` text
    of date-formatted cells.  ``formula_cells`` (only collected when
    *want_formulas* is true) maps coordinates of formula cells to whether a
    cached value is stored for them; it is ``None`` otherwise.
    """
    serials = {}
    formula_cells = {} if want_formulas else None
    row_number = 0
    column_number = 0
    with archive.open(part) as source:
        # Row numbers are handled on the opening tag, because cells without an
        # explicit reference inherit their row from the enclosing row element.
        for event, element in ElementTree.iterparse(source, events=('start', 'end')):
            name = _local_name(element.tag)
            if event == 'start':
                if name == 'row':
                    reference = element.get('r')
                    if reference:
                        try:
                            row_number = int(reference)
                        except (TypeError, ValueError):
                            row_number += 1
                    else:
                        row_number += 1
                    column_number = 0
                continue
            if name == 'c':
                reference = element.get('r')
                if reference:
                    try:
                        row_number, column_number = coordinate_to_tuple(reference)
                    except ValueError:
                        column_number += 1
                else:
                    column_number += 1
                has_formula = False
                has_value = False
                value_text = None
                for child in element:
                    child_name = _local_name(child.tag)
                    if child_name == 'f':
                        has_formula = True
                    elif child_name in ('v', 'is'):
                        has_value = child.text is not None or len(child) > 0
                        if child_name == 'v':
                            value_text = child.text
                position = (row_number, column_number)
                if formula_cells is not None and has_formula:
                    formula_cells[position] = has_value
                if value_text is not None and element.get('t') in (None, 'n'):
                    try:
                        style_id = int(element.get('s') or 0)
                    except (TypeError, ValueError):
                        style_id = 0
                    if style_id in date_styles:
                        try:
                            serials[position] = float(value_text)
                        except (TypeError, ValueError):
                            pass
                element.clear()
            elif name == 'row':
                element.clear()
    return {'serials': serials, 'formula_cells': formula_cells}


def _scan_workbook_metadata(in_stream, want_provenance, active_only=False):
    """Collect raw cell metadata from an XLSX stream.

    Returns ``(stream, {sheet title: metadata})``.  With *active_only* only the
    workbook's active sheet is scanned, which is the only one an
    ``import_set()`` reads.  The returned stream is rewound so openpyxl can
    parse it; a non-seekable stream is replaced by an in-memory copy.  A
    stream that is not a readable zip archive yields an empty mapping, leaving
    the error reporting to openpyxl.
    """
    try:
        in_stream.seek(0)
    except (AttributeError, OSError, ValueError):
        try:
            in_stream = BytesIO(in_stream.read())
        except (AttributeError, OSError, ValueError):
            return in_stream, {}

    metadata = {}
    try:
        with zipfile.ZipFile(in_stream) as archive:
            names = set(archive.namelist())
            sheet_parts, styles_part, active_title = _package_parts(archive, names)
            if active_only and active_title is not None:
                # ``import_set()`` only reads the active sheet; scanning the
                # others would be wasted work.  Without a known active sheet
                # every sheet is scanned and the caller picks its own.
                sheet_parts = {
                    title: part for title, part in sheet_parts.items()
                    if title == active_title
                }
            date_styles = _date_style_ids(archive, styles_part)
            if date_styles or want_provenance:
                for title, part in sheet_parts.items():
                    try:
                        metadata[title] = _scan_sheet_cells(
                            archive, part, date_styles, want_provenance,
                        )
                    except (ElementTree.ParseError, KeyError, OSError):
                        continue
    except (zipfile.BadZipFile, AttributeError, OSError, ValueError):
        metadata = {}
    finally:
        try:
            in_stream.seek(0)
        except (AttributeError, OSError, ValueError):
            pass
    return in_stream, metadata


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

        If the dataset was imported from a workbook using the 1904 date system
        (``dataset.xlsx_date_epoch == 1904``), the exported workbook uses the same
        date system, so its date serial numbers are not read against the other
        epoch.

        """
        wb = Workbook()
        cls._apply_date_system(wb, dataset)
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

        A workbook has a single date system, so the first sheet recording one
        with ``xlsx_date_epoch`` decides it for the whole book.
        """

        wb = Workbook()
        for dset in databook._datasets:
            if cls._apply_date_system(wb, dset):
                break
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

    @staticmethod
    def _apply_date_system(wb, dataset):
        """Give *wb* the date system recorded on *dataset* when it was imported.

        Returns True when the 1904 date system was applied.  Reusing the
        recorded date system keeps re-exported date serial numbers from being
        read against the other epoch.
        """
        if getattr(dataset, 'xlsx_date_epoch', None) == DATE_SYSTEM_1904:
            wb.epoch = MAC_EPOCH
            return True
        return False

    @staticmethod
    def _close_workbook(workbook):
        close = getattr(workbook, 'close', None)
        if close is not None:
            close()

    @staticmethod
    def _clear_date_metadata(dset):
        """Drop date-system metadata left over from a previous import.

        ``import_sheet`` may be called directly, without the ``wipe()`` that
        the other import entry points perform.
        """
        for attribute in ('xlsx_date_epoch', 'date_provenance'):
            dset.__dict__.pop(attribute, None)

    @classmethod
    def import_sheet(cls, dset, sheet, headers=True, skip_lines=0, *,
                     epoch=None, date_provenance=False,
                     date_serial_policy='reject', sheet_metadata=None):
        """Populates dataset with sheet.

        ``epoch`` is the workbook date-system epoch (openpyxl's
        ``WINDOWS_EPOCH`` or ``MAC_EPOCH``); it is read from the sheet's
        workbook when omitted.  The date system (1900 or 1904) is recorded as
        ``dset.xlsx_date_epoch`` so a later ``export('xlsx')`` writes date
        serial numbers against the same epoch.

        If ``date_provenance`` is true, a ``date_provenance`` mapping is
        attached to the dataset::

            {
                'date_system': 1900,          # or 1904, None if unknown
                'cells': {
                    # (data row, column), 0-based, headers excluded.
                    (0, 0): {
                        'kind': 'datetime',    # kind of the imported value
                        'source': 'literal',   # empty/formula_cache/
                                               # formula_cache_missing/
                                               # literal/unknown
                        'format_kind': 'date', # when date-formatted
                        'serial': 44927,       # raw Excel serial
                        'has_time': False,     # time-of-day fraction present
                        'anomaly': None,       # e.g. 'fictitious_leap_day'
                    },
                },
            }

        ``kind`` describes the imported value: ``empty``, ``bool``, ``date``,
        ``datetime``, ``error``, ``number``, ``text``, ``time`` or
        ``timedelta``.  ``format_kind`` is the kind given by the cell number
        format and is only present on date-formatted cells; it is what decides
        that a cell is a date, so a plain number that only looks like a date
        serial is recorded as ``'number'`` without a ``serial``.  ``source``
        records where the value actually came from: an empty cell, a literal
        value, or the cached value of a formula (formula cells without a cached
        value are ``'formula_cache_missing'``).  ``'unknown'`` means the
        package could not be scanned, so the origin is unknown.

        ``date_serial_policy`` controls anomalous date serial values: the
        fictitious 1900-02-29 (serial 60 of the 1900 system), negative
        serials, and serials that cannot be converted to a date.  ``'reject'``
        (the default) raises :class:`tablib.exceptions.InvalidDateSerial`;
        ``'keep_serial'`` keeps the raw serial number as the cell value, so a
        later re-export cannot turn it into a different date.

        ``sheet_metadata`` is the mapping produced by
        :func:`_scan_sheet_cells` for this sheet.  It holds the raw serial of
        each date cell, which openpyxl's conversion cannot provide (serial 59
        and the fictitious serial 60 both come out as 1900-02-28).
        """
        _check_date_serial_policy(date_serial_policy)
        cls._clear_date_metadata(dset)

        dset.title = sheet.title

        if epoch is None:
            epoch = getattr(getattr(sheet, 'parent', None), 'epoch', None)
        date_system = _date_system_from_epoch(epoch)
        if date_system is not None:
            dset.xlsx_date_epoch = date_system

        sheet_metadata = sheet_metadata or {}
        provenance = {'date_system': date_system, 'cells': {}} if date_provenance else None

        for i, row in enumerate(sheet.rows):
            if i < skip_lines:
                continue
            is_header_row = i == skip_lines and headers
            data_row = i - skip_lines - (1 if headers else 0)
            row_vals = []
            row_records = []
            for j, cell in enumerate(row):
                position = _cell_position(cell, i, j)
                value, record = cls._import_cell(
                    cell, position, sheet_metadata, epoch,
                    date_system, date_serial_policy, want_record=provenance is not None,
                )
                row_vals.append(value)
                row_records.append(record)

            if is_header_row:
                dset.headers = row_vals
                continue

            cell_count = len(row_vals)
            if i > skip_lines and cell_count < dset.width:
                row_vals += [''] * (dset.width - cell_count)
            dset.append(row_vals)

            if provenance is not None:
                for j, record in enumerate(row_records):
                    if record is not None:
                        provenance['cells'][(data_row, j)] = record
                # Cells missing from a ragged row are empty cells too.
                for j in range(cell_count, dset.width):
                    provenance['cells'][(data_row, j)] = {'kind': 'empty', 'source': 'empty'}

        if provenance is not None:
            dset.date_provenance = provenance

    @classmethod
    def _import_cell(cls, cell, position, sheet_metadata, epoch, date_system,
                     date_serial_policy, want_record):
        """Return ``(value, provenance record or None)`` for one parsed cell."""
        value = cell.value
        format_kind = _number_format_kind(cell)
        kind = _cell_kind(cell, format_kind)

        serial = (sheet_metadata.get('serials') or {}).get(position)
        if serial is None and kind in DATE_KINDS:
            serial = _serial_from_value(value, epoch)

        anomaly = _serial_anomaly(serial, format_kind, date_system, epoch)
        preserved = False
        if anomaly is not None:
            if date_serial_policy == 'reject':
                coordinate = _cell_coordinate(cell, position)
                raise InvalidDateSerial(
                    f'Cell {coordinate} holds the Excel date serial {serial!r} '
                    f'({anomaly}) in the {date_system or "unknown"} date system. '
                    "Import with date_serial_policy='keep_serial' to keep the raw "
                    'serial number instead of a converted date.'
                )
            # Keep the raw serial number so it cannot be read as another date.
            value = _serial_number(serial)
            preserved = True
            # A date-formatted cell is reported with its date kind so it is
            # never confused with a plain number, even though its value is now
            # the raw serial.
            kind = format_kind

        if not want_record:
            return value, None

        record = {
            'kind': kind,
            'source': _cell_source(position, value, sheet_metadata),
        }
        if format_kind is not None:
            record['format_kind'] = format_kind
        if kind in DATE_KINDS:
            if serial is not None:
                record['serial'] = _serial_number(serial)
            record['has_time'] = _has_time_fraction(serial, value)
        if anomaly is not None:
            record['anomaly'] = anomaly
        if preserved:
            record['serial_preserved'] = True
        return value, record

    @classmethod
    def import_set(cls, dset, in_stream, headers=True, read_only=True, skip_lines=0,
                   date_provenance=False, date_serial_policy='reject'):
        """Returns dataset from XLSX stream.

        See :meth:`import_sheet` for the ``date_provenance`` and
        ``date_serial_policy`` parameters.
        """
        _check_date_serial_policy(date_serial_policy)

        dset.wipe()

        in_stream, metadata = _scan_workbook_metadata(
            in_stream, date_provenance, active_only=True,
        )
        xls_book = load_workbook(in_stream, read_only=read_only, data_only=True)
        try:
            sheet = xls_book.active
            cls.import_sheet(
                dset, sheet, headers, skip_lines,
                epoch=getattr(xls_book, 'epoch', None),
                date_provenance=date_provenance,
                date_serial_policy=date_serial_policy,
                sheet_metadata=metadata.get(sheet.title),
            )
        finally:
            cls._close_workbook(xls_book)

    @classmethod
    def import_book(cls, dbook, in_stream, headers=True, read_only=True,
                    date_provenance=False, date_serial_policy='reject'):
        """Returns databook from XLSX stream.

        See :meth:`import_sheet` for the ``date_provenance`` and
        ``date_serial_policy`` parameters.
        """
        _check_date_serial_policy(date_serial_policy)

        dbook.wipe()

        in_stream, metadata = _scan_workbook_metadata(in_stream, date_provenance)
        xls_book = load_workbook(in_stream, read_only=read_only, data_only=True)
        try:
            for sheet in xls_book.worksheets:
                dset = tablib.Dataset()
                cls.import_sheet(
                    dset, sheet, headers,
                    epoch=getattr(xls_book, 'epoch', None),
                    date_provenance=date_provenance,
                    date_serial_policy=date_serial_policy,
                    sheet_metadata=metadata.get(sheet.title),
                )
                dbook.add_sheet(dset)
        finally:
            cls._close_workbook(xls_book)

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
