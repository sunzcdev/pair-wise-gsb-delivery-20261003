from __future__ import annotations

import io
import logging
import posixpath
import re
import warnings
from contextlib import ExitStack
from glob import has_magic
from pathlib import Path

# for backwards compat, we export cache things from here too
from fsspec.caching import (  # noqa: F401
    BaseCache,
    BlockCache,
    BytesCache,
    MMapCache,
    ReadAheadCache,
    caches,
)
from fsspec.compression import compr
from fsspec.config import conf
from fsspec.exceptions import FileChangedError
from fsspec.registry import available_protocols, filesystem, get_filesystem_class
from fsspec.utils import (
    _unstrip_protocol,
    build_name_function,
    infer_compression,
    stringify_path,
)

logger = logging.getLogger("fsspec")


class OpenFile:
    """
    File-like object to be used in a context

    Can layer (buffered) text-mode and compression over any file-system, which
    are typically binary-only.

    These instances are safe to serialize, as the low-level file object
    is not created until invoked using ``with``.

    Parameters
    ----------
    fs: FileSystem
        The file system to use for opening the file. Should be a subclass or duck-type
        with ``fsspec.spec.AbstractFileSystem``
    path: str
        Location to open
    mode: str like 'rb', optional
        Mode of the opened file
    compression: str or None, optional
        Compression to apply
    encoding: str or None, optional
        The encoding to use if opened in text mode.
    errors: str or None, optional
        How to handle encoding errors if opened in text mode.
    newline: None or str
        Passed to TextIOWrapper in text mode, how to handle line endings.
    """

    def __init__(
        self,
        fs,
        path,
        mode="rb",
        compression=None,
        encoding=None,
        errors=None,
        newline=None,
    ):
        self.fs = fs
        self.path = path
        self.mode = mode
        self.compression = get_compression(path, compression)
        self.encoding = encoding
        self.errors = errors
        self.newline = newline
        self.fobjects = []

    def __reduce__(self):
        return (
            OpenFile,
            (
                self.fs,
                self.path,
                self.mode,
                self.compression,
                self.encoding,
                self.errors,
                self.newline,
            ),
        )

    def __repr__(self):
        return f"<OpenFile '{self.path}'>"

    def __enter__(self):
        mode = self.mode.replace("t", "").replace("b", "") + "b"

        try:
            f = self.fs.open(self.path, mode=mode)
        except FileNotFoundError as e:
            if has_magic(self.path):
                raise FileNotFoundError(
                    f"{self.path} not found. "
                    "The URL contains glob characters: you maybe needed\n"
                    "to pass expand=True in fsspec.open() or the storage_options of \n"
                    "your library. You can also set the config value 'open_expand'\n"
                    "before import, or fsspec.core.DEFAULT_EXPAND at runtime, to True."
                ) from e
            raise

        self.fobjects = [f]

        try:
            if self.compression is not None:
                compress = compr[self.compression]
                f = compress(f, mode=mode[0])
                self.fobjects.append(f)

            if "b" not in self.mode:
                # assume, for example, that 'r' is equivalent to 'rt' as in builtin
                f = PickleableTextIOWrapper(
                    f, encoding=self.encoding, errors=self.errors, newline=self.newline
                )
                self.fobjects.append(f)
        except BaseException:
            self.close()
            raise

        return self.fobjects[-1]

    def __exit__(self, *args):
        self.close()

    @property
    def full_name(self):
        return _unstrip_protocol(self.path, self.fs)

    def open(self):
        """Materialise this as a real open file without context

        The OpenFile object should be explicitly closed to avoid enclosed file
        instances persisting. You must, therefore, keep a reference to the OpenFile
        during the life of the file-like it generates.
        """
        return self.__enter__()

    def close(self):
        """Close all encapsulated file objects"""

        def close_one(f):
            try:
                if "r" not in self.mode and not f.closed:
                    f.flush()
            finally:
                f.close()

        # Unwind in reverse order, attempting every close even if one fails.
        with ExitStack() as stack:
            for f in self.fobjects:
                stack.callback(close_one, f)
        self.fobjects.clear()


