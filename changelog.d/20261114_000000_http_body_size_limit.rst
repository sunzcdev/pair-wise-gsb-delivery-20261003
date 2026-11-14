Added
-----

*   Add optional ``session`` and ``size_limit`` parameters
    to ``feedparser.http.get()``.
    When ``size_limit`` is given, the response body is read incrementally,
    received and decoded byte counts are tracked in a ``BodyBudget``,
    and the limit is enforced on the actual decoded bytes --
    even for chunked responses or responses without a Content-Length header.
    Exceeding the limit, corrupt compressed data, and interrupted requests
    set ``bozo``/``bozo_exception`` to distinguishable errors
    (``BodyTooLargeError``, ``ContentDecodingError``,
    and ``requests.RequestException`` respectively)
    instead of returning a partially read body.
    The response is always closed;
    a caller-provided session is left open.
