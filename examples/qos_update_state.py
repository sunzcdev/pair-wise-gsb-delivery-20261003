"""
Track the effective state of a dynamically adjusted QoS prefetch count.

This example uses a controlled ``basic_qos`` callback instead of a real
broker, so it can be run offline.  It shows how several adjustments made in
the same round are merged into a single call, how a failed call keeps the
value pending instead of reporting it as applied, and how the value is sent
again after the "connection" is revived.

Run it with::

    python examples/qos_update_state.py
"""
from __future__ import annotations

from kombu import Consumer, Exchange, Queue


class ControlledChannel:
    """Channel stand-in that records the QoS calls it receives."""

    def __init__(self, fail_next=0):
        self.calls = []
        self.fail_next = fail_next

    def basic_qos(self, prefetch_size, prefetch_count, apply_global):
        if self.fail_next:
            self.fail_next -= 1
            raise OSError('connection lost while sending basic.qos')
        self.calls.append((prefetch_size, prefetch_count, apply_global))
        return prefetch_count


def show(label, state):
    print(
        f'{label}: desired={state.desired} sent={state.sent} '
        f'pending={state.pending} applied={state.applied} '
        f'in_sync={state.in_sync} failures={state.failures}'
    )


exchange = Exchange('media', 'direct')
queue = Queue('video', exchange=exchange, routing_key='video')
channel = ControlledChannel()
consumer = Consumer(channel, [queue], auto_declare=False)

# 1. Several adjustments in one round are merged into a single call.
print('-- merging adjustments in one round --')
with consumer.qos_batch():
    consumer.qos_update(prefetch_count=10)
    consumer.qos_update(prefetch_count=25)
    consumer.qos_update(prefetch_count=5)
assert channel.calls == [(0, 5, False)], channel.calls
show('after batch', consumer.qos_state())

# 2. A failing call is not reported as applied: the value stays pending.
print('-- a failed call stays pending --')
channel.fail_next = 1
try:
    consumer.qos(prefetch_count=12)
except OSError as exc:
    print('basic_qos failed:', exc)
show('after failure', consumer.qos_state())
assert consumer.qos_state().pending == 12
assert consumer.qos_state().sent == 5

# 3. Reapplying after the connection revived sends the value again.
print('-- reapply after revival --')
channel.fail_next = 0
consumer.reapply_qos()
show('after reapply', consumer.qos_state())
assert channel.calls[-1] == (0, 12, False)

# 4. Zero keeps its protocol meaning of "no limit".
print('-- zero means unlimited --')
consumer.qos(prefetch_count=0)
show('unlimited', consumer.qos_state())
assert consumer.qos_state().sent == 0

print('final basic_qos calls:', channel.calls)