class OpenFiles(list):
    """List of OpenFile instances

    Can be used in a single context, which opens and closes all of the
    contained files. Normal list access to get the elements works as
    normal.

    A special case is made for caching filesystems - the files will
    be down/uploaded together at the start or end of the context, and
    this may happen concurrently, if the target filesystem supports it.
    """

    def __init__(self, *args, mode="rb", fs=None):
        self.mode = mode
        self.fs = fs
        self.files = []
        super().__init__(*args)

    def __enter__(self):
        if self.fs is None:
            raise ValueError("Context has already been used")

        fs = self.fs
        while True:
            if hasattr(fs, "open_many"):
                # check for concurrent cache download; or set up for upload
                self.files = fs.open_many(self)
                return self.files
            if hasattr(fs, "fs") and fs.fs is not None:
                fs = fs.fs
            else:
                break
        with ExitStack() as stack:
            files = [stack.enter_context(s) for s in self]
            # All files opened: transfer cleanup to OpenFiles.__exit__ so they
            # stay open after this temporary entry-failure guard exits.
            stack.pop_all()
            return files

    def __exit__(self, *args):
        fs = self.fs
        with ExitStack() as stack:
            if "r" in self.mode:
                # open_many() returns files without populating the OpenFile objects.
                for f in reversed(self.files):
                    stack.callback(f.close)
            for s in reversed(self):
                stack.callback(s.close)
        if "r" not in self.mode:
            while True:
                if hasattr(fs, "open_many"):
                    # check for concurrent cache upload
                    fs.commit_many(self.files)
                    return
                if hasattr(fs, "fs") and fs.fs is not None:
                    fs = fs.fs
                else:
                    break

    def __getitem__(self, item):
        out = super().__getitem__(item)
        if isinstance(item, slice):
            return OpenFiles(out, mode=self.mode, fs=self.fs)
        return out

    def __repr__(self):
        return f"<List of {len(self)} OpenFile instances>"


def open_files(
    urlpath,
    mode="rb",
    compression=None,
    encoding="utf8",
    errors=None,
    name_function=None,
    num=1,
    protocol=None,
    newline=None,
    auto_mkdir=True,
    expand=True,
    **kwargs,
):
    """Given a path or paths, return a list of ``OpenFile`` objects.

    For writing, a str path must contain the "*" character, which will be filled
    in by increasing numbers, e.g., "part*" ->  "part1", "part2" if num=2.

    For either reading or writing, can instead provide explicit list of paths.

    Parameters
    ----------
    urlpath: string or list
        Absolute or relative filepath(s). Prefix with a protocol like ``s3://``
        to read from alternative filesystems. To read from multiple files you
        can pass a globstring or a list of paths, with the caveat that they
        must all have the same protocol.
    mode: 'rb', 'wt', etc.
    compression: string or None
        If given, open file using compression codec. Can either be a compression
        name (a key in ``fsspec.compression.compr``) or "infer" to guess the
        compression from the filename suffix.
    encoding: str
        For text mode only
    errors: None or str
        Passed to TextIOWrapper in text mode
    name_function: function or None
        if opening a set of files for writing, those files do not yet exist,
        so we need to generate their names by formatting the urlpath for
        each sequence number
    num: int [1]
        if writing mode, number of files we expect to create (passed to
        name+function)
    protocol: str or None
        If given, overrides the protocol found in the URL.
    newline: bytes or None
        Used for line terminator in text mode. If None, uses system default;
        if blank, uses no translation.
    auto_mkdir: bool (True)
        If in write mode, this will ensure the target directory exists before
        writing, by calling ``fs.mkdirs(exist_ok=True)``.
    expand: bool
    **kwargs: dict
        Extra options that make sense to a particular storage connection, e.g.
        host, port, username, password, etc.

    Examples
    --------
    >>> files = open_files('2015-*-*.csv')  # doctest: +SKIP
    >>> files = open_files(
    ...     's3://bucket/2015-*-*.csv.gz', compression='gzip'
    ... )  # doctest: +SKIP

    Returns
    -------
    An ``OpenFiles`` instance, which is a list of ``OpenFile`` objects that can
    be used as a single context

    Notes
    -----
    For a full list of the available protocols and the implementations that
    they map across to see the latest online documentation:

    - For implementations built into ``fsspec`` see
      https://filesystem-spec.readthedocs.io/en/latest/api.html#built-in-implementations
    - For implementations in separate packages see
      https://filesystem-spec.readthedocs.io/en/latest/api.html#other-known-implementations
    """
    fs, fs_token, paths = get_fs_token_paths(
        urlpath,
        mode,
        num=num,
        name_function=name_function,
        storage_options=kwargs,
        protocol=protocol,
        expand=expand,
    )
    if fs.protocol == "file":
        fs.auto_mkdir = auto_mkdir
    elif "r" not in mode and auto_mkdir:
        parents = {fs._parent(path) for path in paths}
        for parent in parents:
            try:
                fs.makedirs(parent, exist_ok=True)
            except PermissionError:
                pass
    return OpenFiles(
        [
            OpenFile(
                fs,
                path,
                mode=mode,
                compression=compression,
                encoding=encoding,
                errors=errors,
                newline=newline,
            )
            for path in paths
        ],
        mode=mode,
        fs=fs,
    )


