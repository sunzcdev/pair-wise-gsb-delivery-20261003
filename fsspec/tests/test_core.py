import os
import pickle
import tempfile
import zipfile
from contextlib import contextmanager
from pathlib import Path

import pytest

import fsspec
from fsspec.core import (
    OpenFile,
    OpenFiles,
    _expand_paths,
    _unescape_chain_bit,
    escape_chain_part,
    expand_paths_if_needed,
    get_compression,
    get_fs_token_paths,
    open_files,
    open_local,
    un_chain,
)


@contextmanager
def memory_zip(data=None, name="archive.zip"):
    """A zip archive stored on the shared in-memory filesystem."""
    data = data or {"inner": b"hello"}
    fs = fsspec.filesystem("memory")
    with fs.open(name, "wb") as f:
        with zipfile.ZipFile(f, "w") as z:
            for k, v in data.items():
                z.writestr(k, v)
    try:
        yield fs, name
    finally:
        # remove the file directly from the store: names may contain
        # characters (e.g. "?") that glob-based removal would misread
        fs.store.pop(fs._strip_protocol(name), None)


@contextmanager
def tempzip(data=None):
    data = data or {}
    f = tempfile.mkstemp(suffix="zip")[1]
    with zipfile.ZipFile(f, mode="w") as z:
        for k, v in data.items():
            z.writestr(k, v)
    try:
        yield f
    finally:
        try:
            os.remove(f)
        except OSError:
            pass


@pytest.mark.parametrize(
    "path, name_function, num, out",
    [
        [["apath"], None, 1, ["apath"]],
        ["apath.*.csv", None, 1, ["apath.0.csv"]],
        ["apath.*.csv", None, 2, ["apath.0.csv", "apath.1.csv"]],
        ["a*", lambda x: "abc"[x], 2, ["aa", "ab"]],
        ["out", None, 2, ["out/0.part", "out/1.part"]],
        ["/out/", None, 1, ["/out/0.part"]],
    ],
)
def test_expand_paths(path, name_function, num, out):
    assert _expand_paths(path, name_function, num) == out


@pytest.mark.parametrize(
    "create_files, path, out",
    [
        [["apath"], "apath", ["apath"]],
        [["apath1"], "apath*", ["apath1"]],
        [["apath1", "apath2"], "apath*", ["apath1", "apath2"]],
        [["apath1", "apath2"], "apath[1]", ["apath1"]],
        [["apath1", "apath11"], "apath?", ["apath1"]],
    ],
)
def test_expand_paths_if_needed_in_read_mode(create_files, path, out):
    d = str(tempfile.mkdtemp())
    for f in create_files:
        f = os.path.join(d, f)
        open(f, "w").write("test")

    path = os.path.join(d, path)

    fs = fsspec.filesystem("file")
    res = expand_paths_if_needed([path], "r", 0, fs, None)
    assert [os.path.basename(p) for p in res] == out


@pytest.mark.parametrize("mode", ["w", "w+", "x", "x+"])
def test_expand_paths_if_needed_in_write_mode(mode):
    d = str(tempfile.mkdtemp())
    path = os.path.join(d, "part*.csv")

    fs = fsspec.filesystem("file")
    res = expand_paths_if_needed([path], mode, 2, fs, None)
    assert [os.path.basename(p) for p in res] == ["part0.csv", "part1.csv"]


def test_expand_error():
    with pytest.raises(ValueError):
        _expand_paths("*.*", None, 1)


@pytest.mark.parametrize("mode", ["w", "w+", "x", "x+"])
def test_expand_fs_token_paths(mode):
    assert len(get_fs_token_paths("path", mode, num=2, expand=True)[-1]) == 2


def test_get_fs_token_paths_protocol_mismatch():
    with pytest.raises(ValueError, match=r"from \['memory://a', 'file:///b'\]$"):
        get_fs_token_paths(["memory://a", "file:///b"])


def test_openfile_api(m):
    m.open("somepath", "wb").write(b"data")
    of = OpenFile(m, "somepath")
    assert str(of) == "<OpenFile 'somepath'>"
    f = of.open()
    assert f.read() == b"data"
    f.close()
    with OpenFile(m, "somepath", mode="rt") as f:
        assert f.read() == "data"


