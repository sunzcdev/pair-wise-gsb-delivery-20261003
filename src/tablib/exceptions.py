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


class ODSValidationError(TablibException, ValueError):
    """An ODS document contains invalid or unsupported content.

    Raised when an OpenDocument Spreadsheet cannot be parsed safely, for
    example because of an invalid ``table:number-rows-repeated`` /
    ``table:number-columns-repeated`` value, an expansion that would exceed
    the configured row/column budget, or an unexpected covered table cell.
    """