#: Fields of an ``fs.info()`` dict used to pin the identity of a planned file
_IDENTITY_FIELDS = (
    "size",
    "mtime",
    "created",
    "etag",
    "checksum",
    "md5",
    "version_id",
)


def _file_identity(info):
    """Extract the stable, comparable parts of an ``fs.info()`` dict."""
    return {k: info[k] for k in _IDENTITY_FIELDS if info.get(k) is not None}


def _close_open_files(open_files):
    """Close every given ``OpenFile``, returning any errors encountered."""
    errors = []
    for of in open_files:
        try:
            of.close()
        except Exception as exc:  # noqa: BLE001 - collected, not raised here
            errors.append(exc)
    return errors


class OpenFilesPlan:
    """A fixed, explicit plan for opening a batch of files as a single round.

    A plan is created by :func:`plan_open_files`, which expands any glob
    pattern *once* and pins the resulting concrete paths (in expansion
    order) together with the identity of each file at plan time. Files
    created after planning, even if they match the original pattern, never
    join this round silently.

    Using the plan as a context manager opens every pinned file (compression
    and text wrapping still apply to each concrete path) and records the
    per-item outcome in ``results``. If a pinned file was replaced or
    removed since planning, the ``on_change`` policy decides whether the
    round is rejected (``"raise"``), merely reported (``"warn"``) or opened
    anyway (``"ignore"``). If opening an individual item fails, the
    ``on_error`` policy decides whether the whole batch is rejected
    (``"fail"``, the default) or the successfully opened items are kept
    (``"keep"``).

    Plans are safe to serialize (pickle): the state never contains open
    file handles, only paths, identities, policies and results.

    Attributes
    ----------
    files: OpenFiles
        The pinned ``OpenFile`` objects, in expansion order.
    identities: dict
        Mapping of path to the identity recorded at plan time (``None`` if
        the file did not exist). Only populated in read mode.
    results: list of dict
        One entry per planned file, in expansion order, with keys ``path``,
        ``status`` ("pending", "opened" or "failed") and ``error`` (a string
        or None). Reset every time the plan context is entered.
    """

    #: Valid values for the ``on_change`` policy
    ON_CHANGE_VALUES = ("raise", "warn", "ignore")
    #: Valid values for the ``on_error`` policy
    ON_ERROR_VALUES = ("fail", "keep")

    def __init__(self, files, identities=None, on_change="raise", on_error="fail"):
        if on_change not in self.ON_CHANGE_VALUES:
            raise ValueError(
                f"on_change must be one of {self.ON_CHANGE_VALUES}, got {on_change!r}"
            )
        if on_error not in self.ON_ERROR_VALUES:
            raise ValueError(
                f"on_error must be one of {self.ON_ERROR_VALUES}, got {on_error!r}"
            )
        self.files = files
        self.identities = dict(identities or {})
        self.on_change = on_change
        self.on_error = on_error
        self.results = [self._new_result(of) for of in self.files]
        self._opened_files = []
        self._entered = False

    @staticmethod
    def _new_result(of):
        return {"path": of.path, "status": "pending", "error": None}

    @property
    def paths(self):
        """The concrete paths of this round, in expansion order."""
        return tuple(of.path for of in self.files)

    def check_changes(self):
        """Compare current file identities against the ones pinned at plan time.

        Returns
        -------
        list of (path, planned_identity, current_identity)
            One tuple per tracked path that was modified, removed or created
            since the plan was made. Empty if everything is unchanged.
        """
        fs = self.files.fs
        changes = []
        for of in self.files:
            if of.path not in self.identities:
                # untracked (e.g. write-mode plans)
                continue
            planned = self.identities[of.path]
            try:
                current = _file_identity(fs.info(of.path))
            except FileNotFoundError:
                current = None
            if current != planned:
                changes.append((of.path, planned, current))
        return changes

    def __enter__(self):
        if self._entered:
            raise RuntimeError("This plan's context is already active")
        changes = self.check_changes()
        if changes:
            message = "Files changed since the plan was created: " + ", ".join(
                path for path, _, _ in changes
            )
            if self.on_change == "raise":
                raise FileChangedError(message)
            elif self.on_change == "warn":
                warnings.warn(message, stacklevel=2)
        self.results = [self._new_result(of) for of in self.files]
        opened = []
        file_objects = []
        try:
            for i, of in enumerate(self.files):
                try:
                    fo = of.open()
                except Exception as exc:
                    self.results[i]["status"] = "failed"
                    self.results[i]["error"] = f"{type(exc).__name__}: {exc}"
                    if self.on_error == "fail":
                        raise
                else:
                    self.results[i]["status"] = "opened"
                    opened.append(of)
                    file_objects.append(fo)
        except BaseException:
            # Reject the whole batch: close whatever was opened, but a
            # cleanup failure must never mask the original error.
            for exc in _close_open_files(opened):
                logger.warning("Error closing file while aborting plan: %s", exc)
            raise
        self._opened_files = opened
        self._entered = True
        return file_objects

    def __exit__(self, exc_type, exc_value, traceback):
        self._entered = False
        opened, self._opened_files = self._opened_files, []
        errors = _close_open_files(opened)
        if errors:
            if exc_type is None:
                raise ExceptionGroup(
                    "Errors while closing files of an OpenFilesPlan", errors
                )
            # A failure is already propagating: cleanup errors must not
            # mask that main cause, so only log them.
            for exc in errors:
                logger.warning("Error closing planned file: %s", exc)
        return False

    def __getstate__(self):
        state = self.__dict__.copy()
        # A serialized plan must never carry open file handles
        state["_opened_files"] = []
        state["_entered"] = False
        return state

    def __len__(self):
        return len(self.files)

    def __iter__(self):
        return iter(self.files)

    def __getitem__(self, item):
        return self.files[item]

    def __repr__(self):
        return f"<OpenFilesPlan of {len(self)} pinned files>"


