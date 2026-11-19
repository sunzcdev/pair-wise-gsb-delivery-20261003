# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

import pytest

import dns.btreezone
import dns.name
import dns.rdataclass
import dns.rdataset
import dns.rdatatype
import dns.set
import dns.transaction
import dns.versioned
import dns.zone
import dns.zonecheck

# A structurally valid base zone: apex SOA + NS, an apex nameserver address,
# and an ordinary service node.
BASE_TEXT = """$TTL 300
$ORIGIN example.
@ SOA ns.example. hostmaster.example. 1 2 3 4 5
@ NS ns.example.
ns A 192.0.2.1
www A 192.0.2.8
"""

SOA_RDS = dns.rdataset.from_text(
    "IN", "SOA", 300, "ns.example. hostmaster.example. 1 2 3 4 5"
)
NS_RDS = dns.rdataset.from_text("IN", "NS", 300, "ns.example.")
A_RDS = dns.rdataset.from_text("IN", "A", 300, "192.0.2.1")
CNAME_RDS = dns.rdataset.from_text("IN", "CNAME", 300, "target.example.")
RRSIG_CNAME_RDS = dns.rdataset.from_text(
    "IN",
    "RRSIG",
    300,
    "CNAME 10 2 300 20260101000000 20251201000000 12345 example. FAKE",
)
RRSIG_A_RDS = dns.rdataset.from_text(
    "IN",
    "RRSIG",
    300,
    "A 10 2 300 20260101000000 20251201000000 12345 example. FAKE",
)
# 32 bytes of digest, valid for digest type 2 (SHA-256).
DS_RDS = dns.rdataset.from_text("IN", "DS", 300, "12345 8 2 " + "AA" * 32)


@pytest.fixture(params=[dns.zone.Zone, dns.versioned.Zone])
def zone(request):
    return dns.zone.from_text(
        BASE_TEXT, "example.", check_origin=False, zone_factory=request.param
    )


@pytest.fixture
def btree_zone():
    return dns.zone.from_text(
        BASE_TEXT, "example.", check_origin=False, zone_factory=dns.btreezone.Zone
    )


def rules_for(report):
    return {violation.rule for violation in report}


def names_for(report, rule):
    return {violation.name for violation in report if violation.rule == rule}


# A minimal TransactionManager without the zone Node layer's automatic CNAME
# conflict resolution.  It lets us build structurally invalid views that hold
# conflicting records simultaneously, which is exactly what a commit-time
# validator must be able to report without "fixing" the data.
class RawDB(dns.transaction.TransactionManager):
    def __init__(self):
        self.rdatasets = {}

    def reader(self):
        return RawTransaction(self, False, True)

    def writer(self, replacement=False):
        return RawTransaction(self, replacement, False)

    def origin_information(self):
        return (dns.name.from_text("example."), True, dns.name.empty)

    def get_class(self):
        return dns.rdataclass.IN


class RawTransaction(dns.transaction.Transaction):
    def __init__(self, db, replacement, read_only):
        super().__init__(db, replacement, read_only)
        self.rdatasets = {} if replacement else dict(db.rdatasets)

    def _get_rdataset(self, name, rdtype, covers):
        return self.rdatasets.get((name, rdtype, covers))

    def _put_rdataset(self, name, rdataset):
        self.rdatasets[(name, rdataset.rdtype, rdataset.covers)] = rdataset

    def _delete_name(self, name):
        for key in [key for key in self.rdatasets if key[0] == name]:
            del self.rdatasets[key]

    def _delete_rdataset(self, name, rdtype, covers):
        self.rdatasets.pop((name, rdtype, covers), None)

    def _name_exists(self, name):
        return any(key[0] == name for key in self.rdatasets)

    def _changed(self):
        return not self.read_only and len(self.rdatasets) > 0

    def _end_transaction(self, commit):
        if commit:
            self.manager.rdatasets = self.rdatasets

    def _set_origin(self, origin):
        pass

    def _iterate_rdatasets(self):
        for (name, _rdtype, _covers), rdataset in self.rdatasets.items():
            yield (name, rdataset)

    def _iterate_names(self):
        return iter({name for name, _, _ in self.rdatasets})

    def _get_node(self, name):
        return None


# ---------------------------------------------------------------------------
# Configuration: supported rules only
# ---------------------------------------------------------------------------


