import pytest

import httpx


def hello_world(request: httpx.Request) -> httpx.Response:
    return httpx.Response(200, text="Hello, world")


def test_client_queryparams():
    client = httpx.Client(params={"a": "b"})
    assert isinstance(client.params, httpx.QueryParams)
    assert client.params["a"] == "b"


def test_client_queryparams_string():
    client = httpx.Client(params="a=b")
    assert isinstance(client.params, httpx.QueryParams)
    assert client.params["a"] == "b"

    client = httpx.Client()
    client.params = "a=b"
    assert isinstance(client.params, httpx.QueryParams)
    assert client.params["a"] == "b"


def test_client_queryparams_echo():
    url = "http://example.org/echo_queryparams"
    client_queryparams = "first=str"
    request_queryparams = {"second": "dict"}
    client = httpx.Client(
        transport=httpx.MockTransport(hello_world), params=client_queryparams
    )
    response = client.get(url, params=request_queryparams)

    assert response.status_code == 200
    assert response.url == "http://example.org/echo_queryparams?first=str&second=dict"


def test_client_queryparams_remove_client_default():
    """
    A `REMOVE` marker removes every client-level value for a
    query parameter name.
    """
    url = "http://example.org/echo_queryparams"
    client_queryparams = [("first", "1"), ("first", "2"), ("second", "3")]
    request_queryparams = {"first": httpx.REMOVE, "third": "4"}
    client = httpx.Client(
        transport=httpx.MockTransport(hello_world), params=client_queryparams
    )
    response = client.get(url, params=request_queryparams)

    assert response.status_code == 200
    assert response.url == "http://example.org/echo_queryparams?second=3&third=4"

    # The client-level configuration is left unchanged.
    assert client.params.get_list("first") == ["1", "2"]
    # The request-level mapping is left unchanged.
    assert request_queryparams == {"first": httpx.REMOVE, "third": "4"}


def test_client_queryparams_remove_with_list_params():
    url = "http://example.org/echo_queryparams"
    client = httpx.Client(
        transport=httpx.MockTransport(hello_world), params={"first": "1"}
    )
    response = client.get(url, params=[("second", "2"), ("first", httpx.REMOVE)])

    assert response.status_code == 200
    assert response.url == "http://example.org/echo_queryparams?second=2"


def test_client_queryparams_remove_is_distinct_from_empty_value():
    """
    An empty-string value overrides a client-level query parameter
    with an empty value, rather than removing it.
    """
    url = "http://example.org/echo_queryparams"
    client = httpx.Client(
        transport=httpx.MockTransport(hello_world), params={"first": "1"}
    )
    response = client.get(url, params={"first": ""})

    assert response.status_code == 200
    assert response.url == "http://example.org/echo_queryparams?first="


def test_client_queryparams_remove_marker_not_allowed_on_queryparams_instances():
    with pytest.raises(TypeError):
        httpx.QueryParams({"first": httpx.REMOVE})