def plan_open_files(
    urlpath,
    mode="rb",
    compression=None,
    encoding="utf8",
    errors=None,
    name_function=None,
    num=1,
    protocol=None,
    newline=None,
    auto_mkdir=True,
    expand=True,
    on_change="raise",
    on_error="fail",
    **kwargs,
):
    """Plan a fixed batch of files to be opened together as one round.

    This is the explicit planning entry point for batch processing with
    ``open_files``: any glob pattern is expanded immediately, and the
    resulting concrete paths (in expansion order) and file identities are
    pinned in the returned :class:`OpenFilesPlan`. Files created after this
    call, even if they match the pattern, do not join the round.

    Parameters
    ----------
    urlpath, mode, compression, encoding, errors, name_function, num, \
protocol, newline, auto_mkdir, expand, **kwargs
        Exactly as for :func:`open_files`; compression and text-mode
        wrapping are applied to each concrete path when the plan is opened.
    on_change: "raise" | "warn" | "ignore"
        What to do when entering the plan context if a pinned file was
        replaced, removed or created since planning: reject the round
        (``"raise"``, the default, raising :class:`FileChangedError`),
        report the change with a warning and continue (``"warn"``), or
        proceed regardless (``"ignore"``). Identities are only tracked in
        read mode.
    on_error: "fail" | "keep"
        What to do if opening an individual item fails: reject the whole
        batch, closing anything already opened (``"fail"``, the default),
        or keep the successfully opened items and record the failures in
        ``plan.results`` (``"keep"``).

    Returns
    -------
    OpenFilesPlan
        A serializable plan; use it as a context manager to open the whole
        pinned batch at once.

    Examples
    --------
    >>> plan = plan_open_files("memory://bucket/*.csv")  # doctest: +SKIP
    >>> with plan as files:  # exactly the files pinned at plan time
    ...     data = [f.read() for f in files]  # doctest: +SKIP
    """
    files = open_files(
        urlpath,
        mode=mode,
        compression=compression,
        encoding=encoding,
        errors=errors,
        name_function=name_function,
        num=num,
        protocol=protocol,
        newline=newline,
        auto_mkdir=auto_mkdir,
        expand=expand,
        **kwargs,
    )
    identities = {}
    if "r" in mode:
        for of in files:
            try:
                identities[of.path] = _file_identity(files.fs.info(of.path))
            except FileNotFoundError:
                identities[of.path] = None
    return OpenFilesPlan(
        files, identities=identities, on_change=on_change, on_error=on_error
    )


