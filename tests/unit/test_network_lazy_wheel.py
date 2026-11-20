import email
import zipfile
from collections.abc import Iterable, Iterator
from pathlib import Path
from typing import TYPE_CHECKING, TypedDict

import pytest

from pip._vendor.packaging.utils import canonicalize_name
from pip._vendor.packaging.version import Version

from pip._internal.exceptions import InvalidWheel
from pip._internal.metadata import MemoryWheel, get_wheel_distribution
from pip._internal.network.lazy_wheel import (
    HTTPRangeRequestUnsupported,
    LazyWheelContentChangedError,
    LazyZipOverHTTP,
    dist_from_wheel_url,
)
from pip._internal.network.session import PipSession

from tests.lib import TestData
from tests.lib.server import MockServer, file_response

if TYPE_CHECKING:
    from _typeshed.wsgi import StartResponse, WSGIApplication, WSGIEnvironment


class _RequestLogEntry(TypedDict):
    method: str
    range: str | None
    tag: str


class _SwapState(TypedDict):
    tag: str
    switch_after: int | None
    range_requests: int
    validator: str
    requests: list[_RequestLogEntry]


MYPY_0_782_WHL = (
    "https://files.pythonhosted.org/packages/9d/65/"
    "b96e844150ce18b9892b155b780248955ded13a2581d31872e7daa90a503/"
    "mypy-0.782-py3-none-any.whl"
)
MYPY_0_782_REQS = {
    "typed-ast<1.5.0,>=1.4.0",
    "typing-extensions>=3.7.4",
    "mypy-extensions<0.5.0,>=0.4.3",
    'psutil>=4.0; extra == "dmypy"',
}

# Small enough that reading one of the test wheels needs several range
# requests, which is what makes a mid-read representation swap observable.
SMALL_CHUNK_SIZE = 512


@pytest.fixture
def session() -> PipSession:
    return PipSession()


@pytest.fixture
def mypy_whl_no_range(mock_server: MockServer, shared_data: TestData) -> Iterator[str]:
    mypy_whl = shared_data.packages / "mypy-0.782-py3-none-any.whl"
    mock_server.set_responses([file_response(mypy_whl)])
    mock_server.start()
    base_address = f"http://{mock_server.host}:{mock_server.port}"
    yield "{}/{}".format(base_address, "mypy-0.782-py3-none-any.whl")
    mock_server.stop()


@pytest.mark.network
def test_dist_from_wheel_url(session: PipSession) -> None:
    """Test if the acquired distribution contain correct information."""
    dist = dist_from_wheel_url(canonicalize_name("mypy"), MYPY_0_782_WHL, session)
    assert dist.canonical_name == "mypy"
    assert dist.version == Version("0.782")
    extras = list(dist.iter_provided_extras())
    assert extras == ["dmypy"]
    assert {str(d) for d in dist.iter_dependencies(extras)} == MYPY_0_782_REQS


def test_dist_from_wheel_url_no_range(
    session: PipSession, mypy_whl_no_range: str
) -> None:
    """Test handling when HTTP range requests are not supported."""
    with pytest.raises(HTTPRangeRequestUnsupported):
        dist_from_wheel_url(canonicalize_name("mypy"), mypy_whl_no_range, session)


@pytest.mark.network
def test_dist_from_wheel_url_not_zip(session: PipSession) -> None:
    """Test handling with the given URL does not point to a ZIP."""
    with pytest.raises(InvalidWheel):
        dist_from_wheel_url(
            canonicalize_name("python"), "https://www.python.org/", session
        )


