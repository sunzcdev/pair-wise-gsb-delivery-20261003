from __future__ import annotations

import logging
import sys
from pathlib import Path
from typing import TYPE_CHECKING, BinaryIO
from unittest.mock import MagicMock, call, patch

import pytest

from pip._vendor.urllib3.connectionpool import HTTPConnectionPool
from pip._vendor.urllib3.exceptions import ProtocolError, ProxyError, ReadTimeoutError

from pip._internal.exceptions import (
    ConnectionFailedError,
    ConnectionTimeoutError,
    DownloadDestinationCollision,
    IncompleteDownloadError,
    ProxyConnectionError,
    SSLMissingError,
)
from pip._internal.models.link import Link
from pip._internal.network.download import (
    Downloader,
    _FileDownload,
    _get_http_response_size,
    _log_download,
    parse_content_disposition,
    sanitize_content_filename,
)
from pip._internal.network.session import PipSession
from pip._internal.network.utils import HEADERS

from tests.lib.requests_mocks import BrokenStream, MockResponse
from tests.lib.server import Body, MockServer

if TYPE_CHECKING:
    from _typeshed.wsgi import StartResponse, WSGIEnvironment


@pytest.mark.parametrize(
    "url, headers, from_cache, range_start, expected",
    [
        (
            "http://example.com/foo.tgz",
            {},
            False,
            None,
            "Downloading foo.tgz",
        ),
        (
            "http://example.com/foo.tgz",
            {"content-length": "2"},
            False,
            None,
            "Downloading foo.tgz (2 bytes)",
        ),
        (
            "http://example.com/foo.tgz",
            {"content-length": "2"},
            True,
            None,
            "Using cached foo.tgz (2 bytes)",
        ),
        (
            "https://files.pythonhosted.org/foo.tgz",
            {},
            False,
            None,
            "Downloading foo.tgz",
        ),
        (
            "https://files.pythonhosted.org/foo.tgz",
            {"content-length": "2"},
            False,
            None,
            "Downloading foo.tgz (2 bytes)",
        ),
        (
            "https://files.pythonhosted.org/foo.tgz",
            {"content-length": "2"},
            True,
            None,
            "Using cached foo.tgz",
        ),
        (
            "http://example.com/foo.tgz",
            {"content-length": "200"},
            False,
            100,
            "Resuming download foo.tgz (100 bytes/200 bytes)",
        ),
    ],
)
def test_log_download(
    caplog: pytest.LogCaptureFixture,
    url: str,
    headers: dict[str, str],
    from_cache: bool,
    range_start: int | None,
    expected: str,
) -> None:
    caplog.set_level(logging.INFO)
    resp = MockResponse(b"")
    resp.url = url
    resp.headers.update(headers)
    if from_cache:
        resp.from_cache = from_cache
    link = Link(url)
    total_length = _get_http_response_size(resp)
    _log_download(
        resp,
        link,
        progress_bar="on",
        total_length=total_length,
        range_start=range_start,
    )

    assert len(caplog.records) == 1
    record = caplog.records[0]
    assert record.levelname == "INFO"
    assert expected in record.message


@pytest.mark.parametrize(
    "content_length, expected",
    [
        ("0", 0),
        ("36", 36),
        ("", None),
        ("not-a-number", None),
        # A negative length must not be passed through: it would make
        # _FileDownload.is_incomplete() treat a truncated download as complete.
        ("-1", None),
    ],
)
def test_get_http_response_size(content_length: str, expected: int | None) -> None:
    resp = MockResponse(b"")
    resp.headers["content-length"] = content_length
    assert _get_http_response_size(resp) == expected


@pytest.mark.parametrize(
    "filename, expected",
    [
        ("dir/file", "file"),
        ("../file", "file"),
        ("../../file", "file"),
        ("../", ""),
        ("../..", ".."),
        ("/", ""),
    ],
)
def test_sanitize_content_filename(filename: str, expected: str) -> None:
    """
    Test inputs where the result is the same for Windows and non-Windows.
    """
    assert sanitize_content_filename(filename) == expected


