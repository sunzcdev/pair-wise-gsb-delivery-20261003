class TablibException(Exception):
    """Tablib common exception."""


class InvalidDatasetType(TablibException, TypeError):
    """Only Datasets can be added to a Databook."""


class InvalidDimensions(TablibException, ValueError):
    """The size of the column or row doesn't fit the table dimensions."""


class InvalidDatasetIndex(TablibException, IndexError):
    """Outside of Dataset size."""


class HeadersNeeded(TablibException, AttributeError):
    """Header parameter must be given when appending a column to this Dataset."""


class UnsupportedFormat(TablibException, NotImplementedError):
    """Format not supported."""


class InvalidDateSerial(TablibException, ValueError):
    """A date cell holds a serial value that cannot be turned into a date.

    Raised while importing an XLSX sheet when a date-formatted cell holds an
    anomalous serial value (the fictitious 1900-02-29, a negative serial, or a
    serial outside the representable date range) and the ``date_serial_policy``
    is ``'reject'``.
    """
