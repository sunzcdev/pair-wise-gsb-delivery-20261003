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


class MissingFieldError(Exception):
    """Raised when a value cannot be read because a field is absent.

    Used by validation to mark a result that could not be determined
    because a dependency field was missing from the table header or from
    an individual row.
    """

    def __init__(self, value):
        self.value = value

    def __str__(self):
        return 'missing field: %r' % (self.value,)


class ArgumentError(Exception):

    def __init__(self, message):
        self.message = message

    def __str__(self):
        return 'argument error: %s' % self.message