@pytest.mark.parametrize(
    "filename, win_expected, non_win_expected",
    [
        ("dir\\file", "file", "dir\\file"),
        ("..\\file", "file", "..\\file"),
        ("..\\..\\file", "file", "..\\..\\file"),
        ("..\\", "", "..\\"),
        ("..\\..", "..", "..\\.."),
        ("\\", "", "\\"),
    ],
)
def test_sanitize_content_filename__platform_dependent(
    filename: str, win_expected: str, non_win_expected: str
) -> None:
    """
    Test inputs where the result is different for Windows and non-Windows.
    """
    if sys.platform == "win32":
        expected = win_expected
    else:
        expected = non_win_expected
    assert sanitize_content_filename(filename) == expected


@pytest.mark.parametrize(
    "content_disposition, default_filename, expected",
    [
        ('attachment;filename="../file"', "df", "file"),
    ],
)
def test_parse_content_disposition(
    content_disposition: str, default_filename: str, expected: str
) -> None:
    actual = parse_content_disposition(content_disposition, default_filename)
    assert actual == expected


@pytest.mark.parametrize(
    "resume_retries,mock_responses,expected_resume_args,expected_bytes",
    [
        # If content-length is not provided, the download will
        # always "succeed" since we don't have a way to check if
        # the download is complete.
        (
            0,
            [({}, 200, b"0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89")],
            [],
            b"0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89",
        ),
        # Complete download (content-length matches body)
        (
            0,
            [({"content-length": "36"}, 200, b"0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89")],
            [],
            b"0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89",
        ),
        # Incomplete download without resume retries
        (
            0,
            [({"content-length": "36"}, 200, b"0cfa7e9d-1868-4dd7-9fb3-")],
            [],
            None,
        ),
        # Incomplete download with resume retries
        (
            5,
            [
                ({"content-length": "36"}, 200, b"0cfa7e9d-1868-4dd7-9fb3-"),
                ({"content-length": "12"}, 206, b"f2561d5dfd89"),
            ],
            [(24, None)],
            b"0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89",
        ),
        # If the server responds with 200 (e.g. no range header support or the file
        # has changed between the requests) the downloader should restart instead of
        # attempting to resume. The downloaded file should not be affected.
        (
            5,
            [
                ({"content-length": "36"}, 200, b"0cfa7e9d-1868-4dd7-9fb3-"),
                ({"content-length": "36"}, 200, b"0cfa7e9d-1868-"),
                (
                    {"content-length": "36"},
                    200,
                    b"0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89",
                ),
            ],
            [(24, None), (14, None)],
            b"0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89",
        ),
        # File size could change between requests. Make sure this is handled correctly.
        (
            5,
            [
                ({"content-length": "36"}, 200, b"0cfa7e9d-1868-4dd7-9fb3-"),
                (
                    {"content-length": "40"},
                    200,
                    b"new-0cfa7e9d-1868-4dd7-9fb3-f2561d5d",
                ),
                ({"content-length": "4"}, 206, b"fd89"),
            ],
            [(24, None), (36, None)],
            b"new-0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89",
        ),
        # The downloader should fail after N resume_retries attempts.
        # This prevents the downloader from getting stuck if the connection
        # is unstable and the server does NOT support range requests.
        (
            1,
            [
                ({"content-length": "36"}, 200, b"0cfa7e9d-1868-4dd7-9fb3-"),
                ({"content-length": "36"}, 200, b"0cfa7e9d-1868-"),
            ],
            [(24, None)],
            None,
        ),
        # The downloader should use the If-Range header to make the range
        # request conditional if it is possible to check for modifications
        # (e.g. if we know the creation time of the initial response).
        (
            5,
            [
                (
                    {
                        "content-length": "36",
                        "last-modified": "Wed, 21 Oct 2015 07:28:00 GMT",
                    },
                    200,
                    b"0cfa7e9d-1868-4dd7-9fb3-",
                ),
                (
                    {
                        "content-length": "42",
                        "last-modified": "Wed, 21 Oct 2015 07:30:00 GMT",
                    },
                    200,
                    b"new-0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89",
                ),
                (
                    {
                        "content-length": "12",
                        "last-modified": "Wed, 21 Oct 2015 07:54:00 GMT",
                    },
                    200,
                    b"f2561d5dfd89",
                ),
            ],
            [
                (24, "Wed, 21 Oct 2015 07:28:00 GMT"),
                (40, "Wed, 21 Oct 2015 07:30:00 GMT"),
            ],
            b"f2561d5dfd89",
        ),
        # ETag is preferred over Last-Modified for the If-Range condition.
        (
            5,
            [
                (
                    {
                        "content-length": "36",
                        "last-modified": "Wed, 21 Oct 2015 07:28:00 GMT",
                        "etag": '"33a64df551425fcc55e4d42a148795d9f25f89d4"',
                    },
                    200,
                    b"0cfa7e9d-1868-4dd7-9fb3-",
                ),
                (
                    {
                        "content-length": "12",
                        "last-modified": "Wed, 21 Oct 2015 07:54:00 GMT",
                        "etag": '"33a64df551425fcc55e4d42a148795d9f25f89d4"',
                    },
                    200,
                    b"f2561d5dfd89",
                ),
            ],
            [(24, '"33a64df551425fcc55e4d42a148795d9f25f89d4"')],
            b"f2561d5dfd89",
        ),
    ],
)
def test_downloader(
    resume_retries: int,
    mock_responses: list[tuple[dict[str, str], int, bytes]],
    # list of (range_start, if_range)
    expected_resume_args: list[tuple[int | None, str | None]],
    # expected_bytes is None means the download should fail
    expected_bytes: bytes | None,
    tmpdir: Path,
) -> None:
    session = PipSession(resume_retries=resume_retries)
    link = Link("http://example.com/foo.tgz")
    downloader = Downloader(session, "on")

    responses = []
    for headers, status_code, body in mock_responses:
        resp = MockResponse(body)
        resp.headers.update(headers)
        resp.status_code = status_code
        responses.append(resp)
    _http_get_mock = MagicMock(side_effect=responses)

    with patch.object(Downloader, "_http_get", _http_get_mock):
        if expected_bytes is None:
            with pytest.raises(IncompleteDownloadError):
                downloader(link, str(tmpdir))
            # Make sure the incomplete file is removed
            assert not (tmpdir / "foo.tgz").exists()
        else:
            filepath, _ = downloader(link, str(tmpdir))
            with open(filepath, "rb") as downloaded_file:
                downloaded_bytes = downloaded_file.read()
                assert downloaded_bytes == expected_bytes

    calls = [call(link)]  # the initial GET request
    for range_start, if_range in expected_resume_args:
        headers = {**HEADERS, "Range": f"bytes={range_start}-"}
        if if_range:
            headers["If-Range"] = if_range
        calls.append(call(link, headers))

    # Make sure that the downloader makes additional requests for resumption
    _http_get_mock.assert_has_calls(calls)