def _un_chain(path, kwargs):
    # Avoid a circular import
    from fsspec.implementations.chained import ChainedFileSystem

    if "::" in path:
        x = re.compile(".*[^a-z]+.*")  # test for non protocol-like single word
        known_protocols = set(available_protocols())
        bits = []

        # split on '::', then ensure each bit has a protocol
        for p in path.split("::"):
            if p in known_protocols:
                bits.append(p + "://")
            elif "://" in p or x.match(p):
                bits.append(p)
            else:
                bits.append(p + "://")
    else:
        bits = [path]

    # [[url, protocol, kwargs], ...]
    out = []
    previous_bit = None
    kwargs = kwargs.copy()

    for bit in reversed(bits):
        protocol = kwargs.pop("protocol", None) or split_protocol(bit)[0] or "file"
        cls = get_filesystem_class(protocol)
        extra_kwargs = cls._get_kwargs_from_urls(bit)
        kws = kwargs.pop(protocol, {}).copy()

        if bit is bits[0]:
            kws.update(kwargs)

        kw = dict(
            **{k: v for k, v in extra_kwargs.items() if k not in kws or v != kws[k]},
            **kws,
        )
        bit = cls._strip_protocol(bit)

        if (
            "target_protocol" not in kw
            and issubclass(cls, ChainedFileSystem)
            and not bit
        ):
            # replace bit if we are chaining and no path given
            bit = previous_bit

        out.append((bit, protocol, kw))
        previous_bit = bit

    out.reverse()
    return out


def url_to_fs(url, **kwargs):
    """
    Turn fully-qualified and potentially chained URL into filesystem instance

    Parameters
    ----------
    url : str
        The fsspec-compatible URL
    **kwargs: dict
        Extra options that make sense to a particular storage connection, e.g.
        host, port, username, password, etc.

    Returns
    -------
    filesystem : FileSystem
        The new filesystem discovered from ``url`` and created with
        ``**kwargs``.
    urlpath : str
        The file-systems-specific URL for ``url``.
    """
    url = stringify_path(url)
    # non-FS arguments that appear in fsspec.open()
    # inspect could keep this in sync with open()'s signature
    known_kwargs = {
        "compression",
        "encoding",
        "errors",
        "expand",
        "mode",
        "name_function",
        "newline",
        "num",
    }
    kwargs = {k: v for k, v in kwargs.items() if k not in known_kwargs}
    chain = _un_chain(url, kwargs)
    inkwargs = {}
    # Reverse iterate the chain, creating a nested target_* structure
    for i, ch in enumerate(reversed(chain)):
        urls, protocol, kw = ch
        if i == len(chain) - 1:
            inkwargs = dict(**kw, **inkwargs)
            continue
        inkwargs["target_options"] = dict(**kw, **inkwargs)
        inkwargs["target_protocol"] = protocol
        inkwargs["fo"] = urls
    urlpath, protocol, _ = chain[0]
    fs = filesystem(protocol, **inkwargs)
    return fs, urlpath


DEFAULT_EXPAND = conf.get("open_expand", False)


