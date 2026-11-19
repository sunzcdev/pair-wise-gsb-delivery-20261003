# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

import io
import textwrap
import unittest
from unittest import mock

import dns.name
import dns.rdata
import dns.rdataset
import dns.rrset
import dns.versioned
import dns.zone
from dns.zone import (
    DigestExclusionReason,
    DigestRecordResult,
    DigestStatus,
    DigestVerificationResult,
    ExcludedRRset,
)


class ZoneDigestTestCase(unittest.TestCase):
    # Examples from RFC 8976, fixed per errata.

    simple_example = textwrap.dedent("""
        example.      86400  IN  SOA     ns1 admin 2018031900 (
                                         1800 900 604800 86400 )
                      86400  IN  NS      ns1
                      86400  IN  NS      ns2
                      86400  IN  ZONEMD  2018031900 1 1 (
                                         c68090d90a7aed71
                                         6bc459f9340e3d7c
                                         1370d4d24b7e2fc3
                                         a1ddc0b9a87153b9
                                         a9713b3c9ae5cc27
                                         777f98b8e730044c )
        ns1           3600   IN  A       203.0.113.63
        ns2           3600   IN  AAAA    2001:db8::63
    """)

    complex_example = textwrap.dedent("""
        example.      86400  IN  SOA     ns1 admin 2018031900 (
                                         1800 900 604800 86400 )
                      86400  IN  NS      ns1
                      86400  IN  NS      ns2
                      86400  IN  ZONEMD  2018031900 1 1 (
                                         a3b69bad980a3504
                                         e1cffcb0fd6397f9
                                         3848071c93151f55
                                         2ae2f6b1711d4bd2
                                         d8b39808226d7b9d
                                         b71e34b72077f8fe )
        ns1           3600   IN  A       203.0.113.63
        NS2           3600   IN  AAAA    2001:db8::63
        occluded.sub  7200   IN  TXT     "I'm occluded but must be digested"
        sub           7200   IN  NS      ns1
        duplicate     300    IN  TXT     "I must be digested just once"
        duplicate     300    IN  TXT     "I must be digested just once"
        foo.test.     555    IN  TXT     "out-of-zone data must be excluded"
        UPPERCASE     3600   IN  TXT     "canonicalize uppercase owner names"
        *             777    IN  PTR     dont-forget-about-wildcards
        mail          3600   IN  MX      20 MAIL1
        mail          3600   IN  MX      10 Mail2.Example.
        sortme        3600   IN  AAAA    2001:db8::5:61
        sortme        3600   IN  AAAA    2001:db8::3:62
        sortme        3600   IN  AAAA    2001:db8::4:63
        sortme        3600   IN  AAAA    2001:db8::1:65
        sortme        3600   IN  AAAA    2001:db8::2:64
        non-apex      900    IN  ZONEMD  2018031900 1 1 (
                                         616c6c6f77656420
                                         6275742069676e6f
                                         7265642e20616c6c
                                         6f77656420627574
                                         2069676e6f726564
                                         2e20616c6c6f7765 )
    """)

    multiple_digests_example = textwrap.dedent("""
        example.      86400  IN  SOA     ns1 admin 2018031900 (
                                         1800 900 604800 86400 )
        example.      86400  IN  NS      ns1.example.
        example.      86400  IN  NS      ns2.example.
        example.      86400  IN  ZONEMD  2018031900 1 1 (
                                         62e6cf51b02e54b9
                                         b5f967d547ce4313
                                         6792901f9f88e637
                                         493daaf401c92c27
                                         9dd10f0edb1c56f8
                                         080211f8480ee306 )
        example.      86400  IN  ZONEMD  2018031900 1 2 (
                                         08cfa1115c7b948c
                                         4163a901270395ea
                                         226a930cd2cbcf2f
                                         a9a5e6eb85f37c8a
                                         4e114d884e66f176
                                         eab121cb02db7d65
                                         2e0cc4827e7a3204
                                         f166b47e5613fd27 )
        example.      86400  IN  ZONEMD  2018031900 1 240 (
                                         e2d523f654b9422a
                                         96c5a8f44607bbee )
        example.      86400  IN  ZONEMD  2018031900 241 1 (
                                         e1846540e33a9e41
                                         89792d18d5d131f6
                                         05fc283eaaaaaaaa
                                         aaaaaaaaaaaaaaaa
                                         aaaaaaaaaaaaaaaa
                                         aaaaaaaaaaaaaaaa)
        ns1.example.  3600   IN  A       203.0.113.63
        ns2.example.  86400  IN  TXT     "This example has multiple digests"
        NS2.EXAMPLE.  3600   IN  AAAA    2001:db8::63
    """)

    def _get_zonemd(self, zone):
        return zone.get_rdataset(zone.origin, "ZONEMD")

    def _replace_zonemd(self, zone, *rdatas):
        zonemd = self._get_zonemd(zone)
        zone.replace_rdataset(
            dns.name.empty, dns.rdataset.from_rdata(zonemd.ttl, *rdatas)
        )

    def test_zonemd_simple(self):
        zone = dns.zone.from_text(self.simple_example, origin="example")
        zone.verify_digest()
        zonemd = self._get_zonemd(zone)
        self.assertEqual(zonemd[0], zone.compute_digest(zonemd[0].hash_algorithm))

    def test_zonemd_simple_absolute(self):
        zone = dns.zone.from_text(
            self.simple_example, origin="example", relativize=False
        )
        zone.verify_digest()
        zonemd = self._get_zonemd(zone)
        self.assertEqual(zonemd[0], zone.compute_digest(zonemd[0].hash_algorithm))

    def test_zonemd_complex(self):
        zone = dns.zone.from_text(self.complex_example, origin="example")
        zone.verify_digest()
        zonemd = self._get_zonemd(zone)
        self.assertEqual(zonemd[0], zone.compute_digest(zonemd[0].hash_algorithm))

    def test_zonemd_multiple_digests(self):
        zone = dns.zone.from_text(self.multiple_digests_example, origin="example")
        zone.verify_digest()

        zonemd = self._get_zonemd(zone)
        for rr in zonemd:
            if rr.scheme == 1 and rr.hash_algorithm in (1, 2):
                zone.verify_digest(rr)
                self.assertEqual(rr, zone.compute_digest(rr.hash_algorithm))
            else:
                with self.assertRaises(dns.zone.DigestVerificationFailure):
                    zone.verify_digest(rr)

    def test_zonemd_no_digest(self):
        zone = dns.zone.from_text(self.simple_example, origin="example")
        zone.delete_rdataset(dns.name.empty, "ZONEMD")
        with self.assertRaises(dns.zone.NoDigest):
            zone.verify_digest()

    def test_zonemd_serial_mismatch(self):
        # RFC 8976 Sec. 4 step 5.1.  The apex ZONEMD RRset is not covered by the
        # digest, so only the serial changes here; the digest stays correct.
        zone = dns.zone.from_text(self.simple_example, origin="example")
        zonemd = self._get_zonemd(zone)
        self.assertNotEqual(zonemd[0].serial, 1)
        wrong_serial = zonemd[0].replace(serial=1)
        with self.assertRaises(dns.zone.DigestVerificationFailure):
            zone.verify_digest(wrong_serial)
        self._replace_zonemd(zone, wrong_serial)
        with self.assertRaises(dns.zone.DigestVerificationFailure):
            zone.verify_digest()

    def test_zonemd_no_soa(self):
        zone = dns.zone.from_text(
            self.simple_example, origin="example", check_origin=False
        )
        zone.delete_rdataset(dns.name.empty, "SOA")
        with self.assertRaises(dns.zone.DigestVerificationFailure):
            zone.verify_digest()

    def test_zonemd_duplicate_scheme_and_hash_algorithm(self):
        # RFC 8976 Sec. 4 step 4.  Adding a ZONEMD RR does not change the digest,
        # so the original RR still has the right one, but its scheme and hash
        # algorithm are no longer unique.
        zone = dns.zone.from_text(self.simple_example, origin="example")
        zonemd = self._get_zonemd(zone)
        self._replace_zonemd(zone, zonemd[0], zonemd[0].replace(digest=b"\xab" * 48))
        with self.assertRaises(dns.zone.DigestVerificationFailure):
            zone.verify_digest()

    def test_zonemd_duplicate_does_not_disqualify_other_tuples(self):
        # Only the ZONEMD RRs sharing the duplicated tuple are disqualified, so
        # the SHA-512 digest of this zone still verifies it.
        zone = dns.zone.from_text(self.multiple_digests_example, origin="example")
        zonemd = self._get_zonemd(zone)
        sha384 = [rr for rr in zonemd if (rr.scheme, rr.hash_algorithm) == (1, 1)][0]
        self._replace_zonemd(zone, *zonemd, sha384.replace(digest=b"\xab" * 48))
        zone.verify_digest()

    sha384_hash = "ab" * 48
    sha512_hash = "ab" * 64

    def test_zonemd_parse_rdata(self):
        dns.rdata.from_text("IN", "ZONEMD", "100 1 1 " + self.sha384_hash)
        dns.rdata.from_text("IN", "ZONEMD", "100 1 2 " + self.sha512_hash)
        dns.rdata.from_text("IN", "ZONEMD", "100 100 1 " + self.sha384_hash)
        # an unknown hash algorithm parses with any digest of at least the
        # RFC 8976 Sec. 2.2.4 minimum of 12 octets.
        dns.rdata.from_text("IN", "ZONEMD", "100 1 100 " + "ab" * 12)

    def test_zonemd_unknown_scheme(self):
        zone = dns.zone.from_text(self.simple_example, origin="example")
        with self.assertRaises(dns.zone.UnsupportedDigestScheme):
            zone.compute_digest(dns.zone.DigestHashAlgorithm.SHA384, 2)

    def test_zonemd_unknown_hash_algorithm(self):
        zone = dns.zone.from_text(self.simple_example, origin="example")
        with self.assertRaises(dns.zone.UnsupportedDigestHashAlgorithm):
            zone.compute_digest(5)

    def test_zonemd_invalid_digest_length(self):
        with self.assertRaises(dns.exception.SyntaxError):
            dns.rdata.from_text("IN", "ZONEMD", "100 1 2 " + self.sha384_hash)
        with self.assertRaises(dns.exception.SyntaxError):
            dns.rdata.from_text("IN", "ZONEMD", "100 2 1 " + self.sha512_hash)

    def test_zonemd_digest_too_short(self):
        # RFC 8976 Sec. 2.2.4: the digest must be at least 12 octets even for a
        # private-use hash algorithm the length is otherwise not checked for.
        with self.assertRaises(dns.exception.SyntaxError):
            dns.rdata.from_text("IN", "ZONEMD", "100 1 100 " + "ab" * 11)
        # serial 100, scheme 1, hash_algorithm 100, then an 11-octet digest.
        short = b"\x00\x00\x00\x64\x01\x64" + b"\xab" * 11
        with self.assertRaises(dns.exception.FormError):
            dns.rdata.from_wire("IN", "ZONEMD", short, 0, len(short))

    def test_zonemd_parse_rdata_reserved(self):
        with self.assertRaises(dns.exception.SyntaxError):
            dns.rdata.from_text("IN", "ZONEMD", "100 0 1 " + self.sha384_hash)
        with self.assertRaises(dns.exception.SyntaxError):
            dns.rdata.from_text("IN", "ZONEMD", "100 1 0 " + self.sha384_hash)

    sorting_zone = textwrap.dedent("""
    @  86400  IN  SOA     ns1 admin 2018031900 (
                                     1800 900 604800 86400 )
              86400  IN  NS      ns1
              86400  IN  NS      ns2
              86400  IN  RP      n1.example. a.
              86400  IN  RP      n1. b.
    """)

    def test_relative_zone_sorting(self):
        z1 = dns.zone.from_text(self.sorting_zone, "example.", relativize=True)
        z2 = dns.zone.from_text(self.sorting_zone, "example.", relativize=False)
        zmd1 = z1.compute_digest(dns.zone.DigestHashAlgorithm.SHA384)
        zmd2 = z2.compute_digest(dns.zone.DigestHashAlgorithm.SHA384)
        self.assertEqual(zmd1, zmd2)