def _incomplete_response() -> MockResponse:
    response = MockResponse(b"partial")
    response.headers.update({"content-length": "12"})
    response.status_code = 200
    return response


def test_incomplete_download_is_closed_before_removal(tmpdir: Path) -> None:
    """Windows can't delete an open file."""
    session = PipSession(resume_retries=0)
    link = Link("http://example.com/foo.tgz")
    downloader = Downloader(session, "on")

    downloads: list[_FileDownload] = []

    def record_download(
        link: Link, output_file: BinaryIO, size: int | None
    ) -> _FileDownload:
        download = _FileDownload(link, output_file, size)
        downloads.append(download)
        return download

    def remove_like_windows(filename: str) -> None:
        if not downloads[-1].output_file.closed:
            raise PermissionError("The file is being used by another process")
        Path(filename).unlink()

    with (
        patch.object(
            Downloader, "_http_get", MagicMock(return_value=_incomplete_response())
        ),
        patch("pip._internal.network.download._FileDownload", record_download),
        patch("pip._internal.network.download.os.remove", remove_like_windows),
        pytest.raises(IncompleteDownloadError),
    ):
        downloader(link, str(tmpdir))

    assert not (tmpdir / "foo.tgz").exists()


def test_failed_removal_does_not_mask_incomplete_download(tmpdir: Path) -> None:
    session = PipSession(resume_retries=0)
    link = Link("http://example.com/foo.tgz")
    downloader = Downloader(session, "on")

    def remove_denied(filename: str) -> None:
        raise PermissionError("The file is being used by another process")

    with (
        patch.object(
            Downloader, "_http_get", MagicMock(return_value=_incomplete_response())
        ),
        patch("pip._internal.network.download.os.remove", remove_denied),
        pytest.raises(IncompleteDownloadError),
    ):
        downloader(link, str(tmpdir))

    # Writes go to a sibling temporary file, so the interrupted download can
    # never leave partial bytes at the destination even when cleanup fails.
    assert not (tmpdir / "foo.tgz").exists()
    # The temp file could not be removed, but it must not masquerade as the
    # finished wheel: it stays a dot-prefixed ".part" file instead.
    part_files = list(tmpdir.glob(".foo.tgz.*.part"))
    assert len(part_files) == 1


