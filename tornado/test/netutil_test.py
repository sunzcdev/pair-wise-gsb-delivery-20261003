import errno
import signal
import socket
import sys
import time
import typing
import unittest
from subprocess import Popen
from unittest import mock

from tornado.netutil import (
    BlockingResolver,
    OverrideResolver,
    ThreadedResolver,
    bind_sockets,
    is_valid_ip,
)
from tornado.test.util import (
    AsyncTestCase,
    TestCase,
    abstract_base_test,
    skipIfNoNetwork,
)
from tornado.testing import bind_unused_port, gen_test

try:
    import pycares  # type: ignore
except ImportError:
    pycares = None
else:
    from tornado.platform.caresresolver import CaresResolver


@abstract_base_test
class _ResolverTestMixin(AsyncTestCase):
    resolver: typing.Any = None

    @gen_test
    def test_localhost(self):
        addrinfo = yield self.resolver.resolve("localhost", 80, socket.AF_UNSPEC)
        # Most of the time localhost resolves to either the ipv4 loopback
        # address alone, or ipv4+ipv6. But some versions of pycares will only
        # return the ipv6 version, so we have to check for either one alone.
        self.assertTrue(
            ((socket.AF_INET, ("127.0.0.1", 80)) in addrinfo)
            or ((socket.AF_INET6, ("::1", 80)) in addrinfo),
            f"loopback address not found in {addrinfo}",
        )


# It is impossible to quickly and consistently generate an error in name
# resolution, so test this case separately, using mocks as needed.
@abstract_base_test
class _ResolverErrorTestMixin(AsyncTestCase):
    resolver: typing.Any = None

    @gen_test
    def test_bad_host(self):
        with self.assertRaises(IOError):
            yield self.resolver.resolve("an invalid domain", 80, socket.AF_UNSPEC)


def _failing_getaddrinfo(*args):
    """Dummy implementation of getaddrinfo for use in mocks"""
    raise socket.gaierror(errno.EIO, "mock: lookup failed")


@skipIfNoNetwork
class BlockingResolverTest(_ResolverTestMixin):
    def setUp(self):
        super().setUp()
        self.resolver = BlockingResolver()


# getaddrinfo-based tests need mocking to reliably generate errors;
# some configurations are slow to produce errors and take longer than
# our default timeout.
class BlockingResolverErrorTest(_ResolverErrorTestMixin):
    def setUp(self):
        super().setUp()
        self.resolver = BlockingResolver()
        self.real_getaddrinfo = socket.getaddrinfo
        socket.getaddrinfo = _failing_getaddrinfo

    def tearDown(self):
        socket.getaddrinfo = self.real_getaddrinfo
        super().tearDown()


class OverrideResolverTest(_ResolverTestMixin):
    def setUp(self):
        super().setUp()
        mapping = {
            ("google.com", 80): ("1.2.3.4", 80),
            ("google.com", 80, socket.AF_INET): ("1.2.3.4", 80),
            ("google.com", 80, socket.AF_INET6): (
                "2a02:6b8:7c:40c:c51e:495f:e23a:3",
                80,
            ),
        }
        self.resolver = OverrideResolver(BlockingResolver(), mapping)

    @gen_test
    def test_resolve_multiaddr(self):
        result = yield self.resolver.resolve("google.com", 80, socket.AF_INET)
        self.assertIn((socket.AF_INET, ("1.2.3.4", 80)), result)

        result = yield self.resolver.resolve("google.com", 80, socket.AF_INET6)
        self.assertIn(
            (socket.AF_INET6, ("2a02:6b8:7c:40c:c51e:495f:e23a:3", 80, 0, 0)), result
        )


@skipIfNoNetwork
class ThreadedResolverTest(_ResolverTestMixin):
    def setUp(self):
        super().setUp()
        self.resolver = ThreadedResolver()

    def tearDown(self):
        self.resolver.close()
        # ThreadedResolver uses a global thread pool, so we have to shut it down
        if ThreadedResolver._threadpool is not None:
            ThreadedResolver._threadpool.shutdown(wait=True)
            ThreadedResolver._threadpool = None
        super().tearDown()