def test_openfile_open(m):
    of = OpenFile(m, "somepath", mode="wt")
    f = of.open()
    f.write("hello")
    assert m.size("somepath") == 0  # no flush yet
    of.close()
    assert m.size("somepath") == 5


@pytest.fixture
def recorded_local_files(monkeypatch):
    fs = fsspec.filesystem("file")
    opened = []
    original_open = fs.open

    def record_open(*args, **kwargs):
        stream = original_open(*args, **kwargs)
        opened.append(stream)
        return stream

    monkeypatch.setattr(fs, "open", record_open)
    try:
        yield fs, opened
    finally:
        for stream in opened:
            stream.close()


def test_openfile_failed_text_wrapper_closes_file(tmp_path, recorded_local_files):
    fs, opened = recorded_local_files
    path = tmp_path / "file.txt"
    path.write_text("content")
    file = OpenFile(fs, str(path), mode="rt", encoding="unknown-codec")
    with pytest.raises(LookupError):
        with file:
            pass
    assert len(opened) == 1
    assert opened[0].closed


def test_openfiles_failed_entry_closes_previous_files(tmp_path, recorded_local_files):
    fs, opened = recorded_local_files
    path = tmp_path / "file.txt"
    path.write_text("content")
    files = OpenFiles(
        [OpenFile(fs, str(path)), OpenFile(fs, str(tmp_path / "missing.txt"))], fs=fs
    )
    with pytest.raises(FileNotFoundError):
        with files:
            pass
    assert len(opened) == 1
    assert opened[0].closed


def test_openfile_flush_failure_still_closes_file(
    tmp_path, recorded_local_files, monkeypatch
):
    fs, _ = recorded_local_files
    file = OpenFile(fs, str(tmp_path / "file.txt"), mode="wb")
    stream = file.open()

    def fail_flush():
        raise OSError("flush failed")

    monkeypatch.setattr(stream, "flush", fail_flush)
    with pytest.raises(OSError, match="flush failed"):
        file.close()
    assert stream.closed


@pytest.mark.parametrize("failure", ["flush", "close"])
def test_openfiles_close_failure_still_closes_other_files(
    tmp_path, recorded_local_files, monkeypatch, failure
):
    fs, opened = recorded_local_files
    files = OpenFiles(
        [OpenFile(fs, str(tmp_path / name), mode="wb") for name in ["first", "second"]],
        mode="wb",
        fs=fs,
    )
    with pytest.raises(OSError, match="close failed"):
        with files as streams:
            streams[0].write(b"first")
            streams[1].write(b"second")
            original_close = streams[0].close

            def fail():
                if failure == "close":
                    was_closed = streams[0].closed
                    original_close()
                    if was_closed:
                        return
                raise OSError("close failed")

            monkeypatch.setattr(streams[0], failure, fail)
    assert all(stream.closed for stream in opened)
    assert (tmp_path / "second").read_bytes() == b"second"


@pytest.mark.parametrize("protocol", ["filecache", "simplecache"])
def test_cached_openfiles_close_failure_still_closes_other_readers(
    tmp_path, m, monkeypatch, protocol
):
    m.pipe({"first": b"first", "second": b"second"})
    files = fsspec.open_files(
        [f"{protocol}::memory:///{name}" for name in ["first", "second"]],
        cache_storage=str(tmp_path),
    )
    opened = []
    try:
        with pytest.raises(OSError, match="close failed"):
            with files as streams:
                opened = streams
                assert [stream.read() for stream in streams] == [b"first", b"second"]
                original_close = streams[0].close

                def fail_close():
                    was_closed = streams[0].closed
                    original_close()
                    if not was_closed:
                        raise OSError("close failed")

                monkeypatch.setattr(streams[0], "close", fail_close)
        assert all(stream.closed for stream in opened)
    finally:
        for stream in opened:
            stream.close()


def test_openfiles_preserves_close_order_for_duplicate_write_paths(tmp_path):
    path = tmp_path / "file.txt"
    with fsspec.open_files([str(path), str(path)], mode="wb") as files:
        files[0].write(b"first")
        files[1].write(b"second")
    assert path.read_bytes() == b"second"


