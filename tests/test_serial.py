# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

import datetime
import random
import unittest

import pytest

import dns.btreezone
import dns.name
import dns.serial
import dns.transaction
import dns.versioned
import dns.zone


def S2(v):
    return dns.serial.Serial(v, bits=2)


def S8(v):
    return dns.serial.Serial(v, bits=8)


class SerialTestCase(unittest.TestCase):
    def test_rfc_1982_2_bit_cases(self):
        self.assertEqual(S2(0) + S2(1), S2(1))
        self.assertEqual(S2(1) + S2(1), S2(2))
        self.assertEqual(S2(2) + S2(1), S2(3))
        self.assertEqual(S2(3) + S2(1), S2(0))
        self.assertTrue(S2(1) > S2(0))
        self.assertTrue(S2(2) > S2(1))
        self.assertTrue(S2(3) > S2(2))
        self.assertTrue(S2(0) > S2(3))
        self.assertFalse(S2(2) > S2(0))
        self.assertFalse(S2(0) > S2(2))
        self.assertFalse(S2(2) < S2(0))
        self.assertFalse(S2(0) < S2(2))

    def test_rfc_1982_8_bit_cases(self):
        self.assertEqual(S8(255) + S8(1), S8(0))
        self.assertEqual(S8(100) + S8(100), S8(200))
        self.assertEqual(S8(200) + S8(100), S8(44))
        self.assertTrue(S8(1) > S8(0))
        self.assertTrue(S8(44) > S8(0))
        self.assertTrue(S8(100) > S8(0))
        self.assertTrue(S8(100) > S8(44))
        self.assertTrue(S8(200) > S8(100))
        self.assertTrue(S8(255) > S8(200))
        self.assertTrue(S8(0) > S8(255))
        self.assertTrue(S8(255) < S8(0))
        self.assertTrue(S8(100) > S8(255))
        self.assertTrue(S8(0) > S8(200))
        self.assertTrue(S8(44) > S8(200))
        self.assertFalse(S8(0) > S8(128))
        self.assertFalse(S8(128) > S8(0))
        self.assertFalse(S8(0) < S8(128))
        self.assertFalse(S8(128) < S8(0))
        self.assertFalse(S8(1) > S8(129))
        self.assertFalse(S8(129) > S8(1))

    def test_incremental_ops(self):
        v = S8(255)
        v += 1
        self.assertEqual(v, 0)
        v = S8(255)
        v += S8(1)
        self.assertEqual(v, 0)
        v = S8(0)
        v -= 1
        self.assertEqual(v, 255)
        v = S8(0)
        v -= S8(1)
        self.assertEqual(v, 255)

    def test_sub(self):
        self.assertEqual(S8(0) - S8(1), S8(255))

    def test_addition_bounds(self):
        self.assertRaises(ValueError, lambda: S8(0) + 128)
        self.assertRaises(ValueError, lambda: S8(0) - 128)

        def bad1():
            v = S8(0)
            v += 128

        self.assertRaises(ValueError, bad1)

        def bad2():
            v = S8(0)
            v -= 128

        self.assertRaises(ValueError, bad2)

    def test_casting(self):
        self.assertTrue(S8(0) == 0)
        self.assertTrue(S8(0) != 1)
        self.assertTrue(S8(0) < 1)
        self.assertTrue(S8(0) <= 1)
        self.assertTrue(S8(0) > 255)
        self.assertTrue(S8(0) >= 255)

    def test_uncastable(self):
        self.assertRaises(ValueError, lambda: S8(0) + "a")
        self.assertRaises(ValueError, lambda: S8(0) - "a")

        def bad1():
            v = S8(0)
            v += "a"

        self.assertRaises(ValueError, bad1)

        def bad2():
            v = S8(0)
            v -= "a"

        self.assertRaises(ValueError, bad2)

    def test_uncomparable(self):
        self.assertFalse(S8(0) == S2(0))
        self.assertFalse(S8(0) == "a")
        self.assertTrue(S8(0) != "a")
        self.assertRaises(TypeError, lambda: S8(0) < "a")
        self.assertRaises(TypeError, lambda: S8(0) <= "a")
        self.assertRaises(TypeError, lambda: S8(0) > "a")
        self.assertRaises(TypeError, lambda: S8(0) >= "a")

    def test_modulo(self):
        self.assertEqual(S8(-1), 255)
        self.assertEqual(S8(257), 1)

    def test_repr(self):
        self.assertEqual(repr(S8(1)), "dns.serial.Serial(1, 8)")

    def test_not_equal(self):
        self.assertNotEqual(S8(0), S8(1))
        self.assertNotEqual(S8(0), S2(0))

    def test_hashable(self):
        # Can be stored in a set.
        seen = {S8(0), S8(1), S8(2)}
        self.assertIn(S8(0), seen)
        self.assertIn(S8(1), seen)
        self.assertIn(S8(2), seen)

        # Can be used as a dict key.
        d = {S8(100): "zone-a"}
        self.assertEqual(d[S8(100)], "zone-a")