def test_downloader_resumes_on_protocol_error(tmpdir: Path) -> None:
    """A ProtocolError mid-stream should trigger resume logic, not crash."""
    session = PipSession(resume_retries=3)
    link = Link("http://example.com/foo.tgz")
    downloader = Downloader(session, "on")

    # First response: raises ProtocolError after partial read
    broken_resp = MockResponse(b"0cfa7e9d-1868-4dd7-9fb3-")
    broken_resp.headers.update({"content-length": "36"})
    broken_resp.status_code = 200
    broken_resp.raw = BrokenStream(b"0cfa7e9d-1868-4dd7-9fb3-")

    # Second response: successful resume
    resume_resp = MockResponse(b"f2561d5dfd89")
    resume_resp.headers.update({"content-length": "12"})
    resume_resp.status_code = 206

    _http_get_mock = MagicMock(side_effect=[broken_resp, resume_resp])

    with patch.object(Downloader, "_http_get", _http_get_mock):
        filepath, _ = downloader(link, str(tmpdir))

    with open(filepath, "rb") as f:
        assert f.read() == b"0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89"


@pytest.mark.parametrize(
    "resume_error",
    [
        ProtocolError("Connection broken"),
        ReadTimeoutError(HTTPConnectionPool("example.com"), None, "Read timed out"),
        OSError("Connection broken"),
    ],
)
def test_downloader_retries_low_level_errors_during_resume(
    resume_error: Exception, tmpdir: Path
) -> None:
    """Low-level errors raised while fetching a resume response are retried."""
    session = PipSession(resume_retries=5)
    link = Link("http://example.com/foo.tgz")
    downloader = Downloader(session, "on")

    # Initial response: raises ProtocolError after a partial read
    broken_resp = MockResponse(b"0cfa7e9d-1868-4dd7-9fb3-")
    broken_resp.headers.update({"content-length": "36"})
    broken_resp.status_code = 200
    broken_resp.raw = BrokenStream(b"0cfa7e9d-1868-4dd7-9fb3-")

    # Final resume that completes the file
    resume_resp = MockResponse(b"f2561d5dfd89")
    resume_resp.headers.update({"content-length": "12"})
    resume_resp.status_code = 206

    _http_get_mock = MagicMock(side_effect=[broken_resp, resume_error, resume_resp])

    with patch.object(Downloader, "_http_get", _http_get_mock):
        filepath, _ = downloader(link, str(tmpdir))

    assert _http_get_mock.call_count == 3
    with open(filepath, "rb") as f:
        assert f.read() == b"0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89"


