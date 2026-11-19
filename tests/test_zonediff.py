# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

import unittest

import dns.rdataclass
import dns.rdatatype
import dns.versioned
import dns.zone
import dns.zonediff

source_text = """$TTL 3600
$ORIGIN example.
@ soa foo bar 10 2 3 4 5
@ ns ns1
@ ns ns2
ns1 a 10.0.0.1
ns2 a 10.0.0.2
www a 10.0.0.1
www a 10.0.0.2
old a 10.0.0.99
"""

target_text = """$TTL 3600
$ORIGIN example.
@ soa foo bar 11 2 3 4 5
@ ns ns1
@ ns ns2
ns1 a 10.0.0.1
ns2 a 10.0.0.2
www a 10.0.0.1
www a 10.0.0.3
new a 10.0.0.100
"""


def make_source():
    return dns.zone.from_text(source_text, "example.", relativize=True)


def make_target():
    return dns.zone.from_text(target_text, "example.", relativize=True)


# The same zone as source_text, but with no $ORIGIN directive so it can be
# loaded with a different origin.
source_text_no_origin = source_text.replace("$ORIGIN example.\n", "")


class ZoneDiffTestCase(unittest.TestCase):
    def test_diff_contents(self):
        source = make_source()
        target = make_target()
        plan = source.diff(target)
        self.assertEqual(plan.origin, dns.name.from_text("example."))
        self.assertEqual(plan.rdclass, dns.rdataclass.IN)
        self.assertEqual(plan.serial, 10)
        changes = {(c.name, c.rdtype): c for c in plan}
        self.assertEqual(len(changes), 4)
        www = changes[(dns.name.from_text("www.example."), dns.rdatatype.A)]
        self.assertIsNotNone(www.before)
        self.assertIsNotNone(www.after)
        self.assertEqual(len(www.before), 2)
        self.assertEqual(len(www.after), 2)
        old = changes[(dns.name.from_text("old.example."), dns.rdatatype.A)]
        self.assertIsNotNone(old.before)
        self.assertIsNone(old.after)
        new = changes[(dns.name.from_text("new.example."), dns.rdatatype.A)]
        self.assertIsNone(new.before)
        self.assertIsNotNone(new.after)
        soa = changes[(dns.name.from_text("example."), dns.rdatatype.SOA)]
        self.assertEqual(soa.before[0].serial, 10)
        self.assertEqual(soa.after[0].serial, 11)

    def test_apply_makes_target(self):
        source = make_source()
        target = make_target()
        plan = source.diff(target)
        source.apply_diff(plan)
        self.assertEqual(source, target)
        # And there is nothing left to change.
        self.assertEqual(len(source.diff(target)), 0)

    def test_apply_in_explicit_transaction(self):
        source = make_source()
        target = make_target()
        plan = dns.zonediff.diff(source, target)
        with source.writer() as txn:
            plan.apply(txn)
        self.assertEqual(source, target)

    def test_apply_to_versioned_zone(self):
        source = dns.zone.from_text(
            source_text, "example.", zone_factory=dns.versioned.Zone
        )
        target = make_target()
        plan = dns.zonediff.diff(source, target)
        plan.apply_to(source)
        self.assertEqual(len(dns.zonediff.diff(source, target)), 0)

    def test_identical_zones_empty_plan(self):
        source = make_source()
        other = make_source()
        plan = source.diff(other)
        self.assertEqual(len(plan), 0)
        source.apply_diff(plan)
        self.assertEqual(source, other)

    def test_name_case_is_normalized(self):
        upper = dns.zone.from_text(source_text.upper(), "EXAMPLE.")
        lower = dns.zone.from_text(source_text, "example.")
        plan = dns.zonediff.diff(upper, lower)
        self.assertEqual(len(plan), 0)

    def test_relativity_is_normalized(self):
        relative = dns.zone.from_text(source_text, "example.", relativize=True)
        absolute = dns.zone.from_text(source_text, "example.", relativize=False)
        plan = dns.zonediff.diff(relative, absolute)
        self.assertEqual(len(plan), 0)
        plan2 = dns.zonediff.diff(absolute, relative)
        self.assertEqual(len(plan2), 0)

    def test_ttl_change_is_a_change(self):
        text2 = source_text.replace("www a 10.0.0.1", "www 300 a 10.0.0.1")
        source = make_source()
        other = dns.zone.from_text(text2, "example.")
        plan = source.diff(other)
        self.assertEqual(len(plan), 1)
        change = list(plan)[0]
        self.assertEqual(change.name, dns.name.from_text("www.example."))
        self.assertEqual(change.before.ttl, 3600)
        self.assertEqual(change.after.ttl, 300)
        source.apply_diff(plan)
        rds = source.get_rdataset("www", "a")
        self.assertEqual(rds.ttl, 300)

    def test_serial_precondition_failure_is_atomic(self):
        source = make_source()
        target = make_target()
        plan = source.diff(target)
        # Change the serial so the plan's serial precondition fails.
        with source.writer() as txn:
            txn.update_serial()
        before = source.to_text()
        with self.assertRaises(dns.zonediff.PreconditionFailed):
            source.apply_diff(plan)
        self.assertEqual(source.to_text(), before)

    def test_rrset_precondition_failure_is_atomic(self):
        source = make_source()
        target = make_target()
        plan = source.diff(target)
        # Change an RRset the plan has a precondition on.  Note the plan
        # would delete "old" before touching "www" if it were applied
        # naively, so this also verifies nothing is half applied.
        with source.writer() as txn:
            txn.replace("www", dns.rdataset.from_text("in", "a", 3600, "10.9.9.9"))
        before = source.to_text()
        with self.assertRaises(dns.zonediff.PreconditionFailed):
            source.apply_diff(plan)
        self.assertEqual(source.to_text(), before)

    def test_deleted_rrset_precondition_failure(self):
        source = make_source()
        target = make_target()
        plan = source.diff(target)
        # Remove an RRset the plan expects to update.
        with source.writer() as txn:
            txn.delete("old", "a")
        before = source.to_text()
        with self.assertRaises(dns.zonediff.PreconditionFailed):
            source.apply_diff(plan)
        self.assertEqual(source.to_text(), before)

    def test_origin_mismatch_on_apply(self):
        source = make_source()
        target = make_target()
        plan = source.diff(target)
        other = dns.zone.from_text(source_text_no_origin, "other.")
        with self.assertRaises(dns.zonediff.PreconditionFailed):
            other.apply_diff(plan)

    def test_diff_requires_same_origin(self):
        source = make_source()
        other = dns.zone.from_text(source_text_no_origin, "other.")
        with self.assertRaises(ValueError):
            source.diff(other)

    def test_diff_requires_same_class(self):
        source = make_source()
        other = dns.zone.Zone("example.", rdclass=dns.rdataclass.CH)
        with self.assertRaises(ValueError):
            source.diff(other)

    def test_diff_requires_origin(self):
        source = make_source()
        no_origin = dns.zone.Zone(None)
        with self.assertRaises(ValueError):
            source.diff(no_origin)
        with self.assertRaises(ValueError):
            no_origin.diff(source)

    def test_no_soa_serial_is_none(self):
        text = "$TTL 3600\n$ORIGIN example.\n@ ns ns1\nns1 a 10.0.0.1\n"
        z1 = dns.zone.from_text(text, "example.", check_origin=False)
        z2 = dns.zone.from_text(text, "example.", check_origin=False)
        plan = z1.diff(z2)
        self.assertIsNone(plan.serial)
        self.assertEqual(len(plan), 0)
        z1.apply_diff(plan)

    def test_plan_is_not_text_lines(self):
        # The plan is a structured object with identity and preconditions,
        # not a list of add/delete text lines.
        source = make_source()
        target = make_target()
        plan = source.diff(target)
        self.assertIsInstance(plan, dns.zonediff.ZoneDiffPlan)
        self.assertTrue(all(isinstance(c, dns.zonediff.RrsetChange) for c in plan))
        text = str(plan)
        self.assertIn("example.", text)
        self.assertIn("10", text)

    def test_non_relativized_apply(self):
        source = dns.zone.from_text(source_text, "example.", relativize=False)
        target = dns.zone.from_text(target_text, "example.", relativize=False)
        plan = source.diff(target)
        source.apply_diff(plan)
        self.assertEqual(source, target)


if __name__ == "__main__":
    unittest.main()