class CompareTestCase(unittest.TestCase):
    def test_compare_ordinary(self):
        self.assertEqual(dns.serial.compare(1, 2), -1)
        self.assertEqual(dns.serial.compare(2, 2), 0)
        self.assertEqual(dns.serial.compare(3, 2), 1)

    def test_compare_wraparound(self):
        # 0 follows 2**32 - 1.
        self.assertEqual(dns.serial.compare(0, dns.serial.MAX_SERIAL), 1)
        self.assertEqual(dns.serial.compare(dns.serial.MAX_SERIAL, 0), -1)
        # ... and is not smaller in the ordinary integer sense.
        self.assertFalse(0 > dns.serial.MAX_SERIAL)

    def test_compare_undefined(self):
        half = dns.serial.HALF_SERIAL
        self.assertIsNone(dns.serial.compare(0, half))
        self.assertIsNone(dns.serial.compare(half, 0))
        self.assertIsNone(dns.serial.compare(half - 1, dns.serial.MAX_SERIAL))
        # Just short of half is defined.
        self.assertEqual(dns.serial.compare(half - 1, 0), 1)
        self.assertEqual(dns.serial.compare(half + 1, 0), -1)

    def test_compare_wraps_bits(self):
        self.assertEqual(dns.serial.compare(256, 0, bits=8), 0)
        self.assertIsNone(dns.serial.compare(128, 0, bits=8))

    def test_is_update(self):
        self.assertTrue(dns.serial.is_update(1, 0))
        self.assertFalse(dns.serial.is_update(0, 0))
        self.assertFalse(dns.serial.is_update(0, 1))
        self.assertTrue(dns.serial.is_update(0, dns.serial.MAX_SERIAL))
        with pytest.raises(dns.serial.SerialComparisonUndefined):
            dns.serial.is_update(dns.serial.HALF_SERIAL, 0)
        with pytest.raises(dns.serial.SerialComparisonUndefined):
            dns.serial.is_update(0, dns.serial.HALF_SERIAL)


class IncrementPolicyTestCase(unittest.TestCase):
    def test_default(self):
        plan = dns.serial.plan_serial(1)
        self.assertEqual(plan.serial, 2)
        self.assertEqual(plan.current, 1)
        self.assertEqual(plan.policy, dns.serial.IncrementPolicy())

    def test_step(self):
        plan = dns.serial.plan_serial(10, dns.serial.IncrementPolicy(5))
        self.assertEqual(plan.serial, 15)

    def test_wraparound(self):
        plan = dns.serial.plan_serial(dns.serial.MAX_SERIAL)
        self.assertEqual(plan.serial, 0)
        # The wrapped value must be accepted because RFC 1982 says 0 follows
        # 2**32 - 1, even though 0 is not greater than 2**32 - 1 as an integer.
        self.assertTrue(dns.serial.is_update(plan.serial, plan.current))

    def test_wraparound_with_step(self):
        plan = dns.serial.plan_serial(dns.serial.MAX_SERIAL - 1, "fixed:5")
        self.assertEqual(plan.serial, 3)

    def test_bad_step(self):
        for step in (0, -1, dns.serial.HALF_SERIAL, dns.serial.MAX_SERIAL + 1):
            with self.assertRaises(dns.serial.BadSerialPolicy):
                dns.serial.IncrementPolicy(step)

    def test_bad_step_type(self):
        with self.assertRaises(dns.serial.BadSerialPolicy):
            dns.serial.IncrementPolicy("1")  # type: ignore[arg-type]
        with self.assertRaises(dns.serial.BadSerialPolicy):
            dns.serial.IncrementPolicy(True)


