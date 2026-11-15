import http
import time
from http.cookiejar import Cookie

import pytest

import httpx


def test_cookies():
    cookies = httpx.Cookies({"name": "value"})
    assert cookies["name"] == "value"
    assert "name" in cookies
    assert len(cookies) == 1
    assert dict(cookies) == {"name": "value"}
    assert bool(cookies) is True

    del cookies["name"]
    assert "name" not in cookies
    assert len(cookies) == 0
    assert dict(cookies) == {}
    assert bool(cookies) is False


def test_cookies_update():
    cookies = httpx.Cookies()
    more_cookies = httpx.Cookies()
    more_cookies.set("name", "value", domain="example.com")

    cookies.update(more_cookies)
    assert dict(cookies) == {"name": "value"}
    assert cookies.get("name", domain="example.com") == "value"


def test_cookies_with_domain():
    cookies = httpx.Cookies()
    cookies.set("name", "value", domain="example.com")
    cookies.set("name", "value", domain="example.org")

    with pytest.raises(httpx.CookieConflict):
        cookies["name"]

    cookies.clear(domain="example.com")
    assert len(cookies) == 1


def test_cookies_with_domain_and_path():
    cookies = httpx.Cookies()
    cookies.set("name", "value", domain="example.com", path="/subpath/1")
    cookies.set("name", "value", domain="example.com", path="/subpath/2")
    cookies.clear(domain="example.com", path="/subpath/1")
    assert len(cookies) == 1
    cookies.delete("name", domain="example.com", path="/subpath/2")
    assert len(cookies) == 0


def test_multiple_set_cookie():
    jar = http.cookiejar.CookieJar()
    headers = [
        (
            b"Set-Cookie",
            b"1P_JAR=2020-08-09-18; expires=Tue, 08-Sep-2099 18:33:35 GMT; "
            b"path=/; domain=.example.org; Secure",
        ),
        (
            b"Set-Cookie",
            b"NID=204=KWdXOuypc86YvRfBSiWoW1dEXfSl_5qI7sxZY4umlk4J35yNTeNEkw15"
            b"MRaujK6uYCwkrtjihTTXZPp285z_xDOUzrdHt4dj0Z5C0VOpbvdLwRdHatHAzQs7"
            b"7TsaiWY78a3qU9r7KP_RbSLvLl2hlhnWFR2Hp5nWKPsAcOhQgSg; expires=Mon, "
            b"08-Feb-2099 18:33:35 GMT; path=/; domain=.example.org; HttpOnly",
        ),
    ]
    request = httpx.Request("GET", "https://www.example.org")
    response = httpx.Response(200, request=request, headers=headers)

    cookies = httpx.Cookies(jar)
    cookies.extract_cookies(response)

    assert len(cookies) == 2


def test_cookies_can_be_a_list_of_tuples():
    cookies_val = [("name1", "val1"), ("name2", "val2")]

    cookies = httpx.Cookies(cookies_val)

    assert len(cookies.items()) == 2
    for k, v in cookies_val:
        assert cookies[k] == v


def test_cookies_repr():
    cookies = httpx.Cookies()
    cookies.set(name="foo", value="bar", domain="http://blah.com")
    cookies.set(name="fizz", value="buzz", domain="http://hello.com")

    assert repr(cookies) == (
        "<Cookies[<Cookie foo=bar for http://blah.com />,"
        " <Cookie fizz=buzz for http://hello.com />]>"
    )


def cookie_header(cookies: httpx.Cookies, url: str) -> str | None:
    request = httpx.Request("GET", url)
    cookies.set_cookie_header(request)
    return request.headers.get("cookie")


def set_cookie(cookies: httpx.Cookies, value: str) -> None:
    request = httpx.Request("GET", "https://www.example.com/")
    response = httpx.Response(200, headers={"set-cookie": value}, request=request)
    cookies.extract_cookies(response)


