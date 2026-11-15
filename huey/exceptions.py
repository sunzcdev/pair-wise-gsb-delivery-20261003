class HueyException(Exception): pass
class ConfigurationError(HueyException): pass
class TaskLockedException(HueyException): pass
class ResultTimeout(HueyException): pass
class TaskTimeout(HueyException): pass

class RateLimitExceeded(HueyException):
    def __init__(self, key, delay, retry=True):
        self.key, self.delay, self.retry = key, delay, retry
        super(RateLimitExceeded, self).__init__(key, delay, retry)

    def __str__(self):
        if self.retry:
            return 'Rate limit exceeded on "%s", retry in %0.1fs' % (
                self.key, self.delay)
        return 'Rate limit exceeded on "%s"' % self.key

class CancelExecution(Exception):
    def __init__(self, msg=None, retry=None):
        self.retry = retry
        args = () if msg is None else (msg,)
        super(CancelExecution, self).__init__(*args)
class RetryTask(Exception):
    def __init__(self, msg=None, eta=None, delay=None, *args, **kwargs):
        self.eta, self.delay = eta, delay
        super(RetryTask, self).__init__(msg, *args, **kwargs)
class TaskException(Exception):
    def __init__(self, metadata=None, *args):
        self.metadata = metadata or {}
        super(TaskException, self).__init__(*args)

    def __str__(self):
        return self.metadata.get('error') or 'unknown error'


class RetryDecisionError(HueyException):
    """
    Raised when a task's retry-policy callback itself fails, or when it
    returns an unusable decision. Wraps both the exception raised by the task
    and the error encountered while deciding how to retry it. The task is not
    retried again -- retrying would just re-enter the broken policy, risking
    an endless loop.
    """
    def __init__(self, task_error, decision_error):
        self.task_error = task_error
        self.decision_error = decision_error
        super(RetryDecisionError, self).__init__(
            'retry policy failed for %r: %r' % (task_error, decision_error))

    def __repr__(self):
        return 'RetryDecisionError(task_error=%r, decision_error=%r)' % (
            self.task_error, self.decision_error)


class RetryDeadlineExceeded(HueyException):
    """
    Raised when a retry-policy callback asks for a next attempt that would
    run after the task's total deadline (or the deadline has already passed).
    Marks an explicit, final failure rather than silently dropping the retry
    or scheduling it outside the allowed window.
    """
    def __init__(self, task_error, deadline, eta=None):
        self.task_error = task_error
        self.deadline = deadline
        self.eta = eta
        super(RetryDeadlineExceeded, self).__init__(
            'retry deadline %s exceeded (planned eta %s) for %r' % (
                deadline, eta, task_error))

    def __repr__(self):
        return 'RetryDeadlineExceeded(deadline=%r, eta=%r, task_error=%r)' % (
            self.deadline, self.eta, self.task_error)


# Not failures while a retry is pending.
RETRYING_EXCEPTIONS = (RetryTask, RateLimitExceeded, TaskLockedException)