class DatePolicyTestCase(unittest.TestCase):
    @staticmethod
    def at(*args):
        return datetime.datetime(*args, tzinfo=datetime.timezone.utc)

    def test_first_update_of_day(self):
        plan = dns.serial.plan_serial(
            2026111899, "date", now=self.at(2026, 11, 19, 12, 0)
        )
        self.assertEqual(plan.serial, 2026111900)

    def test_same_day_increment(self):
        plan = dns.serial.plan_serial(
            2026111900, "date", now=self.at(2026, 11, 19, 23, 59)
        )
        self.assertEqual(plan.serial, 2026111901)
        plan = dns.serial.plan_serial(
            2026111942, "date", now=self.at(2026, 11, 19, 23, 59)
        )
        self.assertEqual(plan.serial, 2026111943)

    def test_new_day_resets_counter(self):
        plan = dns.serial.plan_serial(
            2026111942, "date", now=self.at(2026, 11, 20, 0, 0)
        )
        self.assertEqual(plan.serial, 2026112000)

    def test_same_day_counter_exhausted(self):
        # 2026111999 is the last serial available on 2026-11-19; another update
        # that day must fail rather than produce an 11-digit number which
        # looks like a date-style serial but is not one.
        with self.assertRaises(dns.serial.SerialExhausted):
            dns.serial.plan_serial(
                2026111999, "date", now=self.at(2026, 11, 19, 23, 59)
            )

    def test_naive_datetime_is_utc(self):
        plan = dns.serial.plan_serial(
            2026111800, "date", now=datetime.datetime(2026, 11, 19, 1, 0)
        )
        self.assertEqual(plan.serial, 2026111900)

    def test_timezone_converted_to_utc(self):
        tz = datetime.timezone(datetime.timedelta(hours=9))
        # 2026-11-19 08:00 +09:00 is 2026-11-18 23:00 UTC.
        plan = dns.serial.plan_serial(
            2026111700, "date", now=datetime.datetime(2026, 11, 19, 8, 0, tzinfo=tz)
        )
        self.assertEqual(plan.serial, 2026111800)

    def test_current_date_in_the_future(self):
        # The existing serial carries a date ahead of "now".  A serial based
        # on today would go backwards in RFC 1982 order, so planning must
        # fail rather than hand back a value secondaries would ignore.
        with self.assertRaises(dns.serial.SerialNotUpdated):
            dns.serial.plan_serial(
                2027111900, "date", now=self.at(2026, 11, 19, 12, 0)
            )

    def test_far_ahead_unsigned_serial(self):
        # A serial near 2**32 is "close" to the date-style 2026111900 in
        # circular terms only when it is within half the space.  Here it is
        # not, so a date-style serial would look newer and is accepted.
        plan = dns.serial.plan_serial(
            dns.serial.MAX_SERIAL, "date", now=self.at(2026, 11, 19, 12, 0)
        )
        self.assertEqual(plan.serial, 2026111900)

    def test_not_a_date_serial(self):
        # A non-date current serial (e.g. a small integer) is simply treated
        # as "not today"; the new date-style value is used if it is an update.
        plan = dns.serial.plan_serial(1, "date", now=self.at(2026, 11, 19, 12, 0))
        self.assertEqual(plan.serial, 2026111900)

    def test_crossed_style_boundary_is_rejected(self):
        # The current serial is a plain number far ahead of the date-style
        # range in circular terms, so a date-style value would go backwards.
        # Planning must fail instead of returning a value secondaries would
        # ignore.
        with self.assertRaises(dns.serial.SerialNotUpdated):
            dns.serial.plan_serial(
                3_000_000_000, "date", now=self.at(2026, 11, 19, 12, 0)
            )

    def test_invalid_embedded_date(self):
        # A ten-digit serial whose first eight digits are not a real date is
        # not treated as a date-style serial.  Here it carries a date ahead of
        # today, so a date-style value for today would go backwards and
        # planning must refuse it.
        with self.assertRaises(dns.serial.SerialNotUpdated):
            dns.serial.plan_serial(
                2026130100, "date", now=self.at(2026, 11, 19, 12, 0)
            )

    def test_date_too_large(self):
        # 9999123100 does not fit in a 32-bit serial number.
        with self.assertRaises(dns.serial.BadSerialPolicy):
            dns.serial.plan_serial(1, "date", now=self.at(9999, 12, 31, 0, 0))

    def test_date_year_too_small(self):
        # A year with fewer than four digits cannot be written as YYYYMMDDnn.
        with self.assertRaises(dns.serial.BadSerialPolicy):
            dns.serial.plan_serial(1, "date", now=self.at(1, 1, 1, 0, 0))

    def test_half_space_apart(self):
        # If today's base happens to be exactly half the space from the
        # current serial, the comparison is undefined and planning fails.
        base = 2026111900
        current = (base + dns.serial.HALF_SERIAL) % dns.serial.SERIAL_MODULUS
        with self.assertRaises(dns.serial.SerialComparisonUndefined):
            dns.serial.plan_serial(current, "date", now=self.at(2026, 11, 19))


