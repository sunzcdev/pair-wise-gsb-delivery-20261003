# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

# Permission to use, copy, modify, and distribute this software and its
# documentation for any purpose with or without fee is hereby granted,
# provided that the above copyright notice and this permission notice
# appear in all copies.
#
# THE SOFTWARE IS PROVIDED "AS IS" AND NOMINUM DISCLAIMS ALL WARRANTIES
# WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
# MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL Dnspython BE LIABLE FOR
# ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
# WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
# ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT
# OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

import pytest

import dns.name
import dns.nsecproof
import dns.rdatatype
import dns.rrset
import dns.zone

ORIGIN = dns.name.from_text("example.")


def nsec(owner, next, *types):
    return dns.rrset.from_text(owner, 300, "IN", "NSEC", f"{next} " + " ".join(types))


# A complete in-zone NSEC chain, in canonical order:
#
#   example. -> a.example. -> c.example. -> x.example.
#   -> *.x.example. -> *.z.example. -> example. (wrap-around)
NSEC_EXAMPLE = nsec("example.", "a.example.", "NS", "SOA", "NSEC", "RRSIG")
NSEC_A = nsec("a.example.", "c.example.", "A", "NSEC", "RRSIG")
NSEC_C = nsec("c.example.", "x.example.", "CNAME", "NSEC", "RRSIG")
NSEC_X = nsec("x.example.", "*.x.example.", "A", "NSEC", "RRSIG")
NSEC_WX = nsec("*.x.example.", "*.z.example.", "A", "NSEC", "RRSIG")
NSEC_WZ = nsec("*.z.example.", "example.", "CNAME", "NSEC", "RRSIG")
CHAIN = [NSEC_EXAMPLE, NSEC_A, NSEC_C, NSEC_X, NSEC_WX, NSEC_WZ]


def test_nodata():
    proof = dns.nsecproof.prove("a.example.", "MX", "example.", CHAIN)
    assert proof.result is dns.nsecproof.Result.NODATA
    assert proof.evidence == (NSEC_A,)
    assert proof.evidence[0] is NSEC_A
    assert proof.closest_encloser is None
    assert proof.wildcard is None
    assert "a.example." in proof.reason


def test_nodata_qtype_exists_is_conflict():
    proof = dns.nsecproof.prove("a.example.", "A", "example.", CHAIN)
    assert proof.result is dns.nsecproof.Result.CONFLICT
    assert proof.evidence == ()


def test_nodata_cname_not_ignored():
    # The bitmap at c.example. has no A, but it has CNAME, so a
    # negative answer for A cannot be proven.
    proof = dns.nsecproof.prove("c.example.", "A", "example.", CHAIN)
    assert proof.result is dns.nsecproof.Result.CONFLICT


def test_nodata_qtype_cname():
    # Asking for CNAME at a name whose bitmap has no CNAME is NODATA.
    proof = dns.nsecproof.prove("a.example.", "CNAME", "example.", CHAIN)
    assert proof.result is dns.nsecproof.Result.NODATA


def test_nxdomain():
    proof = dns.nsecproof.prove("b.example.", "A", "example.", CHAIN)
    assert proof.result is dns.nsecproof.Result.NXDOMAIN
    assert proof.evidence == (NSEC_A, NSEC_EXAMPLE)
    assert proof.closest_encloser == ORIGIN
    assert proof.wildcard == dns.name.from_text("*.example.")


def test_nxdomain_wraparound():
    # zz.example. sorts after the last NSEC owner and is covered by
    # the wrap-around interval (*.z.example. -> example.).
    proof = dns.nsecproof.prove("zz.example.", "A", "example.", CHAIN)
    assert proof.result is dns.nsecproof.Result.NXDOMAIN
    assert proof.evidence == (NSEC_WZ, NSEC_EXAMPLE)