def test_unknown_rules_are_rejected():
    for bad in ("no-such-rule", 7, object()):
        with pytest.raises(dns.zonecheck.UnknownRule, match="unknown zone check rule"):
            dns.zonecheck.CommitValidator([bad])
    with pytest.raises(dns.zonecheck.UnknownRule):
        dns.zonecheck.Rule.make("not-a-rule")


def test_rule_make_and_default_rule_set():
    assert dns.zonecheck.Rule.make("cname") is dns.zonecheck.Rule.CNAME
    assert (
        dns.zonecheck.Rule.make(dns.zonecheck.Rule.APEX_NS)
        is dns.zonecheck.Rule.APEX_NS
    )
    validator = dns.zonecheck.CommitValidator()
    assert validator.rules == dns.zonecheck.ALL_RULES
    assert set(dns.zonecheck.Rule) == {
        dns.zonecheck.Rule.APEX_SOA,
        dns.zonecheck.Rule.APEX_NS,
        dns.zonecheck.Rule.CNAME,
        dns.zonecheck.Rule.DELEGATION_BOUNDARY,
    }


def test_rule_subset_and_empty_set(zone):
    validator = dns.zonecheck.CommitValidator([dns.zonecheck.Rule.CNAME])
    assert validator.rules == frozenset({dns.zonecheck.Rule.CNAME})
    # Text rule names and Rule members are accepted interchangeably.
    assert dns.zonecheck.CommitValidator(
        ["cname", dns.zonecheck.Rule.APEX_SOA]
    ).rules == (frozenset({dns.zonecheck.Rule.CNAME, dns.zonecheck.Rule.APEX_SOA}))
    with zone.writer() as txn:
        txn.delete(dns.name.empty, dns.rdatatype.NS)
        # Only CNAME is enabled, so the missing apex NS must not be reported.
        assert validator.validate(txn).ok
    assert dns.zonecheck.CommitValidator([]).rules == frozenset()


def test_report_truthiness_and_unknown_origin():
    db = RawDB()
    with db.writer() as txn:
        txn._put_rdataset(dns.name.from_text("www", None), A_RDS)
        good = dns.zonecheck.CommitValidator([]).validate(txn)
        bad = dns.zonecheck.validate(txn)
    # A report is truthy exactly when it has no violations.
    assert bool(good) is True
    assert bool(bad) is False
    # A manager whose view has no origin cannot have its apex determined.
    no_origin_db = RawDB()
    no_origin_db.origin_information = lambda: (None, True, None)
    with no_origin_db.reader() as txn:
        with pytest.raises(dns.zonecheck.UnknownOrigin):
            dns.zonecheck.validate(txn)


def test_multi_rdata_singleton_rrsets_are_reported(zone):
    # Singleton types cannot hold two rdatas through the public API (the
    # rdataset enforces singleton semantics), so build the invalid state
    # directly through the underlying set to exercise the validator's own
    # length checks.
    r1 = dns.rdata.from_text("IN", "SOA", "ns1.example. hostmaster 1 2 3 4 5")
    r2 = dns.rdata.from_text("IN", "SOA", "ns2.example. hostmaster 1 2 3 4 5")
    double_soa = dns.rdataset.Rdataset(dns.rdataclass.IN, dns.rdatatype.SOA)
    double_soa.update_ttl(300)
    double_soa.update(dns.rdataset.from_rdata(300, r1))
    # Bypass the singleton guard by adding straight into the backing set.
    dns.set.Set.add(double_soa, r2)
    assert len(double_soa) == 2

    c1 = dns.rdata.from_text("IN", "CNAME", "one.example.")
    c2 = dns.rdata.from_text("IN", "CNAME", "two.example.")
    double_cname = dns.rdataset.Rdataset(dns.rdataclass.IN, dns.rdatatype.CNAME)
    double_cname.update_ttl(300)
    dns.set.Set.add(double_cname, c1)
    dns.set.Set.add(double_cname, c2)
    assert len(double_cname) == 2

    with zone.writer() as txn:
        txn.version.put_rdataset(dns.name.empty, double_soa)
        txn.version.put_rdataset(dns.name.from_text("multi", None), double_cname)
        report = dns.zonecheck.validate(txn)
    messages = [v.message for v in report]
    assert "apex SOA RRset must contain exactly one rdata" in messages
    assert "CNAME RRset must contain exactly one rdata" in messages


# ---------------------------------------------------------------------------
# Happy path
# ---------------------------------------------------------------------------