class PolicyParsingTestCase(unittest.TestCase):
    def test_known_names(self):
        self.assertEqual(
            dns.serial.make_policy("increment"), dns.serial.IncrementPolicy()
        )
        self.assertEqual(
            dns.serial.make_policy("fixed"), dns.serial.IncrementPolicy()
        )
        self.assertEqual(
            dns.serial.make_policy("increment:7"), dns.serial.IncrementPolicy(7)
        )
        self.assertEqual(
            dns.serial.make_policy("fixed:7"), dns.serial.IncrementPolicy(7)
        )
        self.assertEqual(dns.serial.make_policy("date"), dns.serial.DatePolicy())
        self.assertEqual(
            dns.serial.make_policy("datecounter"), dns.serial.DatePolicy()
        )
        # Case and whitespace are ignored.
        self.assertEqual(
            dns.serial.make_policy(" DATE "), dns.serial.DatePolicy()
        )

    def test_policy_object_passthrough(self):
        policy = dns.serial.IncrementPolicy(3)
        self.assertIs(dns.serial.make_policy(policy), policy)

    def test_bad_policies(self):
        for text in ("bogus", "", "increment:", "increment:x", "date:1", "fixed:-2"):
            with self.assertRaises(dns.serial.BadSerialPolicy):
                dns.serial.make_policy(text)
        with self.assertRaises(dns.serial.BadSerialPolicy):
            dns.serial.make_policy(1)  # type: ignore[arg-type]