def test_nxdomain_deep_name():
    proof = dns.nsecproof.prove("www.a.example.", "A", "example.", CHAIN)
    assert proof.result is dns.nsecproof.Result.NXDOMAIN
    assert proof.closest_encloser == dns.name.from_text("a.example.")
    assert proof.wildcard == dns.name.from_text("*.a.example.")
    # Both the qname and the wildcard are covered by a.example. ->
    # c.example., so a single NSEC is the evidence.
    assert proof.evidence == (NSEC_A,)


def test_nxdomain_closest_encloser_above_parent():
    # foo.example. does not exist; the closest encloser of
    # w.foo.example. is the apex, not the parent.
    proof = dns.nsecproof.prove("w.foo.example.", "A", "example.", CHAIN)
    assert proof.result is dns.nsecproof.Result.NXDOMAIN
    assert proof.closest_encloser == ORIGIN
    assert proof.evidence == (NSEC_C, NSEC_EXAMPLE)


def test_nodata_multiple_bitmap_windows():
    multi = nsec("a.example.", "c.example.", "A", "NSEC", "RRSIG", "TYPE1000")
    proof = dns.nsecproof.prove("a.example.", "TYPE2000", "example.", [multi])
    assert proof.result is dns.nsecproof.Result.NODATA
    proof = dns.nsecproof.prove("a.example.", "TYPE1000", "example.", [multi])
    assert proof.result is dns.nsecproof.Result.CONFLICT


def test_nxdomain_missing_wildcard_proof_is_insufficient():
    proof = dns.nsecproof.prove("b.example.", "A", "example.", [NSEC_A])
    assert proof.result is dns.nsecproof.Result.INSUFFICIENT
    assert proof.evidence == ()
    assert proof.closest_encloser == ORIGIN
    assert proof.wildcard == dns.name.from_text("*.example.")


def test_no_covering_nsec_is_insufficient():
    proof = dns.nsecproof.prove("b.example.", "A", "example.", [])
    assert proof.result is dns.nsecproof.Result.INSUFFICIENT


def test_qname_out_of_zone_is_insufficient():
    proof = dns.nsecproof.prove("www.other.org.", "A", "example.", CHAIN)
    assert proof.result is dns.nsecproof.Result.INSUFFICIENT


def test_out_of_zone_nsec_not_evidence():
    # This out-of-zone NSEC wraps around and would cover b.example.,
    # but out-of-zone records cannot be evidence for this zone.
    evil = nsec("zzz.other.org.", "c.example.", "A", "NSEC", "RRSIG")
    proof = dns.nsecproof.prove("b.example.", "A", "example.", [evil])
    assert proof.result is dns.nsecproof.Result.INSUFFICIENT


def test_next_out_of_zone_not_evidence():
    bad = nsec("a.example.", "zzz.other.org.", "A", "NSEC", "RRSIG")
    proof = dns.nsecproof.prove("b.example.", "A", "example.", [bad])
    assert proof.result is dns.nsecproof.Result.INSUFFICIENT


def test_wildcard_nodata():
    proof = dns.nsecproof.prove("foo.x.example.", "TXT", "example.", CHAIN)
    assert proof.result is dns.nsecproof.Result.NODATA
    assert proof.closest_encloser == dns.name.from_text("x.example.")
    assert proof.wildcard == dns.name.from_text("*.x.example.")
    # The wildcard NSEC both covers the qname and matches the wildcard.
    assert proof.evidence == (NSEC_WX,)


def test_wildcard_qtype_exists_is_conflict():
    proof = dns.nsecproof.prove("foo.x.example.", "A", "example.", CHAIN)
    assert proof.result is dns.nsecproof.Result.CONFLICT


def test_wildcard_cname_not_ignored():
    proof = dns.nsecproof.prove("foo.z.example.", "A", "example.", CHAIN)
    assert proof.result is dns.nsecproof.Result.CONFLICT


def test_inconsistent_chain_is_conflict():
    # example. -> x.example. covers a.example., which exists per
    # NSEC_A; the chain is contradictory.
    bad = nsec("example.", "x.example.", "NS", "SOA", "NSEC", "RRSIG")
    proof = dns.nsecproof.prove("b.example.", "A", "example.", [bad, NSEC_A])
    assert proof.result is dns.nsecproof.Result.CONFLICT


