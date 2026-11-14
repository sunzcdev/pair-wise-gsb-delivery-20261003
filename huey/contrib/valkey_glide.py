import base64

from glide_sync import GlideClient
from glide_sync import GlideClientConfiguration
from glide_sync import NodeAddress
from glide_sync import RangeByIndex
from glide_sync import Script
from glide_sync import Transaction

from huey.api import RedisHuey
from huey.constants import EmptyData
from huey.constants import RESCHEDULE_NOT_FOUND
from huey.constants import RESCHEDULE_OK
from huey.exceptions import ConfigurationError
from huey.storage import PQUEUE_TO_SCHEDULE_LUA
from huey.storage import QUEUE_REPLACE_LUA
from huey.storage import QUEUE_TO_SCHEDULE_LUA
from huey.storage import RedisStorage
from huey.storage import SCHEDULE_POP_LUA
from huey.storage import SCHEDULE_REMOVE_LUA
from huey.storage import SCHEDULE_RESCHEDULE_LUA


class ValkeyGlideStorage(RedisStorage):
    def __init__(self, name='huey', blocking=False, read_timeout=1,
                 client=None, client_config=None, host=None, port=None,
                 client_name=None, **client_config_params):
        if sum(1 for p in (client, client_config, host) if p) > 1:
            raise ConfigurationError('Specify only one of the following: '
                                     '"client", "client_config" or "host"')

        if client is not None:
            self.conn = client
        elif client_config is not None:
            self.conn = GlideClient.create(client_config)
        else:
            host = host or '127.0.0.1'
            port = port or 6379
            config = GlideClientConfiguration(
                [NodeAddress(host, port)],
                **client_config_params)
            self.conn = GlideClient.create(config)

        self._pop = Script(SCHEDULE_POP_LUA)
        self._queue_to_schedule = Script(QUEUE_TO_SCHEDULE_LUA)
        self._pqueue_to_schedule = Script(PQUEUE_TO_SCHEDULE_LUA)
        self._schedule_reschedule = Script(SCHEDULE_RESCHEDULE_LUA)
        self._schedule_remove = Script(SCHEDULE_REMOVE_LUA)
        self._queue_replace = Script(QUEUE_REPLACE_LUA)

        self.name = self.clean_name(name)
        self.queue_key = 'huey.redis.%s' % self.name
        self.schedule_key = 'huey.schedule.%s' % self.name
        self.result_key = 'huey.results.%s' % self.name
        self.counter_key = 'huey.counters.%s' % self.name
        # Glide can't block, so results are polled.
        self.notify_prefix = 'huey.notify.%s.' % self.name
        self.notify_result = False
        self.notify_result_ttl = None

        if client_name is not None:
            self.conn.client_setname(client_name)

        if blocking:
            raise ConfigurationError('Valkey glide does not seem to work '
                                     'properly with blocking operations.')
        self.read_timeout = read_timeout

    def enqueue(self, data, priority=None):
        if priority:
            raise NotImplementedError('Task priorities are not supported by '
                                      'this storage.')
        self.conn.lpush(self.queue_key, [data])

    def dequeue(self):
        return self.conn.rpop(self.queue_key)

    def add_to_schedule(self, data, ts):
        data = base64.b64encode(data).decode('utf8')
        self.conn.zadd(self.schedule_key, {data: self.convert_ts(ts)})

    def read_schedule(self, ts):
        unix_ts = self.convert_ts(ts)
        tasks = self.conn.invoke_script(self._pop, keys=[self.schedule_key],
                                        args=[str(unix_ts)])
        return [] if tasks is None else [base64.b64decode(t) for t in tasks]

    def scheduled_items(self, limit=None):
        stop = limit - 1 if limit else -1
        tasks = self.conn.zrange(self.schedule_key, RangeByIndex(0, stop))
        return [base64.b64decode(t) for t in tasks]

    def reschedule_from_queue(self, old_data, new_data, ts):
        # The queue stores raw bytes while the schedule stores base64-encoded
        # messages, so encode the replacement before invoking the lua script.
        new_encoded = base64.b64encode(new_data).decode('utf8')
        score = str(self.convert_ts(ts))
        moved = self.conn.invoke_script(
            self._queue_to_schedule,
            keys=[self.queue_key, self.schedule_key],
            args=[old_data, new_encoded, score])
        return RESCHEDULE_OK if moved else RESCHEDULE_NOT_FOUND

    def reschedule_from_schedule(self, old_data, new_data, ts):
        # Messages in the schedule are base64-encoded.
        old_encoded = base64.b64encode(old_data).decode('utf8')
        new_encoded = base64.b64encode(new_data).decode('utf8')
        score = str(self.convert_ts(ts))
        updated = self.conn.invoke_script(
            self._schedule_reschedule,
            keys=[self.schedule_key],
            args=[old_encoded, new_encoded, score])
        return RESCHEDULE_OK if updated else RESCHEDULE_NOT_FOUND

    def replace_in_queue(self, old_data, new_data):
        # The ready queue stores raw (unencoded) messages.
        replaced = self.conn.invoke_script(
            self._queue_replace,
            keys=[self.queue_key],
            args=[old_data, new_data])
        return RESCHEDULE_OK if replaced else RESCHEDULE_NOT_FOUND

    def remove_from_schedule(self, old_data):
        # Messages in the schedule are base64-encoded.
        old_encoded = base64.b64encode(old_data).decode('utf8')
        removed = self.conn.invoke_script(
            self._schedule_remove,
            keys=[self.schedule_key],
            args=[old_encoded])
        return RESCHEDULE_OK if removed else RESCHEDULE_NOT_FOUND

    def put_data(self, key, value, is_result=False):
        self.conn.hset(self.result_key, {key: value})

    def peek_data(self, key):
        tx = Transaction()
        tx.hexists(self.result_key, key)
        tx.hget(self.result_key, key)
        exists, val = getattr(self.conn, 'exec')(tx, False)
        return EmptyData if not exists else val

    def peek_many(self, keys):
        values = self.conn.hmget(self.result_key, keys)
        return {k: v for k, v in zip(keys, values) if v is not None}

    def put_if_empty(self, key, value, ttl=None):
        if ttl is not None:
            raise NotImplementedError('ttl is not supported by this storage.')
        return self.conn.hsetnx(self.result_key, key, value)

    def pop_data(self, key):
        tx = Transaction()
        tx.hexists(self.result_key, key)
        tx.hget(self.result_key, key)
        tx.hdel(self.result_key, [key])
        exists, val, n = getattr(self.conn, 'exec')(tx, False)
        return EmptyData if not exists else val

    def delete_data(self, key):
        return self.conn.hdel(self.result_key, [key]) != 0

    def flush_queue(self):
        self.conn.delete([self.queue_key])

    def flush_schedule(self):
        self.conn.delete([self.schedule_key])

    def flush_results(self):
        self.conn.delete([self.result_key])

    def delete_counter(self, key):
        self.conn.hdel(self.counter_key, [key])

    def flush_counters(self):
        self.conn.delete([self.counter_key])


class ValkeyGlideHuey(RedisHuey):
    storage_class = ValkeyGlideStorage