class ThreadedResolverErrorTest(_ResolverErrorTestMixin):
    def setUp(self):
        super().setUp()
        self.resolver = BlockingResolver()
        self.real_getaddrinfo = socket.getaddrinfo
        socket.getaddrinfo = _failing_getaddrinfo

    def tearDown(self):
        socket.getaddrinfo = self.real_getaddrinfo
        super().tearDown()


@skipIfNoNetwork
@unittest.skipIf(sys.platform == "win32", "preexec_fn not available on win32")
class ThreadedResolverImportTest(TestCase):
    def test_import(self):
        TIMEOUT = 5

        # Test for a deadlock when importing a module that runs the
        # ThreadedResolver at import-time. See resolve_test.py for
        # full explanation.
        command = [sys.executable, "-c", "import tornado.test.resolve_test_helper"]

        start = time.time()
        popen = Popen(command, preexec_fn=lambda: signal.alarm(TIMEOUT))
        while time.time() - start < TIMEOUT:
            return_code = popen.poll()
            if return_code is not None:
                self.assertEqual(0, return_code)
                return  # Success.
            time.sleep(0.05)

        self.fail("import timed out")


# We do not test errors with CaresResolver:
# Some DNS-hijacking ISPs (e.g. Time Warner) return non-empty results
# with an NXDOMAIN status code.  Most resolvers treat this as an error;
# C-ares returns the results, making the "bad_host" tests unreliable.
# C-ares will try to resolve even malformed names, such as the
# name with spaces used in this test.
@skipIfNoNetwork
@unittest.skipIf(pycares is None, "pycares module not present")
@unittest.skipIf(sys.platform == "win32", "pycares doesn't return loopback on windows")
@unittest.skipIf(sys.platform == "darwin", "pycares doesn't return 127.0.0.1 on darwin")
class CaresResolverTest(_ResolverTestMixin):
    def setUp(self):
        super().setUp()
        self.resolver = CaresResolver()


class IsValidIPTest(TestCase):
    def test_is_valid_ip(self):
        self.assertTrue(is_valid_ip("127.0.0.1"))
        self.assertTrue(is_valid_ip("4.4.4.4"))
        self.assertTrue(is_valid_ip("::1"))
        self.assertTrue(is_valid_ip("2620:0:1cfe:face:b00c::3"))
        self.assertFalse(is_valid_ip("www.google.com"))
        self.assertFalse(is_valid_ip("localhost"))
        self.assertFalse(is_valid_ip("4.4.4.4<"))
        self.assertFalse(is_valid_ip(" 127.0.0.1"))
        self.assertFalse(is_valid_ip(""))
        self.assertFalse(is_valid_ip(" "))
        self.assertFalse(is_valid_ip("\n"))
        self.assertFalse(is_valid_ip("\x00"))
        self.assertFalse(is_valid_ip("a" * 100))


class TestPortAllocation(TestCase):
    def test_same_port_allocation(self):
        sockets = bind_sockets(0, "localhost")
        try:
            port = sockets[0].getsockname()[1]
            self.assertTrue(all(s.getsockname()[1] == port for s in sockets[1:]))
        finally:
            for sock in sockets:
                sock.close()

    @unittest.skipIf(
        not hasattr(socket, "SO_REUSEPORT"), "SO_REUSEPORT is not supported"
    )
    def test_reuse_port(self):
        sockets: list[socket.socket] = []
        sock, port = bind_unused_port(reuse_port=True)
        try:
            sockets = bind_sockets(port, "127.0.0.1", reuse_port=True)
            self.assertTrue(all(s.getsockname()[1] == port for s in sockets))
        finally:
            sock.close()
            for sock in sockets:
                sock.close()


