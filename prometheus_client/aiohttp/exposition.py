from __future__ import annotations

from aiohttp import hdrs, web
from aiohttp.typedefs import Handler

from ..exposition import _bake_output
from ..registry import Collector, REGISTRY


def make_aiohttp_handler(
    registry: Collector = REGISTRY,
    disable_compression: bool = False,
) -> Handler:
    """Create a aiohttp handler which serves the metrics from a registry."""

    async def prometheus_handler(request: web.Request) -> web.Response:
        # Prepare parameters
        params = {key: request.query.getall(key) for key in request.query.keys()}
        accept_header = ",".join(request.headers.getall(hdrs.ACCEPT, []))
        accept_encoding_header = ",".join(request.headers.getall(hdrs.ACCEPT_ENCODING, []))
        # Bake output. Compression is negotiated uniformly with the other
        # exporter entry points, honouring quality values and identity.
        status, headers, output = _bake_output(
            registry,
            accept_header,
            accept_encoding_header,
            params,
            disable_compression,
        )
        return web.Response(
            status=int(status.split(" ")[0]),
            headers=headers,
            body=output,
        )

    return prometheus_handler