@pytest.mark.parametrize(
    "resume_error",
    [
        ConnectionFailedError(
            "https://example.com/foo.tgz",
            "example.com",
            ConnectionError("Connection broken"),
        ),
        ConnectionTimeoutError(
            "https://example.com/foo.tgz",
            "example.com",
            kind="read",
            timeout=15,
        ),
        ProxyConnectionError(
            "https://example.com/foo.tgz",
            "https://proxy.example.com",
            ProxyError("Cannot connect to proxy", OSError("Connection broken")),
        ),
    ],
)
def test_downloader_retries_diagnostic_connection_errors_during_resume(
    resume_error: Exception, tmpdir: Path
) -> None:
    """Diagnostic connection errors during resume should consume a resume retry."""
    session = PipSession(resume_retries=5)
    link = Link("http://example.com/foo.tgz")
    downloader = Downloader(session, "on")

    broken_resp = MockResponse(b"0cfa7e9d-1868-4dd7-9fb3-")
    broken_resp.headers.update({"content-length": "36"})
    broken_resp.status_code = 200

    resume_resp = MockResponse(b"f2561d5dfd89")
    resume_resp.headers.update({"content-length": "12"})
    resume_resp.status_code = 206

    _http_get_mock = MagicMock(side_effect=[broken_resp, resume_error, resume_resp])

    with patch.object(Downloader, "_http_get", _http_get_mock):
        filepath, _ = downloader(link, str(tmpdir))

    assert _http_get_mock.call_count == 3
    with open(filepath, "rb") as f:
        assert f.read() == b"0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89"


def test_downloader_does_not_retry_on_ssl_missing_error(tmpdir: Path) -> None:
    """SSL errors during resume should fail immediately because retries can't help."""
    session = PipSession(resume_retries=5)
    link = Link("http://example.com/foo.tgz")
    downloader = Downloader(session, "on")

    broken_resp = MockResponse(b"0cfa7e9d-1868-4dd7-9fb3-")
    broken_resp.headers.update({"content-length": "36"})
    broken_resp.status_code = 200
    resume_error = SSLMissingError("https://example.com/foo.tgz")

    _http_get_mock = MagicMock(side_effect=[broken_resp, resume_error])

    with patch.object(Downloader, "_http_get", _http_get_mock):
        with pytest.raises(type(resume_error)):
            downloader(link, str(tmpdir))

    assert _http_get_mock.call_count == 2


def test_downloader_resumes_on_truncated_http_stream(
    mock_server: MockServer, tmpdir: Path
) -> None:
    """A truncated stream raises a real urllib3 ProtocolError that resume recovers."""
    body = b"0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89"

    def truncated(environ: WSGIEnvironment, start_response: StartResponse) -> Body:
        # Advertise the full length but send only part of the body
        start_response("200 OK", [("Content-Length", str(len(body)))])
        return [body[:10]]

    def resumed(environ: WSGIEnvironment, start_response: StartResponse) -> Body:
        start = int(environ["HTTP_RANGE"].split("=", 1)[1].split("-", 1)[0])
        start_response(
            "206 Partial Content",
            [
                ("Content-Length", str(len(body) - start)),
                ("Content-Range", f"bytes {start}-{len(body) - 1}/{len(body)}"),
            ],
        )
        return [body[start:]]

    mock_server.set_responses([truncated, resumed])
    mock_server.start()
    url = f"http://{mock_server.host}:{mock_server.port}/foo.tgz"

    session = PipSession(resume_retries=3)
    downloader = Downloader(session, "on")
    filepath, _ = downloader(Link(url), str(tmpdir))

    with open(filepath, "rb") as f:
        assert f.read() == body


def test_downloader_crashes_on_mismatched_resume_offset(tmpdir: Path) -> None:
    """A 206 whose Content-Range starts at a different offset than requested
    must fail, otherwise the misplaced bytes would corrupt the file."""
    body = b"0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89"
    session = PipSession(resume_retries=5)
    link = Link("http://example.com/foo.tgz")
    downloader = Downloader(session, "on")

    # Incomplete first response (24 of 36 bytes).
    first = MockResponse(body[:24])
    first.headers.update({"content-length": "36"})
    first.status_code = 200

    # The resume asks for bytes=24- but the server answers with content that
    # (per its Content-Range) starts at offset 0.
    mismatched = MockResponse(b"XXXXXXXXXXXX")
    mismatched.headers.update(
        {"content-length": "12", "content-range": "bytes 0-11/36"}
    )
    mismatched.status_code = 206

    _http_get_mock = MagicMock(side_effect=[first, mismatched])
    with patch.object(Downloader, "_http_get", _http_get_mock):
        with pytest.raises(IncompleteDownloadError):
            downloader(link, str(tmpdir))

    assert not (tmpdir / "foo.tgz").exists()


