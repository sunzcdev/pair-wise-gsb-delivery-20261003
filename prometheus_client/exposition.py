import base64
from contextlib import closing
from functools import partial
import gzip
from http.server import BaseHTTPRequestHandler
import os
import socket
from socketserver import ThreadingMixIn
import ssl
import sys
import threading
from typing import (
    Any, Callable, Dict, List, Literal, Optional, Sequence, Tuple, Union,
)
from urllib.error import HTTPError
from urllib.parse import parse_qs, quote_plus, urlparse
from urllib.request import (
    BaseHandler, build_opener, HTTPHandler, HTTPRedirectHandler, HTTPSHandler,
    Request,
)
from wsgiref.simple_server import make_server, WSGIRequestHandler, WSGIServer

from .openmetrics import exposition as openmetrics
from .registry import Collector, REGISTRY
from .utils import floatToGoString, parse_version

try:
    import snappy  # type: ignore
    SNAPPY_AVAILABLE = True
except ImportError:
    snappy = None  # type: ignore
    SNAPPY_AVAILABLE = False

__all__ = (
    'CONTENT_TYPE_LATEST',
    'CONTENT_TYPE_PLAIN_0_0_4',
    'CONTENT_TYPE_PLAIN_1_0_0',
    'delete_from_gateway',
    'generate_latest',
    'instance_ip_grouping_key',
    'make_asgi_app',
    'make_wsgi_app',
    'MetricsHandler',
    'push_to_gateway',
    'pushadd_to_gateway',
    'start_http_server',
    'start_wsgi_server',
    'write_to_textfile',
)

CONTENT_TYPE_PLAIN_0_0_4 = 'text/plain; version=0.0.4; charset=utf-8'
"""Content type of the compatibility format"""

CONTENT_TYPE_PLAIN_1_0_0 = 'text/plain; version=1.0.0; charset=utf-8'
"""Content type of the latest format"""

CONTENT_TYPE_LATEST = CONTENT_TYPE_PLAIN_1_0_0
CompressionType = Optional[Literal['gzip', 'snappy']]


class _PrometheusRedirectHandler(HTTPRedirectHandler):
    """
    Allow additional methods (e.g. PUT) and data forwarding in redirects.

    Use of this class constitute a user's explicit agreement to the
    redirect responses the Prometheus client will receive when using it.
    You should only use this class if you control or otherwise trust the
    redirect behavior involved and are certain it is safe to full transfer
    the original request (method and data) to the redirected URL. For
    example, if you know there is a cosmetic URL redirect in front of a
    local deployment of a Prometheus server, and all redirects are safe,
    this is the class to use to handle redirects in that case.

    The standard HTTPRedirectHandler does not forward request data nor
    does it allow redirected PUT requests (which Prometheus uses for some
    operations, for example `push_to_gateway`) because these cannot
    generically guarantee no violations of HTTP RFC 2616 requirements for
    the user to explicitly confirm redirects that could have unexpected
    side effects (such as rendering a PUT request non-idempotent or
    creating multiple resources not named in the original request).
    """

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        """
        Apply redirect logic to a request.

        See parent HTTPRedirectHandler.redirect_request for parameter info.

        If the redirect is disallowed, this raises the corresponding HTTP error.
        If the redirect can't be determined, return None to allow other handlers
        to try. If the redirect is allowed, return the new request.

        This method specialized for the case when (a) the user knows that the
        redirect will not cause unacceptable side effects for any request method,
        and (b) the user knows that any request data should be passed through to
        the redirect. If either condition is not met, this should not be used.
        """
        # note that requests being provided by a handler will use get_method to
        # indicate the method, by monkeypatching this, instead of setting the
        # Request object's method attribute.
        m = getattr(req, "method", req.get_method())
        if not (code in (301, 302, 303, 307) and m in ("GET", "HEAD")
                or code in (301, 302, 303) and m in ("POST", "PUT")):
            raise HTTPError(req.full_url, code, msg, headers, fp)
        new_request = Request(
            newurl.replace(' ', '%20'),  # space escaping in new url if needed.
            headers=req.headers,
            origin_req_host=req.origin_req_host,
            unverifiable=True,
            data=req.data,
        )
        new_request.method = m
        return new_request


