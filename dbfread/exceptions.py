class DBFNotFound(IOError):
    """Raised if the DBF file was not found."""
    pass


class MissingMemoFile(IOError):
    """Raised if the corresponding memo file was not found."""


class _MemoErrorContext(object):
    """Context and formatting shared by all memo errors.

    Kept as a plain mixin so the concrete errors can inherit from
    different builtin exception types (``IOError`` and ``ValueError``)
    without an instance layout conflict.
    """
    def _set_context(self, message, record, field, block):
        self.message = message
        self.record = record
        self.field = field
        self.block = block

    def __str__(self):
        context = []
        if self.record is not None:
            context.append('record {}'.format(self.record))
        if self.field is not None:
            context.append('field {!r}'.format(self.field))
        if self.block is not None:
            context.append('block {}'.format(self.block))

        if context:
            return '{} ({})'.format(self.message, ', '.join(context))
        else:
            return self.message


class MemoError(_MemoErrorContext, IOError):
    """Base class for errors while reading memo (FPT/DBT) files.

    The attributes ``record`` (1-based physical DBF record number),
    ``field`` (name of the memo field) and ``block`` (memo block number
    from the DBF record) are filled in when known, and are included in
    the string representation of the error so a failure can be traced
    back to the offending record and field.
    """
    def __init__(self, message, record=None, field=None, block=None):
        self._set_context(message, record, field, block)
        IOError.__init__(self, message)


class MemoBlockOutOfRange(MemoError):
    """The memo block number points outside the memo file."""
    pass


class MemoHeaderCorrupt(MemoError):
    """The memo block header is damaged, incomplete or invalid."""
    pass


class MemoTruncated(MemoError):
    """The memo body ends before its declared length or terminator."""
    pass


class MemoReferenceInvalid(_MemoErrorContext, ValueError):
    """The memo reference stored in the DBF field is not a valid number.

    This also subclasses ``ValueError`` so code that caught the old
    ``ValueError`` from the memo index parser keeps working. It carries
    the same ``record``, ``field`` and ``block`` context as the other
    memo errors.
    """
    def __init__(self, message, record=None, field=None, block=None):
        self._set_context(message, record, field, block)
        ValueError.__init__(self, message)


# Exceptions that carry memo context and should get the field name and
# record number attached while reading.
MEMO_ERRORS = (MemoError, MemoReferenceInvalid)


__all__ = ['DBFNotFound', 'MissingMemoFile', 'MemoError',
           'MemoBlockOutOfRange', 'MemoHeaderCorrupt', 'MemoTruncated',
           'MemoReferenceInvalid', 'MEMO_ERRORS']
