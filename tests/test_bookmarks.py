"""
Tests for resumable reading (bookmarks).
"""

import hashlib
import io
import json
import os
import pathlib

import attrs
import pytest

import jsonlines
from jsonlines.jsonlines import BOOKMARK_FORMAT_VERSION


SAMPLE = b'{"a": 1}\n{"b": 2}\n{"c": 3}\n'
LINE1 = b'{"a": 1}\n'
LINE2 = b'{"b": 2}\n'
PREFIX2 = LINE1 + LINE2


def make_reader(data: bytes = SAMPLE) -> jsonlines.Reader:
    return jsonlines.Reader(io.BytesIO(data))


def test_bookmark_requires_binary_seekable_stream() -> None:
    for stream in (io.StringIO("1\n"), ["1"], [b"1"]):
        reader = jsonlines.Reader(stream)
        reader.read()
        with pytest.raises(jsonlines.BookmarkError):
            reader.bookmark()


def test_bookmark_before_any_read() -> None:
    reader = make_reader()
    bookmark = reader.bookmark()
    assert bookmark.offset == 0
    assert bookmark.lineno == 0
    assert bookmark.size == len(SAMPLE)
    assert bookmark.algorithm == "sha256"
    assert bookmark.digest == hashlib.sha256(b"").hexdigest()
    assert bookmark.version == BOOKMARK_FORMAT_VERSION


def test_bookmark_after_each_record() -> None:
    reader = make_reader()
    assert reader.read() == {"a": 1}
    bm1 = reader.bookmark()
    assert bm1.offset == len(LINE1)
    assert bm1.lineno == 1
    assert bm1.digest == hashlib.sha256(LINE1).hexdigest()

    assert reader.read() == {"b": 2}
    bm2 = reader.bookmark()
    assert bm2.offset == len(PREFIX2)
    assert bm2.lineno == 2
    assert bm2.digest == hashlib.sha256(PREFIX2).hexdigest()
    assert bm2.size == len(SAMPLE)


def test_bookmark_after_eof() -> None:
    reader = make_reader()
    assert list(reader) == [{"a": 1}, {"b": 2}, {"c": 3}]
    bookmark = reader.bookmark()
    assert bookmark.offset == len(SAMPLE)
    assert bookmark.lineno == 3
    assert bookmark.digest == hashlib.sha256(SAMPLE).hexdigest()


def test_restore_does_not_redeliver_records() -> None:
    reader = make_reader()
    reader.read()
    reader.read()
    bookmark = reader.bookmark()

    restored = jsonlines.Reader.restore(io.BytesIO(SAMPLE), bookmark)
    assert list(restored) == [{"c": 3}]
    assert restored.bookmark().lineno == 3


def test_restore_seeks_to_record_boundary() -> None:
    reader = make_reader()
    reader.read()
    bookmark = reader.bookmark()

    fp = io.BytesIO(SAMPLE)
    restored = jsonlines.Reader.restore(fp, bookmark)
    assert fp.tell() == bookmark.offset
    assert restored.read() == {"b": 2}
    assert fp.tell() == len(PREFIX2)


def test_restore_accepts_appended_data() -> None:
    reader = make_reader()
    reader.read()
    bookmark = reader.bookmark()

    grown = SAMPLE + b'{"d": 4}\n'
    restored = jsonlines.Reader.restore(io.BytesIO(grown), bookmark)
    assert list(restored) == [{"b": 2}, {"c": 3}, {"d": 4}]
    # The appended data is reflected in a fresh bookmark.
    new_bookmark = restored.bookmark()
    assert new_bookmark.size == len(grown)
    assert new_bookmark.digest == hashlib.sha256(grown).hexdigest()


def test_restore_rejects_changed_prefix() -> None:
    reader = make_reader()
    reader.read()
    reader.read()
    bookmark = reader.bookmark()

    tampered = b'{"a": 999}\n' + SAMPLE[len(LINE1):]
    with pytest.raises(jsonlines.BookmarkError, match="prefix"):
        jsonlines.Reader.restore(io.BytesIO(tampered), bookmark)


def test_restore_rejects_truncation_before_offset() -> None:
    reader = make_reader()
    reader.read()
    reader.read()
    bookmark = reader.bookmark()

    with pytest.raises(jsonlines.BookmarkError, match="truncated"):
        jsonlines.Reader.restore(io.BytesIO(LINE1), bookmark)