def _swapping_wheel_response(
    wheels: dict[str, Path],
    state: _SwapState,
    *,
    emit_validators: bool,
) -> "WSGIApplication":
    """Build a WSGI app serving one URL whose file can change mid-read.

    ``wheels`` maps a version tag ("1.0" / "2.0") to an actual wheel file.
    The app always reports the length and (optionally) ETag of the wheel it is
    currently serving.  When ``state["switch_after"]`` range requests have been
    answered, it atomically starts serving the other wheel, simulating a mirror
    replacing a same-named file between two reads.
    """

    def validator_headers(tag: str) -> list[tuple[str, str]]:
        if not emit_validators:
            return []
        # Opaque, quoted ETag like a real server would send.
        if state.get("validator") == "last-modified":
            # Distinct Last-Modified timestamp per representation.
            timestamp = "Mon, 01 Jan 2024 00:00:00 GMT"
            if tag == "2.0":
                timestamp = "Tue, 02 Jan 2024 00:00:00 GMT"
            return [("Last-Modified", timestamp)]
        return [("ETag", f'"{tag}"')]

    def current_tag() -> str:
        return state["tag"]

    def maybe_switch(range_requests: int) -> None:
        switch_after = state["switch_after"]
        if switch_after is not None and range_requests >= switch_after:
            state["tag"] = "2.0" if state["tag"] == "1.0" else "1.0"
            # Only switch once.
            state["switch_after"] = None

    def responder(
        environ: "WSGIEnvironment", start_response: "StartResponse"
    ) -> Iterable[bytes]:
        tag = current_tag()
        path = wheels[tag]
        data = path.read_bytes()
        headers = [
            ("Content-Type", "application/octet-stream"),
            ("Accept-Ranges", "bytes"),
            ("Content-Length", str(len(data))),
            *validator_headers(tag),
        ]
        state["requests"].append(
            {
                "method": environ["REQUEST_METHOD"],
                "range": environ.get("HTTP_RANGE"),
                "tag": tag,
            }
        )

        if environ["REQUEST_METHOD"] == "HEAD":
            start_response("200 OK", headers)
            return []

        range_header = environ.get("HTTP_RANGE")
        if range_header is None:
            # Full, non-range download (the explicit fallback path).
            start_response("200 OK", headers)
            return [data]

        assert range_header.startswith("bytes=")
        start_text, end_text = range_header[len("bytes=") :].split("-", 1)
        range_start = int(start_text)
        range_end = int(end_text) if end_text else len(data) - 1
        range_end = min(range_end, len(data) - 1)
        chunk = data[range_start : range_end + 1]
        range_requests = state["range_requests"] + 1
        state["range_requests"] = range_requests
        start_response(
            "206 Partial Content",
            [
                ("Content-Type", "application/octet-stream"),
                ("Content-Length", str(len(chunk))),
                (
                    "Content-Range",
                    f"bytes {range_start}-{range_end}/{len(data)}",
                ),
                *validator_headers(tag),
            ],
        )
        # Switching after the response for this request has been chosen: the
        # next range request then sees the replacement representation.
        maybe_switch(range_requests)
        return [chunk]

    return responder


@pytest.fixture
def simplewheel_versions(shared_data: TestData) -> dict[str, Path]:
    packages = shared_data.packages
    return {
        "1.0": packages / "simplewheel-1.0-py2.py3-none-any.whl",
        "2.0": packages / "simplewheel-2.0-py2.py3-none-any.whl",
    }


def _serve(
    mock_server: MockServer,
    simplewheel_versions: dict[str, Path],
    *,
    emit_validators: bool,
    switch_after: int | None,
    validator: str = "etag",
) -> tuple[str, _SwapState]:
    """Start a stateful wheel server and return (url, state).

    The responder is installed as the mock's side_effect itself (not as a
    one-element list) so it keeps handling every request instead of being
    consumed after a single call.
    """
    state: _SwapState = {
        "tag": "1.0",
        "switch_after": switch_after,
        "range_requests": 0,
        "validator": validator,
        "requests": [],
    }
    responder = _swapping_wheel_response(
        simplewheel_versions, state, emit_validators=emit_validators
    )
    mock_server.set_responder(responder)
    mock_server.start()
    url = f"http://{mock_server.host}:{mock_server.port}/simplewheel.whl"
    return url, state


def test_lazy_wheel_stable_etag_reads_single_representation(
    session: PipSession,
    mock_server: MockServer,
    simplewheel_versions: dict[str, Path],
) -> None:
    """With an unchanged ETag the lazy path returns the served representation."""
    url, state = _serve(
        mock_server, simplewheel_versions, emit_validators=True, switch_after=None
    )
    try:
        with LazyZipOverHTTP(url, session, chunk_size=SMALL_CHUNK_SIZE) as zf:
            wheel = MemoryWheel(zf.name, zf)  # type: ignore[arg-type]
            dist = get_wheel_distribution(wheel, canonicalize_name("simplewheel"))
        assert dist.version == Version("1.0")
        # A small chunk size forces several range requests for one ~1.8KB wheel.
        assert state["range_requests"] > 1
    finally:
        mock_server.stop()