def test_single_name_zone():
    solo = nsec("example.", "example.", "NS", "SOA", "NSEC", "RRSIG")
    proof = dns.nsecproof.prove("foo.example.", "A", "example.", [solo])
    assert proof.result is dns.nsecproof.Result.NXDOMAIN
    assert proof.evidence == (solo,)
    proof = dns.nsecproof.prove("example.", "MX", "example.", [solo])
    assert proof.result is dns.nsecproof.Result.NODATA
    assert proof.evidence == (solo,)


def test_zone_object():
    zone = dns.zone.Zone(ORIGIN)
    proof = dns.nsecproof.prove("a.example.", "MX", zone, CHAIN)
    assert proof.result is dns.nsecproof.Result.NODATA
    assert proof.origin == ORIGIN


def test_string_and_int_inputs():
    proof = dns.nsecproof.prove("b.example.", "A", "example.", CHAIN)
    assert proof.result is dns.nsecproof.Result.NXDOMAIN
    proof = dns.nsecproof.prove("a.example.", 1, "example.", CHAIN)
    assert proof.result is dns.nsecproof.Result.CONFLICT


def test_case_insensitive():
    proof = dns.nsecproof.prove("B.EXAMPLE.", "A", "EXAMPLE.", CHAIN)
    assert proof.result is dns.nsecproof.Result.NXDOMAIN
    assert proof.qname == dns.name.from_text("b.example.")
    assert proof.origin == ORIGIN


def test_relative_qname_raises():
    with pytest.raises(ValueError):
        dns.nsecproof.prove(dns.name.from_text("foo", None), "A", "example.", CHAIN)


def test_non_nsec_rrset_raises():
    a = dns.rrset.from_text("a.example.", 300, "IN", "A", "10.0.0.1")
    with pytest.raises(ValueError):
        dns.nsecproof.prove("b.example.", "A", "example.", [a])


def test_non_rrset_raises():
    with pytest.raises(TypeError):
        dns.nsecproof.prove("b.example.", "A", "example.", ["hello"])


def test_bad_zone_type_raises():
    with pytest.raises(TypeError):
        dns.nsecproof.prove("b.example.", "A", 123, CHAIN)


def test_originless_zone_raises():
    zone = dns.zone.Zone(ORIGIN)
    zone.origin = None
    with pytest.raises(ValueError):
        dns.nsecproof.prove("b.example.", "A", zone, CHAIN)


def test_check():
    proof = dns.nsecproof.prove("b.example.", "A", "example.", CHAIN)
    assert proof.check(CHAIN)
    assert proof.check(proof.evidence)
    # Missing the wildcard evidence changes the recomputed result.
    assert not proof.check([NSEC_A])
    assert not proof.check([])
    # Bad input cannot confirm anything.
    a = dns.rrset.from_text("a.example.", 300, "IN", "A", "10.0.0.1")
    assert not proof.check([a])


def test_check_nodata():
    proof = dns.nsecproof.prove("a.example.", "MX", "example.", CHAIN)
    assert proof.check(CHAIN)
    assert proof.check([NSEC_A])
    assert not proof.check([NSEC_C])


def test_check_bad_input():
    # An INSUFFICIENT proof has no evidence, so checking reaches the
    # recomputation, which rejects the non-NSEC RRset.
    proof = dns.nsecproof.prove("b.example.", "A", "example.", [])
    assert proof.result is dns.nsecproof.Result.INSUFFICIENT
    a = dns.rrset.from_text("a.example.", 300, "IN", "A", "10.0.0.1")
    assert not proof.check([a])
    assert proof.check([])


def test_result_repr():
    assert repr(dns.nsecproof.Result.NXDOMAIN) == "<Result.NXDOMAIN>"


def test_proof_repr():
    proof = dns.nsecproof.prove("a.example.", "MX", "example.", CHAIN)
    assert repr(proof) == "<Proof NODATA qname=a.example. qtype=MX>"
