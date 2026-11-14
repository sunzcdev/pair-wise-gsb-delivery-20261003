"""Explicit compensation-chain (saga) helper for Huey.

A :class:`Saga` runs a serial chain of stages. Each stage may register a
compensation callback used to undo external side-effects (temp files,
remote resources, reservations, ...) when a later stage fails.

Semantics:

* Only stages that *truly completed* enter the compensation list.
* Compensations run in the reverse of completion order.
* A stage that was never executed, or that itself failed, is not
  compensated.
* While a stage still has ordinary retries left, no compensation is
  triggered -- unwinding only begins once retries are exhausted.
* If a compensation fails, both the original failure and the
  compensation failure are preserved on the result object, and the
  unfinished compensations can be retried explicitly via
  :meth:`SagaResult.retry_compensations` without re-executing any
  forward task that already succeeded.
* Side-effects are invoked with at-least-once semantics: a compensation
  may be called more than once (e.g. after an explicit retry), so
  compensation callbacks must be re-entrant / idempotent.
"""

import time

from huey.exceptions import HueyException


class CompensationError(HueyException):
    """Raised when one or more compensations fail.

    Carries both the original failure that triggered the unwind and the
    list of ``(stage_name, exception)`` compensation failures.
    """
    def __init__(self, original_error, errors):
        self.original_error = original_error
        self.errors = list(errors)
        super(CompensationError, self).__init__(
            'compensation(s) failed after %r: %s' % (
                original_error,
                ', '.join('%s: %r' % item for item in self.errors)))


class Stage(object):
    """A single forward step plus its optional compensation."""
    def __init__(self, action, compensate=None, name=None, retries=0,
                 retry_delay=0, retry_backoff=1.0):
        if not callable(action):
            raise TypeError('action must be callable')
        if compensate is not None and not callable(compensate):
            raise TypeError('compensate must be callable or None')
        self.action = action
        self.compensate = compensate
        self.name = name or getattr(action, '__name__', None) \
            or 'stage-%d' % id(action)
        self.retries = retries
        self.retry_delay = retry_delay
        self.retry_backoff = retry_backoff

    def __repr__(self):
        return '<Stage: %s>' % self.name


class CompensationRecord(object):
    """Tracks the outcome of one compensation invocation."""
    PENDING = 'pending'
    DONE = 'done'
    FAILED = 'failed'

    def __init__(self, stage, value=None):
        self.stage = stage
        self.value = value  # Forward result handed to the compensation.
        self.status = CompensationRecord.PENDING
        self.error = None
        self.attempts = 0

    @property
    def name(self):
        return self.stage.name

    def __repr__(self):
        return '<CompensationRecord: %s (%s)>' % (self.name, self.status)


class SagaResult(object):
    """Outcome of running a :class:`Saga`.

    Attributes:

    * ``completed`` -- names of stages that completed, in order.
    * ``results`` -- mapping of stage name to its return value.
    * ``failed_stage`` / ``error`` -- the stage and exception that ended
      the chain (``None`` on success).
    * ``compensations`` -- :class:`CompensationRecord` list in the order
      the compensations were (first) attempted, i.e. reverse completion
      order.
    """
    def __init__(self, saga):
        self.saga = saga
        self.completed = []
        self.results = {}
        self.failed_stage = None
        self.error = None
        self.compensations = []

    @property
    def ok(self):
        return self.error is None

    @property
    def pending_compensations(self):
        return [record for record in self.compensations
                if record.status != CompensationRecord.DONE]

    def retry_compensations(self, raise_on_error=False):
        """Re-run only the unfinished compensations.

        Forward tasks are never re-executed. Compensations that already
        succeeded are skipped, so a compensation may observe more than
        one call overall (at-least-once) and must be re-entrant.

        Returns ``True`` if no unfinished compensations remain.
        """
        errors = self._run_compensations(
            [record for record in self.compensations
             if record.status != CompensationRecord.DONE])
        if errors and raise_on_error:
            raise CompensationError(self.error, errors)
        return not self.pending_compensations

    def _run_compensations(self, records):
        errors = []
        for record in records:
            try:
                record.stage.compensate(record.value)
            except Exception as exc:
                record.status = CompensationRecord.FAILED
                record.error = exc
                errors.append((record.name, exc))
            else:
                record.status = CompensationRecord.DONE
                record.error = None
            record.attempts += 1
        return errors


class Saga(object):
    """A serial chain of stages with registered compensations."""
    def __init__(self, name=None):
        self.name = name or 'saga'
        self.stages = []

    def add(self, action, compensate=None, name=None, retries=0,
            retry_delay=0, retry_backoff=1.0):
        """Append a stage. ``compensate`` is called with the stage's
        return value if a later stage fails after all retries."""
        stage = Stage(action, compensate=compensate, name=name,
                      retries=retries, retry_delay=retry_delay,
                      retry_backoff=retry_backoff)
        self.stages.append(stage)
        return stage

    def stage(self, name=None, compensate=None, retries=0, retry_delay=0,
              retry_backoff=1.0):
        """Decorator form of :meth:`add`."""
        def decorator(fn):
            self.add(fn, compensate=compensate, name=name, retries=retries,
                     retry_delay=retry_delay, retry_backoff=retry_backoff)
            return fn
        return decorator

    def run(self, raise_on_compensation_error=False):
        """Execute the chain, unwinding completed stages on failure.

        Returns a :class:`SagaResult`. When
        ``raise_on_compensation_error`` is true and any compensation
        fails, a :class:`CompensationError` is raised that preserves both
        the original failure and the compensation failure(s).
        """
        result = SagaResult(self)
        completed = []  # (stage, value) pairs eligible for compensation.
        for stage in self.stages:
            attempt = 0
            while True:
                try:
                    value = stage.action()
                except Exception as exc:
                    if attempt < stage.retries:
                        # Ordinary retry: no compensation is triggered
                        # while retries remain.
                        attempt += 1
                        if stage.retry_delay:
                            time.sleep(stage.retry_delay *
                                       (stage.retry_backoff ** (attempt - 1)))
                        continue
                    # Retries exhausted: unwind only completed stages.
                    result.failed_stage = stage.name
                    result.error = exc
                    errors = self._unwind(result, completed)
                    if errors and raise_on_compensation_error:
                        raise CompensationError(exc, errors)
                    return result
                else:
                    result.results[stage.name] = value
                    result.completed.append(stage.name)
                    if stage.compensate is not None:
                        completed.append((stage, value))
                    break
        return result

    def _unwind(self, result, completed):
        # Compensations run in the reverse of completion order; a failed
        # compensation is recorded but does not stop the remaining ones.
        records = []
        for stage, value in reversed(completed):
            record = CompensationRecord(stage, value)
            result.compensations.append(record)
            records.append(record)
        return result._run_compensations(records)


# Descriptive alias.
CompensationChain = Saga