def test_valid_zone_passes(zone):
    with zone.reader() as txn:
        report = dns.zonecheck.validate(txn)
    assert report.ok
    assert len(report) == 0
    assert str(report) == "zone transaction structural validation passed"


def test_valid_delegation_with_glue_passes(zone):
    with zone.writer() as txn:
        txn.add("child", dns.rdataset.from_text("IN", "NS", 300, "ns1.child"))
        txn.add("ns1.child", dns.rdataset.from_text("IN", "A", 300, "192.0.2.10"))
        txn.add("ns1.child", dns.rdataset.from_text("IN", "AAAA", 300, "2001:db8::1"))
        txn.add("child", DS_RDS)
    with zone.reader() as txn:
        report = dns.zonecheck.validate(txn)
    assert report.ok


def test_absolute_and_relative_views_equivalent():
    for relativize in (True, False):
        z = dns.zone.from_text(
            BASE_TEXT,
            "example.",
            relativize=relativize,
            check_origin=False,
        )
        with z.reader() as txn:
            assert dns.zonecheck.validate(txn).ok


# ---------------------------------------------------------------------------
# Apex SOA / NS
# ---------------------------------------------------------------------------


def test_missing_apex_soa_and_ns():
    z = dns.zone.from_text(
        "$TTL 300\n$ORIGIN example.\nwww A 192.0.2.8\n",
        "example.",
        check_origin=False,
    )
    with z.reader() as txn:
        report = dns.zonecheck.validate(txn)
    assert not report.ok
    assert rules_for(report) == {
        dns.zonecheck.Rule.APEX_SOA,
        dns.zonecheck.Rule.APEX_NS,
    }
    apex_violations = [v for v in report if v.name == dns.name.empty]
    assert {v.rule for v in apex_violations} == {
        dns.zonecheck.Rule.APEX_SOA,
        dns.zonecheck.Rule.APEX_NS,
    }
    # Absence is reported with a None RRset, not by inventing or deleting data.
    assert all(v.rdataset is None for v in apex_violations)


def test_deleting_apex_ns_in_txn_is_reported(zone):
    validator = dns.zonecheck.CommitValidator()
    with zone.writer() as txn:
        txn.delete(dns.name.empty, dns.rdatatype.NS)
        report = validator.validate(txn)
    assert rules_for(report) == {dns.zonecheck.Rule.APEX_NS}
    assert report.violations[0].message == "zone apex has no NS RRset"


def test_non_apex_soa_is_reported():
    # The base Transaction.add() rejects non-apex SOA, but a version
    # implementation (such as btreezone's) reached through its version API can
    # still store one, so the validator must catch it independently.
    z = dns.zone.from_text(
        BASE_TEXT, "example.", check_origin=False, zone_factory=dns.btreezone.Zone
    )
    with z.writer() as txn:
        name = z._validate_name("elsewhere")
        txn.version.put_rdataset(name, SOA_RDS)
        report = dns.zonecheck.validate(txn)
    violations = [v for v in report if v.rule == dns.zonecheck.Rule.APEX_SOA]
    assert any("only allowed at the zone apex" in v.message for v in violations)
    assert names_for(report, dns.zonecheck.Rule.APEX_SOA) == {
        dns.name.from_text("elsewhere", None)
    }


# ---------------------------------------------------------------------------
# CNAME exclusivity
# ---------------------------------------------------------------------------


def test_apex_cname_is_reported(zone):
    with zone.writer() as txn:
        # Adding an apex CNAME makes the Node layer drop the regular SOA/NS
        # rdatasets; the validator must report the full resulting structure
        # rather than hiding any of it.
        txn.add(dns.name.empty, CNAME_RDS)
        report = dns.zonecheck.validate(txn)
    cname_violations = [v for v in report if v.rule == dns.zonecheck.Rule.CNAME]
    assert len(cname_violations) == 1
    assert cname_violations[0].name == dns.name.empty
    assert cname_violations[0].rdataset.rdtype == dns.rdatatype.CNAME
    assert {dns.zonecheck.Rule.APEX_SOA, dns.zonecheck.Rule.APEX_NS} <= rules_for(
        report
    )