def _bake_output(registry, accept_header, accept_encoding_header, params, disable_compression):
    """Bake output for metrics output."""
    # Choose the correct plain text format of the output.
    encoder, content_type = choose_encoder(accept_header)
    if encoder is None:
        # The client explicitly excluded every format this exporter serves.
        # Answer clearly instead of sending a format the client refused.
        body = (b'406 Not Acceptable: the Accept header excludes every format '
                b'this exporter can serve (text/plain versions 0.0.4 and 1.0.0, '
                b'application/openmetrics-text versions 1.0.0 and 2.0.0).\n')
        return '406 Not Acceptable', [('Content-Type', 'text/plain; charset=utf-8')], body
    if 'name[]' in params:
        registry = registry.restricted_registry(params['name[]'])
    output = encoder(registry)
    headers = [('Content-Type', content_type)]
    # If gzip encoding required, gzip the output.
    if not disable_compression and gzip_accepted(accept_encoding_header):
        output = gzip.compress(output)
        headers.append(('Content-Encoding', 'gzip'))
    return '200 OK', headers, output


def make_wsgi_app(registry: Collector = REGISTRY, disable_compression: bool = False) -> Callable:
    """Create a WSGI app which serves the metrics from a registry."""

    def prometheus_app(environ, start_response):
        # Prepare parameters
        accept_header = environ.get('HTTP_ACCEPT')
        accept_encoding_header = environ.get('HTTP_ACCEPT_ENCODING')
        params = parse_qs(environ.get('QUERY_STRING', ''))
        method = environ['REQUEST_METHOD']

        if method == 'OPTIONS':
            status = '200 OK'
            headers = [('Allow', 'OPTIONS,GET')]
            output = b''
        elif method != 'GET':
            status = '405 Method Not Allowed'
            headers = [('Allow', 'OPTIONS,GET')]
            output = '# HTTP {}: {}; use OPTIONS or GET\n'.format(status, method).encode()
        elif environ['PATH_INFO'] == '/favicon.ico':
            # Serve empty response for browsers
            status = '200 OK'
            headers = []
            output = b''
        else:
            # Note: For backwards compatibility, the URI path for GET is not
            # constrained to the documented /metrics, but any path is allowed.
            # Bake output
            status, headers, output = _bake_output(registry, accept_header, accept_encoding_header, params, disable_compression)
        # Return output
        start_response(status, headers)
        return [output]

    return prometheus_app


class _SilentHandler(WSGIRequestHandler):
    """WSGI handler that does not log requests."""

    def log_message(self, format, *args):
        """Log nothing."""


class ThreadingWSGIServer(ThreadingMixIn, WSGIServer):
    """Thread per request HTTP server."""
    # Make worker threads "fire and forget". Beginning with Python 3.7 this
    # prevents a memory leak because ``ThreadingMixIn`` starts to gather all
    # non-daemon threads in a list in order to join on them at server close.
    daemon_threads = True


def _get_best_family(address, port):
    """Automatically select address family depending on address"""
    # HTTPServer defaults to AF_INET, which will not start properly if
    # binding an ipv6 address is requested.
    # This function is based on what upstream python did for http.server
    # in https://github.com/python/cpython/pull/11767
    infos = socket.getaddrinfo(address, port, type=socket.SOCK_STREAM, flags=socket.AI_PASSIVE)
    family, _, _, _, sockaddr = next(iter(infos))
    return family, sockaddr[0]


def _get_ssl_ctx(
        certfile: str,
        keyfile: str,
        protocol: int,
        cafile: Optional[str] = None,
        capath: Optional[str] = None,
        client_auth_required: bool = False,
        tls_min_version: Optional[ssl.TLSVersion] = None,
        tls_max_version: Optional[ssl.TLSVersion] = None
) -> ssl.SSLContext:
    """Load context supports SSL."""
    ssl_cxt = ssl.SSLContext(protocol=protocol)

    if cafile is not None or capath is not None:
        try:
            ssl_cxt.load_verify_locations(cafile, capath)
        except IOError as exc:
            exc_type = type(exc)
            msg = str(exc)
            raise exc_type(f"Cannot load CA certificate chain from file "
                           f"{cafile!r} or directory {capath!r}: {msg}")
    else:
        try:
            ssl_cxt.load_default_certs(purpose=ssl.Purpose.CLIENT_AUTH)
        except IOError as exc:
            exc_type = type(exc)
            msg = str(exc)
            raise exc_type(f"Cannot load default CA certificate chain: {msg}")

    if client_auth_required:
        ssl_cxt.verify_mode = ssl.CERT_REQUIRED

    try:
        ssl_cxt.load_cert_chain(certfile=certfile, keyfile=keyfile)
    except IOError as exc:
        exc_type = type(exc)
        msg = str(exc)
        raise exc_type(f"Cannot load server certificate file {certfile!r} or "
                       f"its private key file {keyfile!r}: {msg}")

    if tls_min_version is not None:
        ssl_cxt.minimum_version = tls_min_version
    if tls_max_version is not None:
        ssl_cxt.maximum_version = tls_max_version

    return ssl_cxt