class _FakeBoundSocket:
    """Socket double used to exercise bind_sockets' retry logic.

    Instances record everything that happens to them so tests can verify
    that handles opened during failed binding rounds are released.
    """

    def __init__(
        self,
        family: int,
        socktype: int,
        proto: int,
        controller: "_RetryBindController",
    ):
        self.family = family
        self.socktype = socktype
        self.proto = proto
        self._controller = controller
        self._bound = False
        self._listening = False
        self._closed = False
        self._sockname: typing.Any = None

    def setsockopt(self, *args: typing.Any) -> None:
        self._check_open()

    def setblocking(self, flag: bool) -> None:
        self._check_open()

    def bind(self, addr: typing.Any) -> None:
        self._check_open()
        # Delegate to the controller, which decides whether this bind
        # succeeds and which port the socket ends up on.
        self._sockname = self._controller.handle_bind(self, addr)
        self._bound = True

    def getsockname(self) -> typing.Any:
        if self._sockname is None:
            raise OSError(errno.ENOTCONN, "not bound")
        return self._sockname

    def listen(self, backlog: int) -> None:
        self._check_open()
        if not self._bound:
            raise OSError(errno.EINVAL, "not bound")
        self._listening = True

    def close(self) -> None:
        self._closed = True

    def fileno(self) -> int:
        return id(self) % 10000

    def _check_open(self) -> None:
        if self._closed:
            raise OSError(errno.EBADF, "bad file descriptor")

    @property
    def closed(self) -> bool:
        return self._closed

    @property
    def listening(self) -> bool:
        return self._listening


class _RetryBindController:
    """Drives _FakeBoundSocket behavior for retry tests.

    For the first ``fail_rounds`` rounds, the bind of the second address
    family (AF_INET6, when AF_INET is bound first) raises EADDRINUSE.
    Afterwards every bind succeeds, with a fresh ephemeral port per round.
    """

    def __init__(self, fail_rounds: int = 1) -> None:
        self.fail_rounds = fail_rounds
        self.round = 0
        self.sockets: list[_FakeBoundSocket] = []
        self._round_seen_ipv4 = False

    def make_socket(
        self, family: int, socktype: int, proto: int
    ) -> _FakeBoundSocket:
        # A new IPv4 socket marks the start of another binding round.
        if family == socket.AF_INET:
            if self._round_seen_ipv4:
                self.round += 1
            self._round_seen_ipv4 = True
        fake = _FakeBoundSocket(family, socktype, proto, self)
        self.sockets.append(fake)
        return fake

    def handle_bind(self, fake: _FakeBoundSocket, addr: typing.Any) -> typing.Any:
        host = addr[0]
        requested_port = addr[1]
        if fake.family == socket.AF_INET6 and (
            requested_port != 0 and self.round < self.fail_rounds
        ):
            # The IPv4 socket in this round took an ephemeral port which
            # happens to be unavailable for IPv6.
            raise OSError(errno.EADDRINUSE, "address already in use")
        if requested_port == 0:
            port = 10000 + self.round * 100 + len(self.sockets)
        else:
            port = requested_port
        if fake.family == socket.AF_INET6:
            return (host, port, 0, 0)
        return (host, port)


def _dual_stack_getaddrinfo(
    host: typing.Any,
    port: int,
    family: int,
    socktype: int,
    proto: int,
    flags: int,
) -> list[typing.Any]:
    results = [
        (
            socket.AF_INET,
            socket.SOCK_STREAM,
            0,
            "",
            ("127.0.0.1", port),
        ),
        (
            socket.AF_INET6,
            socket.SOCK_STREAM,
            0,
            "",
            ("::1", port, 0, 0),
        ),
    ]
    if family == socket.AF_INET:
        return results[:1]
    if family == socket.AF_INET6:
        return results[1:]
    return results


