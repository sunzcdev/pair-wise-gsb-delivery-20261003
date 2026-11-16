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


class BatchError(TablibException):
    """Base class for errors collected while adding a batch of rows.

    The error carries the zero-based position of the offending row within the
    supplied batch, so the caller can locate the exact source row.

    :param index: zero-based position of the offending row within the batch
    :param original: the underlying exception that caused the failure
    """

    message = 'Batch ingestion failed'

    def __init__(self, index, original):
        self.index = index
        self.original = original
        detail = str(original) or type(original).__name__
        super().__init__(f'{self.message} at row {index}: {detail}')

    def __reduce__(self):
        return type(self), (self.index, self.original)


class InvalidBatchRow(BatchError, InvalidDimensions):
    """A row of a batch could not be validated and turned into dataset row.

    This includes dimension mismatches as well as failures raised while
    computing dynamic columns for the row. The ``original`` attribute holds
    the underlying exception.

    It also subclasses :class:`InvalidDimensions` so code written for
    ``Dataset.extend`` keeps catching dimension errors.
    """

    message = 'Invalid row in batch'


class BatchSourceError(BatchError):
    """The iterable supplying a batch raised while yielding a row.

    This is deliberately distinct from :class:`InvalidBatchRow`: the source
    itself failed to produce a row, as opposed to producing a row that did
    not pass validation.
    """

    message = 'Batch source raised while yielding row'