def test_restore_rejects_text_stream() -> None:
    bookmark = make_reader().bookmark()
    text_stream: object = io.StringIO(SAMPLE.decode())
    with pytest.raises(jsonlines.BookmarkError):
        jsonlines.Reader.restore(text_stream, bookmark)  # type: ignore[arg-type]


def test_restore_rejects_non_seekable_stream() -> None:
    class NonSeekable(io.RawIOBase):
        def readable(self) -> bool:
            return True

        def seekable(self) -> bool:
            return False

    bookmark = make_reader().bookmark()
    non_seekable: object = NonSeekable()
    with pytest.raises(jsonlines.BookmarkError, match="seekable"):
        jsonlines.Reader.restore(non_seekable, bookmark)  # type: ignore[arg-type]


def test_no_bookmark_after_invalid_line() -> None:
    reader = jsonlines.Reader(io.BytesIO(b"1\nbroken\n3\n"))
    assert reader.read() == 1
    good = reader.bookmark()
    with pytest.raises(jsonlines.InvalidLineError):
        reader.read()
    # The stream now sits past the last committed record: no bookmark allowed.
    with pytest.raises(jsonlines.BookmarkError, match="record boundary"):
        reader.bookmark()
    # The previously created bookmark remains valid and unchanged.
    assert good.offset == len(b"1\n")
    assert good.lineno == 1
    # Reading the next good record re-establishes a valid boundary.
    assert reader.read() == 3
    recovered = reader.bookmark()
    assert recovered.lineno == 3
    assert recovered.offset == len(b"1\nbroken\n3\n")


def test_no_bookmark_inside_half_utf8_character() -> None:
    data = b"1\n\xff\n3\n"
    reader = jsonlines.Reader(io.BytesIO(data))
    assert reader.read() == 1
    good = reader.bookmark()
    with pytest.raises(jsonlines.InvalidLineError):
        reader.read()
    with pytest.raises(jsonlines.BookmarkError):
        reader.bookmark()
    # The committed boundary is still safely after record 1.
    assert good.offset == 2
    assert good.digest == hashlib.sha256(b"1\n").hexdigest()


def test_skip_empty_counts_physical_lines() -> None:
    data = b"\n1\n3\n"
    reader = jsonlines.Reader(io.BytesIO(data))
    assert reader.read(skip_empty=True) == 1  # empty line 1 + line 2
    bookmark = reader.bookmark()
    assert bookmark.lineno == 2
    assert bookmark.offset == len(b"\n1\n")
    assert bookmark.digest == hashlib.sha256(b"\n1\n").hexdigest()

    restored = jsonlines.Reader.restore(io.BytesIO(data), bookmark)
    assert list(restored) == [3]
    assert restored.bookmark().lineno == 3


def test_bookmark_multibyte_utf8_boundary() -> None:
    line = '"’"\n'.encode("utf-8")  # multi-byte character plus newline
    data = line + b"2\n"
    reader = jsonlines.Reader(io.BytesIO(data))
    assert reader.read() == "’"
    bookmark = reader.bookmark()
    assert bookmark.offset == len(line)
    restored = jsonlines.Reader.restore(io.BytesIO(data), bookmark)
    assert restored.read() == 2


def test_caller_keeps_ownership_of_passed_stream() -> None:
    fp = io.BytesIO(SAMPLE)
    reader = jsonlines.Reader(fp)
    reader.read()
    bookmark = reader.bookmark()
    reader.close()
    assert not fp.closed  # passed-in streams are never closed by the reader

    fp2 = io.BytesIO(SAMPLE)
    restored = jsonlines.Reader.restore(fp2, bookmark)
    assert restored._should_close_fp is False
    restored.close()
    assert not fp2.closed


def test_restore_from_path_closes_opened_file(tmp_path: "pathlib.Path") -> None:
    path = tmp_path / "data.jsonl"
    path.write_bytes(SAMPLE)

    reader = jsonlines.Reader(io.BytesIO(SAMPLE))
    reader.read()
    bookmark = reader.bookmark()

    with jsonlines.Reader.restore(os.fspath(path), bookmark) as restored:
        assert restored._should_close_fp is True
        assert list(restored) == [{"b": 2}, {"c": 3}]
    assert restored._closed


