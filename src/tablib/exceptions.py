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


class UnsupportedFormat(TablibException, NotImplementedError):
    """Format not supported."""


class InvalidMissingPolicy(TablibException, ValueError):
    """Missing-value policy must be given explicitly as 'skip' or 'error'."""


class SheetNotFound(TablibException, KeyError):
    """The given Dataset is not a member of this Databook."""


class SheetReferenced(TablibException, RuntimeError):
    """The sheet takes part in registered references and cannot be removed
    without explicitly unbinding them first."""
