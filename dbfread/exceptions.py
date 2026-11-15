class DBFNotFound(IOError):
    """Raised if the DBF file was not found."""
    pass


class MissingMemoFile(IOError):
    """Raised if the corresponding memo file was not found."""


class DBFChangedError(IOError):
    """Raised when a consistent read detects that a file changed.

    This is raised when a table opened with ``consistent=True`` (or
    ``copy_to_temp=True``) finds that the DBF file or its memo file has
    been replaced, truncated, removed or otherwise changed while it is
    being read.

    The data read up to this point can not be trusted, so any records
    already yielded should be treated as incomplete. If the table was
    reading from a private temporary copy that copy is removed before the
    exception is raised.

    This is a best-effort check.  Without co-operation from the writer it
    is impossible to guarantee a true transactional snapshot, so a change
    could in theory go unnoticed.
    """


__all__ = ['DBFNotFound', 'MissingMemoFile', 'DBFChangedError']
