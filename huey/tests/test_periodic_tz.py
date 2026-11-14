import datetime
import unittest

from zoneinfo import ZoneInfo

from huey import crontab
from huey.api import MemoryHuey
from huey.tests.base import BaseTestCase
from huey.utils import local_to_utc


NEW_YORK = 'America/New_York'
TOKYO = 'Asia/Tokyo'

# Reference DST transitions for America/New_York:
# * 2026-03-08: at 07:00 UTC the clock jumps from 01:59:59 EST to 03:00:00
#   EDT, so local times 02:00-02:59 never occur.
# * 2026-11-01: at 06:00 UTC the clock falls back from 01:59:59 EDT to
#   01:00:00 EST, so local times 01:00-01:59 occur twice (05:xx and 06:xx UTC).


def ts(*args):
    return datetime.datetime(*args)


def clock(start, end, step=60):
    """Fixed clock sequence from start to end (inclusive), no sleeping."""
    seq = []
    current = start
    delta = datetime.timedelta(seconds=step)
    while current <= end:
        seq.append(current)
        current += delta
    return seq


class TestPeriodicTaskTimezone(BaseTestCase):
    def get_huey(self):
        return MemoryHuey(utc=True)

    def run_clock(self, timestamps):
        """Run the scheduler's periodic check over a fixed clock sequence."""
        fired = []
        for timestamp in timestamps:
            for task in self.huey.read_periodic(timestamp):
                self.huey.enqueue(task)
                fired.append((timestamp, task.name))
        return fired

    def test_triggers_on_zone_local_time(self):
        @self.huey.periodic_task(crontab(minute='0', hour='9'),
                                 timezone=NEW_YORK)
        def task_ny():
            pass

        @self.huey.periodic_task(crontab(minute='0', hour='9'))
        def task_utc():
            pass

        fired = self.run_clock(clock(ts(2026, 1, 5, 8, 55),
                                     ts(2026, 1, 5, 14, 5)))
        # The legacy (no-timezone) task fires at 09:00 UTC, while the New
        # York task fires at 09:00 local time, i.e. 14:00 UTC (EST is UTC-5).
        self.assertEqual(fired, [
            (ts(2026, 1, 5, 9, 0), 'task_utc'),
            (ts(2026, 1, 5, 14, 0), 'task_ny'),
        ])

    def test_day_rollover(self):
        @self.huey.periodic_task(crontab.daily(), timezone=NEW_YORK)
        def task_midnight():
            pass

        # Step a fixed clock, minute-by-minute, across three New York
        # midnights (05:00 UTC during EST).
        fired = self.run_clock(clock(ts(2026, 1, 1, 4, 50),
                                     ts(2026, 1, 3, 5, 10)))
        self.assertEqual(fired, [
            (ts(2026, 1, 1, 5, 0), 'task_midnight'),
            (ts(2026, 1, 2, 5, 0), 'task_midnight'),
            (ts(2026, 1, 3, 5, 0), 'task_midnight'),
        ])
        self.assertEqual(len(self.huey), 3)

    def test_spring_forward_skip(self):
        @self.huey.periodic_task(crontab(minute='30', hour='2'),
                                 timezone=NEW_YORK,
                                 dst_nonexistent='skip')
        def task_skip():
            pass

        # 02:30 does not exist on 2026-03-08 in New York: never fires.
        fired = self.run_clock(clock(ts(2026, 3, 8, 6, 30),
                                     ts(2026, 3, 8, 8, 0)))
        self.assertEqual(fired, [])

    def test_spring_forward_shift(self):
        @self.huey.periodic_task(crontab(minute='30', hour='2'),
                                 timezone=NEW_YORK,
                                 dst_nonexistent='shift')
        def task_shift():
            pass

        # 02:30 does not exist; the occurrence is shifted to the first valid
        # minute after the gap, 03:00 EDT == 07:00 UTC.
        fired = self.run_clock(clock(ts(2026, 3, 8, 6, 30),
                                     ts(2026, 3, 8, 8, 0)))
        self.assertEqual(fired, [(ts(2026, 3, 8, 7, 0), 'task_shift')])

    def test_fall_back_once(self):
        @self.huey.periodic_task(crontab(minute='30', hour='1'),
                                 timezone=NEW_YORK,
                                 dst_ambiguous='once')
        def task_once():
            pass

        # 01:30 occurs twice (05:30 and 06:30 UTC); run only on the first.
        fired = self.run_clock(clock(ts(2026, 11, 1, 5, 0),
                                     ts(2026, 11, 1, 7, 30)))
        self.assertEqual(fired, [(ts(2026, 11, 1, 5, 30), 'task_once')])

    def test_fall_back_twice(self):
        @self.huey.periodic_task(crontab(minute='30', hour='1'),
                                 timezone=NEW_YORK,
                                 dst_ambiguous='twice')
        def task_twice():
            pass

        # Both occurrences of 01:30 fire: 05:30 UTC (EDT) and 06:30 UTC (EST).
        fired = self.run_clock(clock(ts(2026, 11, 1, 5, 0),
                                     ts(2026, 11, 1, 7, 30)))
        self.assertEqual(fired, [
            (ts(2026, 11, 1, 5, 30), 'task_twice'),
            (ts(2026, 11, 1, 6, 30), 'task_twice'),
        ])

    def test_scheduler_restart_does_not_reclaim_occurrence(self):
        @self.huey.periodic_task(crontab(minute='0', hour='9'),
                                 timezone=NEW_YORK)
        def task_a():
            pass

        now = ts(2026, 1, 5, 14, 0)  # 09:00 in New York.
        scheduler = self.consumer()._create_scheduler()
        scheduler.enqueue_periodic_tasks(now)
        self.assertEqual(len(self.huey), 1)

        # Simulate a scheduler restart within the same minute: a brand-new
        # scheduler must not treat the already-claimed occurrence as new.
        scheduler = self.consumer()._create_scheduler()
        scheduler.enqueue_periodic_tasks(now)
        self.assertEqual(len(self.huey), 1)

    def test_clock_rollback_does_not_reclaim_occurrence(self):
        @self.huey.periodic_task(crontab(minute='0', hour='9'),
                                 timezone=NEW_YORK)
        def task_a():
            pass

        fired = self.run_clock(clock(ts(2026, 1, 5, 13, 55),
                                     ts(2026, 1, 5, 14, 5)))
        self.assertEqual(fired, [(ts(2026, 1, 5, 14, 0), 'task_a')])

        # The system clock rolls back ten minutes and replays: the claimed
        # occurrence must not be enqueued a second time.
        fired = self.run_clock(clock(ts(2026, 1, 5, 13, 50),
                                     ts(2026, 1, 5, 14, 10)))
        self.assertEqual(fired, [])
        self.assertEqual(len(self.huey), 1)

    def test_different_timezones_do_not_interfere(self):
        @self.huey.periodic_task(crontab(minute='0', hour='9'),
                                 timezone=NEW_YORK)
        def task_ny():
            pass

        @self.huey.periodic_task(crontab(minute='0', hour='9'),
                                 timezone=TOKYO)
        def task_tokyo():
            pass

        sequence = clock(ts(2026, 1, 5, 23, 50), ts(2026, 1, 6, 14, 10))
        fired = self.run_clock(sequence)
        # 09:00 JST == 00:00 UTC; 09:00 EST == 14:00 UTC.
        self.assertEqual(fired, [
            (ts(2026, 1, 6, 0, 0), 'task_tokyo'),
            (ts(2026, 1, 6, 14, 0), 'task_ny'),
        ])

        # Replaying the same window (e.g. after a restart) claims nothing.
        self.assertEqual(self.run_clock(sequence), [])
        self.assertEqual(len(self.huey), 2)

    def test_legacy_task_behavior_unchanged(self):
        @self.huey.periodic_task(crontab(minute='0', hour='9'))
        def task_p():
            pass

        now = ts(2026, 1, 5, 9, 0)
        # Tasks without a timezone are not de-duplicated: a scheduler restart
        # within the same minute sees the task as due again (legacy behavior).
        self.assertEqual(len(self.huey.read_periodic(now)), 1)
        self.assertEqual(len(self.huey.read_periodic(now)), 1)
        self.assertEqual(self.huey.read_periodic(ts(2026, 1, 5, 9, 1)), [])

    def test_timezone_accepts_tzinfo_instance(self):
        @self.huey.periodic_task(crontab(minute='0', hour='9'),
                                 timezone=ZoneInfo(NEW_YORK))
        def task_ny():
            pass

        @self.huey.periodic_task(crontab(minute='0', hour='9'),
                                 timezone=datetime.timezone.utc)
        def task_fixed():
            pass

        fired = self.run_clock(clock(ts(2026, 1, 5, 8, 55),
                                     ts(2026, 1, 5, 14, 5)))
        self.assertEqual(fired, [
            (ts(2026, 1, 5, 9, 0), 'task_fixed'),
            (ts(2026, 1, 5, 14, 0), 'task_ny'),
        ])

    def test_utc_false_instance(self):
        huey = MemoryHuey(utc=False)

        @huey.periodic_task(crontab(minute='7'), timezone='UTC')
        def task_p():
            pass

        # With utc=False the timestamp is system-local time; it is converted
        # to UTC before being evaluated in the task's timezone.
        base = ts(2026, 3, 1, 12, 0)
        fired = []
        for i in range(120):
            timestamp = base + datetime.timedelta(minutes=i)
            for task in huey.read_periodic(timestamp):
                fired.append(timestamp)
        expected = [base + datetime.timedelta(minutes=i) for i in range(120)
                    if local_to_utc(base + datetime.timedelta(minutes=i))
                       .minute == 7]
        self.assertEqual(fired, expected)
        self.assertTrue(len(fired) > 0)

    def test_invalid_options(self):
        with self.assertRaises(ValueError):
            self.huey.periodic_task(crontab(), dst_ambiguous='thrice')
        with self.assertRaises(ValueError):
            self.huey.periodic_task(crontab(), dst_nonexistent='postpone')
        with self.assertRaises(ValueError):
            self.huey.periodic_task(crontab(), timezone=12345)
        with self.assertRaises(KeyError):  # ZoneInfoNotFoundError.
            self.huey.periodic_task(crontab(), timezone='Bogus/Zone')


if __name__ == '__main__':
    unittest.main()
