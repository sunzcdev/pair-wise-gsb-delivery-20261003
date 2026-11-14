import unittest

from huey.saga import CompensationChain
from huey.saga import CompensationError
from huey.saga import CompensationRecord
from huey.saga import Saga


class TestSaga(unittest.TestCase):
    def make_stage(self, log, name, fail=False, compensate=None,
                   compensate_fail=False, **kwargs):
        def action():
            log.append('run:%s' % name)
            if fail:
                raise ValueError('boom:%s' % name)
            return name.upper()

        def default_compensation(value):
            log.append('undo:%s(%s)' % (name, value))
            if compensate_fail:
                raise RuntimeError('undo-boom:%s' % name)

        return dict(action=action, name=name,
                    compensate=compensate or default_compensation,
                    compensate_fail=compensate_fail, kwargs=kwargs)

    def build(self, log, specs):
        saga = Saga('test')
        for spec in specs:
            kw = dict(spec.pop('kwargs'))
            saga.add(spec['action'], compensate=spec['compensate'],
                     name=spec['name'], **kw)
        return saga

    def test_success_no_compensation(self):
        log = []
        specs = [self.make_stage(log, 'a'), self.make_stage(log, 'b')]
        saga = self.build(log, specs)
        result = saga.run()
        self.assertTrue(result.ok)
        self.assertEqual(result.completed, ['a', 'b'])
        self.assertEqual(result.results, {'a': 'A', 'b': 'B'})
        self.assertEqual(log, ['run:a', 'run:b'])
        self.assertEqual(result.compensations, [])

    def test_failure_unwinds_completed_in_reverse(self):
        log = []
        specs = [self.make_stage(log, 'a'), self.make_stage(log, 'b'),
                 self.make_stage(log, 'c', fail=True),
                 self.make_stage(log, 'd')]
        saga = self.build(log, specs)
        result = saga.run()
        self.assertFalse(result.ok)
        self.assertEqual(result.failed_stage, 'c')
        self.assertIsInstance(result.error, ValueError)
        # Failed stage 'c' and never-run stage 'd' are not compensated;
        # completed stages unwind in reverse completion order.
        self.assertEqual(log, ['run:a', 'run:b', 'run:c',
                               'undo:b(B)', 'undo:a(A)'])
        self.assertEqual([r.name for r in result.compensations], ['b', 'a'])
        self.assertTrue(all(r.status == CompensationRecord.DONE
                            for r in result.compensations))
        self.assertEqual(result.pending_compensations, [])

    def test_stage_without_compensation_is_skipped(self):
        log = []
        saga = Saga()
        saga.add(lambda: log.append('run:a') or 'A', name='a',
                 compensate=lambda v: log.append('undo:a'))
        saga.add(lambda: log.append('run:b') or 'B', name='b')  # no undo
        def fail():
            log.append('run:c')
            raise ValueError('x')
        saga.add(fail, name='c', compensate=lambda v: log.append('undo:c'))
        result = saga.run()
        self.assertEqual(log, ['run:a', 'run:b', 'run:c', 'undo:a'])
        self.assertEqual(result.completed, ['a', 'b'])

    def test_retries_do_not_trigger_compensation(self):
        log = []
        attempts = {'n': 0}
        def flaky():
            attempts['n'] += 1
            log.append('try:b#%d' % attempts['n'])
            if attempts['n'] < 3:
                raise ValueError('not yet')
            return 'B'
        saga = Saga()
        saga.add(lambda: log.append('run:a') or 'A', name='a',
                 compensate=lambda v: log.append('undo:a'))
        saga.add(flaky, name='b', retries=2,
                 compensate=lambda v: log.append('undo:b'))
        result = saga.run()
        self.assertTrue(result.ok)
        self.assertEqual(log, ['run:a', 'try:b#1', 'try:b#2', 'try:b#3'])

    def test_retries_exhausted_then_unwind(self):
        log = []
        def always_fail():
            log.append('try:b')
            raise ValueError('nope')
        saga = Saga()
        saga.add(lambda: log.append('run:a') or 'A', name='a',
                 compensate=lambda v: log.append('undo:a'))
        saga.add(always_fail, name='b', retries=2,
                 compensate=lambda v: log.append('undo:b'))
        result = saga.run()
        self.assertFalse(result.ok)
        # b retried 3x total, its own compensation never runs.
        self.assertEqual(log, ['run:a', 'try:b', 'try:b', 'try:b', 'undo:a'])

    def test_compensation_failure_preserves_both_errors(self):
        log = []
        specs = [self.make_stage(log, 'a'),
                 self.make_stage(log, 'b', compensate_fail=True),
                 self.make_stage(log, 'c', fail=True)]
        saga = self.build(log, specs)
        result = saga.run(raise_on_compensation_error=False)
        self.assertFalse(result.ok)
        self.assertIsInstance(result.error, ValueError)  # original failure
        rec_a, rec_b = result.compensations[1], result.compensations[0]
        self.assertEqual(rec_b.status, CompensationRecord.FAILED)
        self.assertIsInstance(rec_b.error, RuntimeError)  # comp. failure
        self.assertEqual(rec_a.status, CompensationRecord.DONE)
        self.assertEqual(result.pending_compensations, [rec_b])

    def test_raise_on_compensation_error(self):
        log = []
        specs = [self.make_stage(log, 'a', compensate_fail=True),
                 self.make_stage(log, 'b', fail=True)]
        saga = self.build(log, specs)
        exc = None
        try:
            saga.run(raise_on_compensation_error=True)
        except CompensationError as e:
            exc = e
        self.assertTrue(exc is not None)
        self.assertIsInstance(exc.original_error, ValueError)
        self.assertEqual(len(exc.errors), 1)
        self.assertEqual(exc.errors[0][0], 'a')
        self.assertIsInstance(exc.errors[0][1], RuntimeError)

    def test_retry_compensations_only_unfinished(self):
        log = []
        state = {'fail_b': True}
        def undo_b(value):
            log.append('undo:b(%s)' % value)
            if state['fail_b']:
                raise RuntimeError('transient')
        saga = Saga()
        saga.add(lambda: log.append('run:a') or 'A', name='a',
                 compensate=lambda v: log.append('undo:a(%s)' % v))
        saga.add(lambda: log.append('run:b') or 'B', name='b',
                 compensate=undo_b)
        def fail():
            log.append('run:c')
            raise ValueError('x')
        saga.add(fail, name='c')
        result = saga.run()
        self.assertEqual(log, ['run:a', 'run:b', 'run:c',
                               'undo:b(B)', 'undo:a(A)'])
        self.assertEqual(len(result.pending_compensations), 1)

        # Fix the external condition and explicitly retry compensations.
        state['fail_b'] = False
        self.assertTrue(result.retry_compensations())
        # Only the failed compensation re-ran; forward tasks untouched.
        self.assertEqual(log, ['run:a', 'run:b', 'run:c',
                               'undo:b(B)', 'undo:a(A)', 'undo:b(B)'])
        rec_b = result.compensations[0]
        self.assertEqual(rec_b.status, CompensationRecord.DONE)
        self.assertEqual(rec_b.attempts, 2)  # at-least-once semantics.
        self.assertEqual(result.pending_compensations, [])

    def test_retry_compensations_raise_on_error(self):
        saga = Saga()
        saga.add(lambda: 'A', name='a',
                 compensate=lambda v: (_ for _ in ()).throw(RuntimeError('x')))
        saga.add(lambda: (_ for _ in ()).throw(ValueError('y')), name='b')
        result = saga.run()
        self.assertRaises(CompensationError, result.retry_compensations,
                          raise_on_error=True)

    def test_compensation_receives_forward_value(self):
        seen = []
        saga = Saga()
        saga.add(lambda: {'temp': '/tmp/x'}, name='make',
                 compensate=lambda v: seen.append(v))
        saga.add(lambda: (_ for _ in ()).throw(ValueError()), name='boom')
        saga.run()
        self.assertEqual(seen, [{'temp': '/tmp/x'}])

    def test_decorator_registration(self):
        log = []
        saga = Saga()

        @saga.stage(compensate=lambda v: log.append('undo:step'))
        def step():
            log.append('run:step')

        @saga.stage()
        def boom():
            raise ValueError()

        result = saga.run()
        self.assertEqual(log, ['run:step', 'undo:step'])
        self.assertEqual(result.failed_stage, 'boom')

    def test_alias(self):
        self.assertIs(CompensationChain, Saga)


if __name__ == '__main__':
    unittest.main()
