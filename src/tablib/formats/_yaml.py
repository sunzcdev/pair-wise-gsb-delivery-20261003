""" Tablib - YAML Support.
"""

__lazy_modules__ = {"copy", "tablib.exceptions", "yaml"}

import copy

import yaml

import tablib
from tablib.exceptions import InvalidDimensions, UnsupportedFormat

NULL_TAG = 'tag:yaml.org,2002:null'
MERGE_TAG = 'tag:yaml.org,2002:merge'

# Plain (unquoted, untagged) scalars spelled like this keep their YAML null
# meaning instead of being imported as text.
NULL_TEXT_MARKERS = frozenset({'', '~', 'null', 'Null', 'NULL'})


class TextScalarLoader(yaml.SafeLoader):
    """Safe YAML loader that imports plain scalars as text.

    PyYAML's default resolver guesses the type of a plain scalar from its
    spelling, so a source identifier such as ``001`` becomes ``1``, a date
    like ``2021-01-01`` becomes a :class:`datetime.date` and ``yes`` becomes
    ``True``. This loader only converts scalars whose type is stated explicitly
    in the document: quoted scalars stay text and tagged scalars (``!!int``,
    ``!!bool``, ``!!float``, ``!!timestamp``, ...) are converted as requested.
    Null markers and the ``<<`` merge key keep their YAML meaning so explicit
    nulls and merged mappings still work.

    Deriving from :class:`yaml.SafeLoader` keeps the safety guarantee: tags
    that would build arbitrary Python objects (``!!python/object`` or any
    unknown local tag) are rejected with a
    :class:`yaml.constructor.ConstructorError`.
    """

    def resolve(self, kind, value, implicit):
        if kind is yaml.ScalarNode and implicit[0]:
            if value in NULL_TEXT_MARKERS:
                return NULL_TAG
            if value == '<<':
                return MERGE_TAG
            return self.DEFAULT_SCALAR_TAG
        return super().resolve(kind, value, implicit)


def parse_document(in_stream, preserve_text=False):
    """Parse a single YAML document from `in_stream`.

    With `preserve_text`, plain scalars are kept as written, see
    :class:`TextScalarLoader`. The default mode keeps PyYAML's implicit typing.
    """
    loader_class = TextScalarLoader if preserve_text else yaml.SafeLoader
    loader = loader_class(in_stream)
    try:
        return loader.get_single_data()
    finally:
        loader.dispose()


def copy_cell(value):
    """Return the value to store in a :class:`tablib.Dataset` cell.

    An alias (``*anchor``) resolves to the very same object everywhere it is
    referenced. Dataset cells must not share mutable state, so container values
    are copied on import: changing the cell of one row never changes another
    row, and a row copied through an anchor is independent from its original.
    Immutable scalars are stored as they are.
    """
    if isinstance(value, (dict, list, set, tuple)):
        return copy.deepcopy(value)
    return value


def prepare_rows(payload, where):
    """Validate loaded YAML rows and return ``(rows, headers)``.

    `payload` is the loaded YAML value and `where` names it in error messages.
    The whole document is checked before the caller touches the Dataset, so an
    invalid document never half-fills or wipes existing data.
    """
    if payload is None:
        return [], None
    if not isinstance(payload, (list, tuple)):
        raise UnsupportedFormat(
            f'{where} must be a sequence of rows, not {type(payload).__name__}.'
        )
    if not payload:
        return [], None

    first = payload[0]
    if isinstance(first, dict):
        headers = list(first)
        rows = []
        for index, row in enumerate(payload):
            if not isinstance(row, dict):
                raise UnsupportedFormat(
                    f'{where} row {index} must be a mapping, not {type(row).__name__}.'
                )
            missing = [key for key in headers if key not in row]
            extra = [key for key in row if key not in headers]
            if missing or extra:
                raise InvalidDimensions(
                    f'{where} row {index} keys do not match the first row '
                    f'(missing: {missing!r}, unexpected: {extra!r}).'
                )
            # Read cells in header order rather than in each mapping's own
            # order, so columns stay aligned whatever order a row lists its keys.
            rows.append([copy_cell(row[key]) for key in headers])
        return rows, headers

    if isinstance(first, (list, tuple)):
        width = len(first)
        rows = []
        for index, row in enumerate(payload):
            if not isinstance(row, (list, tuple)):
                raise UnsupportedFormat(
                    f'{where} row {index} must be a sequence, not {type(row).__name__}.'
                )
            if len(row) != width:
                raise InvalidDimensions(
                    f'{where} row {index} has {len(row)} values, expected {width}.'
                )
            rows.append([copy_cell(cell) for cell in row])
        return rows, None

    raise UnsupportedFormat(
        f'{where} row 0 must be a mapping or a sequence, not {type(first).__name__}.'
    )


def prepare_sheets(payload):
    """Validate a loaded YAML book and return ``(title, rows, headers)`` tuples."""
    if payload is None:
        return []
    if not isinstance(payload, (list, tuple)):
        raise UnsupportedFormat(
            'YAML book import expects a sequence of sheets, '
            f'not {type(payload).__name__}.'
        )

    sheets = []
    for index, sheet in enumerate(payload):
        if not isinstance(sheet, dict) or 'title' not in sheet or 'data' not in sheet:
            raise UnsupportedFormat(
                f"YAML sheet {index} must be a mapping with 'title' and 'data' keys."
            )
        rows, headers = prepare_rows(sheet['data'], f'YAML sheet {index} data')
        sheets.append((sheet['title'], rows, headers))
    return sheets


def fill_dataset(dset, rows, headers):
    """Replace the contents of `dset` with already validated rows."""
    dset.wipe()
    if headers is not None:
        dset.headers = headers
    for row in rows:
        dset.append(row)


class YAMLFormat:
    title = 'yaml'
    extensions = ('yaml', 'yml')

    @classmethod
    def export_set(cls, dataset):
        """Returns YAML representation of Dataset."""
        return yaml.safe_dump(
            dataset._package(),
            default_flow_style=None,
            allow_unicode=True,
            sort_keys=False,
        )

    @classmethod
    def export_book(cls, databook):
        """Returns YAML representation of Databook."""
        return yaml.safe_dump(
            databook._package(),
            default_flow_style=None,
            allow_unicode=True,
            sort_keys=False,
        )

    @classmethod
    def import_set(cls, dset, in_stream, preserve_text=False):
        """Returns dataset from YAML stream.

        With `preserve_text=True`, plain scalars are imported as text and only
        explicitly typed scalars are converted, see :class:`TextScalarLoader`.
        """
        payload = parse_document(in_stream, preserve_text=preserve_text)
        rows, headers = prepare_rows(payload, 'YAML dataset import')
        fill_dataset(dset, rows, headers)

    @classmethod
    def import_book(cls, dbook, in_stream, preserve_text=False):
        """Returns databook from YAML stream.

        With `preserve_text=True`, plain scalars are imported as text and only
        explicitly typed scalars are converted, see :class:`TextScalarLoader`.
        """
        payload = parse_document(in_stream, preserve_text=preserve_text)
        sheets = prepare_sheets(payload)

        dbook.wipe()
        for title, rows, headers in sheets:
            data = tablib.Dataset()
            data.title = title
            fill_dataset(data, rows, headers)
            dbook.add_sheet(data)

    @classmethod
    def detect(cls, stream):
        """Returns True if given stream is valid YAML."""
        try:
            _yaml = yaml.safe_load(stream)
            if isinstance(_yaml, (list, tuple, dict)):
                return True
            else:
                return False
        except (yaml.parser.ParserError, yaml.reader.ReaderError,
                yaml.scanner.ScannerError):
            return False
