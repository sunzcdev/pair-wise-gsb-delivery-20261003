WORKER_THREAD = 'thread'
WORKER_GREENLET = 'greenlet'
WORKER_PROCESS = 'process'
WORKER_TYPES = (WORKER_THREAD, WORKER_GREENLET, WORKER_PROCESS)

# Failure policies for pre-execute and post-execute hooks.
HOOK_IGNORE = 'ignore'  # Log the failure and continue (default).
HOOK_ABORT = 'abort'    # Required hook: failure aborts / fails the task.
HOOK_POLICIES = (HOOK_IGNORE, HOOK_ABORT)


class EmptyData(object):
    pass