def test_open_local_w_cache():
    d1 = str(tempfile.mkdtemp())
    f1 = os.path.join(d1, "f1")
    open(f1, "w").write("test1")
    d2 = str(tempfile.mkdtemp())
    fn = open_local(f"simplecache://{f1}", cache_storage=d2, target_protocol="file")
    assert isinstance(fn, str)
    assert open(fn).read() == "test1"
    assert d2 in fn


def test_open_local_w_magic():
    d1 = str(tempfile.mkdtemp())
    f1 = os.path.join(d1, "f1")
    open(f1, "w").write("test1")
    fn = open_local(os.path.join(d1, "f*"))
    assert len(fn) == 1
    assert isinstance(fn, list)


def test_open_local_w_list_of_str():
    d1 = str(tempfile.mkdtemp())
    f1 = os.path.join(d1, "f1")
    open(f1, "w").write("test1")
    fn = open_local([f1, f1])
    assert len(fn) == 2
    assert isinstance(fn, list)
    assert all(isinstance(elem, str) for elem in fn)


def test_open_local_w_path():
    d1 = str(tempfile.mkdtemp())
    f1 = os.path.join(d1, "f1")
    open(f1, "w").write("test1")
    p = Path(f1)
    fn = open_local(p)
    assert isinstance(fn, str)


def test_open_local_w_list_of_path():
    d1 = str(tempfile.mkdtemp())
    f1 = os.path.join(d1, "f1")
    open(f1, "w").write("test1")
    p = Path(f1)
    fn = open_local([p, p])
    assert len(fn) == 2
    assert isinstance(fn, list)
    assert all(isinstance(elem, str) for elem in fn)


def test_xz_lzma_compressions():
    pytest.importorskip("lzma")
    # Ensure that both 'xz' and 'lzma' compression names can be parsed
    assert get_compression("some_file.xz", "infer") == "xz"
    assert get_compression("some_file.xz", "xz") == "xz"
    assert get_compression("some_file.xz", "lzma") == "lzma"


def test_list():
    here = os.path.abspath(os.path.dirname(__file__))
    flist = os.listdir(here)
    plist = [os.path.join(here, p).replace("\\", "/") for p in flist]
    of = open_files(plist)
    assert len(of) == len(flist)
    assert [f.path for f in of] == plist


def test_open_expand(m, monkeypatch):
    m.pipe("/myfile", b"hello")
    with pytest.raises(FileNotFoundError, match=r"(?s)^/my\* not found.*expand=True"):
        with fsspec.open("memory://my*", expand=False):
            pass
    with fsspec.open("memory://my*", expand=True) as f:
        assert f.path == "/myfile"
    monkeypatch.setattr(fsspec.core, "DEFAULT_EXPAND", True)
    with fsspec.open("memory://my*") as f:
        assert f.path == "/myfile"


def test_pathobject(tmpdir):
    import pathlib

    tmpdir = str(tmpdir)
    plist_str = [os.path.join(str(tmpdir), f).replace("\\", "/") for f in ["a", "b"]]
    open(plist_str[0], "w").write("first file")
    open(plist_str[1], "w").write("second file")
    plist = [pathlib.Path(p) for p in plist_str]
    of = open_files(plist)
    assert len(of) == 2
    assert [f.path for f in of] == plist_str

    of = open_files(plist[0])
    assert len(of) == 1
    assert of[0].path == plist_str[0]
    with of[0] as f:
        assert f.read() == open(plist_str[0], "rb").read()


def test_automkdir(tmpdir):
    dir = os.path.join(str(tmpdir), "a")
    of = fsspec.open(os.path.join(dir, "afile"), "w", auto_mkdir=False)
    with pytest.raises(IOError):
        with of:
            pass

    dir = os.path.join(str(tmpdir), "b")
    of = fsspec.open(os.path.join(dir, "bfile"), "w", auto_mkdir=True)
    with of:
        pass

    assert "bfile" in os.listdir(dir)

    dir = os.path.join(str(tmpdir), "c")
    with pytest.raises(FileNotFoundError):
        of = fsspec.open(os.path.join(dir, "bfile"), "w", auto_mkdir=False)
        with of:
            pass