def start_wsgi_server(
        port: int,
        addr: str = '0.0.0.0',
        registry: Collector = REGISTRY,
        certfile: Optional[str] = None,
        keyfile: Optional[str] = None,
        client_cafile: Optional[str] = None,
        client_capath: Optional[str] = None,
        protocol: int = ssl.PROTOCOL_TLS_SERVER,
        client_auth_required: bool = False,
        tls_min_version: Optional[ssl.TLSVersion] = None,
        tls_max_version: Optional[ssl.TLSVersion] = None
) -> Tuple[WSGIServer, threading.Thread]:
    """Starts a WSGI server for prometheus metrics as a daemon thread."""

    class TmpServer(ThreadingWSGIServer):
        """Copy of ThreadingWSGIServer to update address_family locally"""

    TmpServer.address_family, addr = _get_best_family(addr, port)
    app = make_wsgi_app(registry)
    httpd = make_server(addr, port, app, TmpServer, handler_class=_SilentHandler)
    if certfile and keyfile:
        context = _get_ssl_ctx(
            certfile,
            keyfile,
            protocol,
            client_cafile,
            client_capath,
            client_auth_required,
            tls_min_version,
            tls_max_version
        )
        httpd.socket = context.wrap_socket(httpd.socket, server_side=True)
    t = threading.Thread(target=httpd.serve_forever)
    t.daemon = True
    t.start()

    return httpd, t


start_http_server = start_wsgi_server


def generate_latest(registry: Collector = REGISTRY, escaping: str = openmetrics.UNDERSCORES) -> bytes:
    """
    Generates the exposition format using the basic Prometheus text format.

    Params:
        registry: Collector to export data from.
        escaping: Escaping scheme used for metric and label names.

    Returns: UTF-8 encoded string containing the metrics in text format.
    """

    def sample_line(samples):
        if samples.labels:
            labelstr = '{0}'.format(','.join(
                # Label values always support UTF-8
                ['{}="{}"'.format(
                    openmetrics.escape_label_name(k, escaping), openmetrics._escape(v, openmetrics.ALLOWUTF8, False))
                    for k, v in sorted(samples.labels.items())]))
        else:
            labelstr = ''
        timestamp = ''
        if samples.timestamp is not None:
            # Convert to milliseconds.
            timestamp = f' {int(float(samples.timestamp) * 1000):d}'
        if escaping != openmetrics.ALLOWUTF8 or openmetrics._is_valid_legacy_metric_name(samples.name):
            if labelstr:
                labelstr = '{{{0}}}'.format(labelstr)
            return f'{openmetrics.escape_metric_name(samples.name, escaping)}{labelstr} {floatToGoString(samples.value)}{timestamp}\n'
        maybe_comma = ''
        if labelstr:
            maybe_comma = ','
        return f'{{{openmetrics.escape_metric_name(samples.name, escaping)}{maybe_comma}{labelstr}}} {floatToGoString(samples.value)}{timestamp}\n'

    output = []
    for metric in registry.collect():
        try:
            mname = metric.name
            mtype = metric.type
            # Munging from OpenMetrics into Prometheus format.
            if mtype == 'counter':
                mname = mname + '_total'
            elif mtype == 'info':
                mname = mname + '_info'
                mtype = 'gauge'
            elif mtype == 'stateset':
                mtype = 'gauge'
            elif mtype == 'gaugehistogram':
                # A gauge histogram is really a gauge,
                # but this captures the structure better.
                mtype = 'histogram'
            elif mtype == 'unknown':
                mtype = 'untyped'

            output.append('# HELP {} {}\n'.format(
                openmetrics.escape_metric_name(mname, escaping), metric.documentation.replace('\\', r'\\').replace('\n', r'\n')))
            output.append(f'# TYPE {openmetrics.escape_metric_name(mname, escaping)} {mtype}\n')

            om_samples: Dict[str, List[str]] = {}
            for s in metric.samples:
                for suffix in ['_created', '_gsum', '_gcount']:
                    if s.name == metric.name + suffix:
                        # OpenMetrics specific sample, put in a gauge at the end.
                        om_samples.setdefault(suffix, []).append(sample_line(s))
                        break
                else:
                    output.append(sample_line(s))
        except Exception as exception:
            exception.args = (exception.args or ('',)) + (metric,)
            raise

        for suffix, lines in sorted(om_samples.items()):
            output.append('# HELP {} {}\n'.format(openmetrics.escape_metric_name(metric.name + suffix, escaping),
                                                  metric.documentation.replace('\\', r'\\').replace('\n', r'\n')))
            output.append(f'# TYPE {openmetrics.escape_metric_name(metric.name + suffix, escaping)} gauge\n')
            output.extend(lines)
    return ''.join(output).encode('utf-8')


