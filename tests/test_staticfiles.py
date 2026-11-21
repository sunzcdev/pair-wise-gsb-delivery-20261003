import gzip
import os
import stat
import tempfile
import time
from pathlib import Path
from typing import Any

import anyio
import pytest

from starlette.applications import Starlette
from starlette.exceptions import HTTPException
from starlette.middleware import Middleware
from starlette.middleware.base import BaseHTTPMiddleware, RequestResponseEndpoint
from starlette.requests import Request
from starlette.responses import Response
from starlette.routing import Mount
from starlette.staticfiles import StaticFiles
from starlette.websockets import WebSocketDisconnect
from tests.types import TestClientFactory


def test_staticfiles(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    path = os.path.join(tmpdir, "example.txt")
    with open(path, "w") as file:
        file.write("<file content>")

    app = StaticFiles(directory=tmpdir)
    client = test_client_factory(app)
    response = client.get("/example.txt")
    assert response.status_code == 200
    assert response.text == "<file content>"


def test_staticfiles_websocket(tmp_path: Path, test_client_factory: TestClientFactory) -> None:
    app = Starlette(routes=[Mount("/static", app=StaticFiles(directory=tmp_path))])
    client = test_client_factory(app)

    with pytest.raises(WebSocketDisconnect) as exc:
        with client.websocket_connect("/static/example.txt"):
            pass  # pragma: no cover - The connection is rejected before entering the context.

    assert exc.value.code == 1000


def test_staticfiles_with_pathlib(tmp_path: Path, test_client_factory: TestClientFactory) -> None:
    path = tmp_path / "example.txt"
    with open(path, "w") as file:
        file.write("<file content>")

    app = StaticFiles(directory=tmp_path)
    client = test_client_factory(app)
    response = client.get("/example.txt")
    assert response.status_code == 200
    assert response.text == "<file content>"


def test_staticfiles_head_with_middleware(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    """
    see https://github.com/Kludex/starlette/pull/935
    """
    path = os.path.join(tmpdir, "example.txt")
    with open(path, "w") as file:
        file.write("x" * 100)

    async def does_nothing_middleware(request: Request, call_next: RequestResponseEndpoint) -> Response:
        response = await call_next(request)
        return response

    routes = [Mount("/static", app=StaticFiles(directory=tmpdir), name="static")]
    middleware = [Middleware(BaseHTTPMiddleware, dispatch=does_nothing_middleware)]
    app = Starlette(routes=routes, middleware=middleware)

    client = test_client_factory(app)
    response = client.head("/static/example.txt")
    assert response.status_code == 200
    assert response.headers.get("content-length") == "100"


def test_staticfiles_with_package(test_client_factory: TestClientFactory) -> None:
    app = StaticFiles(packages=["tests"])
    client = test_client_factory(app)
    response = client.get("/example.txt")
    assert response.status_code == 200
    assert response.text == "123\n"

    app = StaticFiles(packages=[("tests", "statics")])
    client = test_client_factory(app)
    response = client.get("/example.txt")
    assert response.status_code == 200
    assert response.text == "123\n"


def test_staticfiles_post(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    path = os.path.join(tmpdir, "example.txt")
    with open(path, "w") as file:
        file.write("<file content>")

    routes = [Mount("/", app=StaticFiles(directory=tmpdir), name="static")]
    app = Starlette(routes=routes)
    client = test_client_factory(app)

    response = client.post("/example.txt")
    assert response.status_code == 405
    assert response.headers["allow"] == "GET, HEAD"
    assert response.text == "Method Not Allowed"


def test_staticfiles_with_directory_returns_404(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    path = os.path.join(tmpdir, "example.txt")
    with open(path, "w") as file:
        file.write("<file content>")

    routes = [Mount("/", app=StaticFiles(directory=tmpdir), name="static")]
    app = Starlette(routes=routes)
    client = test_client_factory(app)

    response = client.get("/")
    assert response.status_code == 404
    assert response.text == "Not Found"


def test_staticfiles_with_missing_file_returns_404(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    path = os.path.join(tmpdir, "example.txt")
    with open(path, "w") as file:
        file.write("<file content>")

    routes = [Mount("/", app=StaticFiles(directory=tmpdir), name="static")]
    app = Starlette(routes=routes)
    client = test_client_factory(app)

    response = client.get("/404.txt")
    assert response.status_code == 404
    assert response.text == "Not Found"


def test_staticfiles_instantiated_with_missing_directory(tmpdir: Path) -> None:
    with pytest.raises(RuntimeError) as exc_info:
        path = os.path.join(tmpdir, "no_such_directory")
        StaticFiles(directory=path)
    assert "does not exist" in str(exc_info.value)


def test_staticfiles_configured_with_missing_directory(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    path = os.path.join(tmpdir, "no_such_directory")
    app = StaticFiles(directory=path, check_dir=False)
    client = test_client_factory(app)
    with pytest.raises(RuntimeError) as exc_info:
        client.get("/example.txt")
    assert "does not exist" in str(exc_info.value)


def test_staticfiles_configured_with_file_instead_of_directory(
    tmpdir: Path, test_client_factory: TestClientFactory
) -> None:
    path = os.path.join(tmpdir, "example.txt")
    with open(path, "w") as file:
        file.write("<file content>")

    app = StaticFiles(directory=path, check_dir=False)
    client = test_client_factory(app)
    with pytest.raises(RuntimeError) as exc_info:
        client.get("/example.txt")
    assert "is not a directory" in str(exc_info.value)


def test_staticfiles_config_check_occurs_only_once(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    app = StaticFiles(directory=tmpdir)
    client = test_client_factory(app)
    assert not app.config_checked

    with pytest.raises(HTTPException):
        client.get("/")

    assert app.config_checked

    with pytest.raises(HTTPException):
        client.get("/")


def test_staticfiles_prevents_breaking_out_of_directory(tmpdir: Path) -> None:
    directory = os.path.join(tmpdir, "foo")
    os.mkdir(directory)

    path = os.path.join(tmpdir, "example.txt")
    with open(path, "w") as file:
        file.write("outside root dir")

    app = StaticFiles(directory=directory)
    # We can't test this with 'httpx', so we test the app directly here.
    path = app.get_path({"path": "/../example.txt"})
    scope = {"method": "GET"}

    with pytest.raises(HTTPException) as exc_info:
        anyio.run(app.get_response, path, scope)

    assert exc_info.value.status_code == 404
    assert exc_info.value.detail == "Not Found"


def test_staticfiles_never_read_file_for_head_method(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    path = os.path.join(tmpdir, "example.txt")
    with open(path, "w") as file:
        file.write("<file content>")

    app = StaticFiles(directory=tmpdir)
    client = test_client_factory(app)
    response = client.head("/example.txt")
    assert response.status_code == 200
    assert response.content == b""
    assert response.headers["content-length"] == "14"


def test_staticfiles_304_with_etag_match(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    path = os.path.join(tmpdir, "example.txt")
    with open(path, "w") as file:
        file.write("<file content>")

    app = StaticFiles(directory=tmpdir)
    client = test_client_factory(app)
    first_resp = client.get("/example.txt")
    assert first_resp.status_code == 200
    last_etag = first_resp.headers["etag"]
    second_resp = client.get("/example.txt", headers={"if-none-match": last_etag})
    assert second_resp.status_code == 304
    assert second_resp.content == b""
    second_resp = client.get("/example.txt", headers={"if-none-match": f'W/{last_etag}, "123"'})
    assert second_resp.status_code == 304
    assert second_resp.content == b""
    second_resp = client.get("/example.txt", headers={"if-none-match": f'"123",\tW/{last_etag}'})
    assert second_resp.status_code == 304
    assert second_resp.content == b""


@pytest.mark.parametrize("method", ["GET", "HEAD"])
@pytest.mark.parametrize("if_none_match", ["*", " \t* \t"])
def test_staticfiles_304_with_if_none_match_wildcard(
    tmp_path: Path,
    test_client_factory: TestClientFactory,
    method: str,
    if_none_match: str,
) -> None:
    (tmp_path / "example.txt").write_text("<file content>", encoding="utf-8")

    app = StaticFiles(directory=tmp_path)
    client = test_client_factory(app)
    response = client.request(method, "/example.txt", headers={"if-none-match": if_none_match})
    assert response.status_code == 304
    assert response.content == b""


@pytest.mark.parametrize("if_none_match", ['"123"', '"*"', '"foo,*,bar"'])
def test_staticfiles_200_with_etag_mismatch(
    tmp_path: Path,
    test_client_factory: TestClientFactory,
    if_none_match: str,
) -> None:
    (tmp_path / "example.txt").write_text("<file content>", encoding="utf-8")

    app = StaticFiles(directory=tmp_path)
    client = test_client_factory(app)
    response = client.get("/example.txt", headers={"if-none-match": if_none_match})
    assert response.status_code == 200
    assert response.content == b"<file content>"


def test_staticfiles_200_with_etag_mismatch_and_timestamp_match(
    tmpdir: Path, test_client_factory: TestClientFactory
) -> None:
    path = tmpdir / "example.txt"
    path.write_text("<file content>", encoding="utf-8")

    app = StaticFiles(directory=tmpdir)
    client = test_client_factory(app)
    first_resp = client.get("/example.txt")
    assert first_resp.status_code == 200
    assert first_resp.headers["etag"] != '"123"'
    last_modified = first_resp.headers["last-modified"]
    # If `if-none-match` is present, `if-modified-since` is ignored.
    second_resp = client.get("/example.txt", headers={"if-none-match": '"123"', "if-modified-since": last_modified})
    assert second_resp.status_code == 200
    assert second_resp.content == b"<file content>"


def test_staticfiles_304_with_last_modified_compare_last_req(
    tmpdir: Path, test_client_factory: TestClientFactory
) -> None:
    path = os.path.join(tmpdir, "example.txt")
    file_last_modified_time = time.mktime(time.strptime("2013-10-10 23:40:00", "%Y-%m-%d %H:%M:%S"))
    with open(path, "w") as file:
        file.write("<file content>")
    os.utime(path, (file_last_modified_time, file_last_modified_time))

    app = StaticFiles(directory=tmpdir)
    client = test_client_factory(app)
    # last modified less than last request, 304
    response = client.get("/example.txt", headers={"If-Modified-Since": "Thu, 11 Oct 2013 15:30:19 GMT"})
    assert response.status_code == 304
    assert response.content == b""
    # last modified greater than last request, 200 with content
    response = client.get("/example.txt", headers={"If-Modified-Since": "Thu, 20 Feb 2012 15:30:19 GMT"})
    assert response.status_code == 200
    assert response.content == b"<file content>"


def test_staticfiles_html_normal(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    path = os.path.join(tmpdir, "404.html")
    with open(path, "w") as file:
        file.write("<h1>Custom not found page</h1>")
    path = os.path.join(tmpdir, "dir")
    os.mkdir(path)
    path = os.path.join(path, "index.html")
    with open(path, "w") as file:
        file.write("<h1>Hello</h1>")

    app = StaticFiles(directory=tmpdir, html=True)
    client = test_client_factory(app)

    response = client.get("/dir/")
    assert response.url == "http://testserver/dir/"
    assert response.status_code == 200
    assert response.text == "<h1>Hello</h1>"

    response = client.get("/dir")
    assert response.url == "http://testserver/dir/"
    assert response.status_code == 200
    assert response.text == "<h1>Hello</h1>"

    response = client.get("/dir/index.html")
    assert response.url == "http://testserver/dir/index.html"
    assert response.status_code == 200
    assert response.text == "<h1>Hello</h1>"

    response = client.get("/missing")
    assert response.status_code == 404
    assert response.text == "<h1>Custom not found page</h1>"


def test_staticfiles_html_without_index(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    path = os.path.join(tmpdir, "404.html")
    with open(path, "w") as file:
        file.write("<h1>Custom not found page</h1>")
    path = os.path.join(tmpdir, "dir")
    os.mkdir(path)

    app = StaticFiles(directory=tmpdir, html=True)
    client = test_client_factory(app)

    response = client.get("/dir/")
    assert response.url == "http://testserver/dir/"
    assert response.status_code == 404
    assert response.text == "<h1>Custom not found page</h1>"

    response = client.get("/dir")
    assert response.url == "http://testserver/dir"
    assert response.status_code == 404
    assert response.text == "<h1>Custom not found page</h1>"

    response = client.get("/missing")
    assert response.status_code == 404
    assert response.text == "<h1>Custom not found page</h1>"


def test_staticfiles_html_without_404(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    path = os.path.join(tmpdir, "dir")
    os.mkdir(path)
    path = os.path.join(path, "index.html")
    with open(path, "w") as file:
        file.write("<h1>Hello</h1>")

    app = StaticFiles(directory=tmpdir, html=True)
    client = test_client_factory(app)

    response = client.get("/dir/")
    assert response.url == "http://testserver/dir/"
    assert response.status_code == 200
    assert response.text == "<h1>Hello</h1>"

    response = client.get("/dir")
    assert response.url == "http://testserver/dir/"
    assert response.status_code == 200
    assert response.text == "<h1>Hello</h1>"

    with pytest.raises(HTTPException) as exc_info:
        response = client.get("/missing")
    assert exc_info.value.status_code == 404


def test_staticfiles_html_only_files(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    path = os.path.join(tmpdir, "hello.html")
    with open(path, "w") as file:
        file.write("<h1>Hello</h1>")

    app = StaticFiles(directory=tmpdir, html=True)
    client = test_client_factory(app)

    with pytest.raises(HTTPException) as exc_info:
        response = client.get("/")
    assert exc_info.value.status_code == 404

    response = client.get("/hello.html")
    assert response.status_code == 200
    assert response.text == "<h1>Hello</h1>"


def test_staticfiles_cache_invalidation_for_deleted_file_html_mode(
    tmpdir: Path, test_client_factory: TestClientFactory
) -> None:
    path_404 = os.path.join(tmpdir, "404.html")
    with open(path_404, "w") as file:
        file.write("<p>404 file</p>")
    path_some = os.path.join(tmpdir, "some.html")
    with open(path_some, "w") as file:
        file.write("<p>some file</p>")

    common_modified_time = time.mktime(time.strptime("2013-10-10 23:40:00", "%Y-%m-%d %H:%M:%S"))
    os.utime(path_404, (common_modified_time, common_modified_time))
    os.utime(path_some, (common_modified_time, common_modified_time))

    app = StaticFiles(directory=tmpdir, html=True)
    client = test_client_factory(app)

    resp_exists = client.get("/some.html")
    assert resp_exists.status_code == 200
    assert resp_exists.text == "<p>some file</p>"

    resp_cached = client.get(
        "/some.html",
        headers={"If-Modified-Since": resp_exists.headers["last-modified"]},
    )
    assert resp_cached.status_code == 304

    os.remove(path_some)

    resp_deleted = client.get(
        "/some.html",
        headers={"If-Modified-Since": resp_exists.headers["last-modified"]},
    )
    assert resp_deleted.status_code == 404
    assert resp_deleted.text == "<p>404 file</p>"


def test_staticfiles_with_invalid_dir_permissions_returns_401(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    (tmp_path / "example.txt").write_bytes(b"<file content>")

    original_mode = tmp_path.stat().st_mode
    tmp_path.chmod(stat.S_IRWXO)
    try:
        routes = [
            Mount(
                "/",
                app=StaticFiles(directory=os.fsdecode(tmp_path)),
                name="static",
            )
        ]
        app = Starlette(routes=routes)
        client = test_client_factory(app)

        response = client.get("/example.txt")
        assert response.status_code == 401
        assert response.text == "Unauthorized"
    finally:
        tmp_path.chmod(original_mode)


def test_staticfiles_with_missing_dir_returns_404(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    path = os.path.join(tmpdir, "example.txt")
    with open(path, "w") as file:
        file.write("<file content>")

    routes = [Mount("/", app=StaticFiles(directory=tmpdir), name="static")]
    app = Starlette(routes=routes)
    client = test_client_factory(app)

    response = client.get("/foo/example.txt")
    assert response.status_code == 404
    assert response.text == "Not Found"


def test_staticfiles_access_file_as_dir_returns_404(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    path = os.path.join(tmpdir, "example.txt")
    with open(path, "w") as file:
        file.write("<file content>")

    routes = [Mount("/", app=StaticFiles(directory=tmpdir), name="static")]
    app = Starlette(routes=routes)
    client = test_client_factory(app)

    response = client.get("/example.txt/foo")
    assert response.status_code == 404
    assert response.text == "Not Found"


def test_staticfiles_null_byte_in_path(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    routes = [Mount("/", app=StaticFiles(directory=tmpdir), name="static")]
    app = Starlette(routes=routes)
    client = test_client_factory(app)

    response = client.get("/example%00.txt")
    assert response.status_code == 404


@pytest.mark.skipif(not hasattr(os, "pathconf"), reason="os.pathconf is Unix-only")
def test_staticfiles_filename_too_long(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    routes = [Mount("/", app=StaticFiles(directory=tmpdir), name="static")]
    app = Starlette(routes=routes)
    client = test_client_factory(app)

    path_max_size = os.pathconf("/", "PC_PATH_MAX")
    response = client.get(f"/{'a' * path_max_size}.txt")
    assert response.status_code == 404
    assert response.text == "Not Found"


def test_staticfiles_unhandled_os_error_returns_500(
    tmpdir: Path,
    test_client_factory: TestClientFactory,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    def mock_timeout(*args: Any, **kwargs: Any) -> None:
        raise TimeoutError

    path = os.path.join(tmpdir, "example.txt")
    with open(path, "w") as file:
        file.write("<file content>")

    routes = [Mount("/", app=StaticFiles(directory=tmpdir), name="static")]
    app = Starlette(routes=routes)
    client = test_client_factory(app, raise_server_exceptions=False)

    monkeypatch.setattr("starlette.staticfiles.StaticFiles.lookup_path", mock_timeout)

    response = client.get("/example.txt")
    assert response.status_code == 500
    assert response.text == "Internal Server Error"


def test_staticfiles_follows_symlinks(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    statics_path = os.path.join(tmpdir, "statics")
    os.mkdir(statics_path)

    source_path = tempfile.mkdtemp()
    source_file_path = os.path.join(source_path, "page.html")
    with open(source_file_path, "w") as file:
        file.write("<h1>Hello</h1>")

    statics_file_path = os.path.join(statics_path, "index.html")
    os.symlink(source_file_path, statics_file_path)

    app = StaticFiles(directory=statics_path, follow_symlink=True)
    client = test_client_factory(app)

    response = client.get("/index.html")
    assert response.url == "http://testserver/index.html"
    assert response.status_code == 200
    assert response.text == "<h1>Hello</h1>"


def test_staticfiles_follows_symlink_directories(tmpdir: Path, test_client_factory: TestClientFactory) -> None:
    statics_path = os.path.join(tmpdir, "statics")
    statics_html_path = os.path.join(statics_path, "html")
    os.mkdir(statics_path)

    source_path = tempfile.mkdtemp()
    source_file_path = os.path.join(source_path, "page.html")
    with open(source_file_path, "w") as file:
        file.write("<h1>Hello</h1>")

    os.symlink(source_path, statics_html_path)

    app = StaticFiles(directory=statics_path, follow_symlink=True)
    client = test_client_factory(app)

    response = client.get("/html/page.html")
    assert response.url == "http://testserver/html/page.html"
    assert response.status_code == 200
    assert response.text == "<h1>Hello</h1>"


def test_staticfiles_disallows_path_traversal_with_symlinks(tmpdir: Path) -> None:
    statics_path = os.path.join(tmpdir, "statics")

    root_source_path = tempfile.mkdtemp()
    source_path = os.path.join(root_source_path, "statics")
    os.mkdir(source_path)

    source_file_path = os.path.join(root_source_path, "index.html")
    with open(source_file_path, "w") as file:
        file.write("<h1>Hello</h1>")

    os.symlink(source_path, statics_path)

    app = StaticFiles(directory=statics_path, follow_symlink=True)
    # We can't test this with 'httpx', so we test the app directly here.
    path = app.get_path({"path": "/../index.html"})
    scope = {"method": "GET"}

    with pytest.raises(HTTPException) as exc_info:
        anyio.run(app.get_response, path, scope)

    assert exc_info.value.status_code == 404
    assert exc_info.value.detail == "Not Found"


def test_staticfiles_avoids_path_traversal(tmp_path: Path) -> None:
    statics_path = tmp_path / "static"
    statics_disallow_path = tmp_path / "static_disallow"

    statics_path.mkdir()
    statics_disallow_path.mkdir()

    static_index_file = statics_path / "index.html"
    statics_disallow_path_index_file = statics_disallow_path / "index.html"
    static_file = tmp_path / "static1.txt"

    static_index_file.write_text("<h1>Hello</h1>")
    statics_disallow_path_index_file.write_text("<h1>Private</h1>")
    static_file.write_text("Private")

    app = StaticFiles(directory=statics_path)

    # We can't test this with 'httpx', so we test the app directly here.
    path = app.get_path({"path": "/../static1.txt"})
    with pytest.raises(HTTPException) as exc_info:
        anyio.run(app.get_response, path, {"method": "GET"})

    assert exc_info.value.status_code == 404
    assert exc_info.value.detail == "Not Found"

    path = app.get_path({"path": "/../static_disallow/index.html"})
    with pytest.raises(HTTPException) as exc_info:
        anyio.run(app.get_response, path, {"method": "GET"})

    assert exc_info.value.status_code == 404
    assert exc_info.value.detail == "Not Found"


def test_staticfiles_rejects_absolute_paths(tmp_path: Path) -> None:
    statics_path = tmp_path / "static"
    statics_path.mkdir()
    app = StaticFiles(directory=statics_path)

    full_path, stat_result = app.lookup_path("/etc/passwd")
    assert full_path == ""
    assert stat_result is None


def test_staticfiles_rejects_absolute_windows_paths(tmp_path: Path) -> None:
    statics_path = tmp_path / "static"
    statics_path.mkdir()
    app = StaticFiles(directory=statics_path)

    full_path, stat_result = app.lookup_path("\\\\server\\share")
    assert full_path == ""
    assert stat_result is None


def test_staticfiles_self_symlinks(tmp_path: Path, test_client_factory: TestClientFactory) -> None:
    statics_path = tmp_path / "statics"
    statics_path.mkdir()

    source_file_path = statics_path / "index.html"
    source_file_path.write_text("<h1>Hello</h1>", encoding="utf-8")

    statics_symlink_path = tmp_path / "statics_symlink"
    statics_symlink_path.symlink_to(statics_path)

    app = StaticFiles(directory=statics_symlink_path, follow_symlink=True)
    client = test_client_factory(app)

    response = client.get("/index.html")
    assert response.url == "http://testserver/index.html"
    assert response.status_code == 200
    assert response.text == "<h1>Hello</h1>"


def test_staticfiles_relative_directory_symlinks(test_client_factory: TestClientFactory) -> None:
    app = StaticFiles(directory="tests/statics", follow_symlink=True)
    client = test_client_factory(app)
    response = client.get("/example.txt")
    assert response.status_code == 200
    assert response.text == "123\n"


def _write_precompressed(tmpdir: Path, name: str = "example.txt") -> tuple[Path, Path, Path, bytes]:
    original = tmpdir / name
    gzip_path = tmpdir / f"{name}.gz"
    brotli_path = tmpdir / f"{name}.br"
    payload = b"<file content>" * 10
    original.write_bytes(payload)
    # A real gzip representation: the TestClient transparently decodes gzip,
    # so it must contain valid gzip data.
    gzip_path.write_bytes(gzip.compress(payload))
    # "br" isn't auto-decoded in this environment, so plain bytes are enough.
    brotli_path.write_bytes(b"<brotli representation>")
    return original, gzip_path, brotli_path, payload


def test_staticfiles_precompressed_sends_configured_representation(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    _write_precompressed(tmp_path)
    app = StaticFiles(directory=tmp_path, precompressed={"gzip": ".gz", "br": ".br"})
    client = test_client_factory(app)

    # "br" isn't transparently decoded by the TestClient in this environment.
    response = client.get("/example.txt", headers={"accept-encoding": "br"})
    assert response.status_code == 200
    # The actual precompressed representation is sent.
    assert response.content == b"<brotli representation>"
    assert response.headers["content-encoding"] == "br"
    # Content-Type still describes the original resource.
    assert response.headers["content-type"] == "text/plain; charset=utf-8"
    # Content length belongs to the representation that is sent.
    assert response.headers["content-length"] == str(len(b"<brotli representation>"))
    assert response.headers["vary"] == "accept-encoding"
    assert response.headers["accept-ranges"] == "bytes"


def test_staticfiles_precompressed_preserves_original_media_type(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    (tmp_path / "example.css").write_text("body {}", encoding="utf-8")
    (tmp_path / "example.css.br").write_bytes(b"css-br")

    app = StaticFiles(directory=tmp_path, precompressed={"br": ".br"})
    client = test_client_factory(app)

    response = client.get("/example.css", headers={"accept-encoding": "br"})
    assert response.status_code == 200
    assert response.headers["content-type"] == "text/css; charset=utf-8"
    assert response.headers["content-encoding"] == "br"


def test_staticfiles_precompressed_etag_and_length_belong_to_representation(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    _, _, _, payload = _write_precompressed(tmp_path)
    app = StaticFiles(directory=tmp_path, precompressed={"gzip": ".gz", "br": ".br"})
    client = test_client_factory(app)

    compressed_response = client.get("/example.txt", headers={"accept-encoding": "br"})
    identity_response = client.get("/example.txt", headers={"accept-encoding": "identity"})

    assert compressed_response.headers["etag"] != identity_response.headers["etag"]
    assert compressed_response.headers["content-length"] != identity_response.headers["content-length"]
    assert compressed_response.headers["content-length"] == str(len(b"<brotli representation>"))
    assert identity_response.headers["content-length"] == str(len(payload))
    assert "content-encoding" not in identity_response.headers


def test_staticfiles_precompressed_does_not_304_with_foreign_etag(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    _, _, _, payload = _write_precompressed(tmp_path)
    app = StaticFiles(directory=tmp_path, precompressed={"gzip": ".gz", "br": ".br"})
    client = test_client_factory(app)

    compressed_response = client.get("/example.txt", headers={"accept-encoding": "br"})
    compressed_etag = compressed_response.headers["etag"]

    # The client reuses the compressed ETag while requesting the original
    # representation: this must not produce a 304.
    response = client.get(
        "/example.txt",
        headers={"accept-encoding": "identity", "if-none-match": compressed_etag},
    )
    assert response.status_code == 200
    assert response.content == payload

    # The same ETag still validates the matching representation.
    matching = client.get(
        "/example.txt",
        headers={"accept-encoding": "br", "if-none-match": compressed_etag},
    )
    assert matching.status_code == 304
    assert matching.content == b""
    # The 304 keeps the representation's caching headers.
    assert matching.headers["etag"] == compressed_etag
    assert matching.headers["vary"] == "accept-encoding"


def test_staticfiles_precompressed_304_with_last_modified(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    original, gzip_path, brotli_path, _ = _write_precompressed(tmp_path)
    modified_time = time.mktime(time.strptime("2013-10-10 23:40:00", "%Y-%m-%d %H:%M:%S"))
    os.utime(brotli_path, (modified_time, modified_time))

    app = StaticFiles(directory=tmp_path, precompressed={"gzip": ".gz", "br": ".br"})
    client = test_client_factory(app)

    response = client.get("/example.txt", headers={"accept-encoding": "br"})
    last_modified = response.headers["last-modified"]

    cached = client.get(
        "/example.txt",
        headers={"accept-encoding": "br", "if-modified-since": last_modified},
    )
    assert cached.status_code == 304

    # The original file's timestamp must not be used for the compressed one.
    stale = client.get(
        "/example.txt",
        headers={
            "accept-encoding": "br",
            "if-modified-since": "Thu, 20 Feb 2012 15:30:19 GMT",
        },
    )
    assert stale.status_code == 200
    assert stale.content == b"<brotli representation>"


@pytest.mark.parametrize(
    ("accept_encoding", "expected"),
    [
        ("gzip, br", "gzip"),
        ("br, gzip", "br"),
        ("gzip;q=0.5, br;q=0.9", "br"),
        ("br;q=0.9, gzip", "gzip"),
        ("gzip;q=0, br;q=0", None),
        ("identity", None),
        ("*", "gzip"),
        ("*;q=0", None),
        ("br;q=0, *;q=0.5", "gzip"),
    ],
)
def test_staticfiles_precompressed_encoding_negotiation(
    tmp_path: Path,
    test_client_factory: TestClientFactory,
    accept_encoding: str,
    expected: str | None,
) -> None:
    _write_precompressed(tmp_path)
    app = StaticFiles(directory=tmp_path, precompressed={"gzip": ".gz", "br": ".br"})
    client = test_client_factory(app)

    response = client.get("/example.txt", headers={"accept-encoding": accept_encoding})
    assert response.headers.get("content-encoding") == expected


def test_staticfiles_precompressed_without_accept_encoding_uses_first(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    _write_precompressed(tmp_path)
    app = StaticFiles(directory=tmp_path, precompressed={"gzip": ".gz", "br": ".br"})
    client = test_client_factory(app)

    response = client.get("/example.txt")
    assert response.status_code == 200
    assert response.headers["content-encoding"] == "gzip"


def test_staticfiles_precompressed_falls_back_to_next_encoding(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    _write_precompressed(tmp_path)
    (tmp_path / "example.txt.br").unlink()
    app = StaticFiles(directory=tmp_path, precompressed={"gzip": ".gz", "br": ".br"})
    client = test_client_factory(app)

    response = client.get("/example.txt", headers={"accept-encoding": "br, gzip"})
    assert response.status_code == 200
    assert response.headers["content-encoding"] == "gzip"
    # The TestClient transparently decodes the real gzip payload.
    assert response.content == b"<file content>" * 10


def test_staticfiles_precompressed_missing_files_serves_original(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    payload = b"<file content>" * 10
    (tmp_path / "example.txt").write_bytes(payload)
    app = StaticFiles(directory=tmp_path, precompressed={"gzip": ".gz", "br": ".br"})
    client = test_client_factory(app)

    response = client.get("/example.txt", headers={"accept-encoding": "gzip, br"})
    assert response.status_code == 200
    assert response.content == payload
    assert "content-encoding" not in response.headers
    # The response still varies because negotiation is enabled.
    assert response.headers["vary"] == "accept-encoding"


def test_staticfiles_precompressed_disabled_keeps_original_behavior(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    (tmp_path / "example.txt").write_bytes(b"<file content>")
    (tmp_path / "example.txt.gz").write_bytes(b"<gzip representation>")
    app = StaticFiles(directory=tmp_path)
    client = test_client_factory(app)

    response = client.get("/example.txt", headers={"accept-encoding": "gzip"})
    assert response.status_code == 200
    assert response.content == b"<file content>"
    assert "content-encoding" not in response.headers
    assert "vary" not in response.headers


def test_staticfiles_precompressed_range_uses_representation_size(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    _write_precompressed(tmp_path)
    app = StaticFiles(directory=tmp_path, precompressed={"gzip": ".gz", "br": ".br"})
    client = test_client_factory(app)
    headers = {"accept-encoding": "br"}

    full = client.get("/example.txt", headers=headers)
    representation_size = len(b"<brotli representation>")
    assert int(full.headers["content-length"]) == representation_size

    ranged = client.get("/example.txt", headers={**headers, "range": "bytes=0-4"})
    assert ranged.status_code == 206
    assert ranged.content == b"<brot"
    assert ranged.headers["content-range"] == f"bytes 0-4/{representation_size}"
    assert ranged.headers["content-length"] == "5"
    assert ranged.headers["content-encoding"] == "br"
    assert ranged.headers["content-type"] == "text/plain; charset=utf-8"


def test_staticfiles_precompressed_if_range_matching_etag(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    _write_precompressed(tmp_path)
    app = StaticFiles(directory=tmp_path, precompressed={"gzip": ".gz", "br": ".br"})
    client = test_client_factory(app)

    full = client.get("/example.txt", headers={"accept-encoding": "br"})
    etag = full.headers["etag"]

    matched = client.get(
        "/example.txt",
        headers={
            "accept-encoding": "br",
            "range": "bytes=0-4",
            "if-range": etag,
        },
    )
    assert matched.status_code == 206
    assert matched.headers["etag"] == etag
    assert matched.headers["content-encoding"] == "br"


def test_staticfiles_precompressed_if_range_foreign_etag_ignores_range(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    _write_precompressed(tmp_path)
    app = StaticFiles(directory=tmp_path, precompressed={"gzip": ".gz", "br": ".br"})
    client = test_client_factory(app)

    identity = client.get("/example.txt", headers={"accept-encoding": "identity"})

    mismatched = client.get(
        "/example.txt",
        headers={
            "accept-encoding": "br",
            "range": "bytes=0-4",
            "if-range": identity.headers["etag"],
        },
    )
    assert mismatched.status_code == 200
    assert "content-range" not in mismatched.headers
    assert mismatched.content == b"<brotli representation>"


def test_staticfiles_precompressed_head_matches_get_headers(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    _write_precompressed(tmp_path)
    app = StaticFiles(directory=tmp_path, precompressed={"gzip": ".gz", "br": ".br"})
    client = test_client_factory(app)
    headers = {"accept-encoding": "br"}

    get_response = client.get("/example.txt", headers=headers)
    head_response = client.head("/example.txt", headers=headers)

    assert head_response.status_code == 200
    assert head_response.content == b""
    for header in ("etag", "last-modified", "content-length", "content-encoding", "content-type", "vary"):
        assert head_response.headers[header] == get_response.headers[header]


def test_staticfiles_precompressed_head_range_matches_get_range(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    _write_precompressed(tmp_path)
    app = StaticFiles(directory=tmp_path, precompressed={"gzip": ".gz", "br": ".br"})
    client = test_client_factory(app)

    full = client.get("/example.txt", headers={"accept-encoding": "br"})
    request_headers = {"accept-encoding": "br", "range": "bytes=0-4", "if-range": full.headers["etag"]}
    head_ranged = client.head("/example.txt", headers=request_headers)
    get_ranged = client.get("/example.txt", headers=request_headers)

    assert head_ranged.status_code == get_ranged.status_code == 206
    for header in ("etag", "content-range", "content-length", "content-encoding", "vary"):
        assert head_ranged.headers[header] == get_ranged.headers[header]
    assert head_ranged.content == b""


def test_staticfiles_precompressed_html_mode_index(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    directory = tmp_path / "dir"
    directory.mkdir()
    (directory / "index.html").write_text("<h1>Hello</h1>", encoding="utf-8")
    (directory / "index.html.br").write_bytes(b"<brotli html>")

    app = StaticFiles(directory=tmp_path, html=True, precompressed={"gzip": ".gz", "br": ".br"})
    client = test_client_factory(app)

    response = client.get("/dir/", headers={"accept-encoding": "br"})
    assert response.status_code == 200
    assert response.content == b"<brotli html>"
    assert response.headers["content-encoding"] == "br"
    assert response.headers["content-type"] == "text/html; charset=utf-8"
    assert response.headers["vary"] == "accept-encoding"


def test_staticfiles_precompressed_round_trips_real_gzip(
    tmp_path: Path, test_client_factory: TestClientFactory
) -> None:
    payload = b"<file content>" * 50
    (tmp_path / "example.txt").write_bytes(payload)
    (tmp_path / "example.txt.gz").write_bytes(gzip.compress(payload))

    app = StaticFiles(directory=tmp_path, precompressed={"gzip": ".gz"})
    # Disable the client's automatic content decoding to inspect the raw bytes.
    client = test_client_factory(app)
    response = client.get(
        "/example.txt",
        headers={"accept-encoding": "gzip"},
    )
    # TestClient/httpx transparently decompresses gzip responses.
    assert response.content == payload
    assert response.headers["content-encoding"] == "gzip"