def test_cname_coexistence_in_raw_view_is_reported_without_mutation():
    db = RawDB()
    with db.writer() as txn:
        apex = dns.name.empty
        txn._put_rdataset(apex, SOA_RDS)
        txn._put_rdataset(apex, NS_RDS)
        alias = dns.name.from_text("alias", None)
        txn._put_rdataset(alias, CNAME_RDS)
        txn._put_rdataset(alias, A_RDS)
        report = dns.zonecheck.CommitValidator([dns.zonecheck.Rule.CNAME]).validate(txn)
    assert not report.ok
    # The conflicting A RRset is named; nothing was removed from the view.
    (violation,) = report
    assert violation.name == alias
    assert violation.rdataset.rdtype == dns.rdatatype.A
    assert (
        txn._get_rdataset(alias, dns.rdatatype.CNAME, dns.rdatatype.NONE) is CNAME_RDS
    )
    assert txn._get_rdataset(alias, dns.rdatatype.A, dns.rdatatype.NONE) is A_RDS


def test_rrsig_cname_is_allowed_but_other_rrsig_is_not(zone):
    with zone.writer() as txn:
        txn.add("alias", CNAME_RDS)
        txn.add("alias", RRSIG_CNAME_RDS)
        assert (
            dns.zonecheck.CommitValidator([dns.zonecheck.Rule.CNAME]).validate(txn).ok
        )

    # A zone Node silently resolves CNAME/regular conflicts on write, so the
    # coexistence of a CNAME with an RRSIG(A) (a regular rdataset) is built on
    # the raw manager to exercise the validator's own read-only logic.
    db = RawDB()
    alias = dns.name.from_text("other", None)
    with db.writer() as txn:
        txn._put_rdataset(dns.name.empty, SOA_RDS)
        txn._put_rdataset(dns.name.empty, NS_RDS)
        txn._put_rdataset(alias, CNAME_RDS)
        txn._put_rdataset(alias, RRSIG_A_RDS)
        report = dns.zonecheck.CommitValidator([dns.zonecheck.Rule.CNAME]).validate(txn)
    (violation,) = report
    assert violation.name == alias
    assert violation.rdataset.rdtype == dns.rdatatype.RRSIG
    assert violation.rdataset.covers == dns.rdatatype.A
    # The validator did not remove anything.
    with db.reader() as txn:
        assert txn.get(alias, "CNAME") is not None
        assert txn.get(alias, "RRSIG", "A") is not None


# ---------------------------------------------------------------------------
# Delegation boundary and glue
# ---------------------------------------------------------------------------


def test_non_glue_data_beneath_cut_is_reported(zone):
    with zone.writer() as txn:
        txn.add("child", dns.rdataset.from_text("IN", "NS", 300, "ns1.child"))
        txn.add("ns1.child", dns.rdataset.from_text("IN", "A", 300, "192.0.2.10"))
        # A record at a name that is not referenced by any NS rdata is not
        # glue, even though it sits beneath the cut.
        txn.add("random.child", dns.rdataset.from_text("IN", "A", 300, "192.0.2.11"))
        txn.add("host.child", dns.rdataset.from_text("IN", "TXT", 300, '"no"'))
        report = dns.zonecheck.validate(txn)
    bad = names_for(report, dns.zonecheck.Rule.DELEGATION_BOUNDARY)
    assert bad == {
        dns.name.from_text("random.child", None),
        dns.name.from_text("host.child", None),
    }
    assert all("crosses the zone boundary" in v.message for v in report)


def test_forbidden_rrset_at_delegation_point_is_reported(zone):
    with zone.writer() as txn:
        txn.add("child", dns.rdataset.from_text("IN", "NS", 300, "ns1.child"))
        txn.add("ns1.child", dns.rdataset.from_text("IN", "A", 300, "192.0.2.10"))
        txn.add("child", dns.rdataset.from_text("IN", "TXT", 300, '"cut"'))
        txn.add("child", dns.rdataset.from_text("IN", "MX", 300, "0 mail"))
        report = dns.zonecheck.validate(txn)
    cut_violations = [
        v
        for v in report
        if v.rule == dns.zonecheck.Rule.DELEGATION_BOUNDARY
        and v.name == dns.name.from_text("child", None)
    ]
    assert {v.rdataset.rdtype for v in cut_violations} == {
        dns.rdatatype.TXT,
        dns.rdatatype.MX,
    }
    assert all("not allowed at a delegation point" in v.message for v in cut_violations)