# Versions of each exposition format this exporter can actually serve.
# Unknown versions must never be claimed in a response Content-Type.
_OPENMETRICS_VERSIONS = {
    (1, 0, 0): '1.0.0',
    (2, 0, 0): '2.0.0',
}
_PLAIN_VERSIONS = {
    (0, 0, 4): '0.0.4',
    (1, 0, 0): '1.0.0',
}
_ESCAPING_SCHEMES = (
    openmetrics.ALLOWUTF8,
    openmetrics.UNDERSCORES,
    openmetrics.DOTS,
    openmetrics.VALUES,
)


def choose_encoder(accept_header: Optional[str]) -> Tuple[Optional[Callable[[Collector], bytes]], Optional[str]]:
    """Choose an encoder and Content-Type for the given Accept header.

    The header is evaluated as a list of weighted candidates: each item's
    q-value, version and escaping parameters decide which concrete output
    format it refers to, not just its media type string. Items with q=0
    explicitly exclude the formats they match, and malformed items (bad
    q-value, unknown version, ...) are ignored so they cannot override
    later, well-formed candidates. Between items of equal weight the more
    specific one wins, with remaining ties broken by header order, so the
    choice is stable.

    Returns (None, None) when the header excludes every format this
    exporter can serve, so callers can answer 406 Not Acceptable. When the
    header is absent or mentions nothing we serve, the default Prometheus
    text format is used, unless it was explicitly excluded.
    """
    candidates = []
    exclusions = []
    for index, (media_type, params, quality) in enumerate(_parse_accept_header(accept_header)):
        specificity = _accept_specificity(media_type, params)
        if quality <= 0:
            # q=0 explicitly excludes everything this item matches.
            exclusions.append((media_type, params.get('version'), specificity))
            continue
        offer = _resolve_offer(media_type, params)
        if offer is None:
            continue
        concrete_type, concrete_version, encoder, content_type = offer
        candidates.append((quality, specificity, -index, concrete_type, concrete_version, encoder, content_type))

    # Highest weight first; ties go to the more specific item, then to the
    # earliest item in the header, so the choice is deterministic.
    candidates.sort(key=lambda candidate: (candidate[0], candidate[1], candidate[2]), reverse=True)
    for _, specificity, _neg_index, media_type, version, encoder, content_type in candidates:
        if _is_excluded(exclusions, media_type, version, specificity):
            continue
        return encoder, content_type

    # Nothing acceptable was explicitly requested. Fall back to the default
    # format (text/plain 0.0.4) unless it, too, was explicitly excluded.
    if _is_excluded(exclusions, 'text/plain', '0.0.4', 0):
        return None, None
    return generate_latest, CONTENT_TYPE_PLAIN_0_0_4


def _parse_accept_header(accept_header: Optional[str]) -> List[Tuple[str, Dict[str, str], float]]:
    """Parse an Accept header into (media_type, params, quality) entries.

    Items without a media type or with an unparsable or out-of-range
    q-value are dropped, so a malformed item never overrides later,
    well-formed candidates."""
    entries = []
    for raw_item in (accept_header or '').split(','):
        tokens = raw_item.split(';')
        media_type = tokens[0].strip().lower()
        if '/' not in media_type:
            continue
        params: Dict[str, str] = {}
        quality = 1.0
        valid = True
        for token in tokens[1:]:
            key, sep, value = token.partition('=')
            if not sep:
                continue
            key = key.strip().lower()
            value = value.strip()
            if key == 'q':
                try:
                    quality = float(value)
                except ValueError:
                    valid = False
                    break
                if not 0 <= quality <= 1:
                    valid = False
                    break
            else:
                params[key] = value
        if valid:
            entries.append((media_type, params, quality))
    return entries


def _accept_specificity(media_type: str, params: Dict[str, str]) -> int:
    """Specificity of an Accept item.

    Exact media types beat wildcards, and a version parameter makes an item
    more specific than a bare media type."""
    if media_type == '*/*':
        return 0
    if media_type.endswith('/*'):
        return 1
    return 3 if 'version' in params else 2


