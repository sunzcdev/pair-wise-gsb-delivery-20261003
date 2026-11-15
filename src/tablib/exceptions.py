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


class TagsNotSupported(TablibException, ValueError):
    """The requested operation cannot preserve the source tags of the rows.

    Some derivations (for example :meth:`tablib.Dataset.transpose`) do not
    have a one-to-one correspondence between source rows and result rows, so
    their tags cannot be carried over automatically. The caller must choose an
    explicit policy instead of having the tags dropped silently.
    """


class UnsupportedFormat(TablibException, NotImplementedError):
    """Format not supported."""
