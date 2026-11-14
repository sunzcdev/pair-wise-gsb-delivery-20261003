WORKER_THREAD = 'thread'
WORKER_GREENLET = 'greenlet'
WORKER_PROCESS = 'process'
WORKER_TYPES = (WORKER_THREAD, WORKER_GREENLET, WORKER_PROCESS)


class EmptyData(object):
    pass


# Status codes returned by the storage-layer in-place task rescheduling
# operations (``reschedule_from_queue`` and ``reschedule_from_schedule``).
# The task was found in the expected location and atomically moved.
RESCHEDULE_OK = 'ok'
# The task was not found: it has already been dequeued/claimed by a worker,
# has completed, or never existed.
RESCHEDULE_NOT_FOUND = 'not_found'