def _resolve_offer(media_type: str, params: Dict[str, str]) -> Optional[Tuple[str, str, Callable[[Collector], bytes], str]]:
    """Map a single Accept item to a concrete offer.

    Returns (media_type, version, encoder, content_type) for items naming a
    format this exporter actually serves, else None. Unknown or malformed
    versions never resolve, so they cannot claim support."""
    if media_type == 'application/openmetrics-text':
        return _openmetrics_offer(params)
    if media_type == 'application/*':
        return _openmetrics_offer({})
    if media_type == 'text/plain':
        return _plain_offer(params)
    if media_type in ('text/*', '*/*'):
        return _plain_offer({})
    return None


def _openmetrics_offer(params: Dict[str, str]) -> Tuple[str, str, Callable[[Collector], bytes], str]:
    version = params.get('version')
    if version is None:
        # No version requested: serve 1.0.0 with default escaping. The
        # Content-Type does not claim an escaping parameter here.
        return (
            'application/openmetrics-text',
            '1.0.0',
            partial(openmetrics.generate_latest, escaping=openmetrics.UNDERSCORES, version='1.0.0'),
            openmetrics.CONTENT_TYPE_LATEST,
        )
    canonical = _supported_version(version, _OPENMETRICS_VERSIONS)
    if canonical is None:
        return None
    escaping = _supported_escaping(params.get('escaping'))
    return (
        'application/openmetrics-text',
        canonical,
        partial(openmetrics.generate_latest, escaping=escaping, version=canonical),
        f'application/openmetrics-text; version={canonical}; charset=utf-8; escaping={escaping}',
    )


def _plain_offer(params: Dict[str, str]) -> Tuple[str, str, Callable[[Collector], bytes], str]:
    version = params.get('version')
    if version is None:
        return 'text/plain', '0.0.4', generate_latest, CONTENT_TYPE_PLAIN_0_0_4
    canonical = _supported_version(version, _PLAIN_VERSIONS)
    if canonical is None:
        return None
    if canonical == '0.0.4':
        # The 0.0.4 format has no escaping parameter.
        return 'text/plain', '0.0.4', generate_latest, CONTENT_TYPE_PLAIN_0_0_4
    escaping = _supported_escaping(params.get('escaping'))
    return (
        'text/plain',
        canonical,
        partial(generate_latest, escaping=escaping),
        CONTENT_TYPE_PLAIN_1_0_0 + '; escaping=' + escaping,
    )


def _supported_version(value: str, known_versions: Dict[Tuple[int, ...], str]) -> Optional[str]:
    """Return the canonical string for a supported version, else None.

    A version with a non-numeric part, such as 1.0.0-rc.1 or a quoted
    value, is never treated as supported."""
    parsed = parse_version(value)
    if not all(isinstance(part, int) for part in parsed):
        return None
    return known_versions.get(parsed)


def _supported_escaping(value: Optional[str]) -> str:
    """Return the requested escaping scheme, defaulting to underscores."""
    if value in _ESCAPING_SCHEMES:
        return value
    return openmetrics.UNDERSCORES


def _is_excluded(exclusions: List[Tuple[str, Optional[str], int]], media_type: str, version: Optional[str], specificity: int) -> bool:
    """Check whether a concrete offer is covered by a q=0 exclusion.

    An exclusion only overrides candidates that are no more specific than
    the exclusion itself: a bare ``*/*;q=0`` cannot veto an explicitly
    requested type, while ``text/plain;q=0`` vetoes a ``text/*`` candidate.
    A version parameter on the exclusion narrows it to that version."""
    for excluded_type, excluded_version, excluded_specificity in exclusions:
        if excluded_specificity < specificity:
            continue
        if excluded_type == '*/*':
            type_matches = True
        elif excluded_type.endswith('/*'):
            type_matches = media_type.startswith(excluded_type[:-1])
        else:
            type_matches = excluded_type == media_type
        if not type_matches:
            continue
        if excluded_version is not None and excluded_version != version:
            continue
        return True
    return False


def gzip_accepted(accept_encoding_header: str) -> bool:
    accept_encoding_header = accept_encoding_header or ''
    for accepted in accept_encoding_header.split(','):
        encoding, *params = accepted.split(';')
        if encoding.strip().lower() != 'gzip':
            continue
        for param in params:
            key, sep, value = param.strip().partition('=')
            if sep and key.strip().lower() == 'q':
                try:
                    quality = float(value.strip())
                except ValueError:
                    return False
                return 0 < quality <= 1
        return True
    return False