def open(
    urlpath,
    mode="rb",
    compression=None,
    encoding="utf8",
    errors=None,
    protocol=None,
    newline=None,
    expand=None,
    **kwargs,
) -> OpenFile:
    """Given a path or paths, return one ``OpenFile`` object.

    Parameters
    ----------
    urlpath: string or list
        Absolute or relative filepath. Prefix with a protocol like ``s3://``
        to read from alternative filesystems. Should not include glob
        character(s).
    mode: 'rb', 'wt', etc.
    compression: string or None
        If given, open file using compression codec. Can either be a compression
        name (a key in ``fsspec.compression.compr``) or "infer" to guess the
        compression from the filename suffix.
    encoding: str
        For text mode only
    errors: None or str
        Passed to TextIOWrapper in text mode
    protocol: str or None
        If given, overrides the protocol found in the URL.
    newline: bytes or None
        Used for line terminator in text mode. If None, uses system default;
        if blank, uses no translation.
    expand: bool or None
        Whether to regard file paths containing special glob characters as needing
        expansion (finding the first match) or absolute. Setting False allows using
        paths which do embed such characters. If None (default), this argument
        takes its value from the DEFAULT_EXPAND module variable, which takes
        its initial value from the "open_expand" config value at startup, which will
        be False if not set.
    **kwargs: dict
        Extra options that make sense to a particular storage connection, e.g.
        host, port, username, password, etc.

    Examples
    --------
    >>> openfile = open('2015-01-01.csv')  # doctest: +SKIP
    >>> openfile = open(
    ...     's3://bucket/2015-01-01.csv.gz', compression='gzip'
    ... )  # doctest: +SKIP
    >>> with openfile as f:
    ...     df = pd.read_csv(f)  # doctest: +SKIP
    ...

    Returns
    -------
    ``OpenFile`` object.

    Notes
    -----
    For a full list of the available protocols and the implementations that
    they map across to see the latest online documentation:

    - For implementations built into ``fsspec`` see
      https://filesystem-spec.readthedocs.io/en/latest/api.html#built-in-implementations
    - For implementations in separate packages see
      https://filesystem-spec.readthedocs.io/en/latest/api.html#other-known-implementations
    """
    expand = DEFAULT_EXPAND if expand is None else expand
    out = open_files(
        urlpath=[urlpath],
        mode=mode,
        compression=compression,
        encoding=encoding,
        errors=errors,
        protocol=protocol,
        newline=newline,
        expand=expand,
        **kwargs,
    )
    if not out:
        raise FileNotFoundError(urlpath)
    return out[0]


def open_local(
    url: str | list[str] | Path | list[Path],
    mode: str = "rb",
    **storage_options: dict,
) -> str | list[str]:
    """Open file(s) which can be resolved to local

    For files which either are local, or get downloaded upon open
    (e.g., by file caching)

    Parameters
    ----------
    url: str or list(str)
    mode: str
        Must be read mode
    storage_options:
        passed on to FS for or used by open_files (e.g., compression)
    """
    if "r" not in mode:
        raise ValueError("Can only ensure local files when reading")
    of = open_files(url, mode=mode, **storage_options)
    if not getattr(of[0].fs, "local_file", False):
        raise ValueError(
            "open_local can only be used on a filesystem which"
            " has attribute local_file=True"
        )
    with of as files:
        paths = [f.name for f in files]
    if (isinstance(url, str) and not has_magic(url)) or isinstance(url, Path):
        return paths[0]
    return paths


def get_compression(urlpath, compression):
    if compression == "infer":
        compression = infer_compression(urlpath)
    if compression is not None and compression not in compr:
        raise ValueError(f"Compression type {compression} not supported")
    return compression


def split_protocol(urlpath):
    """Return protocol, path pair"""
    urlpath = stringify_path(urlpath)
    if "://" in urlpath:
        protocol, path = urlpath.split("://", 1)
        if len(protocol) > 1:
            # excludes Windows paths
            return protocol, path
    if urlpath.startswith("data:"):
        return urlpath.split(":", 1)
    return None, urlpath


def strip_protocol(urlpath):
    """Return only path part of full URL, according to appropriate backend"""
    protocol, _ = split_protocol(urlpath)
    cls = get_filesystem_class(protocol)
    return cls._strip_protocol(urlpath)


def expand_paths_if_needed(paths, mode, num, fs, name_function):
    """Expand paths if they have a ``*`` in them (write mode) or any of ``*?[]``
    in them (read mode).

    :param paths: list of paths
    mode: str
        Mode in which to open files.
    num: int
        If opening in writing mode, number of files we expect to create.
    fs: filesystem object
    name_function: callable
        If opening in writing mode, this callable is used to generate path
        names. Names are generated for each partition by
        ``urlpath.replace('*', name_function(partition_index))``.
    :return: list of paths
    """
    expanded_paths = []
    paths = list(paths)

    if "w" in mode or "x" in mode:  # write mode
        if sum(1 for p in paths if "*" in p) > 1:
            raise ValueError(
                "When writing data, only one filename mask can be specified."
            )
        num = max(num, len(paths))

        for curr_path in paths:
            if "*" in curr_path:
                # expand using name_function
                expanded_paths.extend(_expand_paths(curr_path, name_function, num))
            else:
                expanded_paths.append(curr_path)
        # if we generated more paths that asked for, trim the list
        if len(expanded_paths) > num:
            expanded_paths = expanded_paths[:num]

    else:  # read mode
        for curr_path in paths:
            if has_magic(curr_path):
                # expand using glob
                expanded_paths.extend(fs.glob(curr_path))
            else:
                expanded_paths.append(curr_path)

    return expanded_paths


