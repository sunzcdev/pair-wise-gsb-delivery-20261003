""" Tablib - DBF Support.
"""

__lazy_modules__ = {
    "datetime",
    "io",
    "math",
    "struct",
    "tablib._vendor",
}

import datetime
import io
import math
import struct

from .._vendor.dbfpy import dbf


class DBFExportError(ValueError):
    """A dataset cannot be faithfully exported to DBF.

    Raised by `DBFFormat.export_set` when the preflight finds
    conflicting columns or cells that cannot be represented.  The
    message lists every problem found and no output is produced.
    """


class DBFFormat:
    title = 'dbf'
    extensions = ('dbf',)

    DEFAULT_ENCODING = 'utf-8'

    # dBASE III limits enforced when exporting.
    MAX_FIELD_NAME_BYTES = 10
    CHARACTER_FIELD_LENGTH = 80
    NUMERIC_FIELD_LENGTH = 10
    NUMERIC_FIELD_DECIMALS = 8

    @classmethod
    def export_set(cls, dataset, strict=False):
        """Returns DBF representation of a Dataset.

        A preflight pass validates the whole dataset *before* anything
        is written, so an export cannot fail halfway through a record
        or leave behind a seemingly complete file.

        The following public conversion rules are applied:

        * field names are uppercased and truncated to at most 10 bytes
          in the target encoding, cut at a character boundary;
          duplicates after truncation are always rejected;
        * ``None`` is exported as an empty field (blank text, or a
          blank numeric field which reads back as ``0``);
        * dates and datetimes are exported as their ISO string
          representation;
        * non-finite numbers (``inf``, ``-inf``, ``nan``) and numbers
          that do not fit a ``N(10, 8)`` field are exported as blank
          numeric fields;
        * text wider than 80 encoded bytes is truncated at a character
          boundary.

        With ``strict=True`` any value that would be altered by these
        rules is reported instead: a `DBFExportError` listing every
        conflicting column and unrepresentable cell is raised and no
        output is produced.
        """
        fields, rows = cls._preflight(dataset, strict=strict)

        encoding = cls.DEFAULT_ENCODING
        header_length = 32 + 32 * len(fields) + 1
        record_length = 1 + sum(field[2] for field in fields)
        today = datetime.date.today()

        stream = io.BytesIO()
        # 32-byte file header, same layout as written by dbfpy.
        stream.write(struct.pack(
            '<4BI2H',
            0x03,  # dBASE III, no memo fields
            today.year - 1900,
            today.month,
            today.day,
            len(rows),
            header_length,
            record_length,
        ))
        stream.write(b'\x00' * 20)
        for name, type_code, length, decimals in fields:
            # ``name`` already fits in MAX_FIELD_NAME_BYTES encoded
            # bytes thanks to the preflight.
            stream.write(name.encode(encoding).ljust(11, b'\x00'))
            stream.write(type_code.encode('ascii'))
            stream.write(b'\x00' * 4)  # field data address, unused
            stream.write(bytes((length, decimals)))
            stream.write(b'\x00' * 14)  # reserved
        stream.write(b'\r')
        for row in rows:
            stream.write(b' ')  # deletion flag: not deleted
            for (name, type_code, length, decimals), value in zip(fields, row):
                if type_code == 'N':
                    stream.write(cls._encode_numeric(value, length, decimals))
                else:
                    stream.write(cls._encode_character(value, length))
        stream.write(b'\x1a')  # end-of-file marker
        return stream.getvalue()

    @classmethod
    def _preflight(cls, dataset, strict=False):
        """Validate ``dataset`` against the DBF limits before exporting.

        Returns a ``(fields, rows)`` tuple where ``fields`` is a list
        of ``(name, type, length, decimals)`` tuples and ``rows``
        holds the normalized cell values, ready to be encoded.  See
        `export_set` for the public conversion rules.
        """
        problems = []
        duplicates = []

        headers = dataset.headers
        first_row = dataset[0]

        # Field names are uppercased and truncated to fit the 10-byte
        # DBF limit; duplicates are only checked afterwards, because
        # the encoded byte length -- not the character count --
        # decides whether names fit and whether they collide.
        names = []
        for col, header in enumerate(headers):
            name, truncated = cls._truncate_bytes(
                str(header).upper(), cls.MAX_FIELD_NAME_BYTES)
            if truncated:
                problems.append(
                    f"column {col} ({header!r}): field name exceeds "
                    f"{cls.MAX_FIELD_NAME_BYTES} {cls.DEFAULT_ENCODING} bytes, "
                    f"truncated to {name!r}"
                )
            names.append(name)
        seen = {}
        for col, name in enumerate(names):
            if name in seen:
                duplicates.append(
                    f"columns {seen[name]} and {col} share field name "
                    f"{name!r} after encoding and truncation"
                )
            else:
                seen[name] = col

        # Field types follow the first row, as before.
        fields = []
        for name, value in zip(names, first_row):
            if type(value) in (int, float):
                fields.append((name, 'N', cls.NUMERIC_FIELD_LENGTH,
                               cls.NUMERIC_FIELD_DECIMALS))
            else:
                fields.append((name, 'C', cls.CHARACTER_FIELD_LENGTH, 0))

        rows = []
        for row_index, row in enumerate(dataset):
            normalized = []
            for col, (field, value) in enumerate(zip(fields, row)):
                if field[1] == 'N':
                    value = cls._normalize_numeric(
                        value, row_index, col, headers[col], problems)
                else:
                    value = cls._normalize_character(
                        value, row_index, col, headers[col], problems)
                normalized.append(value)
            rows.append(normalized)

        if strict and (duplicates or problems):
            raise DBFExportError(
                "strict DBF export preflight failed:\n  - "
                + "\n  - ".join(duplicates + problems)
            )
        if duplicates:
            # Duplicate field names would silently overwrite each
            # other, so they are rejected in every mode.
            raise DBFExportError(
                "DBF export failed:\n  - " + "\n  - ".join(duplicates)
            )
        return fields, rows

    @classmethod
    def _normalize_numeric(cls, value, row, col, header, problems):
        """Normalize a numeric cell to a number or ``None`` (blank)."""
        if value is None:
            # Public rule: None is exported as an empty numeric field.
            return None
        if not isinstance(value, (int, float)):
            problems.append(
                f"row {row}, column {col} ({header!r}): "
                f"non-numeric value {value!r} in a numeric field"
            )
            return None
        if not math.isfinite(value):
            # Public rule: non-finite numbers export as blank fields.
            problems.append(
                f"row {row}, column {col} ({header!r}): "
                f"non-finite value {value!r} cannot be exported"
            )
            return None
        encoded = f"{value:{cls.NUMERIC_FIELD_LENGTH}.{cls.NUMERIC_FIELD_DECIMALS}f}"
        if len(encoded) > cls.NUMERIC_FIELD_LENGTH:
            dot = encoded.find('.')
            if 0 <= dot <= cls.NUMERIC_FIELD_LENGTH:
                encoded = encoded[:cls.NUMERIC_FIELD_LENGTH]
            else:
                problems.append(
                    f"row {row}, column {col} ({header!r}): "
                    f"value {value!r} overflows a "
                    f"N({cls.NUMERIC_FIELD_LENGTH},{cls.NUMERIC_FIELD_DECIMALS})"
                    " field"
                )
                return None
        if float(encoded) != value:
            problems.append(
                f"row {row}, column {col} ({header!r}): "
                f"value {value!r} loses precision in a "
                f"N({cls.NUMERIC_FIELD_LENGTH},{cls.NUMERIC_FIELD_DECIMALS})"
                f" field (stored as {encoded.strip()})"
            )
            # Non-strict exports keep the historical lossy truncation.
        return value

    @classmethod
    def _normalize_character(cls, value, row, col, header, problems):
        """Normalize a character cell to text fitting the field."""
        if value is None:
            # Public rule: None is exported as an empty character field.
            return ''
        # Dates and datetimes use their ISO representation via str().
        text = str(value)
        encoded = text.encode(cls.DEFAULT_ENCODING)
        if len(encoded) <= cls.CHARACTER_FIELD_LENGTH:
            return text
        problems.append(
            f"row {row}, column {col} ({header!r}): text needs "
            f"{len(encoded)} {cls.DEFAULT_ENCODING} bytes, field holds "
            f"{cls.CHARACTER_FIELD_LENGTH}"
        )
        truncated, _ = cls._truncate_bytes(text, cls.CHARACTER_FIELD_LENGTH)
        return truncated

    @classmethod
    def _truncate_bytes(cls, text, max_bytes):
        """Truncate ``text`` to ``max_bytes`` in the target encoding.

        Returns a ``(text, truncated)`` tuple; the cut never splits a
        multi-byte character, so character count is never mistaken for
        byte count.
        """
        encoded = text.encode(cls.DEFAULT_ENCODING)
        if len(encoded) <= max_bytes:
            return text, False
        truncated = encoded[:max_bytes].decode(
            cls.DEFAULT_ENCODING, errors='ignore')
        return truncated, True

    @classmethod
    def _encode_numeric(cls, value, length, decimals):
        """Encode a normalized numeric cell to exactly ``length`` bytes."""
        if value is None:
            return b' ' * length
        encoded = f"{value:{length}.{decimals}f}"
        if len(encoded) > length:
            # Only precision loss can remain after the preflight; the
            # historical behavior truncates the decimals.
            encoded = encoded[:length]
        return encoded.encode('ascii')

    @classmethod
    def _encode_character(cls, value, length):
        """Encode a normalized character cell to exactly ``length`` bytes."""
        return value.encode(cls.DEFAULT_ENCODING)[:length].ljust(length, b' ')

    @classmethod
    def import_set(cls, dset, in_stream):
        """Returns a dataset from a DBF stream."""

        dset.wipe()
        _dbf = dbf.Dbf(in_stream)
        dset.headers = _dbf.fieldNames
        for record in range(_dbf.recordCount):
            row = [_dbf[record][f] for f in _dbf.fieldNames]
            dset.append(row)

    @classmethod
    def detect(cls, stream):
        """Returns True if the given stream is valid DBF"""
        try:
            dbf.Dbf(stream, readOnly=True)
            return True
        except Exception:
            return False
