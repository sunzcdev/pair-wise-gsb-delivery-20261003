from __future__ import annotations

import io
import logging
import posixpath
import re
from collections.abc import Mapping
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
        must all have the same protocol. A single file may also be given as a
        structured chain specification (a list of ``{"protocol", "path",
        ["kwargs"]}`` mappings or ``(protocol, path[, kwargs])`` tuples);
        see :func:`un_chain`.
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


def escape_chain_part(text):
    """Escape a path or name for inclusion as one segment of a chained URL.

    In a chained URL, segments are separated by a literal ``"::"``, so a name
    which itself contains a colon could produce a spurious separator. To
    embed such a name, every ``":"`` is replaced by ``"%3A"`` and every
    ``"%"`` by ``"%25"``, so the result round-trips exactly. Other
    characters - including query-like characters such as ``?``, ``#``, ``&``
    and ``=`` - are left alone: they are only meaningful to the layer which
    owns the segment, and each layer only ever interprets its own path and
    parameters.

    The inverse transformation is applied by the chain parser exactly once
    per segment, before the segment is handed to its filesystem
    implementation; inner segments are never decoded by outer layers.

    Parameters
    ----------
    text: str
        A path or name belonging to a single layer of the chain. This is
        *not* a URL: do not include a protocol prefix.

    Returns
    -------
    str
        ``text``, safe to embed in one segment of a chained URL.

    Examples
    --------
    >>> escape_chain_part("my::archive.zip")
    'my%3A%3Aarchive.zip'
    >>> "zip://inner::memory://" + escape_chain_part("my::archive.zip")
    'zip://inner::memory://my%3A%3Aarchive.zip'
    """
    return stringify_path(text).replace("%", "%25").replace(":", "%3A")


def _unescape_chain_bit(bit):
    """Reverse chain-level escaping in one segment of a chained URL.

    Only ``%3A`` (``:``) and ``%25`` (``%``) are interpreted, in a single
    left-to-right pass (so ``%253A`` decodes to the literal text ``%3A``).
    Any other ``%`` sequence is left untouched for the layer that owns the
    segment, ensuring no path is ever decoded twice.
    """
    if "%" not in bit:
        return bit
    out = []
    i = 0
    while i < len(bit):
        code = bit[i + 1 : i + 3].lower() if bit[i] == "%" else ""
        if code in ("3a", "25"):
            out.append(":" if code == "3a" else "%")
            i += 3
        else:
            out.append(bit[i])
            i += 1
    return "".join(out)


def _is_chain_segment(item):
    """Does *item* look like one structured chain segment?

    A segment is a mapping like ``{"protocol": ..., "path": ...}`` or a
    ``(protocol, path[, kwargs])`` tuple. Plain strings and path-likes are
    never segments.
    """
    if isinstance(item, Mapping):
        return True
    return (
        isinstance(item, tuple)
        and 2 <= len(item) <= 3
        and isinstance(item[0], str)
        and "://" not in item[0]
        and "::" not in item[0]
    )


def _is_structured_chain(path):
    """Is *path* a structured chain specification rather than a URL string?"""
    if isinstance(path, Mapping):
        return True
    return (
        isinstance(path, list)
        and bool(path)
        and all(_is_chain_segment(item) for item in path)
    )


def _parse_chain_segment(segment):
    """Validate one structured chain segment.

    Returns a ``(protocol, path, kwargs)`` triple. Raises before any
    filesystem is instantiated if the segment is malformed.
    """
    if isinstance(segment, Mapping):
        unknown = set(segment) - {"protocol", "path", "kwargs"}
        if unknown:
            raise ValueError(
                f"Unknown keys {sorted(unknown)} in chain segment; allowed keys "
                "are 'protocol', 'path' and 'kwargs'"
            )
        if "protocol" not in segment or "path" not in segment:
            raise ValueError(
                "Chain segment mappings require 'protocol' and 'path' keys; "
                f"got {sorted(segment)}"
            )
        protocol, path = segment["protocol"], segment["path"]
        kwargs = segment.get("kwargs") or {}
    elif isinstance(segment, tuple) and 2 <= len(segment) <= 3:
        protocol, path = segment[0], segment[1]
        kwargs = segment[2] if len(segment) == 3 else {}
        if kwargs is None:
            kwargs = {}
    else:
        raise TypeError(
            "Chain segments must be mappings {'protocol', 'path', ['kwargs']} "
            f"or (protocol, path[, kwargs]) tuples; got {segment!r}"
        )
    if (
        not isinstance(protocol, str)
        or not protocol
        or "://" in protocol
        or "::" in protocol
    ):
        raise ValueError(f"Invalid protocol in chain segment: {protocol!r}")
    path = stringify_path(path)
    if not isinstance(path, str):
        raise TypeError(f"Chain segment path must be a string, got {path!r}")
    if not isinstance(kwargs, Mapping):
        raise TypeError(f"Chain segment kwargs must be a mapping, got {kwargs!r}")
    return protocol, path, dict(kwargs)