def test_automkdir_readonly(tmpdir):
    dir = os.path.join(str(tmpdir), "d")
    with pytest.raises(FileNotFoundError):
        of = fsspec.open(os.path.join(dir, "dfile"), "r")
        with of:
            pass


def test_openfile_pickle_newline():
    # GH#318
    test = fsspec.open(__file__, newline=b"")

    pickled = pickle.dumps(test)
    restored = pickle.loads(pickled)

    assert test.newline == restored.newline


def test_pickle_after_open_open():
    of = fsspec.open(__file__, mode="rt")
    test = of.open()
    of2 = pickle.loads(pickle.dumps(of))
    test2 = of2.open()
    test.close()

    assert not test2.closed
    of.close()
    of2.close()


# Define a list of special glob characters.
# Note that we need to escape some characters and also consider file system limitations.
# '*' and '?' are excluded because they are not valid for many file systems.
# Similarly, we're careful with '{', '}', and '@' as their special meaning is
# context-specific and might not be considered special for filenames.
# Add tests for more file systems and for more glob magic later
glob_magic_characters = ["[", "]", "!"]
if os.name != "nt":
    glob_magic_characters.extend(("*", "?"))  # not valid on Windows


@pytest.mark.parametrize("char", glob_magic_characters)
def test_open_file_read_with_special_characters(tmp_path, char):
    # Create a filename incorporating the special character
    file_name = f"test{char}.txt"
    file_path = tmp_path / file_name
    expected_content = "Hello, world!"

    with open(file_path, "w") as f:
        f.write(expected_content)

    with fsspec.open(file_path, "r") as f:
        actual_content = f.read()

    assert actual_content == expected_content


@pytest.mark.parametrize("char", glob_magic_characters)
def test_open_files_read_with_special_characters(tmp_path, char):
    # Create a filename incorporating the special character
    file_name = f"test{char}.txt"
    file_path = tmp_path / file_name
    expected_content = "Hello, world!"

    with open(file_path, "w") as f:
        f.write(expected_content)

    with fsspec.open_files(file_path, "r")[0] as f:
        actual_content = f.read()

    assert actual_content == expected_content


@pytest.mark.parametrize("char", glob_magic_characters)
def test_open_file_write_with_special_characters(tmp_path, char, monkeypatch):
    # Create a filename incorporating the special character
    file_name = f"test{char}.txt"
    file_path = tmp_path / file_name
    expected_content = "Hello, world!"

    with fsspec.open(file_path, "w", expand=False) as f:
        f.write(expected_content)

    with open(file_path, "r") as f:
        actual_content = f.read()

    monkeypatch.setattr(fsspec.core, "DEFAULT_EXPAND", False)
    with fsspec.open(file_path, "w") as f:
        f.write(expected_content * 2)

    with open(file_path, "r") as f:
        assert f.read() == actual_content * 2

    assert actual_content == expected_content


@pytest.mark.parametrize("char", glob_magic_characters)
def test_open_files_read_with_special_characters_unexpanded(tmp_path, char):
    # Create a filename incorporating the special character
    file_name = f"test{char}.txt"
    file_path = tmp_path / file_name
    expected_content = "Hello, world!"

    with open(file_path, "w") as f:
        f.write(expected_content)

    with fsspec.open_files(
        urlpath=[os.fspath(file_path)], mode="r", auto_mkdir=False, expand=False
    )[0] as f:
        actual_content = f.read()

    assert actual_content == expected_content


@pytest.mark.parametrize("char", glob_magic_characters)
def test_open_files_write_with_special_characters(tmp_path, char):
    # Create a filename incorporating the special character
    file_name = f"test{char}.txt"
    file_path = tmp_path / file_name
    expected_content = "Hello, world!"

    with fsspec.open_files(
        urlpath=[os.fspath(file_path)], mode="w", auto_mkdir=False, expand=False
    )[0] as f:
        f.write(expected_content)

    with open(file_path, "r") as f:
        actual_content = f.read()

    assert actual_content == expected_content