def test_lazy_wheel_etag_swap_falls_back_to_complete_new_artifact(
    session: PipSession,
    mock_server: MockServer,
    simplewheel_versions: dict[str, Path],
) -> None:
    """A mid-read ETag change must not be stitched together.

    The mirror serves simplewheel 1.0 for the first range request, then swaps
    the same URL to 2.0.  The mismatched validator discards the partial read
    and a single full download is used instead, so the resolver ends up with
    the complete replacement artifact (2.0), never a Frankenstein ZIP.
    """
    url, state = _serve(
        mock_server, simplewheel_versions, emit_validators=True, switch_after=1
    )
    try:
        dist = dist_from_wheel_url(
            canonicalize_name("simplewheel"),
            url,
            session,
            chunk_size=SMALL_CHUNK_SIZE,
        )
        # The fallback full download happens after the swap, so the complete
        # artifact the resolver reads is the new 2.0 representation.
        assert dist.version == Version("2.0")
        # At least two range requests happened (one pre-swap, one post-swap),
        # proving the swap was observed between lazy reads rather than at once.
        assert state["range_requests"] >= 2

        requests_log = state["requests"]
        range_gets = [r for r in requests_log if r["range"] is not None]
        full_gets = [
            r for r in requests_log if r["method"] == "GET" and r["range"] is None
        ]
        # The lazy phase mixed representations: 1.0 first, then the swapped 2.0.
        assert range_gets[0]["tag"] == "1.0"
        assert any(r["tag"] == "2.0" for r in range_gets[1:])
        # Recovery is exactly one whole-file GET with no Range header, and it
        # serves the replacement representation in a single self-consistent read.
        assert len(full_gets) == 1
        assert full_gets[0]["tag"] == "2.0"

        # The resolver ends on the complete replacement artifact: every header
        # of its parsed metadata matches the 2.0 wheel's (pip synthesizes a
        # Description header from the message body, which the raw file does not
        # carry, so only the actual on-disk headers are compared), proving no
        # 1.0 byte survived into the final product.
        with zipfile.ZipFile(simplewheel_versions["2.0"]) as zf:
            metadata_name = next(n for n in zf.namelist() if n.endswith("METADATA"))
            expected_metadata = email.message_from_bytes(zf.read(metadata_name))
        assert {key: dist.metadata[key] for key in expected_metadata} == dict(
            expected_metadata.items()
        )
    finally:
        mock_server.stop()


def test_lazy_wheel_last_modified_swap_also_falls_back(
    session: PipSession,
    mock_server: MockServer,
    simplewheel_versions: dict[str, Path],
) -> None:
    """A server emitting only Last-Modified is protected the same way."""
    url, state = _serve(
        mock_server,
        simplewheel_versions,
        emit_validators=True,
        switch_after=1,
        validator="last-modified",
    )
    try:
        dist = dist_from_wheel_url(
            canonicalize_name("simplewheel"),
            url,
            session,
            chunk_size=SMALL_CHUNK_SIZE,
        )
        assert dist.version == Version("2.0")
        full_gets = [
            r for r in state["requests"] if r["method"] == "GET" and r["range"] is None
        ]
        assert len(full_gets) == 1
        assert full_gets[0]["tag"] == "2.0"
    finally:
        mock_server.stop()


def test_lazy_wheel_detects_swap_at_low_level(
    session: PipSession,
    mock_server: MockServer,
    simplewheel_versions: dict[str, Path],
) -> None:
    """LazyZipOverHTTP itself raises before mixing bytes from two versions."""
    url, state = _serve(
        mock_server, simplewheel_versions, emit_validators=True, switch_after=1
    )
    try:
        with pytest.raises(LazyWheelContentChangedError):
            LazyZipOverHTTP(url, session, chunk_size=SMALL_CHUNK_SIZE)
        # The first range response was accepted as 1.0; the next one already
        # carried the 2.0 ETag and was rejected before any of its bytes were
        # written, so the swap was detected on the second range request.
        assert state["range_requests"] == 2
        assert state["tag"] == "2.0"
    finally:
        mock_server.stop()


def test_lazy_wheel_without_validators_keeps_reading(
    session: PipSession,
    mock_server: MockServer,
    simplewheel_versions: dict[str, Path],
) -> None:
    """Servers without validators keep the previous best-effort behavior.

    No ETag/Last-Modified means there is nothing to compare, so the lazy
    reader must not raise a content-change error; the existing range flow (or
    the ordinary BadZipFile retry loop inside _check_zip) still runs.
    """
    url, state = _serve(
        mock_server, simplewheel_versions, emit_validators=False, switch_after=None
    )
    try:
        with LazyZipOverHTTP(url, session, chunk_size=SMALL_CHUNK_SIZE) as zf:
            assert zf.readable()
            assert state["range_requests"] > 1
    finally:
        mock_server.stop()
