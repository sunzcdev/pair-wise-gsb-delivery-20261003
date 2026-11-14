#=================================================================
class ArchiveLoadFailed(Exception):
    def __init__(self, reason):
        self.msg = str(reason)
        super(ArchiveLoadFailed, self).__init__(self.msg)


#=================================================================
class RecordStreamInvalidated(Exception):
    """ Raised when reading a record payload stream that is no longer
    valid because the ArchiveIterator has already advanced to a later
    record, or the iterator has been closed.

    Only raised when the iterator is created with
    strict_stream_lifecycle=True.
    """
    def __init__(self, reason):
        self.msg = str(reason)
        super(RecordStreamInvalidated, self).__init__(self.msg)
