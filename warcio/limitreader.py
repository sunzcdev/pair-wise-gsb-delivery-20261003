# ============================================================================
class LimitReader(object):
    """
    A reader which will not read more than specified limit
    """

    def __init__(self, stream, limit):
        self.stream = stream
        self.limit = limit
        self._orig_limit = limit
        # set to True when the underlying stream ends before the full
        # declared limit could be read (a short read / truncated record)
        self.truncated = False

    def _update(self, buff):
        length = len(buff)
        self.limit -= length

        # a read that returns nothing while bytes were still expected
        # means the underlying stream ended before the declared length
        if length == 0 and self.limit > 0:
            self.truncated = True

        return buff

    def _note_short_read(self, requested, buff):
        # the underlying stream returned fewer bytes than asked for while
        # the declared limit has not been exhausted: it ended early
        if len(buff) < requested and self.limit > 0:
            self.truncated = True

    def read(self, length=None):
        if length is not None:
            length = min(length, self.limit)
        else:
            length = self.limit

        if length == 0:
            return b''

        buff = self.stream.read(length)
        self._note_short_read(length, buff)
        return self._update(buff)

    def readline(self, length=None):
        if length is not None:
            length = min(length, self.limit)
        else:
            length = self.limit

        if length == 0:
            return b''

        buff = self.stream.readline(length)
        # readline returns early on a newline (normal), but a result that
        # does not end in a newline means the underlying stream ended first:
        # that is a short read while declared bytes remain
        if buff and not buff.endswith(b'\n') and self.limit - len(buff) > 0:
            self.truncated = True
        elif len(buff) == 0 and self.limit > 0:
            self.truncated = True
        return self._update(buff)

    def close(self):
        self.stream.close()

    def tell(self):
        # implement our own tell
        return self._orig_limit - self.limit

    @staticmethod
    def wrap_stream(stream, content_length):
        """
        If given content_length is an int > 0, wrap the stream
        in a LimitReader. Otherwise, return the stream unaltered
        """
        try:
            content_length = int(content_length)
            if content_length >= 0:
                # optimize: if already a LimitStream, set limit to
                # the smaller of the two limits
                if isinstance(stream, LimitReader):
                    stream.limit = min(stream.limit, content_length)
                else:
                    stream = LimitReader(stream, content_length)

        except (ValueError, TypeError):
            pass

        return stream