def test_mismatch():
    pytest.importorskip("s3fs")
    with pytest.raises(ValueError):
        open_files(["s3://test/path.csv", "/other/path.csv"])


def test_url_kwargs_chain(ftp_writable):
    host, port, username, password = ftp_writable
    data = b"hello"
    with fsspec.open(
        "ftp:///afile", "wb", host=host, port=port, username=username, password=password
    ) as f:
        f.write(data)

    with fsspec.open(
        f"simplecache::ftp://{username}:{password}@{host}:{port}//afile", "rb"
    ) as f:
        assert f.read() == data


def test_multi_context(tmpdir):
    fns = [os.path.join(tmpdir, fn) for fn in ["a", "b"]]
    files = open_files(fns, "wb")
    assert isinstance(files, OpenFiles)
    assert isinstance(files[0], OpenFile)
    assert len(files) == 2
    assert isinstance(files[:1], OpenFiles)
    assert len(files[:1]) == 1
    with files as of:
        assert len(of) == 2
        assert not of[0].closed
        assert of[0].name.endswith("a")
    assert of[0].closed
    assert repr(files) == "<List of 2 OpenFile instances>"


def test_not_local():
    with pytest.raises(ValueError, match="attribute local_file=True"):
        open_local("memory://afile")


def test_url_to_fs(ftp_writable):
    host, port, username, password = ftp_writable
    data = b"hello"
    with fsspec.open(f"ftp://{username}:{password}@{host}:{port}/afile", "wb") as f:
        f.write(data)
    fs, url = fsspec.core.url_to_fs(
        f"simplecache::ftp://{username}:{password}@{host}:{port}/afile"
    )
    assert url == "/afile"
    fs, url = fsspec.core.url_to_fs(f"ftp://{username}:{password}@{host}:{port}/afile")
    assert url == "/afile"

    with fsspec.open(f"ftp://{username}:{password}@{host}:{port}/afile.zip", "wb") as f:
        import zipfile

        with zipfile.ZipFile(f, "w") as z:
            with z.open("inner", "w") as f2:
                f2.write(b"hello")
        f.write(data)

    fs, url = fsspec.core.url_to_fs(
        f"zip://inner::ftp://{username}:{password}@{host}:{port}/afile.zip"
    )
    assert url == "inner"
    fs, url = fsspec.core.url_to_fs(
        f"simplecache::zip::ftp://{username}:{password}@{host}:{port}/afile.zip"
    )
    assert url == ""


def test_target_protocol_options(ftp_writable):
    host, port, username, password = ftp_writable
    data = {"afile": b"hello"}
    options = {"host": host, "port": port, "username": username, "password": password}
    with (
        tempzip(data) as lfile,
        fsspec.open("ftp:///archive.zip", "wb", **options) as f,
    ):
        f.write(open(lfile, "rb").read())
    with fsspec.open(
        "zip://afile",
        "rb",
        target_protocol="ftp",
        target_options=options,
        fo="archive.zip",
    ) as f:
        assert f.read() == data["afile"]


def test_chained_url(ftp_writable):
    host, port, username, password = ftp_writable
    data = {"afile": b"hello"}
    cls = fsspec.get_filesystem_class("ftp")
    fs = cls(host=host, port=port, username=username, password=password)
    with tempzip(data) as lfile:
        fs.put_file(lfile, "archive.zip")

    urls = [
        "zip://afile",
        "zip://afile::simplecache",
        "simplecache::zip://afile",
        "simplecache::zip://afile::simplecache",
    ]
    for url in urls:
        url += f"::ftp://{username}:{password}@{host}:{port}/archive.zip"
        with fsspec.open(url, "rb") as f:
            assert f.read() == data["afile"]


def test_automkdir_local():
    fs, _ = fsspec.core.url_to_fs("file://", auto_mkdir=True)
    assert fs.auto_mkdir is True