class ZoneDigestSummaryTestCase(unittest.TestCase):
    # A deliberately small zone so the number of covered records and the
    # effect of each perturbation can be checked by hand.
    small_zone = textwrap.dedent("""
        example. 86400 IN SOA ns1 admin 2018031900 1800 900 604800 86400
        example. 86400 IN NS ns1
        example. 86400 IN NS ns2
        ns1 3600 IN A 203.0.113.63
        ns2 3600 IN AAAA 2001:db8::63
    """)

    # The same zone with every line and every multi-rdata RRset reordered.
    shuffled_zone = textwrap.dedent("""
        ns2 3600 IN AAAA 2001:db8::63
        ns1 3600 IN A 203.0.113.63
        example. 86400 IN NS ns2
        example. 86400 IN SOA ns1 admin 2018031900 1800 900 604800 86400
        example. 86400 IN NS ns1
    """)

    # The same zone written with absolute names.
    absolute_zone = textwrap.dedent("""
        example. 86400 IN SOA ns1.example. admin.example. (
                              2018031900 1800 900 604800 86400 )
        example. 86400 IN NS ns1.example.
        example. 86400 IN NS ns2.example.
        ns1.example. 3600 IN A 203.0.113.63
        ns2.example. 3600 IN AAAA 2001:db8::63
    """)

    def _zone_with_zonemd(self, text=small_zone, **kwargs):
        zone = dns.zone.from_text(text, origin="example", **kwargs)
        zonemd = zone.compute_digest(dns.zone.DigestHashAlgorithm.SHA384)
        zone.replace_rdataset(dns.name.empty, dns.rdataset.from_rdata(86400, zonemd))
        return zone

    # ---- summary structure -------------------------------------------------

    def test_summary_lists_algorithm_serial_and_counts(self):
        zone = self._zone_with_zonemd()
        result = zone.check_digest()
        self.assertIsInstance(result, DigestVerificationResult)
        self.assertTrue(result.valid)
        # The serial comes from the SOA.
        self.assertEqual(result.serial, 2018031900)
        # One result per apex ZONEMD RR.
        self.assertEqual(len(result.records), 1)
        record = result.records[0]
        self.assertIsInstance(record, DigestRecordResult)
        self.assertEqual(record.status, DigestStatus.VALID)
        self.assertTrue(record.valid)
        # The scheme (1), hash algorithm (1), and serial are reported on the
        # record's ZONEMD rdata.
        self.assertEqual(int(record.zonemd.scheme), 1)
        self.assertEqual(int(record.zonemd.hash_algorithm), 1)
        self.assertEqual(record.zonemd.serial, 2018031900)
        # The computed digest is exposed for re-checking, and matches.
        self.assertEqual(record.computed_digest, record.zonemd.digest)
        # SOA(1) + 2xNS + A(1) + AAAA(1) = 5 covered resource records across
        # four covered RRsets; the single apex ZONEMD RR is not covered.
        self.assertEqual(result.covered_rr_count, 5)
        self.assertEqual(result.covered_rdataset_count, 4)
        self.assertEqual(result.excluded_rr_count, 1)

    def test_summary_lists_excluded_record_categories(self):
        zone = self._zone_with_zonemd()
        result = zone.check_digest()
        self.assertEqual(len(result.excluded_rrsets), 1)
        excluded = result.excluded_rrsets[0]
        self.assertIsInstance(excluded, ExcludedRRset)
        self.assertEqual(excluded.reason, DigestExclusionReason.APEX_ZONEMD)
        self.assertEqual(excluded.rdtype, dns.rdatatype.RdataType.ZONEMD)
        self.assertEqual(excluded.covers, dns.rdatatype.RdataType.NONE)
        self.assertEqual(excluded.rr_count, 1)

    def test_summary_excludes_rrsig_covering_apex_zonemd(self):
        zone = self._zone_with_zonemd()
        rrsig = dns.rdata.from_text(
            "IN",
            "RRSIG",
            "ZONEMD 1 1 86400 20300101000000 20200101000000 12345 example. " "Ym9ndXM=",
            origin=zone.origin,
            relativize=True,
            relativize_to=zone.origin,
        )
        zone.replace_rdataset(dns.name.empty, dns.rdataset.from_rdata(86400, rrsig))
        result = zone.check_digest()
        # Both the ZONEMD RRset and its covering RRSIG are excluded, and the
        # digest still verifies because neither is part of the digested data.
        self.assertTrue(result.valid)
        reasons = {item.reason for item in result.excluded_rrsets}
        self.assertEqual(
            reasons,
            {
                DigestExclusionReason.APEX_ZONEMD,
                DigestExclusionReason.APEX_ZONEMD_RRSIG,
            },
        )
        rrsig_excluded = [
            item
            for item in result.excluded_rrsets
            if item.reason == DigestExclusionReason.APEX_ZONEMD_RRSIG
        ][0]
        self.assertEqual(rrsig_excluded.rdtype, dns.rdatatype.RdataType.RRSIG)
        self.assertEqual(rrsig_excluded.covers, dns.rdatatype.RdataType.ZONEMD)
        self.assertEqual(rrsig_excluded.rr_count, 1)
        self.assertEqual(result.excluded_rr_count, 2)
        # The covered record count is unchanged by the excluded RRSIG.
        self.assertEqual(result.covered_rr_count, 5)

    def test_non_apex_zonemd_is_covered(self):
        # RFC 8976 only excludes the apex ZONEMD RRset; a ZONEMD RRset anywhere
        # else in the zone is ordinary covered data.
        zone = self._zone_with_zonemd()
        non_apex = dns.rdata.from_text(
            "IN",
            "ZONEMD",
            "2018031900 1 1 " + "ab" * 48,
        )
        zone.replace_rdataset("elsewhere", dns.rdataset.from_rdata(300, non_apex))
        result = zone.check_digest()
        self.assertEqual(len(result.excluded_rrsets), 1)
        self.assertEqual(
            result.excluded_rrsets[0].reason,
            DigestExclusionReason.APEX_ZONEMD,
        )
        # The non-apex ZONEMD adds one covered resource record and RRset.
        self.assertEqual(result.covered_rr_count, 6)
        self.assertEqual(result.covered_rdataset_count, 5)

    def test_summary_for_multiple_digests_has_mixed_statuses(self):
        zone = dns.zone.from_text(
            ZoneDigestTestCase.multiple_digests_example, origin="example"
        )
        result = zone.check_digest()
        statuses = {
            (int(r.zonemd.scheme), int(r.zonemd.hash_algorithm)): r.status
            for r in result.records
        }
        self.assertEqual(statuses[(1, 1)], DigestStatus.VALID)
        self.assertEqual(statuses[(1, 2)], DigestStatus.VALID)
        # Unsupported hash algorithm and scheme get statuses of their own.
        self.assertEqual(statuses[(1, 240)], DigestStatus.UNSUPPORTED_HASH_ALGORITHM)
        self.assertEqual(statuses[(241, 1)], DigestStatus.UNSUPPORTED_SCHEME)
        # A supported, matching record makes the overall result valid even
        # though other records are unsupported.
        self.assertTrue(result.valid)
        unsupported = [r for r in result.records if not r.status == DigestStatus.VALID]
        for record in unsupported:
            self.assertIsNone(record.computed_digest)

    def test_unsupported_algorithm_is_independent_and_non_fatal(self):
        zone = dns.zone.from_text(ZoneDigestTestCase.simple_example, origin="example")
        zonemd = zone.get_rdataset(zone.origin, "ZONEMD")
        good = zonemd[0]
        # An unknown hash algorithm (240 is private use) alongside the good
        # record; it must be reported independently, not raise, and not affect
        # the good record.
        unknown_hash = good.replace(hash_algorithm=240, digest=b"\xcd" * 16)
        unknown_scheme = good.replace(scheme=241, digest=b"\xef" * 48)
        zone.replace_rdataset(
            dns.name.empty,
            dns.rdataset.from_rdata(zonemd.ttl, good, unknown_hash, unknown_scheme),
        )
        result = zone.check_digest()
        by_tuple = {
            (int(r.zonemd.scheme), int(r.zonemd.hash_algorithm)): r
            for r in result.records
        }
        self.assertEqual(by_tuple[(1, 1)].status, DigestStatus.VALID)
        self.assertEqual(
            by_tuple[(1, 240)].status,
            DigestStatus.UNSUPPORTED_HASH_ALGORITHM,
        )
        self.assertEqual(by_tuple[(241, 1)].status, DigestStatus.UNSUPPORTED_SCHEME)
        self.assertTrue(result.valid)
        # And the boolean flag on each record agrees with the status.
        self.assertTrue(by_tuple[(1, 1)].valid)
        self.assertFalse(by_tuple[(1, 240)].valid)
        self.assertFalse(by_tuple[(241, 1)].valid)

    # ---- per-record failure statuses --------------------------------------

    def test_invalid_digest_is_reported_not_raised(self):
        zone = self._zone_with_zonemd()
        zonemd = zone.get_rdataset(zone.origin, "ZONEMD")
        tampered = zonemd[0].replace(digest=b"\x01" * 48)
        result = zone.check_digest(tampered)
        self.assertFalse(result.valid)
        self.assertEqual(len(result.records), 1)
        self.assertEqual(result.records[0].status, DigestStatus.INVALID)
        # The digest we actually computed is still made available, so the
        # mismatch can be reviewed rather than taken on trust.
        self.assertEqual(
            result.records[0].computed_digest,
            zone._compute_digest(tampered.hash_algorithm, tampered.scheme),
        )
        self.assertNotEqual(result.records[0].computed_digest, tampered.digest)

    def test_serial_mismatch_status(self):
        zone = self._zone_with_zonemd()
        zonemd = zone.get_rdataset(zone.origin, "ZONEMD")
        wrong_serial = zonemd[0].replace(serial=1)
        result = zone.check_digest(wrong_serial)
        self.assertEqual(result.records[0].status, DigestStatus.SERIAL_MISMATCH)
        self.assertFalse(result.valid)
        self.assertIsNone(result.records[0].computed_digest)

    def test_duplicate_tuple_status(self):
        zone = dns.zone.from_text(ZoneDigestTestCase.simple_example, origin="example")
        zonemd = zone.get_rdataset(zone.origin, "ZONEMD")
        duplicate = zonemd[0].replace(digest=b"\xab" * 48)
        zone.replace_rdataset(
            dns.name.empty,
            dns.rdataset.from_rdata(zonemd.ttl, zonemd[0], duplicate),
        )
        result = zone.check_digest()
        self.assertEqual(len(result.records), 2)
        for record in result.records:
            self.assertEqual(record.status, DigestStatus.DUPLICATE)
        self.assertFalse(result.valid)

    def test_no_soa_reports_but_still_digests(self):
        zone = dns.zone.from_text(
            ZoneDigestTestCase.simple_example,
            origin="example",
            check_origin=False,
        )
        zone.delete_rdataset(dns.name.empty, "SOA")
        # check_digest does not raise; the missing SOA is reported via a
        # None serial.  Deleting the SOA also changed the covered content, so
        # the stored digest no longer matches and is reported INVALID rather
        # than silently treated as verified.
        result = zone.check_digest()
        self.assertIsNone(result.serial)
        self.assertEqual(len(result.records), 1)
        self.assertEqual(result.records[0].status, DigestStatus.INVALID)
        self.assertFalse(result.valid)
        # verify_digest still turns this into its historical exception.
        with self.assertRaises(dns.zone.DigestVerificationFailure):
            zone.verify_digest()

    def test_no_digest_raises(self):
        zone = dns.zone.from_text(ZoneDigestTestCase.simple_example, origin="example")
        zone.delete_rdataset(dns.name.empty, "ZONEMD")
        with self.assertRaises(dns.zone.NoDigest):
            zone.check_digest()

    def test_unprocessable_record_is_not_dropped(self):
        # If an rdata cannot be canonicalized, the failure must be surfaced as
        # a status rather than the record being skipped to force a match.
        zone = self._zone_with_zonemd()

        def boom(self, origin=None):
            raise RuntimeError("cannot canonicalize")

        with mock.patch("dns.rdata.Rdata.to_digestable", boom):
            result = zone.check_digest()
        self.assertEqual(result.records[0].status, DigestStatus.COMPUTATION_ERROR)
        self.assertFalse(result.valid)
        # The coverage walk still counts every record; nothing was dropped.
        self.assertEqual(result.covered_rr_count, 5)

    # ---- verification never mutates the zone ------------------------------

    def test_check_and_verify_do_not_modify_zone(self):
        zone = self._zone_with_zonemd()
        before = zone.to_text()
        record_count = sum(1 for _ in zone.iterate_rdatas())
        zone.check_digest()
        zone.verify_digest()
        self.assertEqual(zone.to_text(), before)
        self.assertEqual(sum(1 for _ in zone.iterate_rdatas()), record_count)

    def test_failed_verification_does_not_modify_zone(self):
        zone = self._zone_with_zonemd()
        before = zone.to_text()
        record_count = sum(1 for _ in zone.iterate_rdatas())
        zonemd = zone.get_rdataset(zone.origin, "ZONEMD")
        tampered = zonemd[0].replace(digest=b"\x02" * 48)
        result = zone.check_digest(tampered)
        self.assertFalse(result.valid)
        with self.assertRaises(dns.zone.DigestVerificationFailure):
            zone.verify_digest(tampered)
        self.assertEqual(zone.to_text(), before)
        self.assertEqual(sum(1 for _ in zone.iterate_rdatas()), record_count)

    # ---- canonical-ordering perturbations do not change the digest --------

    def test_rrset_and_node_order_perturbation_is_invisible(self):
        ordered = self._zone_with_zonemd()
        shuffled = self._zone_with_zonemd(self.shuffled_zone)
        self.assertEqual(
            ordered.compute_digest(dns.zone.DigestHashAlgorithm.SHA384),
            shuffled.compute_digest(dns.zone.DigestHashAlgorithm.SHA384),
        )
        # The shuffled zone's own ZONEMD therefore verifies and reports the
        # same coverage.
        result = shuffled.check_digest()
        self.assertTrue(result.valid)
        self.assertEqual(result.covered_rr_count, 5)

    def test_rdata_order_within_rrset_is_invisible(self):
        two_mx = textwrap.dedent("""
            example. 86400 IN SOA ns1 admin 2018031900 1800 900 604800 86400
            example. 86400 IN NS ns1
            mail 300 IN MX 10 a
            mail 300 IN MX 20 b
        """)
        swapped = textwrap.dedent("""
            example. 86400 IN SOA ns1 admin 2018031900 1800 900 604800 86400
            example. 86400 IN NS ns1
            mail 300 IN MX 20 b
            mail 300 IN MX 10 a
        """)
        z1 = dns.zone.from_text(two_mx, origin="example")
        z2 = dns.zone.from_text(swapped, origin="example")
        self.assertEqual(
            z1.compute_digest(dns.zone.DigestHashAlgorithm.SHA384),
            z2.compute_digest(dns.zone.DigestHashAlgorithm.SHA384),
        )

    def test_name_case_perturbation_is_invisible(self):
        upper = self.small_zone.replace("ns1", "NS1").replace("ns2", "NS2")
        lower_zone = self._zone_with_zonemd()
        upper_zone = self._zone_with_zonemd(upper)
        self.assertEqual(
            lower_zone.compute_digest(dns.zone.DigestHashAlgorithm.SHA384),
            upper_zone.compute_digest(dns.zone.DigestHashAlgorithm.SHA384),
        )
        self.assertTrue(upper_zone.check_digest().valid)

    def test_relative_origin_perturbation_is_invisible(self):
        relative = self._zone_with_zonemd(self.small_zone, relativize=True)
        absolute = self._zone_with_zonemd(self.absolute_zone, relativize=False)
        self.assertEqual(
            relative.compute_digest(dns.zone.DigestHashAlgorithm.SHA384),
            absolute.compute_digest(dns.zone.DigestHashAlgorithm.SHA384),
        )
        self.assertTrue(absolute.check_digest().valid)

    # ---- content perturbations must be detected ---------------------------

    def test_ttl_perturbation_is_detected(self):
        changed = self.small_zone.replace("ns1 3600 IN A", "ns1 9999 IN A")
        good = self._zone_with_zonemd()
        bad = dns.zone.from_text(changed, origin="example")
        self.assertNotEqual(
            good.compute_digest(dns.zone.DigestHashAlgorithm.SHA384),
            bad.compute_digest(dns.zone.DigestHashAlgorithm.SHA384),
        )
        # Reusing the good zone's ZONEMD on the TTL-changed zone must fail.
        good_zonemd = good.get_rdataset(good.origin, "ZONEMD")[0]
        result = bad.check_digest(good_zonemd)
        self.assertEqual(result.records[0].status, DigestStatus.INVALID)
        self.assertFalse(result.valid)

    def test_rdata_perturbation_is_detected(self):
        changed = self.small_zone.replace("203.0.113.63", "203.0.113.64")
        good = self._zone_with_zonemd()
        bad = dns.zone.from_text(changed, origin="example")
        self.assertNotEqual(
            good.compute_digest(dns.zone.DigestHashAlgorithm.SHA384),
            bad.compute_digest(dns.zone.DigestHashAlgorithm.SHA384),
        )
        good_zonemd = good.get_rdataset(good.origin, "ZONEMD")[0]
        with self.assertRaises(dns.zone.DigestVerificationFailure):
            bad.verify_digest(good_zonemd)

    def test_owner_name_perturbation_is_detected(self):
        # Renaming an owner changes the canonical owner name in the digest.
        changed = self.small_zone.replace("ns1 3600", "renamed 3600")
        good = self._zone_with_zonemd()
        bad = dns.zone.from_text(changed, origin="example")
        self.assertNotEqual(
            good.compute_digest(dns.zone.DigestHashAlgorithm.SHA384),
            bad.compute_digest(dns.zone.DigestHashAlgorithm.SHA384),
        )

    # ---- verify_digest stays consistent with check_digest -----------------

    def test_verify_and_check_agree(self):
        zone = self._zone_with_zonemd()
        # Valid: verify returns, check reports valid.
        zone.verify_digest()
        self.assertTrue(zone.check_digest().valid)
        # Invalid: verify raises, check reports invalid.
        zonemd = zone.get_rdataset(zone.origin, "ZONEMD")
        tampered = zonemd[0].replace(digest=b"\x03" * 48)
        with self.assertRaises(dns.zone.DigestVerificationFailure):
            zone.verify_digest(tampered)
        self.assertFalse(zone.check_digest(tampered).valid)

    def test_versioned_zone_supports_summary(self):
        # The summary API is inherited by the transactional versioned Zone.
        zone = dns.zone.from_text(
            self.small_zone,
            origin="example",
            zone_factory=dns.versioned.Zone,
        )
        with zone.writer() as txn:
            zonemd = zone.compute_digest(dns.zone.DigestHashAlgorithm.SHA384)
            txn.replace(dns.name.empty, dns.rdataset.from_rdata(86400, zonemd))
        # Reading does not require a transaction and never mutates the zone.
        result = zone.check_digest()
        self.assertTrue(result.valid)
        self.assertEqual(result.covered_rr_count, 5)
        zone.verify_digest()
        # A versioned zone cannot be mutated outside a writer transaction, so
        # the read-only digest API must not have attempted any mutation.
        with self.assertRaises(dns.versioned.UseTransaction):
            zone.replace_rdataset(
                dns.name.empty,
                dns.rdataset.from_rdata(86400, zonemd),
            )
