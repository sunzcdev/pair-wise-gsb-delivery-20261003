class DBFNotFound(IOError):
    """Raised if the DBF file was not found."""
    pass


class MissingMemoFile(IOError):
    """Raised if the corresponding memo file was not found."""


class RecordCountMismatchWarning(UserWarning):
    """The record count in the file header does not match the file."""


class TruncatedRecordWarning(UserWarning):
    """The file ends with a partial record, which is not returned."""


__all__ = ['DBFNotFound', 'MissingMemoFile',
           'RecordCountMismatchWarning', 'TruncatedRecordWarning']
