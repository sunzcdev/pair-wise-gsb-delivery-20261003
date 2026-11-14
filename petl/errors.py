from __future__ import absolute_import, print_function, division


class DuplicateKeyError(Exception):

    def __init__(self, key):
        self.key = key

    def __str__(self):
        return 'duplicate key: %r' % (self.key,)


class FieldSelectionError(Exception):

    def __init__(self, value):
        self.value = value

    def __str__(self):
        return 'selection is not a field or valid field index: %r' % (self.value,)


class ArgumentError(Exception):

    def __init__(self, message):
        self.message = message

    def __str__(self):
        return 'argument error: %s' % self.message


class WindowDataError(ValueError):
    """Base class for event-time window errors.

    ``row`` is the one-based data row number (the header is not counted) and
    ``source_row`` is the corresponding one-based physical row number,
    including the header.
    """

    def __init__(self, message, row=None, value=None, previous=None):
        self.message = message
        self.row = row
        self.data_row = row
        self.rownum = row
        self.line_number = row
        self.source_row = None if row is None else row + 1
        self.value = value
        self.previous = previous
        if row is None:
            text = message
        else:
            text = '%s (data row %d, source row %d)' % (
                message, row, row + 1)
        super(WindowDataError, self).__init__(text)


class WindowOrderError(WindowDataError):
    """Raised when event time goes backwards in a presorted source."""


class WindowTimezoneError(WindowDataError):
    """Raised when naive and timezone-aware event times are mixed."""


class WindowTimeTypeError(WindowDataError):
    """Raised when event-time values are incompatible with each other."""


WindowError = WindowDataError
WindowInputError = WindowDataError
OutOfOrderError = WindowOrderError
OutOfOrderWindowError = WindowOrderError
TimezoneMismatchError = WindowTimezoneError
MixedTimezoneError = WindowTimezoneError
