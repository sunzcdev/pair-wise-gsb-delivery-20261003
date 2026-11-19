import hashlib

import pytest

import fsspec
from fsspec.exceptions import ChecksumUnsupported
from fsspec.implementations.dirfs import DirFileSystem
from fsspec.implementations.memory import MemoryFileSystem
from fsspec.utils import http_content_digest

DATA = b"some data to checksum \x00\x01" * 100


@pytest.fixture
def fs():
    fs = MemoryFileSystem()
    fs.pipe_file("/file", DATA)
    return fs


def test_default_report_is_version_identity(fs):
    report = fs.checksum_report("/file")
    assert report["level"] == "version"
    assert report["algorithm"] == "ukey"
    assert report["value"] == fs.ukey("/file")
    assert report["range"] is None
    assert report["version"] == fs.ukey("/file")


def test_ukey_and_checksum_return_styles_unchanged(fs):
    assert isinstance(fs.ukey("/file"), str)
    assert isinstance(fs.checksum("/file"), int)


def test_client_level_computes_real_digest(fs):
    report = fs.checksum_report("/file", level="client")
    assert report["level"] == "client"
    assert report["algorithm"] == "sha256"
    assert report["value"] == hashlib.sha256(DATA).hexdigest()
    assert report["range"] == (0, len(DATA))
    assert report["version"] == fs.ukey("/file")


def test_client_level_algorithm_and_byte_range(fs):
    report = fs.checksum_report(
        "/file", level="client", algorithm="md5", byte_range=(10, 25)
    )
    assert report["algorithm"] == "md5"
    assert report["value"] == hashlib.md5(DATA[10:35]).hexdigest()
    assert report["range"] == (10, 25)


def test_client_level_bad_algorithm(fs):
    with pytest.raises(ValueError):
        fs.checksum_report("/file", level="client", algorithm="not-an-algorithm")


def test_backend_level_unsupported_raises(fs):
    # a metadata ukey must not be passed off as a content digest
    with pytest.raises(ChecksumUnsupported):
        fs.checksum_report("/file", level="backend")


def test_unknown_level_raises(fs):
    with pytest.raises(ValueError):
        fs.checksum_report("/file", level="everything")


class BackendDigestFileSystem(MemoryFileSystem):
    checksum_capability = "backend"

    def _backend_checksum_report(self, path, algorithm=None):
        return {"algorithm": "md5", "value": "0" * 32, "range": (0, 4)}


def test_backend_level_report():
    fs = BackendDigestFileSystem()
    fs.pipe_file("/file", DATA)
    report = fs.checksum_report("/file")
    assert report["level"] == "backend"
    assert report["algorithm"] == "md5"
    assert report["value"] == "0" * 32
    assert report["range"] == (0, 4)
    assert report["version"] == fs.ukey("/file")


def test_composite_fs_passes_through_real_source():
    fs = BackendDigestFileSystem()
    fs.pipe_file("/dir/file", DATA)
    dirfs = DirFileSystem("/dir", fs)
    assert dirfs.checksum_capability == "backend"
    report = dirfs.checksum_report("file")
    assert report["level"] == "backend"
    assert report["algorithm"] == "md5"
    # capability of the wrapped fs, not the wrapper default
    assert dirfs.checksum_report("file", level="backend")["level"] == "backend"


def test_composite_fs_passthrough_of_unsupported():
    fs = MemoryFileSystem()
    fs.pipe_file("/dir/file", DATA)
    dirfs = DirFileSystem("/dir", fs)
    assert dirfs.checksum_capability == "version"
    with pytest.raises(ChecksumUnsupported):
        dirfs.checksum_report("file", level="backend")


def test_generic_fs_passes_through():
    fs = fsspec.filesystem("generic")
    mem = fsspec.filesystem("memory")
    mem.pipe_file("/file", DATA)
    report = fs.checksum_report("memory://file", level="client")
    assert report["value"] == hashlib.sha256(DATA).hexdigest()


def test_http_digest_plain_etag_is_md5():
    etag = '"%s"' % hashlib.md5(DATA).hexdigest()
    assert http_content_digest({"ETag": etag}) == ("md5", hashlib.md5(DATA).hexdigest())


def test_http_digest_composite_etag_is_not_md5():
    # S3 multipart-style ETag: must not be guessed as a content MD5
    assert http_content_digest({"ETag": '"9e2a4f7c1b8d3e5f6a0b1c2d3e4f5a6b-3"'}) is None


def test_http_digest_weak_etag_is_not_a_digest():
    assert (
        http_content_digest({"ETag": 'W/"9e2a4f7c1b8d3e5f6a0b1c2d3e4f5a6b"'}) is None
    )


def test_http_digest_content_md5_and_digest_header():
    assert http_content_digest({"Content-MD5": "abc123"}) == ("md5", "abc123")
    assert http_content_digest({"Digest": "sha-256=xyz=, md5=qwe="}) == (
        "sha256",
        "xyz=",
    )
    assert http_content_digest({}) is None
