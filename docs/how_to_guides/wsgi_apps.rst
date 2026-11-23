.. _wsgi_apps:

Serve WSGI applications
=======================

Hypercorn directly serves WSGI applications:

.. code-block:: shell

    $ hypercorn module:wsgi_app

WSGI Middleware
---------------

If a WSGI application is being combined with ASGI middleware it is
best to use either ``AsyncioWSGIMiddleware`` or ``TrioWSGIMiddleware``
middleware. To do so simply wrap the WSGI app with the appropriate
middleware for the hypercorn worker,

.. code-block:: python

    from hypercorn.middleware import AsyncioWSGIMiddleware, TrioWSGIMiddleware

    asyncio_app = AsyncioWSGIMiddleware(wsgi_app)
    trio_app = TrioWSGIMiddleware(wsgi_app)

which can then be passed to other middleware served by hypercorn,

Limiting the request body size
------------------------------

As the request body is stored in memory before being processed it is
important to limit the max size. This is configured by the
``wsgi_max_body_size`` configuration attribute.

When using middleware the ``AsyncioWSGIMiddleware`` and
``TrioWSGIMiddleware`` have a default max size that can be configured,

.. code-block:: python

    app = AsyncioWSGIMiddleware(wsgi_app, max_body_size=20)  # Bytes

Sending files with ``wsgi.file_wrapper``
----------------------------------------

The WSGI environ provides a standard ``wsgi.file_wrapper`` entry point as
defined by PEP 3333. An application that already holds an open file can
seek it to the position from which bytes should be transmitted and hand
the handle to the wrapper,

.. code-block:: python

    def wsgi_app(environ, start_response):
        start_response("200 OK", [("Content-Type", "application/octet-stream")])
        file = open("/path/to/file", "rb")
        file.seek(1024)  # only bytes from this position onwards are sent
        return environ["wsgi.file_wrapper"](file)

Only the bytes from the current file position to the end of the file are
delivered. The wrapper takes ownership of the handle and always closes it
once the response finishes, including when the connection is interrupted,
so the application must not close the file itself. The file is always read
via the handed-over handle, never re-opened from its disk path.

If the connection advertises the ASGI
``http.response.zerocopysend`` extension (present in the request scope's
``extensions`` mapping) the wrapper negotiates the specialised
representation and passes the file descriptor, offset and byte count to
the server instead of reading the file into memory. Otherwise the file is
streamed chunk by chunk using the wrapper's iterator, which works with any
file-like object.
