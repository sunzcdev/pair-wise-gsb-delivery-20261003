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


class AmbiguousHeadersError(TablibException, ValueError):
    """Dataset headers cannot be represented unambiguously as mapping keys.

    Raised when a mapping-style view (``Dataset.dict``, JSON or YAML export)
    is requested for a dataset whose headers are duplicated or blank (``None``
    or empty string). Headers are never renamed automatically, so that
    consumers cannot mistake the contract of the exported data.
    """
