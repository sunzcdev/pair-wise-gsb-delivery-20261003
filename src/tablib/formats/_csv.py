""" Tablib - *SV Support.
"""

__lazy_modules__ = {"csv", "io"}

import csv
from io import BufferedIOBase, RawIOBase, StringIO, TextIOWrapper


class CSVFormat:
    title = 'csv'
    extensions = ('csv',)

    DEFAULT_DELIMITER = ','

    # When ``null_value`` is not given, explicit null encoding is disabled and
    # the format behaves exactly like the plain :mod:`csv` module.
    DEFAULT_NULL_VALUE = None

    # Prefix used to escape cell text that would otherwise be confused with
    # the null marker (or with an already escaped cell).
    ESCAPE_PREFIX = '\\'

    # ------------------------------------------------------------------
    # Explicit null encoding helpers
    # ------------------------------------------------------------------

    @classmethod
    def _validate_null_value(cls, null_value):
        """The null marker must be a non-empty string."""
        if not isinstance(null_value, str) or not null_value:
            raise ValueError(
                'null_value must be a non-empty string, or None to disable '
                'explicit null encoding.'
            )

    @classmethod
    def _encode_cell(cls, value, null_value):
        """Encode a single data cell for export.

        ``None`` is written as the null marker. A real string that equals the
        marker, or starts with the escape prefix, is prefixed once so that the
        original text can be recovered on import. Everything else (including
        the empty string) is left untouched.
        """
        if value is None:
            return null_value
        if isinstance(value, str) and (
            value == null_value or value.startswith(cls.ESCAPE_PREFIX)
        ):
            return cls.ESCAPE_PREFIX + value
        return value

    @classmethod
    def _decode_cell(cls, value, null_value):
        """Decode a single imported data cell.

        The decision is based solely on the cell text, never on whether the
        field was quoted, so every :mod:`csv` quoting mode keeps the same
        meaning. Non-string values (e.g. floats produced with
        ``quoting=csv.QUOTE_NONNUMERIC``) are returned unchanged.
        """
        if not isinstance(value, str):
            return value
        if value == null_value:
            return None
        if value.startswith(cls.ESCAPE_PREFIX):
            remainder = value[len(cls.ESCAPE_PREFIX):]
            if remainder == null_value or remainder.startswith(cls.ESCAPE_PREFIX):
                return remainder
        return value

    @classmethod
    def _encode_rows(cls, rows, null_value, has_headers):
        """Encode packaged rows, leaving the header row untouched.

        ``rows`` comes from ``Dataset._package(dicts=False)``, which only
        includes a header row when headers have actually been set.
        """
        if null_value is None:
            return rows
        cls._validate_null_value(null_value)
        first_data_row = 1 if has_headers else 0
        encoded = list(rows)
        for index in range(first_data_row, len(encoded)):
            encoded[index] = [
                cls._encode_cell(cell, null_value) for cell in encoded[index]
            ]
        return encoded

    @staticmethod
    def _coerce_text_stream(in_stream):
        """Return a text stream applying the same rules to bytes and text.

        Raw binary streams and byte strings are decoded as UTF-8; text streams
        are passed through unchanged.
        """
        if isinstance(in_stream, (bytes, bytearray)):
            return StringIO(in_stream.decode('utf-8'), newline='')
        if isinstance(in_stream, (RawIOBase, BufferedIOBase)):
            return TextIOWrapper(in_stream, encoding='utf-8', newline='')
        return in_stream

    # ------------------------------------------------------------------
    # Export / import
    # ------------------------------------------------------------------

    @classmethod
    def export_stream_set(cls, dataset, *, null_value=DEFAULT_NULL_VALUE, **kwargs):
        """Returns CSV representation of Dataset as file-like."""
        stream = StringIO()

        kwargs.setdefault('delimiter', cls.DEFAULT_DELIMITER)

        _csv = csv.writer(stream, **kwargs)

        rows = cls._encode_rows(
            dataset._package(dicts=False),
            null_value,
            has_headers=dataset.headers is not None,
        )
        for row in rows:
            _csv.writerow(row)

        stream.seek(0)
        return stream

    @classmethod
    def export_set(cls, dataset, *, null_value=DEFAULT_NULL_VALUE, **kwargs):
        """Returns CSV representation of Dataset."""
        stream = cls.export_stream_set(dataset, null_value=null_value, **kwargs)
        return stream.getvalue()

    @classmethod
    def import_set(
        cls,
        dset,
        in_stream,
        headers=True,
        skip_lines=0,
        *,
        null_value=DEFAULT_NULL_VALUE,
        **kwargs,
    ):
        """Returns dataset from CSV stream."""

        dset.wipe()

        kwargs.setdefault('delimiter', cls.DEFAULT_DELIMITER)

        if null_value is not None:
            cls._validate_null_value(null_value)

        in_stream = cls._coerce_text_stream(in_stream)
        rows = csv.reader(in_stream, **kwargs)
        for i, row in enumerate(rows):
            if i < skip_lines:
                continue
            if i == skip_lines and headers:
                # Headers are labels, never subject to value conversion.
                dset.headers = row
            elif row:
                if null_value is not None:
                    row = [cls._decode_cell(cell, null_value) for cell in row]
                if i > 0 and len(row) < dset.width:
                    row += [''] * (dset.width - len(row))
                dset.append(row)

    @classmethod
    def detect(cls, stream, delimiter=None):
        """Returns True if given stream is valid CSV."""
        try:
            csv.Sniffer().sniff(stream.read(2048), delimiters=delimiter or cls.DEFAULT_DELIMITER)
            return True
        except Exception:
            return False