class MetricsHandler(BaseHTTPRequestHandler):
    """HTTP handler that gives metrics from ``REGISTRY``."""
    registry: Collector = REGISTRY

    def do_GET(self) -> None:
        # Prepare parameters
        registry = self.registry
        accept_header = self.headers.get('Accept')
        accept_encoding_header = self.headers.get('Accept-Encoding')
        params = parse_qs(urlparse(self.path).query)
        # Bake output
        status, headers, output = _bake_output(registry, accept_header, accept_encoding_header, params, False)
        # Return output
        self.send_response(int(status.split(' ')[0]))
        for header in headers:
            self.send_header(*header)
        self.end_headers()
        self.wfile.write(output)

    def log_message(self, format: str, *args: Any) -> None:
        """Log nothing."""

    @classmethod
    def factory(cls, registry: Collector) -> type:
        """Returns a dynamic MetricsHandler class tied
           to the passed registry.
        """
        # This implementation relies on MetricsHandler.registry
        #  (defined above and defaulted to REGISTRY).

        # As we have unicode_literals, we need to create a str()
        #  object for type().
        cls_name = str(cls.__name__)
        MyMetricsHandler = type(cls_name, (cls, object),
                                {"registry": registry})
        return MyMetricsHandler


def write_to_textfile(path: str, registry: Collector, escaping: str = openmetrics.ALLOWUTF8, tmpdir: Optional[str] = None) -> None:
    """Write metrics to the given path.

    This is intended for use with the Node exporter textfile collector.
    The path must end in .prom for the textfile collector to process it.

    An optional tmpdir parameter can be set to determine where the
    metrics will be temporarily written to. If not set, it will be in
    the same directory as the .prom file. If provided, the path MUST be
    on the same filesystem."""
    if tmpdir is not None:
        filename = os.path.basename(path)
        tmppath = f'{os.path.join(tmpdir, filename)}.{os.getpid()}.{threading.current_thread().ident}'
    else:
        tmppath = f'{path}.{os.getpid()}.{threading.current_thread().ident}'
    try:
        with open(tmppath, 'wb') as f:
            f.write(generate_latest(registry, escaping))

        # rename(2) is atomic but fails on Windows if the destination file exists
        if os.name == 'nt':
            os.replace(tmppath, path)
        else:
            os.rename(tmppath, path)
    except Exception:
        if os.path.exists(tmppath):
            os.remove(tmppath)
        raise


def _make_handler(
        url: str,
        method: str,
        timeout: Optional[float],
        headers: Sequence[Tuple[str, str]],
        data: bytes,
        base_handler: Union[BaseHandler, type],
) -> Callable[[], None]:
    def handle() -> None:
        request = Request(url, data=data)
        request.get_method = lambda: method  # type: ignore
        for k, v in headers:
            request.add_header(k, v)
        resp = build_opener(base_handler).open(request, timeout=timeout)
        if resp.code >= 400:
            raise OSError(f"error talking to pushgateway: {resp.code} {resp.msg}")

    return handle


def default_handler(
        url: str,
        method: str,
        timeout: Optional[float],
        headers: List[Tuple[str, str]],
        data: bytes,
) -> Callable[[], None]:
    """Default handler that implements HTTP/HTTPS connections.

    Used by the push_to_gateway functions. Can be re-used by other handlers."""

    return _make_handler(url, method, timeout, headers, data, HTTPHandler)


def passthrough_redirect_handler(
        url: str,
        method: str,
        timeout: Optional[float],
        headers: List[Tuple[str, str]],
        data: bytes,
) -> Callable[[], None]:
    """
    Handler that automatically trusts redirect responses for all HTTP methods.

    Augments standard HTTPRedirectHandler capability by permitting PUT requests,
    preserving the method upon redirect, and passing through all headers and
    data from the original request. Only use this handler if you control or
    trust the source of redirect responses you encounter when making requests
    via the Prometheus client. This handler will simply repeat the identical
    request, including same method and data, to the new redirect URL."""

    return _make_handler(url, method, timeout, headers, data, _PrometheusRedirectHandler)


def basic_auth_handler(
        url: str,
        method: str,
        timeout: Optional[float],
        headers: List[Tuple[str, str]],
        data: bytes,
        username: Optional[str] = None,
        password: Optional[str] = None,
) -> Callable[[], None]:
    """Handler that implements HTTP/HTTPS connections with Basic Auth.

    Sets auth headers using supplied 'username' and 'password', if set.
    Used by the push_to_gateway functions. Can be re-used by other handlers."""

    def handle():
        """Handler that implements HTTP Basic Auth.
        """
        if username is not None and password is not None:
            auth_value = f'{username}:{password}'.encode()
            auth_token = base64.b64encode(auth_value)
            auth_header = b'Basic ' + auth_token
            headers.append(('Authorization', auth_header))
        default_handler(url, method, timeout, headers, data)()

    return handle