def get_fs_token_paths(
    urlpath,
    mode="rb",
    num=1,
    name_function=None,
    storage_options=None,
    protocol=None,
    expand=True,
):
    """Filesystem, deterministic token, and paths from a urlpath and options.

    Parameters
    ----------
    urlpath: string or iterable
        Absolute or relative filepath, URL (may include protocols like
        ``s3://``), or globstring pointing to data.
    mode: str, optional
        Mode in which to open files.
    num: int, optional
        If opening in writing mode, number of files we expect to create.
    name_function: callable, optional
        If opening in writing mode, this callable is used to generate path
        names. Names are generated for each partition by
        ``urlpath.replace('*', name_function(partition_index))``.
    storage_options: dict, optional
        Additional keywords to pass to the filesystem class.
    protocol: str or None
        To override the protocol specifier in the URL
    expand: bool
        Expand string paths for writing, assuming the path is a directory
    """
    if isinstance(urlpath, (list, tuple, set)):
        if not urlpath:
            raise ValueError("empty urlpath sequence")
        urlpath0 = stringify_path(next(iter(urlpath)))
    else:
        urlpath0 = stringify_path(urlpath)
    storage_options = storage_options or {}
    if protocol:
        storage_options["protocol"] = protocol
    chain = _un_chain(urlpath0, storage_options or {})
    inkwargs = {}
    # Reverse iterate the chain, creating a nested target_* structure
    for i, ch in enumerate(reversed(chain)):
        urls, nested_protocol, kw = ch
        if i == len(chain) - 1:
            inkwargs = dict(**kw, **inkwargs)
            continue
        inkwargs["target_options"] = dict(**kw, **inkwargs)
        inkwargs["target_protocol"] = nested_protocol
        inkwargs["fo"] = urls
    paths, protocol, _ = chain[0]
    fs = filesystem(protocol, **inkwargs)
    if isinstance(urlpath, (list, tuple, set)):
        pchains = [
            _un_chain(stringify_path(u), storage_options or {})[0] for u in urlpath
        ]
        if len({pc[1] for pc in pchains}) > 1:
            raise ValueError(f"Protocol mismatch getting fs from {urlpath}")
        paths = [pc[0] for pc in pchains]
    else:
        paths = fs._strip_protocol(paths)
    if isinstance(paths, (list, tuple, set)):
        if expand:
            paths = expand_paths_if_needed(paths, mode, num, fs, name_function)
        elif not isinstance(paths, list):
            paths = list(paths)
    else:
        if ("w" in mode or "x" in mode) and expand:
            paths = _expand_paths(paths, name_function, num)
        elif "*" in paths:
            paths = [f for f in sorted(fs.glob(paths)) if not fs.isdir(f)]
        else:
            paths = [paths]

    return fs, fs._fs_token, paths


def _expand_paths(path, name_function, num):
    if isinstance(path, str):
        if path.count("*") > 1:
            raise ValueError("Output path spec must contain exactly one '*'.")
        elif "*" not in path:
            # paths are "/"-separated on every filesystem, including local ones
            path = posixpath.join(path, "*.part")

        if name_function is None:
            name_function = build_name_function(num - 1)

        paths = [path.replace("*", name_function(i)) for i in range(num)]
        if paths != sorted(paths):
            logger.warning(
                "In order to preserve order between partitions"
                " paths created with ``name_function`` should "
                "sort to partition order"
            )
    elif isinstance(path, (tuple, list)):
        assert len(path) == num
        paths = list(path)
    else:
        raise ValueError(
            "Path should be either\n"
            "1. A list of paths: ['foo.json', 'bar.json', ...]\n"
            "2. A directory: 'foo/\n"
            "3. A path with a '*' in it: 'foo.*.json'"
        )
    return paths


class PickleableTextIOWrapper(io.TextIOWrapper):
    """TextIOWrapper cannot be pickled. This solves it.

    Requires that ``buffer`` be pickleable, which all instances of
    AbstractBufferedFile are.
    """

    def __init__(
        self,
        buffer,
        encoding=None,
        errors=None,
        newline=None,
        line_buffering=False,
        write_through=False,
    ):
        self.args = buffer, encoding, errors, newline, line_buffering, write_through
        super().__init__(*self.args)

    def __reduce__(self):
        return PickleableTextIOWrapper, self.args