def test_restore_from_pathlib_path(tmp_path: "pathlib.Path") -> None:
    path = tmp_path / "data.jsonl"
    path.write_bytes(SAMPLE)
    bookmark = jsonlines.Reader(io.BytesIO(SAMPLE)).bookmark()
    with jsonlines.Reader.restore(path, bookmark) as restored:
        assert list(restored) == [{"a": 1}, {"b": 2}, {"c": 3}]


def test_restore_passes_custom_loads() -> None:
    def custom_loads(line: object) -> str:
        return "decoded"

    bookmark = make_reader().bookmark()
    restored = jsonlines.Reader.restore(
        io.BytesIO(SAMPLE), bookmark, loads=custom_loads
    )
    assert restored.read() == "decoded"


def test_bookmark_serialization_roundtrip() -> None:
    reader = make_reader()
    reader.read()
    bookmark = reader.bookmark()

    assert jsonlines.Bookmark.from_dict(bookmark.to_dict()) == bookmark
    assert jsonlines.Bookmark.from_json(bookmark.to_json()) == bookmark
    assert json.loads(bookmark.to_json()) == bookmark.to_dict()


def test_bookmark_from_dict_ignores_unknown_fields() -> None:
    reader = make_reader()
    reader.read()
    data = reader.bookmark().to_dict()
    data["future_field"] = "whatever"
    restored = jsonlines.Bookmark.from_dict(data)
    assert restored.offset == data["offset"]


def test_invalid_bookmark_payloads() -> None:
    with pytest.raises(jsonlines.BookmarkError, match="version"):
        jsonlines.Bookmark.from_dict({"offset": 1})
    with pytest.raises(jsonlines.BookmarkError, match="version"):
        jsonlines.Bookmark.from_dict({"version": 999})
    with pytest.raises(jsonlines.BookmarkError, match="json"):
        jsonlines.Bookmark.from_json("not json")
    with pytest.raises(jsonlines.BookmarkError, match="object"):
        jsonlines.Bookmark.from_json("[1, 2]")


def test_restore_rejects_unknown_hash_algorithm() -> None:
    reader = make_reader()
    reader.read()
    bookmark = attrs.evolve(reader.bookmark(), algorithm="md5")
    with pytest.raises(jsonlines.BookmarkError, match="algorithm"):
        jsonlines.Reader.restore(io.BytesIO(SAMPLE), bookmark)


def test_bookmark_name_is_informational() -> None:
    reader = jsonlines.Reader(io.BytesIO(SAMPLE))
    bookmark = reader.bookmark()
    assert "BytesIO" in (bookmark.name or "")


def test_bookmark_is_frozen() -> None:
    bookmark = make_reader().bookmark()
    with pytest.raises(AttributeError):
        bookmark.offset = 123  # type: ignore[misc]  # noqa: F841


def test_bookmark_on_closed_reader() -> None:
    reader = make_reader()
    reader.close()
    with pytest.raises(RuntimeError):
        reader.bookmark()


def test_reader_on_non_zero_offset_stream() -> None:
    # A reader wrapping a stream that is already past position 0 must still
    # account for the skipped bytes in the bookmark prefix digest.
    fp = io.BytesIO(SAMPLE)
    fp.seek(len(LINE1))
    reader = jsonlines.Reader(fp)
    bookmark = reader.bookmark()
    assert bookmark.offset == len(LINE1)
    assert bookmark.digest == hashlib.sha256(LINE1).hexdigest()
    assert reader.read() == {"b": 2}
    bookmark2 = reader.bookmark()
    assert bookmark2.offset == len(PREFIX2)
    assert bookmark2.digest == hashlib.sha256(PREFIX2).hexdigest()


def test_chained_restore_keeps_digest_consistent() -> None:
    # First run: process one record and persist bookmark.
    reader = jsonlines.Reader(io.BytesIO(SAMPLE))
    reader.read()
    bookmark = reader.bookmark()

    # Second run: restore, process one more, persist again.
    reader2 = jsonlines.Reader.restore(io.BytesIO(SAMPLE), bookmark)
    reader2.read()
    bookmark2 = reader2.bookmark()
    assert bookmark2.offset == len(PREFIX2)
    assert bookmark2.digest == hashlib.sha256(PREFIX2).hexdigest()

    # Third run: restore from the second bookmark and finish.
    reader3 = jsonlines.Reader.restore(io.BytesIO(SAMPLE), bookmark2)
    assert list(reader3) == [{"c": 3}]