class BindSocketsRetryTest(TestCase):
    def _patches(self, controller: _RetryBindController) -> list[typing.Any]:
        return [
            mock.patch.object(socket, "getaddrinfo", _dual_stack_getaddrinfo),
            mock.patch.object(socket, "socket", controller.make_socket),
        ]

    def test_retry_succeeds_on_second_attempt(self):
        # The second address family's bind fails with EADDRINUSE on the
        # first round and succeeds on the second.
        controller = _RetryBindController(fail_rounds=1)
        patches = self._patches(controller)
        for patcher in patches:
            patcher.start()
        try:
            sockets = bind_sockets(0, "localhost", max_attempts=2)
        finally:
            for patcher in patches:
                patcher.stop()

        published: list[_FakeBoundSocket] = typing.cast(
            "list[_FakeBoundSocket]", sockets
        )
        try:
            self.assertEqual(len(sockets), 2)
            port = sockets[0].getsockname()[1]
            # Only the final, matching pair is published, and both share
            # the same freshly selected port.
            self.assertEqual(sockets[1].getsockname()[1], port)
            self.assertEqual(controller.round, 1)
            # Four sockets were opened in total (two per round); the two
            # from the failed round must have been released while the
            # returned pair stays open.
            self.assertEqual(len(controller.sockets), 4)
            failed_round_sockets = controller.sockets[:2]
            published_sockets = controller.sockets[2:]
            self.assertTrue(all(sock.closed for sock in failed_round_sockets))
            self.assertTrue(all(not sock.closed for sock in published_sockets))
            self.assertEqual(published, published_sockets)
            self.assertTrue(all(sock.listening for sock in published_sockets))
        finally:
            for sock in sockets:
                sock.close()

    def test_retry_exhausted_raises_and_releases_handles(self):
        # Every round hits the transient conflict; with max_attempts=1
        # there is no retry and the original error propagates.
        controller = _RetryBindController(fail_rounds=1)
        patches = self._patches(controller)
        for patcher in patches:
            patcher.start()
        try:
            with self.assertRaises(OSError) as cm:
                bind_sockets(0, "localhost", max_attempts=1)
            self.assertEqual(cm.exception.errno, errno.EADDRINUSE)
        finally:
            for patcher in patches:
                patcher.stop()

        # The whole failed round's handles are released, nothing leaks.
        self.assertTrue(all(s.closed for s in controller.sockets))
        self.assertEqual(controller.round, 0)

    def test_retry_exhausted_after_multiple_attempts(self):
        controller = _RetryBindController(fail_rounds=10)
        patches = self._patches(controller)
        for patcher in patches:
            patcher.start()
        try:
            with self.assertRaises(OSError) as cm:
                bind_sockets(0, "localhost", max_attempts=3)
            self.assertEqual(cm.exception.errno, errno.EADDRINUSE)
        finally:
            for patcher in patches:
                patcher.stop()

        self.assertEqual(controller.round, 2)
        # Two sockets per failed round, all of them closed.
        self.assertEqual(len(controller.sockets), 6)
        self.assertTrue(all(s.closed for s in controller.sockets))

    def test_no_ipv6_falls_back_without_retry(self):
        # An environment with no IPv6 support: AF_INET6 socket creation
        # reports EAFNOSUPPORT. The declared family is simply skipped,
        # so the single IPv4 socket is returned without any retry.
        controller = _RetryBindController(fail_rounds=0)

        def socket_factory(
            family: int, socktype: int, proto: int
        ) -> _FakeBoundSocket:
            if family == socket.AF_INET6:
                raise OSError(errno.EAFNOSUPPORT, "address family not supported")
            return controller.make_socket(family, socktype, proto)

        with mock.patch.object(
            socket, "getaddrinfo", _dual_stack_getaddrinfo
        ), mock.patch.object(socket, "socket", socket_factory):
            sockets = bind_sockets(0, "localhost", max_attempts=3)

        only_socket = typing.cast("_FakeBoundSocket", sockets[0])
        try:
            self.assertEqual(len(sockets), 1)
            self.assertEqual(only_socket.family, socket.AF_INET)
            self.assertEqual(controller.round, 0)
            self.assertEqual(len(controller.sockets), 1)
            self.assertFalse(controller.sockets[0].closed)
        finally:
            for sock in sockets:
                sock.close()

    def test_invalid_max_attempts(self) -> None:
        for bad_value in (0, -1):
            with self.assertRaises(ValueError):
                bind_sockets(0, "localhost", max_attempts=bad_value)