def tls_auth_handler(
        url: str,
        method: str,
        timeout: Optional[float],
        headers: List[Tuple[str, str]],
        data: bytes,
        certfile: str,
        keyfile: str,
        cafile: Optional[str] = None,
        protocol: int = ssl.PROTOCOL_TLS_CLIENT,
        insecure_skip_verify: bool = False,
) -> Callable[[], None]:
    """Handler that implements an HTTPS connection with TLS Auth.

    The default protocol (ssl.PROTOCOL_TLS_CLIENT) will also enable
    ssl.CERT_REQUIRED and SSLContext.check_hostname by default. This can be
    disabled by setting insecure_skip_verify to True.

    Both this handler and the TLS feature on pushgateay are experimental."""
    context = ssl.SSLContext(protocol=protocol)
    if cafile is not None:
        context.load_verify_locations(cafile)
    else:
        context.load_default_certs()

    if insecure_skip_verify:
        context.check_hostname = False
        context.verify_mode = ssl.CERT_NONE

    context.load_cert_chain(certfile=certfile, keyfile=keyfile)
    handler = HTTPSHandler(context=context)
    return _make_handler(url, method, timeout, headers, data, handler)


def push_to_gateway(
        gateway: str,
        job: str,
        registry: Collector,
        grouping_key: Optional[Dict[str, Any]] = None,
        timeout: Optional[float] = 30,
        handler: Callable = default_handler,
        compression: CompressionType = None,
) -> None:
    """Push metrics to the given pushgateway.

    `gateway` the url for your push gateway. Either of the form
              'http://pushgateway.local', or 'pushgateway.local'.
              Scheme defaults to 'http' if none is provided
    `job` is the job label to be attached to all pushed metrics
    `registry` is a Collector, normally an instance of CollectorRegistry
    `grouping_key` please see the pushgateway documentation for details.
                   Defaults to None
    `timeout` is how long push will attempt to connect before giving up.
              Defaults to 30s, can be set to None for no timeout.
    `handler` is an optional function which can be provided to perform
              requests to the 'gateway'.
              Defaults to None, in which case an http or https request
              will be carried out by a default handler.
              If not None, the argument must be a function which accepts
              the following arguments:
              url, method, timeout, headers, and content
              May be used to implement additional functionality not
              supported by the built-in default handler (such as SSL
              client certicates, and HTTP authentication mechanisms).
              'url' is the URL for the request, the 'gateway' argument
              described earlier will form the basis of this URL.
              'method' is the HTTP method which should be used when
              carrying out the request.
              'timeout' requests not successfully completed after this
              many seconds should be aborted.  If timeout is None, then
              the handler should not set a timeout.
              'headers' is a list of ("header-name","header-value") tuples
              which must be passed to the pushgateway in the form of HTTP
              request headers.
              The function should raise an exception (e.g. IOError) on
              failure.
              'content' is the data which should be used to form the HTTP
              Message Body.
    `compression` selects the payload compression. Supported values are 'gzip'
                  and 'snappy'. Defaults to None (no compression).

    This overwrites all metrics with the same job and grouping_key.
    This uses the PUT HTTP method."""
    _use_gateway('PUT', gateway, job, registry, grouping_key, timeout, handler, compression)


def pushadd_to_gateway(
        gateway: str,
        job: str,
        registry: Optional[Collector],
        grouping_key: Optional[Dict[str, Any]] = None,
        timeout: Optional[float] = 30,
        handler: Callable = default_handler,
        compression: CompressionType = None,
) -> None:
    """PushAdd metrics to the given pushgateway.

    `gateway` the url for your push gateway. Either of the form
              'http://pushgateway.local', or 'pushgateway.local'.
              Scheme defaults to 'http' if none is provided
    `job` is the job label to be attached to all pushed metrics
    `registry` is a Collector, normally an instance of CollectorRegistry
    `grouping_key` please see the pushgateway documentation for details.
                   Defaults to None
    `timeout` is how long push will attempt to connect before giving up.
              Defaults to 30s, can be set to None for no timeout.
    `handler` is an optional function which can be provided to perform
              requests to the 'gateway'.
              Defaults to None, in which case an http or https request
              will be carried out by a default handler.
              See the 'prometheus_client.push_to_gateway' documentation
              for implementation requirements.
    `compression` selects the payload compression. Supported values are 'gzip'
                  and 'snappy'. Defaults to None (no compression).

    This replaces metrics with the same name, job and grouping_key.
    This uses the POST HTTP method."""
    _use_gateway('POST', gateway, job, registry, grouping_key, timeout, handler, compression)


