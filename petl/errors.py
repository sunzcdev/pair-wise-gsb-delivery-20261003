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


class HeaderNormalizationError(Exception):

    """Raised when header normalization produces a collision."""

    def __init__(self, collisions):
        if isinstance(collisions, str):
            self.collisions = {collisions: []}
        else:
            self.collisions = dict(collisions)

    def __str__(self):
        details = ', '.join(
            '%r (columns %s)' % (name, ', '.join(str(i) for i in positions))
            for name, positions in sorted(self.collisions.items())
        )
        return 'header normalization produced duplicate names: ' + details
