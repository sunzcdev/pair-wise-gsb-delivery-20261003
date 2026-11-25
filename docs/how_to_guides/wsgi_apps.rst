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

Streaming the request body
--------------------------

By default the full request body is buffered before the WSGI app is
called. For large uploads it can be preferable to stream the body, so
that the app can read and validate it whilst it arrives. This is
enabled via the ``wsgi_stream_request_body`` configuration attribute,
or the ``stream_request_body`` argument when using middleware,

.. code-block:: python

    app = AsyncioWSGIMiddleware(wsgi_app, stream_request_body=True)

With streaming enabled ``environ["wsgi.input"]`` receives the body
chunk by chunk as it is read, buffering only a bounded amount of
received but unread data. The ``wsgi_max_body_size`` limit still
applies to the cumulative amount received, and any unread remainder is
drained after the app returns so the connection closes or is recycled
cleanly. If the client disconnects mid-body, reading ``wsgi.input``
raises ``hypercorn.app_wrappers.ClientDisconnectedError`` rather than
waiting for data that will never arrive.