@pytest.mark.parametrize("entrypoint", ["open", "url_to_fs", "get_fs_token_paths"])
@pytest.mark.parametrize("chained", [False, True])
def test_protocol_options_not_mutated(entrypoint, chained, tmp_path):
    original = object()
    override = object()
    if chained:
        url = "filecache::memory://protocol-options"
        options = {"cache_storage": str(tmp_path), "cache_check": 10}
        kwargs = {"filecache": options, "memory": {"token": original}}
        explicit = {"cache_check": 20}
    else:
        url = "memory://protocol-options"
        options = {"token": original}
        kwargs = {"memory": options}
        explicit = {"token": override}
    expected = options.copy()

    def resolve(**storage_options):
        if entrypoint == "open":
            return fsspec.open(url, **storage_options).fs
        if entrypoint == "url_to_fs":
            return fsspec.core.url_to_fs(url, **storage_options)[0]
        return get_fs_token_paths(url, storage_options=storage_options)[0]

    fs = resolve(**kwargs, **explicit)
    if chained:
        assert fs.cache_check == 20
        assert fs.fs.storage_options["token"] is original
    else:
        assert fs.storage_options["token"] is override

    # Reusing caller-owned options must not retain the previous call's override.
    fs = resolve(**kwargs)
    if chained:
        assert fs.cache_check == 10
        assert fs.fs.storage_options["token"] is original
    else:
        assert fs.storage_options["token"] is original
    assert options == expected


def test_repeated_argument():
    pytest.importorskip("adlfs")
    from fsspec.core import url_to_fs

    fs, url = url_to_fs(
        "az://DIR@ACCOUNT.blob.core.windows.net/DATA",
        anon=False,
        account_name="ACCOUNT",
    )
    assert fs.storage_options == {"account_name": "ACCOUNT", "anon": False}
    with pytest.raises(TypeError):
        url_to_fs(
            "az://DIR@ACCOUNT.blob.core.windows.net/DATA",
            anon=False,
            account_name="OTHER",
        )


@pytest.mark.parametrize(
    "text",
    [
        "simple",
        "my::archive.zip",
        "a:b:c",
        "100%",
        "a%3Ab",
        "%25",
        "weird?x=1&y=2#frag",
        "dir/",
        "::",
        ":::a:::",
    ],
)
def test_chain_escape_roundtrip(text):
    assert _unescape_chain_bit(escape_chain_part(text)) == text


def test_chain_unescape_only_interprets_chain_escapes():
    # other percent-sequences belong to the layer owning the segment and
    # must pass through untouched
    assert _unescape_chain_bit("a%20b") == "a%20b"
    assert _unescape_chain_bit("a%3Ab") == "a:b"
    assert _unescape_chain_bit("a%3ab") == "a:b"
    assert _unescape_chain_bit("a%25b") == "a%b"
    # single left-to-right pass: %253A is the literal text "%3A"
    assert _unescape_chain_bit("a%253Ab") == "a%3Ab"
    # a bare % is not an escape
    assert _unescape_chain_bit("100%") == "100%"
    assert _unescape_chain_bit("100%3") == "100%3"


def test_un_chain_legacy_string():
    # old-style unambiguous chained URLs parse exactly as before
    assert un_chain("memory://a/b") == [("/a/b", "memory", {})]
    assert un_chain("simplecache::memory://a/b") == [
        ("/a/b", "simplecache", {}),
        ("/a/b", "memory", {}),
    ]
    out = un_chain("zip://inner::memory://archive.zip", {"memory": {"token": 1}})
    assert out == [("inner", "zip", {}), ("/archive.zip", "memory", {"token": 1})]


def test_inner_segment_not_double_decoded():
    # the chain parser decodes its own escapes (%25 -> %) exactly once; the
    # remaining %3A belongs to the memory layer and is left for it
    out = un_chain("simplecache::memory://a%253Ab")
    assert out[-1][0] == "/a%3Ab"


def test_structured_chain_equivalent_to_string():
    with memory_zip() as (_, name):
        string_url = f"zip://inner::memory://{name}"
        chains = [
            [
                {"protocol": "zip", "path": "inner"},
                {"protocol": "memory", "path": name},
            ],
            [("zip", "inner"), ("memory", name)],
        ]
        with fsspec.open(string_url, "rb") as f:
            expected = f.read()
        assert expected == b"hello"
        for chain in chains:
            with fsspec.open(chain, "rb") as f:
                assert f.read() == expected
            assert un_chain(chain) == un_chain(string_url)
        fs1, path1 = fsspec.core.url_to_fs(string_url)
        fs2, path2 = fsspec.core.url_to_fs(chains[0])
        assert path1 == path2 == "inner"
        assert fs1.protocol == fs2.protocol == "zip"


