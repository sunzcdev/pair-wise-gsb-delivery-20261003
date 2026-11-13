from __future__ import absolute_import, print_function, division


class DuplicateKeyError(Exception):

    def __init__(self, key):
        self.key = key

    def __str__(self):
        return 'duplicate key: %r' % (self.key,)


class CardinalityViolation(Exception):
    """Raised when an equi-join cardinality constraint is violated.

    `side` is the side of the join (``'left'`` or ``'right'``) where more
    than one row was observed for `key`, and `count` is the number of rows
    observed for that key.
    """

    def __init__(self, side, key, count):
        self.side = side
        self.key = key
        self.count = count

    def __str__(self):
        return ('cardinality violation on the %s side: key %r is '
                'observed %d times, at most one is allowed'
                % (self.side, self.key, self.count))


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
