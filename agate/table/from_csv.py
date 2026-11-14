import io
import itertools


class _PrefixedTextStream:
    """
    A read-only, sequential text stream that presents a buffered *prefix*
    followed by the unread remainder of another text *stream*.

    Unlike :func:`itertools.chain` of two :class:`io.StringIO` objects, this
    rejoins a line split across the boundary, so a prefix that ends in the
    middle of a quoted field, a multi-line field or a delimiter is handed to
    the CSV reader as continuous text without a spurious record boundary.

    Only :meth:`readline` (and iteration) are required from *stream*, which
    means non-seekable, sequential-only sources such as pipes are supported.
    """
    def __init__(self, prefix, stream):
        self._prefix = io.StringIO(prefix)
        self._stream = stream

    def readline(self):
        if self._prefix is not None:
            line = self._prefix.readline()

            if line == '':
                # The buffered prefix is fully consumed.
                self._prefix = None
            elif line.endswith('\n'):
                # A complete line from the prefix.
                return line
            else:
                # The prefix ended mid-line: join it with the next line of
                # the underlying stream (an empty string simply terminates
                # the final line).
                return line + self._stream.readline()

        return self._stream.readline()

    def __iter__(self):
        return self

    def __next__(self):
        line = self.readline()

        if line == '':
            raise StopIteration

        return line


@classmethod
def from_csv(cls, path, column_names=None, column_types=None, row_names=None, skip_lines=0, header=True, sniff_limit=0,
             encoding='utf-8', row_limit=None, **kwargs):
    """
    Create a new table from a CSV.

    This method uses agate's builtin CSV reader, which supplies encoding
    support for both Python 2 and Python 3.

    :code:`kwargs` will be passed through to the CSV reader.

    :param path:
        Filepath or file-like object from which to read CSV data. A file-like
        object only needs to support sequential reading; it does not have to
        be seekable. If using Python 2, the file should be opened in binary
        mode (`rb`).
    :param column_names:
        See :meth:`.Table.__init__`.
    :param column_types:
        See :meth:`.Table.__init__`.
    :param row_names:
        See :meth:`.Table.__init__`.
    :param skip_lines:
        The number of lines to skip from the top of the file.
    :param header:
        If :code:`True`, the first row of the CSV is assumed to contain column
        names. If :code:`header` and :code:`column_names` are both specified
        then a row will be skipped, but :code:`column_names` will be used.
    :param sniff_limit:
        Limit CSV dialect sniffing to the specified number of bytes. Set to
        None to sniff the entire file. Defaults to 0 (no sniffing). The bytes
        read for sniffing are buffered and replayed to the CSV reader, so the
        source stream need not be seekable. If the dialect cannot be sniffed
        from the sample, any dialect supplied in :code:`kwargs` is used and
        the buffered sample is still read as data.
    :param encoding:
        Character encoding of the CSV file. Note: if passing in a file
        handle it is assumed you have already opened it with the correct
        encoding specified.
    :param row_limit:
        Limit how many rows of data will be read.
    """
    from agate import csv
    from agate.table import Table

    close = False

    try:
        if hasattr(path, 'read'):
            f = path
        else:
            f = open(path, encoding=encoding)

            close = True

        if isinstance(skip_lines, int):
            while skip_lines > 0:
                f.readline()
                skip_lines -= 1
        else:
            raise ValueError('skip_lines argument must be an int')

        handle = f

        if sniff_limit is None:
            # Read the whole stream once: sniff from, and then parse, this
            # buffer so that non-seekable streams are supported too.
            handle = io.StringIO(f.read())
            sample = handle.getvalue()
        elif sniff_limit > 0:
            # Buffer only the sniffing prefix, then replay it followed by the
            # rest of the (possibly non-seekable) stream. No tell()/seek() is
            # required, and the prefix is never dropped from the parsed data.
            sample = f.read(sniff_limit)  # reads *characters*
            handle = _PrefixedTextStream(sample, f)

        if (sniff_limit is None or sniff_limit > 0) and sample:
            # A failed sniff returns None; leave any caller-supplied dialect
            # (or the CSV module's default) in place rather than overriding it.
            dialect = csv.Sniffer().sniff(sample)

            if dialect is not None:
                kwargs['dialect'] = dialect

        reader = csv.reader(handle, header=header, **kwargs)

        if header:
            if column_names is None:
                try:
                    column_names = next(reader)
                except StopIteration:
                    column_names = []
            else:
                try:
                    next(reader)
                except StopIteration:
                    pass

        if row_limit is None:
            rows = reader
        else:
            rows = itertools.islice(reader, row_limit)

        return Table(rows, column_names, column_types, row_names=row_names)
    finally:
        if close:
            f.close()