def test_dnssec_rrsets_allowed_at_cut(zone):
    with zone.writer() as txn:
        txn.add("child", dns.rdataset.from_text("IN", "NS", 300, "ns1.child"))
        txn.add("ns1.child", dns.rdataset.from_text("IN", "A", 300, "192.0.2.10"))
        txn.add("child", DS_RDS)
        txn.add(
            "child",
            dns.rdataset.from_text(
                "IN",
                "NSEC3",
                300,
                "1 0 1 1234567890ABCDEF 1234ABCD NS DS RRSIG",
            ),
        )
        assert (
            dns.zonecheck.CommitValidator([dns.zonecheck.Rule.DELEGATION_BOUNDARY])
            .validate(txn)
            .ok
        )


def test_whole_view_catches_untouched_names(zone):
    # host.child is legitimate authoritative data before any delegation exists.
    with zone.writer() as txn:
        txn.add("host.child", dns.rdataset.from_text("IN", "A", 300, "192.0.2.50"))
        txn.add("host.child", dns.rdataset.from_text("IN", "TXT", 300, '"ok"'))
    with zone.reader() as txn:
        assert dns.zonecheck.validate(txn).ok
    # A later transaction only adds the delegation; it never touches
    # host.child, yet the whole-view validator must reject the publish.
    validator = dns.zonecheck.CommitValidator([dns.zonecheck.Rule.DELEGATION_BOUNDARY])
    with pytest.raises(dns.zonecheck.CommitCheckFailure) as exc_info:
        with zone.writer() as txn:
            validator.attach(txn)
            txn.add("child", dns.rdataset.from_text("IN", "NS", 300, "ns1.child"))
            txn.add("ns1.child", dns.rdataset.from_text("IN", "A", 300, "192.0.2.10"))
    assert names_for(exc_info.value.report, dns.zonecheck.Rule.DELEGATION_BOUNDARY) == {
        dns.name.from_text("host.child", None)
    }
    # The rejected transaction was rolled back, so no delegation is visible.
    with zone.reader() as txn:
        assert txn.get("child", "NS") is None


def test_nested_delegations_pass(zone):
    with zone.writer() as txn:
        txn.add("child", dns.rdataset.from_text("IN", "NS", 300, "ns1.child"))
        txn.add("ns1.child", dns.rdataset.from_text("IN", "A", 300, "192.0.2.10"))
        txn.add(
            "grand.child",
            dns.rdataset.from_text("IN", "NS", 300, "ns1.grand.child"),
        )
        txn.add("ns1.grand.child", dns.rdataset.from_text("IN", "A", 300, "192.0.2.20"))
        assert dns.zonecheck.validate(txn).ok


def test_absolute_in_bailiwick_ns_target_is_relativized_for_glue():
    # A non-zonefile transaction may store an absolute in-bailiwick NS target
    # in a relativized view; the validator must still recognize the matching
    # A RRset as glue rather than unauthorized data.
    db = RawDB()
    child = dns.name.from_text("child", None)
    ns_child_relative = dns.name.from_text("ns1.child", None)
    with db.writer() as txn:
        txn._put_rdataset(dns.name.empty, SOA_RDS)
        txn._put_rdataset(dns.name.empty, NS_RDS)
        txn._put_rdataset(
            child,
            dns.rdataset.from_text("IN", "NS", 300, "ns1.child.example."),
        )
        txn._put_rdataset(
            ns_child_relative, dns.rdataset.from_text("IN", "A", 300, "192.0.2.10")
        )
        report = dns.zonecheck.CommitValidator(
            [dns.zonecheck.Rule.DELEGATION_BOUNDARY]
        ).validate(txn)
    assert report.ok


def test_glue_is_not_treated_as_authoritative(zone):
    # Glue A/AAAA beneath a cut must not be reported as ordinary data, while a
    # TXT at the same glue name still must be.
    with zone.writer() as txn:
        txn.add("child", dns.rdataset.from_text("IN", "NS", 300, "ns1.child"))
        txn.add("ns1.child", dns.rdataset.from_text("IN", "A", 300, "192.0.2.10"))
        txn.add("ns1.child", dns.rdataset.from_text("IN", "TXT", 300, '"glue?"'))
        report = dns.zonecheck.CommitValidator(
            [dns.zonecheck.Rule.DELEGATION_BOUNDARY]
        ).validate(txn)
    (violation,) = report
    assert violation.name == dns.name.from_text("ns1.child", None)
    assert violation.rdataset.rdtype == dns.rdatatype.TXT


