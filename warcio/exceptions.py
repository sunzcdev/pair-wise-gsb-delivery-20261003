#=================================================================
class ArchiveLoadFailed(Exception):
    def __init__(self, reason):
        self.msg = str(reason)
        super(ArchiveLoadFailed, self).__init__(self.msg)


#=================================================================
class WARCWriterError(Exception):
    """Base class for errors raised while building/writing WARC records"""


#=================================================================
class DuplicateRecordIDError(WARCWriterError, ValueError):
    """Raised when an ID provider returns a WARC-Record-ID already used
    by another record of the same writer/builder."""
    def __init__(self, record_id):
        msg = 'Duplicate WARC-Record-ID rejected: {0}'.format(record_id)
        super(DuplicateRecordIDError, self).__init__(msg)
        self.record_id = record_id


#=================================================================
class InvalidWARCDateError(WARCWriterError, ValueError):
    """Raised when a date provider returns a value that is not a valid
    WARC (ISO 8601 UTC) date string."""
    def __init__(self, value):
        msg = 'Invalid WARC-Date rejected: {0!r}'.format(value)
        super(InvalidWARCDateError, self).__init__(msg)
        self.value = value