def delete_from_gateway(
        gateway: str,
        job: str,
        grouping_key: Optional[Dict[str, Any]] = None,
        timeout: Optional[float] = 30,
        handler: Callable = default_handler,
) -> None:
    """Delete metrics from the given pushgateway.

    `gateway` the url for your push gateway. Either of the form
              'http://pushgateway.local', or 'pushgateway.local'.
              Scheme defaults to 'http' if none is provided
    `job` is the job label to be attached to all pushed metrics
    `grouping_key` please see the pushgateway documentation for details.
                   Defaults to None
    `timeout` is how long delete will attempt to connect before giving up.
              Defaults to 30s, can be set to None for no timeout.
    `handler` is an optional function which can be provided to perform
              requests to the 'gateway'.
              Defaults to None, in which case an http or https request
              will be carried out by a default handler.
              See the 'prometheus_client.push_to_gateway' documentation
              for implementation requirements.

    This deletes metrics with the given job and grouping_key.
    This uses the DELETE HTTP method."""
    _use_gateway('DELETE', gateway, job, None, grouping_key, timeout, handler)


def _use_gateway(
        method: str,
        gateway: str,
        job: str,
        registry: Optional[Collector],
        grouping_key: Optional[Dict[str, Any]],
        timeout: Optional[float],
        handler: Callable,
        compression: CompressionType = None,
) -> None:
    gateway_url = urlparse(gateway)
    # See https://bugs.python.org/issue27657 for details on urlparse in py>=3.7.6.
    if not gateway_url.scheme or gateway_url.scheme not in ['http', 'https']:
        gateway = f'http://{gateway}'

    gateway = gateway.rstrip('/')
    url = '{}/metrics/{}/{}'.format(gateway, *_escape_grouping_key("job", job))

    if grouping_key is None:
        grouping_key = {}
    url += ''.join(
        '/{}/{}'.format(*_escape_grouping_key(str(k), str(v)))
        for k, v in sorted(grouping_key.items()))

    data = b''
    headers: List[Tuple[str, str]] = []
    if method != 'DELETE':
        if registry is None:
            registry = REGISTRY
        data = generate_latest(registry)
        data, headers = _compress_payload(data, compression)
    else:
        # DELETE requests still need Content-Type header per test expectations
        headers = [('Content-Type', CONTENT_TYPE_PLAIN_0_0_4)]
        if compression is not None:
            raise ValueError('Compression is not supported for DELETE requests.')

    handler(
        url=url, method=method, timeout=timeout,
        headers=headers, data=data,
    )()


def _compress_payload(data: bytes, compression: CompressionType) -> Tuple[bytes, List[Tuple[str, str]]]:
    headers = [('Content-Type', CONTENT_TYPE_PLAIN_0_0_4)]
    if compression is None:
        return data, headers

    encoding = compression.lower()
    if encoding == 'gzip':
        headers.append(('Content-Encoding', 'gzip'))
        return gzip.compress(data), headers
    if encoding == 'snappy':
        if not SNAPPY_AVAILABLE:
            raise RuntimeError('Snappy compression requires the python-snappy package to be installed.')
        headers.append(('Content-Encoding', 'snappy'))
        compressor = snappy.StreamCompressor()
        compressed = compressor.compress(data)
        flush = getattr(compressor, 'flush', None)
        if callable(flush):
            compressed += flush()
        return compressed, headers
    raise ValueError(f"Unsupported compression type: {compression}")


def _escape_grouping_key(k, v):
    if v == "":
        # Per https://github.com/prometheus/pushgateway/pull/346.
        return k + "@base64", "="
    elif '/' in v or ' ' in v or v in ('.', '..'):
        # Added in Pushgateway 0.9.0.
        # Use base64 encoding for values containing slashes or spaces, and
        # for "." and "..", which would otherwise be removed from the URL
        # path as dot segments (the Pushgateway answers with a redirect).
        return k + "@base64", base64.urlsafe_b64encode(v.encode("utf-8")).decode("utf-8")
    else:
        return k, quote_plus(v)


def instance_ip_grouping_key() -> Dict[str, Any]:
    """Grouping key with instance set to the IP Address of this host."""
    with closing(socket.socket(socket.AF_INET, socket.SOCK_DGRAM)) as s:
        if sys.platform == 'darwin':
            # This check is done this way only on MacOS devices
            # it is done this way because the localhost method does
            # not work.
            # This method was adapted from this StackOverflow answer:
            # https://stackoverflow.com/a/28950776
            s.connect(('10.255.255.255', 1))
        else:
            s.connect(('localhost', 0))

        return {'instance': s.getsockname()[0]}


from .asgi import make_asgi_app  # noqa