def test_downloader_without_content_length(tmpdir: Path) -> None:
    """A response without a Content-Length header should be treated as an
    unknown size and still download fully.

    This guards against MockResponse inventing its own Content-Length, which
    would hide the unknown-size download path from the tests.
    """
    body = b"0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89"
    resp = MockResponse(body)
    resp.status_code = 200

    assert _get_http_response_size(resp) is None

    session = PipSession(resume_retries=0)
    downloader = Downloader(session, "on")
    link = Link("http://example.com/foo.tgz")
    with patch.object(Downloader, "_http_get", MagicMock(return_value=resp)):
        filepath, _ = downloader(link, str(tmpdir))

    with open(filepath, "rb") as downloaded_file:
        assert downloaded_file.read() == body


def test_resumed_download_caching(tmpdir: Path) -> None:
    """Test that resumed downloads are cached properly for future use."""
    cache_dir = tmpdir / "cache"
    session = PipSession(cache=str(cache_dir), resume_retries=5)
    link = Link("https://example.com/foo.tgz")
    downloader = Downloader(session, "on")

    # Mock an incomplete download followed by a successful resume
    incomplete_resp = MockResponse(b"0cfa7e9d-1868-4dd7-9fb3-")
    incomplete_resp.headers.update({"content-length": "36"})
    incomplete_resp.status_code = 200

    resume_resp = MockResponse(b"f2561d5dfd89")
    resume_resp.headers.update({"content-length": "12"})
    resume_resp.status_code = 206

    responses = [incomplete_resp, resume_resp]
    _http_get_mock = MagicMock(side_effect=responses)

    with patch.object(Downloader, "_http_get", _http_get_mock):
        # Perform the download (incomplete then resumed)
        filepath, _ = downloader(link, str(tmpdir))

        # Verify the file was downloaded correctly
        with open(filepath, "rb") as downloaded_file:
            downloaded_bytes = downloaded_file.read()
            expected_bytes = b"0cfa7e9d-1868-4dd7-9fb3-f2561d5dfd89"
            assert downloaded_bytes == expected_bytes

        # Verify that the cache directory was created and contains cache files
        # The resumed download should have been cached for future use
        assert cache_dir.exists()
        cache_files = list(cache_dir.rglob("*"))
        # Should have cache files (both metadata and body files)
        assert len([f for f in cache_files if f.is_file()]) == 2


def _mock_response(body: bytes, url: str) -> MockResponse:
    resp = MockResponse(body)
    resp.url = url
    resp.status_code = 200
    resp.headers.update({"content-length": str(len(body))})
    return resp


def _sha256_hex(data: bytes) -> str:
    import hashlib

    return hashlib.sha256(data).hexdigest()


def test_download_uses_atomic_temp_file_and_cleans_up(tmpdir: Path) -> None:
    """An interrupted download must never leave a complete-looking destination."""
    session = PipSession(resume_retries=0)
    link = Link("http://example.com/foo.whl")
    downloader = Downloader(session, "on")

    response = _mock_response(b"partial-bytes", "http://example.com/foo.whl")
    # Advertise more bytes than the body contains.
    response.headers["content-length"] = "100"

    with (
        patch.object(Downloader, "_http_get", MagicMock(return_value=response)),
        pytest.raises(IncompleteDownloadError),
    ):
        downloader(link, str(tmpdir))

    # The destination name must not exist at all...
    assert not (tmpdir / "foo.whl").exists()
    # ...and no temp file should survive a successful cleanup.
    assert list(tmpdir.glob(".*.part")) == []


