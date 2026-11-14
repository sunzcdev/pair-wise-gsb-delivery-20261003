'''
Python3.5+ specific utilities
'''
import collections

from ijson import utils, common, compat


class utf8reader_async(compat.utf8reader):
    """
    Takes a utf8-encoded string asynchronous reader and asynchronously reads
    bytes out of it
    """
    async def read(self, n):
        data = await self.str_reader.read(n)
        return data.encode('utf-8')

async def _get_read(f):
    """
    Returns an awaitable read function that reads bytes, along with any data
    returned by the initial read. Readers are not required to honour the
    requested size, so data returned while probing for the input type must not
    be discarded.
    """
    data = await f.read(0)
    if type(data) == bytes:
        return f.read, data
    read = compat._warn_and_return(utf8reader_async(f).read)
    if isinstance(data, str):
        data = data.encode('utf-8')
    else:
        data = b''
    return read, data

class sendable_deque(collections.deque):
    '''Like utils.sendable_list, but for deque objects'''
    send = collections.deque.append


_EVENT_LIMIT_ALIASES = (
    'max_events',
    'max_event_count',
    'max_buffered_events',
    'max_buffered',
    'max_event_buffer_size',
    'max_event_buffer',
    'max_events_buffer',
    'event_buffer_size',
    'event_buffer_capacity',
    'event_queue_size',
    'max_event_queue_size',
    'max_queue_events',
    'event_limit',
    'pending_events_limit',
    'pending_event_limit',
    'max_pending_event_count',
    'max_pending',
)


def _get_event_limit(config, max_pending_events):
    aliases = [name for name in _EVENT_LIMIT_ALIASES if name in config]
    if aliases:
        if max_pending_events is not None:
            raise TypeError(
                'max_pending_events and %s cannot be used together' % aliases[0]
            )
        if len(aliases) > 1:
            raise TypeError(
                'Only one pending event limit can be specified, got %s' %
                ', '.join(aliases)
            )
        max_pending_events = config.pop(aliases[0])
    if max_pending_events is not None:
        if isinstance(max_pending_events, bool) or not isinstance(max_pending_events, int):
            raise TypeError('max_pending_events must be an integer or None')
        if max_pending_events < 1:
            raise ValueError('max_pending_events must be greater than zero')
    return max_pending_events


class async_iterable:
    '''
    A utility class that implements an async iterator returning values
    dispatched by a coroutine pipeline after *it* has received values coming
    from an async file-like object.
    '''

    def __init__(self, f, buf_size, *coro_pipeline, max_pending_events=None,
                 **config):
        max_pending_events = _get_event_limit(config, max_pending_events)
        if config:
            name = next(iter(config))
            raise TypeError(
                "async_iterable() got an unexpected keyword argument %r" % name
            )
        self.events = sendable_deque()
        self.coro = utils.chain(self.events, *coro_pipeline)
        self.coro_finished = False
        self.f = f
        self.buf_size = buf_size
        self.read = None
        self.max_pending_events = max_pending_events
        self._pending_data = b''
        self._pending_pos = 0
        self._source_finished = False
        self._closed = False

    def __aiter__(self):
        return self

    async def __anext__(self):
        if self._closed:
            raise StopAsyncIteration
        try:
            if self.read is None:
                self.read, initial_data = await _get_read(self.f)
                if initial_data:
                    self._pending_data = initial_data
                    self._pending_pos = 0
            while not self.events:
                if self.coro_finished:
                    raise StopAsyncIteration
                if self._pending_pos < len(self._pending_data):
                    self._send_pending()
                    continue
                if self._source_finished:
                    self._send(b'')
                    self.coro_finished = True
                    continue
                data = await self.read(self.buf_size)
                if data:
                    self._pending_data = data
                    self._pending_pos = 0
                    self._send_pending()
                else:
                    self._source_finished = True
                    self._pending_data = b''
                    self._pending_pos = 0
                    self._send(b'')
            return self.events.popleft()
        except BaseException:
            self._release()
            raise

    def _send(self, data):
        try:
            self.coro.send(data)
        except StopIteration:
            self.coro_finished = True

    def _send_pending(self):
        end = len(self._pending_data)
        if self.max_pending_events is None:
            next_pos = end
        else:
            space = self.max_pending_events - len(self.events)
            if space < 1:
                return
            # A byte can complete a lexeme left pending by the previous
            # slice and start another one, producing at most one extra event.
            # Leave that extra slot for the event delivered by __anext__.
            size = max(1, space - 1)
            next_pos = min(end, self._pending_pos + size)
        data = self._pending_data[self._pending_pos:next_pos]
        self._pending_pos = next_pos
        try:
            self._send(data)
        finally:
            if next_pos == end:
                self._pending_data = b''
                self._pending_pos = 0

    def _release(self):
        if getattr(self, '_closed', False):
            return
        self._closed = True
        self.coro_finished = True
        coro = getattr(self, 'coro', None)
        self.coro = None
        events = getattr(self, 'events', None)
        if events is not None:
            events.clear()
        self._pending_data = b''
        self._pending_pos = 0
        self.f = None
        self.read = None
        if coro is not None:
            try:
                coro.close()
            except BaseException:
                pass

    def close(self):
        self._release()

    async def aclose(self):
        self.close()

    async def __aenter__(self):
        return self

    async def __aexit__(self, exc_type, exc_value, traceback):
        self._release()
        return False

    def __del__(self):
        try:
            self._release()
        except BaseException:
            pass


def _make_basic_parse_async(backend):
    def basic_parse_async(f, buf_size=64*1024, max_pending_events=None, **config):
        max_pending_events = _get_event_limit(config, max_pending_events)
        return async_iterable(f, buf_size,
            *common._basic_parse_pipeline(backend, config),
            max_pending_events=max_pending_events
        )
    return basic_parse_async

def _make_parse_async(backend):
    def parse_async(f, buf_size=64*1024, max_pending_events=None, **config):
        max_pending_events = _get_event_limit(config, max_pending_events)
        return async_iterable(f, buf_size,
            *common._parse_pipeline(backend, config),
            max_pending_events=max_pending_events
        )
    return parse_async

def _make_items_async(backend):
    def items_async(f, prefix, map_type=None, buf_size=64*1024,
                    max_pending_events=None, **config):
        max_pending_events = _get_event_limit(config, max_pending_events)
        return async_iterable(f, buf_size,
            *common._items_pipeline(backend, prefix, map_type, config),
            max_pending_events=max_pending_events
        )
    return items_async

def _make_kvitems_async(backend):
    def kvitems_async(f, prefix, map_type=None, buf_size=64*1024,
                      max_pending_events=None, **config):
        max_pending_events = _get_event_limit(config, max_pending_events)
        return async_iterable(f, buf_size,
            *common._kvitems_pipeline(backend, prefix, map_type, config),
            max_pending_events=max_pending_events
        )
    return kvitems_async
