When using the ``fast-deps`` lazy wheel feature, pip now verifies that every
HTTP range response still describes the same wheel via the server's
``ETag``/``Last-Modified`` validators. If the served file changes mid-read,
the partial response is discarded and the wheel is downloaded fully in one
request instead of stitching bytes from two different representations
together. Servers that provide no validator keep the previous behavior.