@pytest.mark.parametrize(
    "name",
    ["my::archive.zip", "archive.zip?x=1&y=2", "a::b#frag.zip", "100%.zip"],
)
def test_chained_url_with_escaped_names(name):
    # object names containing "::" or query-like text: the escaped string
    # form and the structured form open the same in-memory archive
    with memory_zip(name=name):
        url = "zip://inner::memory://" + escape_chain_part(name)
        chain = [
            {"protocol": "zip", "path": "inner"},
            {"protocol": "memory", "path": name},
        ]
        assert un_chain(url) == un_chain(chain)
        for spec in (url, chain):
            with fsspec.open(spec, "rb") as f:
                assert f.read() == b"hello"


def test_structured_chain_single_segment():
    with memory_zip():
        with fsspec.open({"protocol": "memory", "path": "archive.zip"}, "rb") as f:
            assert f.read(2) == b"PK"
        fs, path = fsspec.core.url_to_fs([("memory", "archive.zip", {"token": 1})])
        assert path == "/archive.zip"
        assert fs.storage_options["token"] == 1


def test_explicit_protocol_path_not_chain_split():
    # chained implementations hand their target path down with an explicit
    # protocol=; such paths must not be reinterpreted as a chain
    name = "my::archive.zip"
    with memory_zip(name=name):
        with fsspec.open("zip://inner", "rb", fo=name, target_protocol="memory") as f:
            assert f.read() == b"hello"


def test_structured_chain_option_scoping():
    chain = [
        {"protocol": "zip", "path": "inner"},
        {"protocol": "memory", "path": "archive.zip", "kwargs": {"token": "seg"}},
    ]
    # segment kwargs stay on their own layer; protocol-keyed top-level
    # options merge into the matching layer; leftovers go to the outermost
    assert un_chain(chain, {"memory": {"other": 1}, "cache_check": 10}) == [
        ("inner", "zip", {"cache_check": 10}),
        ("/archive.zip", "memory", {"token": "seg", "other": 1}),
    ]
    with memory_zip():
        fs, path = fsspec.core.url_to_fs(chain)
        assert path == "inner"
        assert "token" not in fs.storage_options  # nothing leaked to zip
        assert fs.of.fs.storage_options["token"] == "seg"  # memory got it


def test_mixed_structured_and_plain_paths_raise():
    with pytest.raises(ValueError, match="[Mm]ix"):
        get_fs_token_paths([{"protocol": "memory", "path": "a"}, "memory://b"])
    with pytest.raises(ValueError, match="[Mm]ix"):
        fsspec.core.url_to_fs([("memory", "a"), "memory://b"])


def test_chain_parse_errors_before_backend_connection(monkeypatch):
    def fail(*args, **kwargs):
        raise AssertionError("filesystem() must not be called for invalid chains")

    monkeypatch.setattr(fsspec.core, "filesystem", fail)

    for url in ["zip::::memory://x", "zip://inner::memory://x::", "::memory://x"]:
        with pytest.raises(ValueError, match="[Ee]mpty segment"):
            fsspec.core.url_to_fs(url)

    bad_chains = [
        [],
        [{"protocol": "zip"}],  # missing path
        [{"path": "inner"}],  # missing protocol
        [{"protocol": "zip", "path": "inner", "bogus": 1}],  # unknown key
        [{"protocol": "zip://x", "path": "inner"}],  # bad protocol
        [{"protocol": "zip", "path": "inner", "kwargs": [1]}],  # kwargs not mapping
        [("zip",)],  # tuple too short
        [("zip", "inner", "not-a-mapping")],
        [42],  # not a segment at all
        [{"protocol": "no-such-protocol", "path": "x"}],  # unknown protocol
    ]
    for chain in bad_chains:
        with pytest.raises((TypeError, ValueError)):
            fsspec.core.url_to_fs(chain)