def test_cookies_serialization_preserves_request_scope():
    cookies = httpx.Cookies()
    set_cookie(cookies, "host_only=1; Path=/")
    set_cookie(cookies, "same_name=root; Path=/; Domain=example.com")
    set_cookie(cookies, "same_name=deep; Path=/app; Domain=example.com")

    urls = [
        "https://www.example.com/",
        "https://api.example.com/",
        "https://www.example.com/app/page",
        "https://api.example.com/app/page",
    ]
    expected_headers = [cookie_header(cookies, url) for url in urls]

    restored = httpx.Cookies.from_dict(cookies.to_dict())

    assert [cookie_header(restored, url) for url in urls] == expected_headers
    assert cookie_header(restored, "https://www.example.com/") == (
        "host_only=1; same_name=root"
    )
    assert cookie_header(restored, "https://api.example.com/") == "same_name=root"
    assert cookie_header(restored, "https://www.example.com/app/page") == (
        "same_name=deep; host_only=1; same_name=root"
    )


def test_cookies_serialization_json_roundtrip():
    cookies = httpx.Cookies()
    cookies.set("name", "value", domain="example.com", path="/subpath")

    restored = httpx.Cookies.loads(cookies.dumps())

    assert restored.get("name", domain="example.com", path="/subpath") == "value"
    assert restored.to_dict()["version"] == httpx.Cookies.SERIALIZATION_VERSION


def test_cookies_restore_replace_and_merge():
    cookies = httpx.Cookies()
    cookies.set("keep", "old", domain="example.com")
    cookies.set("other", "value", domain="example.com")

    payload = {
        "version": 1,
        "cookies": [
            {
                "name": "keep",
                "value": "new",
                "domain": "example.com",
                "path": "/",
                "host_only": False,
            }
        ],
    }

    merged = httpx.Cookies(cookies)
    merged.restore(payload, merge=True)
    assert merged.get("keep", domain="example.com") == "new"
    assert merged.get("other", domain="example.com") == "value"

    replaced = httpx.Cookies(cookies)
    replaced.restore(payload)
    assert replaced.get("keep", domain="example.com") == "new"
    assert "other" not in replaced


def test_cookies_serialization_does_not_revive_expired_cookies():
    now = int(time.time())
    payload = {
        "version": 1,
        "cookies": [
            {
                "name": "deleted",
                "value": "",
                "domain": "example.com",
                "path": "/",
                "expires": now - 10,
                "host_only": False,
            },
            {
                "name": "alive",
                "value": "yes",
                "domain": "example.com",
                "path": "/",
                "expires": now + 3600,
                "host_only": False,
            },
        ],
    }

    cookies = httpx.Cookies.from_dict(payload)

    assert "deleted" not in cookies
    assert cookies.get("alive", domain="example.com") == "yes"
    assert [cookie["name"] for cookie in cookies.to_dict()["cookies"]] == ["alive"]


def test_cookies_serialization_omits_expired_cookies():
    cookies = httpx.Cookies()
    cookies.jar.set_cookie(
        Cookie(
            version=0,
            name="deleted",
            value="",
            port=None,
            port_specified=False,
            domain="example.com",
            domain_specified=True,
            domain_initial_dot=False,
            path="/",
            path_specified=True,
            secure=False,
            expires=int(time.time()) - 1,
            discard=False,
            comment=None,
            comment_url=None,
            rest={},
        )
    )

    assert cookies.to_dict()["cookies"] == []


@pytest.mark.parametrize("merge", [False, True])
def test_cookies_restore_invalid_payload_is_atomic(merge):
    cookies = httpx.Cookies()
    cookies.set("existing", "value", domain="example.com")

    payload = {
        "version": 1,
        "cookies": [
            {
                "name": "valid",
                "value": "value",
                "domain": "example.com",
                "path": "/",
            },
            {
                "name": "invalid",
                "value": 123,
                "domain": "example.com",
                "path": "/",
            },
        ],
    }

    with pytest.raises(ValueError):
        cookies.restore(payload, merge=merge)

    assert dict(cookies) == {"existing": "value"}


@pytest.mark.parametrize("version", [None, 2, "2"])
def test_cookies_restore_rejects_unsupported_version(version):
    payload = {"cookies": []}
    if version is not None:
        payload["version"] = version

    with pytest.raises(ValueError):
        httpx.Cookies.from_dict(payload)
