class DBFNotFound(IOError):
    """Raised if the DBF file was not found."""
    pass


class MissingMemoFile(IOError):
    """Raised if the corresponding memo file was not found."""


class MemoError(IOError):
    """Base class for errors while reading memo (FPT/DBT) files.

    The attributes ``record`` (1-based DBF record number), ``field``
    (name of the memo field) and ``block`` (memo block number) are
    filled in when they are known, and are included in the string
    representation of the error.
    """
    def __init__(self, message, record=None, field=None, block=None):
        self.message = message
        self.record = record
        self.field = field
        self.block = block
        IOError.__init__(self, message)

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


class MemoBlockOutOfRange(MemoError):
    """The memo block number points outside the memo file."""
    pass


class MemoHeaderCorrupt(MemoError):
    """The memo block header is damaged or has invalid values."""
    pass


class MemoTruncated(MemoError):
    """The memo data ends before the declared length or terminator."""
    pass


__all__ = ['DBFNotFound', 'MissingMemoFile', 'MemoError',
           'MemoBlockOutOfRange', 'MemoHeaderCorrupt', 'MemoTruncated']
