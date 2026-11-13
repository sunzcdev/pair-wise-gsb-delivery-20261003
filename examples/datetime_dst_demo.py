"""
Demonstrate how agate.DateTime handles DST transitions with fixed inputs.

America/New_York observes daylight saving time; Asia/Shanghai does not.
Run with: python examples/datetime_dst_demo.py
"""
from zoneinfo import ZoneInfo

from agate.data_types import DateTime
from agate.exceptions import CastError

NEW_YORK = ZoneInfo('America/New_York')  # observes DST
SHANGHAI = ZoneInfo('Asia/Shanghai')     # no DST

AMBIGUOUS_WALL_TIME = '2021-11-07 01:30'    # occurs twice in New York (fall back)
NONEXISTENT_WALL_TIME = '2021-03-14 02:30'  # never occurs in New York (spring forward)
NORMAL_WALL_TIME = '2021-06-01 12:30'
AWARE_ISO = '2021-06-01T12:30:00+06:00'


def show(label, datetime_type, value):
    try:
        casted = datetime_type.cast(value)
        print('%-44s -> %-32s fold=%d offset=%s' % (
            label, casted.isoformat(), casted.fold, casted.utcoffset()
        ))
    except CastError as e:
        print('%-44s -> CastError: %s' % (label, e))


print('== America/New_York (DST) ==')
show('normal, ambiguous=earlier', DateTime(timezone=NEW_YORK), NORMAL_WALL_TIME)
show('ambiguous, ambiguous=earlier', DateTime(timezone=NEW_YORK, ambiguous='earlier'), AMBIGUOUS_WALL_TIME)
show('ambiguous, ambiguous=later', DateTime(timezone=NEW_YORK, ambiguous='later'), AMBIGUOUS_WALL_TIME)
show('ambiguous, ambiguous=reject', DateTime(timezone=NEW_YORK, ambiguous='reject'), AMBIGUOUS_WALL_TIME)
show('nonexistent, nonexistent=reject', DateTime(timezone=NEW_YORK, nonexistent='reject'), NONEXISTENT_WALL_TIME)
show('nonexistent, nonexistent=shift_forward', DateTime(timezone=NEW_YORK, nonexistent='shift_forward'), NONEXISTENT_WALL_TIME)
show('nonexistent, nonexistent=shift_backward', DateTime(timezone=NEW_YORK, nonexistent='shift_backward'), NONEXISTENT_WALL_TIME)
show('aware ISO keeps its own offset', DateTime(timezone=NEW_YORK), AWARE_ISO)

print()
print('== Asia/Shanghai (no DST) ==')
show('normal', DateTime(timezone=SHANGHAI), NORMAL_WALL_TIME)
show('same wall time as NY ambiguous case', DateTime(timezone=SHANGHAI), AMBIGUOUS_WALL_TIME)
show('same wall time as NY nonexistent case', DateTime(timezone=SHANGHAI), NONEXISTENT_WALL_TIME)
show('aware ISO keeps its own offset', DateTime(timezone=SHANGHAI), AWARE_ISO)
