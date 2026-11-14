#=================================================================
class ArchiveLoadFailed(Exception):
    def __init__(self, reason):
        self.msg = str(reason)
        super(ArchiveLoadFailed, self).__init__(self.msg)


#=================================================================
class RecordIDMismatch(ArchiveLoadFailed):
    def __init__(self, expected, actual):
        msg = 'Record ID mismatch: expected {0}, found {1}'.format(expected, actual)
        super(RecordIDMismatch, self).__init__(msg)
