#=================================================================
class ArchiveLoadFailed(Exception):
    def __init__(self, reason):
        self.msg = str(reason)
        super(ArchiveLoadFailed, self).__init__(self.msg)


#=================================================================
class ContentEncodingError(ArchiveLoadFailed):
    """ Raised when a record payload can not be fully decoded because an
    encoding layer is unsupported or the data is corrupt partway through.

    The ``encoding`` and ``layer`` attributes identify the failing layer
    (1-based, in the order the codings are listed in the header), so that
    partially decoded content is never silently returned as if decoding
    had fully succeeded.
    """
    def __init__(self, encoding, layer, reason, header='Content-Encoding'):
        self.encoding = encoding
        self.layer = layer
        msg = '{0} layer {1} ("{2}"): {3}'.format(header, layer, encoding, reason)
        super(ContentEncodingError, self).__init__(msg)