class PlanValidationTestCase(unittest.TestCase):
    def test_bad_current_serial(self):
        for value in (-1, dns.serial.SERIAL_MODULUS, 1.0, "1", True):
            with self.assertRaises(dns.serial.BadSerial):
                dns.serial.plan_serial(value)  # type: ignore[arg-type]

    def test_bad_now(self):
        with self.assertRaises(TypeError):
            dns.serial.plan_serial(1, "date", now="now")  # type: ignore[arg-type]

    def test_custom_policy_going_backwards(self):
        class Backwards(dns.serial.SerialPolicy):
            def next_serial(self, current, now):
                return current - 1

        with self.assertRaises(dns.serial.SerialNotUpdated):
            dns.serial.plan_serial(5, Backwards())

    def test_custom_policy_returning_current(self):
        class Same(dns.serial.SerialPolicy):
            def next_serial(self, current, now):
                return current

        with self.assertRaises(dns.serial.SerialNotUpdated):
            dns.serial.plan_serial(5, Same())

    def test_custom_policy_half_space(self):
        class Halfway(dns.serial.SerialPolicy):
            def next_serial(self, current, now):
                return (current + dns.serial.HALF_SERIAL) % dns.serial.SERIAL_MODULUS

        with self.assertRaises(dns.serial.SerialComparisonUndefined):
            dns.serial.plan_serial(0, Halfway())

    def test_custom_policy_out_of_range(self):
        class TooBig(dns.serial.SerialPolicy):
            def next_serial(self, current, now):
                return dns.serial.SERIAL_MODULUS

        with self.assertRaises(dns.serial.BadSerial):
            dns.serial.plan_serial(0, TooBig())

    def test_manual_plan_is_validated(self):
        with self.assertRaises(dns.serial.SerialNotUpdated):
            dns.serial.SerialPlan(5, 5, dns.serial.IncrementPolicy())
        with self.assertRaises(dns.serial.BadSerial):
            dns.serial.SerialPlan(0, -1, dns.serial.IncrementPolicy())
        with self.assertRaises(dns.serial.SerialComparisonUndefined):
            dns.serial.SerialPlan(0, dns.serial.HALF_SERIAL, dns.serial.DatePolicy())


class NextSerialTestCase(unittest.TestCase):
    def test_next_serial(self):
        self.assertEqual(dns.serial.next_serial(41), 42)
        self.assertEqual(dns.serial.next_serial(41, "fixed:4"), 45)
        self.assertEqual(dns.serial.next_serial(dns.serial.MAX_SERIAL, "increment"), 0)

    def test_next_serial_date(self):
        now = datetime.datetime(2026, 11, 19, 12, 0, tzinfo=datetime.timezone.utc)
        self.assertEqual(
            dns.serial.next_serial(2026111899, "date", now=now), 2026111900
        )

    def test_next_serial_propagates_errors(self):
        with self.assertRaises(dns.serial.SerialExhausted):
            dns.serial.next_serial(
                2026111999,
                "date",
                now=datetime.datetime(
                    2026, 11, 19, 12, 0, tzinfo=datetime.timezone.utc
                ),
            )


zone_text = """$TTL 3600
$ORIGIN example.
@ soa ns1 hostmaster 41 7200 3600 604800 3600
@ ns ns1
ns1 a 192.0.2.1
"""

ZONE_FACTORIES = [dns.zone.Zone, dns.versioned.Zone, dns.btreezone.Zone]


def _zone(factory=dns.zone.Zone, relativize=True, text=zone_text):
    return dns.zone.from_text(text, zone_factory=factory, relativize=relativize)


@pytest.mark.parametrize("factory", ZONE_FACTORIES)
@pytest.mark.parametrize("relativize", [True, False])
def test_for_zone_does_not_write(factory, relativize):
    zone = _zone(factory, relativize)
    plan = dns.serial.SerialPlan.for_zone(zone, "increment")
    assert plan.current == 41
    assert plan.serial == 42
    # Planning must not have changed the zone.
    assert zone.get_soa().serial == 41


@pytest.mark.parametrize("factory", ZONE_FACTORIES)
@pytest.mark.parametrize("relativize", [True, False])
def test_apply(factory, relativize):
    zone = _zone(factory, relativize)
    with zone.writer() as txn:
        plan = dns.serial.SerialPlan.for_zone(zone, "increment", txn=txn)
        assert plan.serial == 42
        plan.apply(txn)
        # The other fields of the SOA must be preserved.
        soa = zone.get_soa(txn)
        assert soa.serial == 42
        mname = dns.name.from_text("ns1", None if relativize else zone.origin)
        assert soa.mname == mname
    assert zone.get_soa().serial == 42


@pytest.mark.parametrize("factory", ZONE_FACTORIES)
def test_apply_explicit_name(factory):
    zone = _zone(factory)
    with zone.writer() as txn:
        plan = dns.serial.plan_serial(41, "fixed:10")
        plan.apply(txn, "@")
    assert zone.get_soa().serial == 51


def test_apply_missing_soa():
    zone = _zone()
    with zone.writer() as txn:
        plan = dns.serial.plan_serial(41)
        with pytest.raises(KeyError):
            plan.apply(txn, "unknown")