def _un_chain_structured(chain, kwargs):
    """Turn a structured chain specification into the internal chain list.

    Each layer's path and options are used exactly as given: paths are never
    split on ``"::"`` or unescaped, and a segment's ``kwargs`` apply only to
    that segment's layer - nothing leaks to adjacent layers. As with the
    string form, top-level ``kwargs`` values keyed by a protocol name are
    merged into the matching layer, and any remaining top-level ``kwargs``
    go to the outermost (first) layer.
    """
    # Avoid a circular import
    from fsspec.implementations.chained import ChainedFileSystem

    if isinstance(chain, Mapping):
        chain = [chain]
    segments = [_parse_chain_segment(segment) for segment in chain]
    if not segments:
        raise ValueError("A structured chain must contain at least one segment")

    kwargs = kwargs.copy()
    # explicit segments override a top-level ``protocol=``
    kwargs.pop("protocol", None)

    # [[url, protocol, kwargs], ...]
    out = []
    previous_bit = None

    for i, (protocol, bit, segment_kwargs) in enumerate(reversed(segments)):
        cls = get_filesystem_class(protocol)
        kws = kwargs.pop(protocol, {}).copy()
        kws.update(segment_kwargs)

        if i == len(segments) - 1:
            # the outermost layer collects any leftover top-level options
            kws.update(kwargs)

        bit = cls._strip_protocol(bit)

        if (
            "target_protocol" not in kws
            and issubclass(cls, ChainedFileSystem)
            and not bit
        ):
            # replace bit if we are chaining and no path given
            bit = previous_bit

        out.append((bit, protocol, kws))
        previous_bit = bit

    out.reverse()
    return out


def _un_chain(path, kwargs):
    # Avoid a circular import
    from fsspec.implementations.chained import ChainedFileSystem

    if _is_structured_chain(path):
        return _un_chain_structured(path, kwargs)

    path = stringify_path(path)
    if not isinstance(path, str):
        if isinstance(path, (list, tuple)) and any(map(_is_chain_segment, path)):
            raise ValueError(
                "Cannot mix structured chain segments and plain paths in one list"
            )
        raise TypeError(f"Cannot parse a chained URL from {path!r}")

    if kwargs.get("protocol") or "::" not in path:
        # An explicit protocol means the path belongs to exactly one layer:
        # it is used as-is and never chain-split. This is how chained
        # implementations hand their own path to the next layer without it
        # being reinterpreted as another chain.
        bits = [path]
    else:
        bits = [_unescape_chain_bit(bit) for bit in path.split("::")]
        if any(not bit for bit in bits):
            raise ValueError(f"Empty segment in chained URL {path!r}")
        x = re.compile(".*[^a-z]+.*")  # test for non protocol-like single word
        known_protocols = set(available_protocols())
        classified = []

        # split on '::', then ensure each bit has a protocol
        for p in bits:
            if p in known_protocols:
                classified.append(p + "://")
            elif "://" in p or x.match(p):
                classified.append(p)
            else:
                classified.append(p + "://")
        bits = classified

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


def un_chain(path, storage_options=None):
    """Parse a (possibly chained) URL into per-layer components.

    Parameters
    ----------
    path: str or sequence
        Either a (possibly chained) URL string, or a structured chain: a
        sequence of segments, where each segment is a mapping like
        ``{"protocol": "zip", "path": "inner", "kwargs": {...}}``
        (``"kwargs"`` is optional) or a ``(protocol, path[, kwargs])``
        tuple. A single mapping is treated as a chain of one segment.
    storage_options: dict, optional
        Top-level options, interpreted exactly as for the string form:
        values keyed by a protocol name apply to that layer, and any
        remaining options go to the outermost (first) layer.

    Returns
    -------
    list of (path, protocol, kwargs)
        One tuple per layer, outermost first. ``path`` belongs to the layer
        named by ``protocol``, and ``kwargs`` are the options for that layer
        only.

    Notes
    -----
    In the string form, layers are separated by a literal ``"::"``. A name
    that itself contains a colon can be embedded with
    :func:`escape_chain_part`; the parser decodes ``%3A`` (``:``) and
    ``%25`` (``%``) exactly once per segment, so no layer ever decodes
    another layer's path. All parsing and validation happens before any
    filesystem instance is created, so malformed chains raise immediately
    rather than after a backend connection has been established.
    """
    return _un_chain(path, storage_options or {})


def url_to_fs(url, **kwargs):
    """
    Turn fully-qualified and potentially chained URL into filesystem instance

    Parameters
    ----------
    url : str or sequence
        The fsspec-compatible URL. May also be a structured chain: a
        sequence of ``{"protocol": ..., "path": ..., "kwargs": {...}}``
        mappings or ``(protocol, path[, kwargs])`` tuples, one per layer
        (see :func:`un_chain`). Segment paths are then used verbatim and
        segment kwargs apply only to their own layer.
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
        character(s). May also be a structured chain specification (a list
        of ``{"protocol", "path", ["kwargs"]}`` mappings or
        ``(protocol, path[, kwargs])`` tuples); see :func:`un_chain`.
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
        urlpath=urlpath if _is_structured_chain(urlpath) else [urlpath],
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
        ``s3://``), or globstring pointing to data. May also be a structured
        chain specification (a list of ``{"protocol", "path", ["kwargs"]}``
        mappings or ``(protocol, path[, kwargs])`` tuples); see
        :func:`un_chain`.
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
    structured = _is_structured_chain(urlpath)
    if structured:
        urlpath0 = urlpath
    elif isinstance(urlpath, (list, tuple, set)):
        if not urlpath:
            raise ValueError("empty urlpath sequence")
        if any(map(_is_chain_segment, urlpath)):
            raise ValueError(
                "Cannot mix structured chain segments and plain paths in one list"
            )
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
    if isinstance(urlpath, (list, tuple, set)) and not structured:
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
