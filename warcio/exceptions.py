#=================================================================
class ArchiveLoadFailed(Exception):
    def __init__(self, reason):
        self.msg = str(reason)
        super(ArchiveLoadFailed, self).__init__(self.msg)


#=================================================================
class StrictLengthError(ArchiveLoadFailed):
    """Raised in strict length mode when a record's length can not be
    trusted to locate the end of the record.

    The error always identifies the offending record (by its
    ``WARC-Record-ID`` when available) and the offset at which the record
    starts in the (decompressed) archive stream, so a caller never has to
    guess where the corrupt record began.

    :ivar str reason: human readable description of the length problem
    :ivar record_id: the record id, or None when the record has none
    :type record_id: str | None
    :ivar offset: archive offset of the start of the record, or None when
        the underlying stream does not support ``tell()``
    :type offset: int | None
    """

    def __init__(self, reason, record_id=None, offset=None):
        self.reason = str(reason)
        self.record_id = record_id
        self._offset = offset
        super(StrictLengthError, self).__init__(self._build_message())

    @property
    def offset(self):
        return self._offset

    @offset.setter
    def offset(self, value):
        # allow the iterator to replace the loader's block-buffered offset
        # with the precise archive offset; keep the rendered message in sync
        self._offset = value
        self.msg = self._build_message()
        self.args = (self.msg,)

    def _build_message(self):
        msg = self.reason
        if self.record_id is not None:
            msg += ' (record id: {0})'.format(self.record_id)
        if self._offset is not None:
            msg += ' (archive offset: {0})'.format(self._offset)
        return msg
