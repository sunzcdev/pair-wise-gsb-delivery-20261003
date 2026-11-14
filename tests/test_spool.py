from __future__ import annotations

import io
import os
import warnings

import feedparser
import feedparser.encodings
from feedparser import api


FEED = b"<rss><channel><item><title>t\xc3\xa9xt</title></item></channel></rss>"


class NonSeekable:
    """A stream that cannot be seeked and returns short reads."""

    def __init__(self, data, chunk_size=3):
        if isinstance(data, bytes):
            self.buffer = io.BytesIO(data)
        else:
            self.buffer = io.StringIO(data)
        self.chunk_size = chunk_size
        self.closed = False

    def readable(self):
        return True

    def seekable(self):
        return False

    def read(self, size=-1):
        if size is None or size < 0:
            size = self.chunk_size
        return self.buffer.read(min(size, self.chunk_size))

    def close(self):
        self.closed = True


def test_non_seekable_bytes_are_spooled_and_replayable():
    source = NonSeekable(FEED)
    file = api._open_resource(source, {})
    try:
        # The source is not closed by the library.
        assert not source.closed
        assert file.seekable()
        assert file.read() == FEED

        # The same content must be visible on every pass.
        file.seek(0)
        assert file.read() == FEED
        file.seek(0)
        assert file.read() == FEED
    finally:
        file.close()


def test_non_seekable_text_are_spooled_and_replayable():
    source = NonSeekable(FEED.decode("utf-8"))
    file = api._open_resource(source, {})
    try:
        assert not source.closed
        assert file.seekable()
        assert file.read() == FEED.decode("utf-8")
        file.seek(0)
        assert file.read() == FEED.decode("utf-8")
    finally:
        file.close()


def test_small_content_stays_in_memory(monkeypatch):
    source = NonSeekable(FEED)
    file = api._spool_non_seekable_stream(source, {})
    try:
        # A SpooledTemporaryFile below the limit has not been rolled over.
        assert not getattr(file, "_rolled", False)
        assert file.read() == FEED
    finally:
        file.close()


def test_large_content_rolls_over_to_disk(monkeypatch):
    monkeypatch.setattr(api, "SPOOL_MEMORY_LIMIT", 64)
    monkeypatch.setattr(api, "SPOOL_READ_CHUNK_SIZE", 16)

    data = FEED * 10
    source = NonSeekable(data, chunk_size=7)
    file = api._spool_non_seekable_stream(source, {})
    try:
        assert getattr(file, "_rolled", False)
        temp_name = file._file.name
        assert os.path.exists(temp_name)
        assert file.read() == data
        file.seek(0)
        assert file.read() == data
    finally:
        file.close()
    assert not os.path.exists(temp_name)


def test_rolled_over_spool_is_closed_after_parse(monkeypatch):
    monkeypatch.setattr(api, "SPOOL_MEMORY_LIMIT", 64)
    monkeypatch.setattr(api, "SPOOL_READ_CHUNK_SIZE", 16)

    data = FEED * 10
    source = NonSeekable(data, chunk_size=7)
    result = feedparser.parse(source)

    assert result.entries
    assert not source.closed


def test_spool_is_closed_after_successful_parse():
    source = NonSeekable(FEED)
    result = feedparser.parse(source)

    assert result.entries
    assert not source.closed


def test_spool_is_closed_on_strict_to_loose_fallback(monkeypatch):
    monkeypatch.setattr(api, "_XML_AVAILABLE", False)
    source = NonSeekable(b"<rss><channel><item><title>loose</title>")
    result = feedparser.parse(source)

    assert result.entries
    assert not source.closed


def test_spool_is_closed_when_decoding_falls_back_to_loose(monkeypatch):
    # A long valid prefix followed by bytes that cannot be decoded with the
    # detected encoding makes convert_file_to_utf8() fall back, which must
    # still release the library-owned spool without ResourceWarnings.
    monkeypatch.setattr(api, "SPOOL_MEMORY_LIMIT", 128)
    data = (
        b"<rss><channel><item><title>"
        + b"a" * feedparser.encodings.CONVERT_FILE_PREFIX_LEN
        + "abcd😀".encode("utf-32")
        + b"</title></item></channel></rss>"
    )
    source = NonSeekable(data, chunk_size=11)

    with warnings.catch_warnings():
        warnings.simplefilter("error", ResourceWarning)
        result = feedparser.parse(source)

    assert result.entries
    assert not source.closed


def test_spool_is_closed_for_empty_input():
    source = NonSeekable(b"")
    result = feedparser.parse(source)

    assert not result.entries
    assert not source.closed


def test_caller_seekable_stream_is_not_closed():
    source = io.BytesIO(FEED)
    result = feedparser.parse(source)

    assert result.entries
    assert not source.closed
    # The caller's own stream is handed back untouched and still readable.
    source.seek(0)
    assert source.read() == FEED


class FailingSpooledTemporaryFile:
    def __init__(self, *args, **kwargs):
        self.closed = False

    def write(self, chunk):
        raise OSError("disk full")

    def seek(self, *args):
        raise OSError("disk full")

    def close(self):
        self.closed = True


def test_tempfile_write_failure_is_reported_as_bozo(monkeypatch):
    failing = FailingSpooledTemporaryFile()
    monkeypatch.setattr(
        api.tempfile, "SpooledTemporaryFile", lambda *a, **k: failing
    )

    result = {}
    source = NonSeekable(FEED)
    file = api._spool_non_seekable_stream(source, result)

    # The partial spool is discarded and no truncated content is returned.
    assert failing.closed
    assert file.read() == b""
    assert result["bozo"] is True
    assert isinstance(result["bozo_exception"], OSError)


def test_tempfile_write_failure_during_parse_returns_empty_feed(monkeypatch):
    monkeypatch.setattr(
        api.tempfile,
        "SpooledTemporaryFile",
        lambda *a, **k: FailingSpooledTemporaryFile(),
    )

    source = NonSeekable(FEED)
    result = feedparser.parse(source)

    assert result["bozo"] is True
    assert isinstance(result["bozo_exception"], OSError)
    assert not result["entries"]
    assert not source.closed
