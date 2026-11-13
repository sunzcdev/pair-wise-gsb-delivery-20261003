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


class RecastError(ArgumentError, ValueError):
    """Base class for errors raised while recasting a table."""


class RecastUnknownCategoryError(RecastError):
    """Raised when a fixed recast category domain does not contain a value."""


class RecastConflictError(RecastError, DuplicateKeyError):
    """Raised when a key/category pair occurs more than once."""