def test_download_writes_via_temp_file_then_replaces(tmpdir: Path) -> None:
    """A successful download lands at the destination via an atomic replace."""
    session = PipSession(resume_retries=0)
    body = b"wheel-body-contents"
    link = Link("http://example.com/foo.whl")
    downloader = Downloader(session, "on")

    with patch.object(
        Downloader,
        "_http_get",
        MagicMock(return_value=_mock_response(body, link.url)),
    ):
        filepath, _ = downloader(link, str(tmpdir))

    assert filepath == str(tmpdir / "foo.whl")
    assert Path(filepath).read_bytes() == body
    assert list(tmpdir.glob(".*.part")) == []


def test_same_named_same_content_is_reused_within_a_run(tmpdir: Path) -> None:
    """Two sources that deliver identical content reuse one destination."""
    session = PipSession(resume_retries=0)
    body = b"identical-wheel"
    first = Link("http://find-links.example.com/foo.whl")
    second = Link("http://direct.example.com/files/foo.whl")
    downloader = Downloader(session, "on")

    get = MagicMock(
        side_effect=[
            _mock_response(body, first.url),
            _mock_response(body, second.url),
        ]
    )
    with patch.object(Downloader, "_http_get", get):
        first_path, _ = downloader(first, str(tmpdir))
        second_path, _ = downloader(second, str(tmpdir))

    assert first_path == second_path == str(tmpdir / "foo.whl")
    assert Path(first_path).read_bytes() == body


def test_same_named_different_content_reports_both_sources(tmpdir: Path) -> None:
    """Same filename, different bytes: the collision names both sources and
    leaves the original file in place."""
    session = PipSession(resume_retries=0)
    first_body = b"first-wheel-content"
    second_body = b"totally-different-wheel"
    first = Link("http://find-links.example.com/foo.whl")
    second = Link("http://direct.example.com/files/foo.whl")
    downloader = Downloader(session, "on")

    get = MagicMock(
        side_effect=[
            _mock_response(first_body, first.url),
            _mock_response(second_body, second.url),
        ]
    )
    with patch.object(Downloader, "_http_get", get):
        first_path, _ = downloader(first, str(tmpdir))
        with pytest.raises(DownloadDestinationCollision) as exc_info:
            downloader(second, str(tmpdir))

    # The pre-existing finished wheel must still be usable.
    assert Path(first_path).read_bytes() == first_body
    message = str(exc_info.value.message) + str(exc_info.value.context)
    assert "find-links.example.com" in message
    assert "direct.example.com" in message
    assert "foo.whl" in message


def test_pre_existing_file_with_matching_content_is_reused(tmpdir: Path) -> None:
    """A finished wheel already in the directory is reused when the download
    turns out to be byte-identical."""
    session = PipSession(resume_retries=0)
    body = b"pre-existing-wheel-bytes"
    destination = tmpdir / "foo.whl"
    destination.write_bytes(body)

    link = Link("http://example.com/foo.whl")
    downloader = Downloader(session, "on")

    with patch.object(
        Downloader,
        "_http_get",
        MagicMock(return_value=_mock_response(body, link.url)),
    ):
        filepath, _ = downloader(link, str(tmpdir))

    assert filepath == str(destination)
    assert Path(filepath).read_bytes() == body


def test_pre_existing_file_with_different_content_is_protected(tmpdir: Path) -> None:
    """A same-named but different incoming file must not overwrite an existing
    finished artifact; both sources are reported instead."""
    session = PipSession(resume_retries=0)
    existing_body = b"already-on-disk"
    incoming_body = b"different-incoming-bytes"
    destination = tmpdir / "foo.whl"
    destination.write_bytes(existing_body)

    link = Link("http://example.com/foo.whl")
    downloader = Downloader(session, "on")

    with patch.object(
        Downloader,
        "_http_get",
        MagicMock(return_value=_mock_response(incoming_body, link.url)),
    ):
        with pytest.raises(DownloadDestinationCollision) as exc_info:
            downloader(link, str(tmpdir))

    # The existing artifact is untouched and stays usable.
    assert destination.read_bytes() == existing_body
    message = str(exc_info.value.message) + str(exc_info.value.context)
    assert "example.com" in message
    assert "foo.whl" in message