def test_apply_no_origin():
    # An absolute zone whose origin is not yet known cannot resolve the
    # default SOA owner name, so apply() must demand one explicitly.
    zone = dns.zone.Zone(None, relativize=False)
    with zone.writer() as txn:
        plan = dns.serial.plan_serial(41)
        with pytest.raises(dns.serial.SerialError):
            plan.apply(txn)


def test_apply_stale_plan():
    zone = _zone()
    plan = dns.serial.plan_serial(zone.get_soa().serial)
    # Someone else bumps the serial before we apply.
    with zone.writer() as txn:
        txn.update_serial()
    with zone.writer() as txn:
        with pytest.raises(dns.serial.SerialChanged):
            plan.apply(txn)
        # Nothing must have been written by the failed apply.
        assert zone.get_soa(txn).serial == 42
    assert zone.get_soa().serial == 42


def test_apply_revalidates_update():
    zone = _zone()
    plan = dns.serial.plan_serial(41, "increment")
    # Simulate a policy which has been mutated after planning: the plan is
    # stale in a way only serial arithmetic can see.
    object.__setattr__(plan, "serial", 1)
    with zone.writer() as txn:
        with pytest.raises(dns.serial.SerialNotUpdated):
            plan.apply(txn)
    assert zone.get_soa().serial == 41


def test_for_soa():
    zone = _zone()
    plan = dns.serial.SerialPlan.for_soa(zone.get_soa(), "fixed:3")
    assert plan.current == 41
    assert plan.serial == 44
    # Planning must not have changed the zone.
    assert zone.get_soa().serial == 41


def test_plan_then_apply_date_style():
    zone = _zone()
    now = datetime.datetime(2026, 11, 19, 9, 0, tzinfo=datetime.timezone.utc)
    with zone.writer() as txn:
        plan = dns.serial.SerialPlan.for_zone(zone, "date", txn=txn, now=now)
        assert plan.serial == 2026111900
        plan.apply(txn)
    assert zone.get_soa().serial == 2026111900


def test_apply_rejects_read_only_transaction():
    zone = _zone()
    plan = dns.serial.plan_serial(41)
    with zone.reader() as txn:
        with pytest.raises(dns.transaction.ReadOnly):
            plan.apply(txn)
    assert zone.get_soa().serial == 41


def test_no_soa():
    zone = dns.zone.Zone("example.")
    with pytest.raises(dns.zone.NoSOA):
        dns.serial.SerialPlan.for_zone(zone, "increment")


def _reference_compare(a, b, bits=32):
    modulus = 2**bits
    half = 2 ** (bits - 1)
    a %= modulus
    b %= modulus
    if a == b:
        return 0
    if (a - b) % modulus < half:
        return 1
    if (b - a) % modulus < half:
        return -1
    return None


def test_compare_agrees_with_reference():
    # Check the comparison against an independent formulation, and against the
    # Serial operators, over a spread of interesting values.
    rng = random.Random(20261119)
    interesting = [
        0,
        1,
        2,
        dns.serial.HALF_SERIAL - 1,
        dns.serial.HALF_SERIAL,
        dns.serial.HALF_SERIAL + 1,
        dns.serial.MAX_SERIAL - 1,
        dns.serial.MAX_SERIAL,
        2026111900,
        1700000000,
    ]
    values = interesting + [
        rng.randrange(dns.serial.SERIAL_MODULUS) for _ in range(200)
    ]
    for a in values:
        for b in values:
            order = dns.serial.compare(a, b)
            assert order == _reference_compare(a, b)
            sa = dns.serial.Serial(a)
            sb = dns.serial.Serial(b)
            if order is None:
                # Neither is greater or less; only equality is defined.
                assert not (sa < sb) and not (sa > sb)
                assert not (sa <= sb) and not (sa >= sb)
            else:
                assert (sa > sb) == (order == 1)
                assert (sa < sb) == (order == -1)
                assert (sa >= sb) == (order >= 0)
                assert (sa <= sb) == (order <= 0)
                assert (sa == sb) == (order == 0)