def test_btreezone_flags_and_validator_agree(btree_zone):
    with btree_zone.writer() as txn:
        txn.add("child", dns.rdataset.from_text("IN", "NS", 300, "ns1.child"))
        txn.add("ns1.child", dns.rdataset.from_text("IN", "A", 300, "192.0.2.10"))
        assert dns.zonecheck.validate(txn).ok
    with btree_zone.writer() as txn:
        txn.add("host.child", dns.rdataset.from_text("IN", "TXT", 300, '"bad"'))
        report = dns.zonecheck.validate(txn)
    assert not report.ok
    assert names_for(report, dns.zonecheck.Rule.DELEGATION_BOUNDARY) == {
        dns.name.from_text("host.child", None)
    }


# ---------------------------------------------------------------------------
# Commit integration
# ---------------------------------------------------------------------------


def test_commit_check_blocks_publish_and_rolls_back(zone):
    validator = dns.zonecheck.CommitValidator()
    if isinstance(zone, dns.versioned.Zone):
        versions_before = len(zone._versions)
    with pytest.raises(dns.zonecheck.CommitCheckFailure) as exc_info:
        with zone.writer() as txn:
            validator.attach(txn)
            txn.delete(dns.name.empty, "SOA")
    assert not exc_info.value.report.ok
    with zone.reader() as txn:
        assert txn.get(dns.name.empty, "SOA") is not None
    if isinstance(zone, dns.versioned.Zone):
        assert len(zone._versions) == versions_before


def test_commit_check_passes_good_transaction(zone):
    validator = dns.zonecheck.CommitValidator()
    with zone.writer() as txn:
        validator.attach(txn)
        txn.add("newhost", dns.rdataset.from_text("IN", "A", 300, "192.0.2.99"))
    with zone.reader() as txn:
        assert txn.get("newhost", "A") is not None


def test_validator_as_transaction_setup_callable(zone):
    validator = dns.zonecheck.CommitValidator()
    # The validator implements __call__ and so can be used wherever a
    # transaction setup callable is expected.
    with zone.writer() as txn:
        validator(txn)
        txn.add("ok", dns.rdataset.from_text("IN", "A", 300, "192.0.2.77"))
    with pytest.raises(dns.zonecheck.CommitCheckFailure):
        with zone.writer() as txn:
            validator.attach(txn)
            txn.delete(dns.name.empty, "NS")


def test_from_text_transaction_setup_rejects_bad_zone():
    with pytest.raises(dns.zonecheck.CommitCheckFailure):
        dns.zone.from_text(
            "$TTL 300\n$ORIGIN example.\nwww A 192.0.2.8\n",
            "example.",
            check_origin=False,
            transaction_setup=dns.zonecheck.CommitValidator(),
        )


def test_check_vs_validate_on_raw_db():
    db = RawDB()
    validator = dns.zonecheck.CommitValidator()
    with db.writer() as txn:
        # No apex records at all.
        txn._put_rdataset(dns.name.from_text("www", None), A_RDS)
        report = validator.validate(txn)
        assert not report.ok
        with pytest.raises(dns.zonecheck.CommitCheckFailure):
            validator.check(txn)
        # Validation never mutates the transaction.
        assert txn._name_exists(dns.name.from_text("www", None))


def test_report_text_names_rrset_and_rule():
    db = RawDB()
    with db.writer() as txn:
        txn._put_rdataset(dns.name.from_text("alias", None), CNAME_RDS)
        txn._put_rdataset(dns.name.from_text("alias", None), A_RDS)
        report = dns.zonecheck.CommitValidator([dns.zonecheck.Rule.CNAME]).validate(txn)
    text = str(report)
    assert "failed" in text
    assert "cname" in text
    assert "alias" in text
    assert "A" in text
    assert [v.rule for v in report] == [dns.zonecheck.Rule.CNAME]
    assert len(tuple(report)) == 1


def test_original_exception_survives_failing_rollback():
    # If the manager's rollback path itself raises, the original commit-check
    # exception must still be the one that propagates.
    class FailingRollbackDB(RawDB):
        class Txn(RawTransaction):
            def _end_transaction(self, commit):
                if commit:
                    super()._end_transaction(True)
                else:
                    raise RuntimeError("rollback machinery broke")

        def writer(self, replacement=False):
            return FailingRollbackDB.Txn(self, replacement, False)

    db = FailingRollbackDB()
    with pytest.raises(dns.zonecheck.CommitCheckFailure):
        with db.writer() as txn:
            dns.zonecheck.CommitValidator().attach(txn)
            txn._put_rdataset(dns.name.from_text("www", None), A_RDS)