def test_link_hash_allows_reuse_without_downloading(tmpdir: Path) -> None:
    """When the link pins a sha256, a matching existing file is reused and no
    HTTP request beyond the initial response lookup needs its body consumed."""
    session = PipSession(resume_retries=0)
    body = b"hash-pinned-wheel"
    digest = _sha256_hex(body)
    destination = tmpdir / "foo.whl"
    destination.write_bytes(body)

    link = Link(f"http://example.com/foo.whl#sha256={digest}")
    downloader = Downloader(session, "on")

    response = _mock_response(body, link.url_without_fragment)
    with patch.object(Downloader, "_http_get", MagicMock(return_value=response)):
        filepath, _ = downloader(link, str(tmpdir))

    assert filepath == str(destination)
    assert destination.read_bytes() == body


def test_link_hash_mismatch_against_existing_file_raises(tmpdir: Path) -> None:
    """A link hash that disagrees with an existing same-named file is a
    collision, not a silent overwrite."""
    session = PipSession(resume_retries=0)
    destination = tmpdir / "foo.whl"
    destination.write_bytes(b"on-disk-bytes")

    digest = _sha256_hex(b"different-bytes")
    link = Link(f"http://example.com/foo.whl#sha256={digest}")
    downloader = Downloader(session, "on")

    response = _mock_response(b"different-bytes", link.url_without_fragment)
    with patch.object(Downloader, "_http_get", MagicMock(return_value=response)):
        with pytest.raises(DownloadDestinationCollision):
            downloader(link, str(tmpdir))

    assert destination.read_bytes() == b"on-disk-bytes"


def test_local_files_same_name_different_content(tmpdir: Path) -> None:
    """End-to-end check through a real PipSession with file:// sources:

    two local files named alike but holding different content, downloaded into
    one directory, collide; the first finished file remains usable.
    """
    from pip._internal.utils.urls import path_to_url

    sources = tmpdir / "sources"
    sources.mkdir()
    first_source = sources / "a"
    second_source = sources / "b"
    first_source.mkdir()
    second_source.mkdir()

    first_body = b"local-wheel-one"
    second_body = b"local-wheel-two-different"
    (first_source / "foo.whl").write_bytes(first_body)
    (second_source / "foo.whl").write_bytes(second_body)

    dest = tmpdir / "dest"
    dest.mkdir()

    session = PipSession(resume_retries=0)
    downloader = Downloader(session, "on")

    first_link = Link(path_to_url(str(first_source / "foo.whl")))
    second_link = Link(path_to_url(str(second_source / "foo.whl")))

    first_path, _ = downloader(first_link, str(dest))
    assert Path(first_path).read_bytes() == first_body

    with pytest.raises(DownloadDestinationCollision) as exc_info:
        downloader(second_link, str(dest))

    # Both local source paths are named in the diagnostic.
    message = str(exc_info.value.message) + str(exc_info.value.context)
    assert "foo.whl" in message
    assert Path(first_path).read_bytes() == first_body


def test_local_files_same_name_same_content(tmpdir: Path) -> None:
    """Two same-named local files with identical content share one destination."""
    from pip._internal.utils.urls import path_to_url

    sources = tmpdir / "sources"
    sources.mkdir()
    first_source = sources / "a"
    second_source = sources / "b"
    first_source.mkdir()
    second_source.mkdir()

    body = b"same-local-wheel"
    (first_source / "foo.whl").write_bytes(body)
    (second_source / "foo.whl").write_bytes(body)

    dest = tmpdir / "dest"
    dest.mkdir()

    session = PipSession(resume_retries=0)
    downloader = Downloader(session, "on")

    first_link = Link(path_to_url(str(first_source / "foo.whl")))
    second_link = Link(path_to_url(str(second_source / "foo.whl")))

    first_path, _ = downloader(first_link, str(dest))
    second_path, _ = downloader(second_link, str(dest))

    assert first_path == second_path == str(dest / "foo.whl")
    assert Path(first_path).read_bytes() == body
