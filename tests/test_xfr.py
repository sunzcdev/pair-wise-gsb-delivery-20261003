# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

import asyncio
import socket
import struct
import sys
import threading

import pytest

import dns.asyncbackend
import dns.asyncquery
import dns.exception
import dns.flags
import dns.message
import dns.name
import dns.opcode
import dns.query
import dns.rdata
import dns.rdataclass
import dns.rdatatype
import dns.tsigkeyring
import dns.versioned
import dns.xfr
import dns.zone

# Some tests use a "nano nameserver" for testing.  It requires trio
# and threading, so try to import it and if it doesn't work, skip
# those tests.
try:
    from .nanonameserver import Server

    _nanonameserver_available = True
except ImportError:
    _nanonameserver_available = False

    class Server(object):
        pass


axfr = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
@ 3600 IN NS ns2
bar.foo 300 IN MX 0 blaz.foo
ns1 3600 IN A 10.0.0.1
ns2 3600 IN A 10.0.0.2
@ 3600 IN SOA foo bar 1 2 3 4 5
"""

axfr1 = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
@ 3600 IN NS ns2
"""
axfr2 = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;ANSWER
bar.foo 300 IN MX 0 blaz.foo
ns1 3600 IN A 10.0.0.1
ns2 3600 IN A 10.0.0.2
@ 3600 IN SOA foo bar 1 2 3 4 5
"""

base = """@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
@ 3600 IN NS ns2
bar.foo 300 IN MX 0 blaz.foo
ns1 3600 IN A 10.0.0.1
ns2 3600 IN A 10.0.0.2
"""

axfr_unexpected_origin = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN SOA foo bar 1 2 3 4 7
"""

axfr_with_nonsubdomain_glue = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
bar.foo 300 IN MX 0 blaz.foo
ns1 3600 IN A 10.0.0.1
ns2 3600 IN A 10.0.0.2
sub 300 IN NS ns1.other.
ns1.other. 300 IN A 1.2.3.4
@ 3600 IN SOA foo bar 1 2 3 4 5
"""

ixfr = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
@ 3600 IN SOA foo bar 4 2 3 4 5
@ 3600 IN SOA foo bar 1 2 3 4 5
bar.foo 300 IN MX 0 blaz.foo
ns2 3600 IN A 10.0.0.2
@ 3600 IN SOA foo bar 2 2 3 4 5
ns2 3600 IN A 10.0.0.4
@ 3600 IN SOA foo bar 2 2 3 4 5
@ 3600 IN SOA foo bar 3 2 3 4 5
ns3 3600 IN A 10.0.0.3
@ 3600 IN SOA foo bar 3 2 3 4 5
@ 3600 IN NS ns2
@ 3600 IN SOA foo bar 4 2 3 4 5
@ 3600 IN SOA foo bar 4 2 3 4 5
"""

compressed_ixfr = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
@ 3600 IN SOA foo bar 4 2 3 4 5
@ 3600 IN SOA foo bar 1 2 3 4 5
bar.foo 300 IN MX 0 blaz.foo
ns2 3600 IN A 10.0.0.2
@ 3600 IN NS ns2
@ 3600 IN SOA foo bar 4 2 3 4 5
ns2 3600 IN A 10.0.0.4
ns3 3600 IN A 10.0.0.3
@ 3600 IN SOA foo bar 4 2 3 4 5
"""

ixfr_expected = """@ 3600 IN SOA foo bar 4 2 3 4 5
@ 3600 IN NS ns1
ns1 3600 IN A 10.0.0.1
ns2 3600 IN A 10.0.0.4
ns3 3600 IN A 10.0.0.3
"""

ixfr_first_message = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
@ 3600 IN SOA foo bar 4 2 3 4 5
"""

ixfr_header = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;ANSWER
"""

ixfr_body = [
    "@ 3600 IN SOA foo bar 1 2 3 4 5",
    "bar.foo 300 IN MX 0 blaz.foo",
    "ns2 3600 IN A 10.0.0.2",
    "@ 3600 IN SOA foo bar 2 2 3 4 5",
    "ns2 3600 IN A 10.0.0.4",
    "@ 3600 IN SOA foo bar 2 2 3 4 5",
    "@ 3600 IN SOA foo bar 3 2 3 4 5",
    "ns3 3600 IN A 10.0.0.3",
    "@ 3600 IN SOA foo bar 3 2 3 4 5",
    "@ 3600 IN NS ns2",
    "@ 3600 IN SOA foo bar 4 2 3 4 5",
    "@ 3600 IN SOA foo bar 4 2 3 4 5",
]

ixfrs = [ixfr_first_message]
ixfrs.extend([ixfr_header + l for l in ixfr_body])

good_empty_ixfr = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
"""

retry_tcp_ixfr = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
@ 3600 IN SOA foo bar 5 2 3 4 5
"""

bad_empty_ixfr = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
@ 3600 IN SOA foo bar 4 2 3 4 5
@ 3600 IN SOA foo bar 4 2 3 4 5
"""

unexpected_end_ixfr = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
@ 3600 IN SOA foo bar 4 2 3 4 5
@ 3600 IN SOA foo bar 1 2 3 4 5
bar.foo 300 IN MX 0 blaz.foo
ns2 3600 IN A 10.0.0.2
@ 3600 IN NS ns2
@ 3600 IN SOA foo bar 3 2 3 4 5
ns2 3600 IN A 10.0.0.4
ns3 3600 IN A 10.0.0.3
@ 3600 IN SOA foo bar 4 2 3 4 5
"""

unexpected_end_ixfr_2 = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
@ 3600 IN SOA foo bar 4 2 3 4 5
@ 3600 IN SOA foo bar 1 2 3 4 5
bar.foo 300 IN MX 0 blaz.foo
ns2 3600 IN A 10.0.0.2
@ 3600 IN NS ns2
"""

bad_serial_ixfr = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
@ 3600 IN SOA foo bar 4 2 3 4 5
@ 3600 IN SOA foo bar 2 2 3 4 5
bar.foo 300 IN MX 0 blaz.foo
ns2 3600 IN A 10.0.0.2
@ 3600 IN NS ns2
@ 3600 IN SOA foo bar 4 2 3 4 5
ns2 3600 IN A 10.0.0.4
ns3 3600 IN A 10.0.0.3
@ 3600 IN SOA foo bar 4 2 3 4 5
"""

ixfr_axfr = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
@ 3600 IN NS ns2
bar.foo 300 IN MX 0 blaz.foo
ns1 3600 IN A 10.0.0.1
ns2 3600 IN A 10.0.0.2
@ 3600 IN SOA foo bar 1 2 3 4 5
"""

ixfr_axfr1 = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
@ 3600 IN NS ns2
"""

ixfr_axfr2 = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
bar.foo 300 IN MX 0 blaz.foo
ns1 3600 IN A 10.0.0.1
ns2 3600 IN A 10.0.0.2
@ 3600 IN SOA foo bar 1 2 3 4 5
"""


def test_basic_axfr():
    z = dns.versioned.Zone("example.")
    m = dns.message.from_text(axfr, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        done = xfr.process_message(m)
        assert done
    ez = dns.zone.from_text(base, "example.")
    assert z == ez


def test_basic_axfr_unversioned():
    z = dns.zone.Zone("example.")
    m = dns.message.from_text(axfr, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        done = xfr.process_message(m)
        assert done
    ez = dns.zone.from_text(base, "example.")
    assert z == ez


def test_basic_axfr_two_parts():
    z = dns.versioned.Zone("example.")
    m1 = dns.message.from_text(axfr1, origin=z.origin, one_rr_per_rrset=True)
    m2 = dns.message.from_text(axfr2, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        done = xfr.process_message(m1)
        assert not done
        done = xfr.process_message(m2)
        assert done
    ez = dns.zone.from_text(base, "example.")
    assert z == ez


def test_axfr_unexpected_origin():
    z = dns.versioned.Zone("example.")
    m = dns.message.from_text(
        axfr_unexpected_origin, origin=z.origin, one_rr_per_rrset=True
    )
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        with pytest.raises(dns.exception.FormError):
            xfr.process_message(m)


def test_axfr_with_non_subdomain_glue():
    z = dns.versioned.Zone("example.")
    m = dns.message.from_text(
        axfr_with_nonsubdomain_glue, origin=z.origin, one_rr_per_rrset=True
    )
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        xfr.process_message(m)


def test_basic_ixfr():
    z = dns.zone.from_text(base, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(ixfr, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1) as xfr:
        done = xfr.process_message(m)
        assert done
    ez = dns.zone.from_text(ixfr_expected, "example.")
    assert z == ez


def test_basic_ixfr_unversioned():
    z = dns.zone.from_text(base, "example.")
    m = dns.message.from_text(ixfr, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1) as xfr:
        done = xfr.process_message(m)
        assert done
    ez = dns.zone.from_text(ixfr_expected, "example.")
    assert z == ez


def test_compressed_ixfr():
    z = dns.zone.from_text(base, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(compressed_ixfr, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1) as xfr:
        done = xfr.process_message(m)
        assert done
    ez = dns.zone.from_text(ixfr_expected, "example.")
    assert z == ez


def test_basic_ixfr_many_parts():
    z = dns.zone.from_text(base, "example.", zone_factory=dns.versioned.Zone)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1) as xfr:
        done = False
        for text in ixfrs:
            assert not done
            m = dns.message.from_text(text, origin=z.origin, one_rr_per_rrset=True)
            done = xfr.process_message(m)
        assert done
    ez = dns.zone.from_text(ixfr_expected, "example.")
    assert z == ez


def test_good_empty_ixfr():
    z = dns.zone.from_text(ixfr_expected, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(good_empty_ixfr, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1) as xfr:
        done = xfr.process_message(m)
        assert done
    ez = dns.zone.from_text(ixfr_expected, "example.")
    assert z == ez


def test_retry_tcp_ixfr():
    z = dns.zone.from_text(ixfr_expected, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(retry_tcp_ixfr, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1, is_udp=True) as xfr:
        with pytest.raises(dns.xfr.UseTCP):
            xfr.process_message(m)


def test_bad_empty_ixfr():
    z = dns.zone.from_text(ixfr_expected, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(bad_empty_ixfr, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=3) as xfr:
        with pytest.raises(dns.exception.FormError):
            xfr.process_message(m)


def test_serial_went_backwards_ixfr():
    z = dns.zone.from_text(ixfr_expected, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(bad_empty_ixfr, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=5) as xfr:
        with pytest.raises(dns.xfr.SerialWentBackwards):
            xfr.process_message(m)


def test_ixfr_is_axfr():
    z = dns.zone.from_text(base, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(ixfr_axfr, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=0xFFFFFFFF) as xfr:
        done = xfr.process_message(m)
        assert done
    ez = dns.zone.from_text(base, "example.")
    assert z == ez


def test_ixfr_is_axfr_two_parts():
    z = dns.versioned.Zone("example.")
    m1 = dns.message.from_text(ixfr_axfr1, origin=z.origin, one_rr_per_rrset=True)
    m2 = dns.message.from_text(ixfr_axfr2, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=0xFFFFFFFF) as xfr:
        done = xfr.process_message(m1)
        assert not done
        done = xfr.process_message(m2)
        assert done
    ez = dns.zone.from_text(base, "example.")
    assert z == ez


def test_ixfr_requires_serial():
    z = dns.zone.from_text(base, "example.", zone_factory=dns.versioned.Zone)
    with pytest.raises(ValueError):
        dns.xfr.Inbound(z, dns.rdatatype.IXFR)


def test_ixfr_unexpected_end_bad_diff_sequence():
    # This is where we get the end serial, but haven't seen all of
    # the expected diffs
    z = dns.zone.from_text(base, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(
        unexpected_end_ixfr, origin=z.origin, one_rr_per_rrset=True
    )
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1) as xfr:
        with pytest.raises(dns.exception.FormError):
            xfr.process_message(m)


def test_udp_ixfr_unexpected_end_just_stops():
    # This is where everything looks good, but the IXFR just stops
    # in the middle.
    z = dns.zone.from_text(base, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(
        unexpected_end_ixfr_2, origin=z.origin, one_rr_per_rrset=True
    )
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1, is_udp=True) as xfr:
        with pytest.raises(dns.exception.FormError):
            xfr.process_message(m)


def test_ixfr_bad_serial():
    z = dns.zone.from_text(base, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(bad_serial_ixfr, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1) as xfr:
        with pytest.raises(dns.exception.FormError):
            xfr.process_message(m)


def test_no_udp_with_axfr():
    z = dns.versioned.Zone("example.")
    with pytest.raises(ValueError):
        with dns.xfr.Inbound(z, dns.rdatatype.AXFR, is_udp=True) as xfr:
            pass


refused = """id 1
opcode QUERY
rcode REFUSED
flags QR AA
;QUESTION
example. IN AXFR
"""

bad_qname = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
not-example. IN IXFR
"""

bad_qtype = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
"""

soa_not_first = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
bar.foo 300 IN MX 0 blaz.foo
"""

soa_not_first_2 = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
@ 300 IN MX 0 blaz.foo
"""

no_answer = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ADDITIONAL
bar.foo 300 IN MX 0 blaz.foo
"""

axfr_answers_after_final_soa = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
@ 3600 IN NS ns2
bar.foo 300 IN MX 0 blaz.foo
ns1 3600 IN A 10.0.0.1
ns2 3600 IN A 10.0.0.2
@ 3600 IN SOA foo bar 1 2 3 4 5
ns3 3600 IN A 10.0.0.3
"""


def test_refused():
    z = dns.zone.from_text(base, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(refused, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1) as xfr:
        with pytest.raises(dns.xfr.TransferError):
            xfr.process_message(m)


def test_bad_qname():
    z = dns.zone.from_text(base, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(bad_qname, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1) as xfr:
        with pytest.raises(dns.exception.FormError):
            xfr.process_message(m)


def test_bad_qtype():
    z = dns.zone.from_text(base, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(bad_qtype, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1) as xfr:
        with pytest.raises(dns.exception.FormError):
            xfr.process_message(m)


def test_soa_not_first():
    z = dns.zone.from_text(base, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(soa_not_first, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1) as xfr:
        with pytest.raises(dns.exception.FormError):
            xfr.process_message(m)
    m = dns.message.from_text(soa_not_first_2, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1) as xfr:
        with pytest.raises(dns.exception.FormError):
            xfr.process_message(m)


def test_no_answer():
    z = dns.zone.from_text(base, "example.", zone_factory=dns.versioned.Zone)
    m = dns.message.from_text(no_answer, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.IXFR, serial=1) as xfr:
        with pytest.raises(dns.exception.FormError):
            xfr.process_message(m)


def test_axfr_answers_after_final_soa():
    z = dns.versioned.Zone("example.")
    m = dns.message.from_text(
        axfr_answers_after_final_soa, origin=z.origin, one_rr_per_rrset=True
    )
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        with pytest.raises(dns.exception.FormError):
            xfr.process_message(m)


keyring = dns.tsigkeyring.from_text({"keyname.": "NjHwPsMKjdN++dOfE5iAiQ=="})

keyname = dns.name.from_text("keyname")


def test_make_query_basic():
    z = dns.versioned.Zone("example.")
    q, s = dns.xfr.make_query(z)
    assert q.question[0].rdtype == dns.rdatatype.AXFR
    assert s is None
    q, s = dns.xfr.make_query(z, serial=None)
    assert q.question[0].rdtype == dns.rdatatype.AXFR
    assert s is None
    q, s = dns.xfr.make_query(z, serial=10)
    assert q.question[0].rdtype == dns.rdatatype.IXFR
    assert q.authority[0].rdtype == dns.rdatatype.SOA
    assert q.authority[0][0].serial == 10
    assert s == 10
    with z.writer() as txn:
        txn.add("@", 300, dns.rdata.from_text("in", "soa", ". . 1 2 3 4 5"))
    q, s = dns.xfr.make_query(z)
    assert q.question[0].rdtype == dns.rdatatype.IXFR
    assert q.authority[0].rdtype == dns.rdatatype.SOA
    assert q.authority[0][0].serial == 1
    assert s == 1
    q, s = dns.xfr.make_query(z, keyring=keyring, keyname=keyname)
    assert q.question[0].rdtype == dns.rdatatype.IXFR
    assert q.authority[0].rdtype == dns.rdatatype.SOA
    assert q.authority[0][0].serial == 1
    assert s == 1
    assert q.keyname == keyname


def test_make_query_bad_serial():
    z = dns.versioned.Zone("example.")
    with pytest.raises(ValueError):
        dns.xfr.make_query(z, serial="hi")
    with pytest.raises(ValueError):
        dns.xfr.make_query(z, serial=-1)
    with pytest.raises(ValueError):
        dns.xfr.make_query(z, serial=4294967296)


def test_extract_serial_from_query():
    z = dns.versioned.Zone("example.")
    q, s = dns.xfr.make_query(z)
    xs = dns.xfr.extract_serial_from_query(q)
    assert s is None
    assert s == xs
    q, s = dns.xfr.make_query(z, serial=10)
    xs = dns.xfr.extract_serial_from_query(q)
    assert s == 10
    assert s == xs
    q = dns.message.make_query("example", "a")
    with pytest.raises(ValueError):
        dns.xfr.extract_serial_from_query(q)


class XFRNanoNameserver(Server):
    def __init__(self):
        super().__init__(origin=dns.name.from_text("example"))

    def handle(self, request):
        try:
            if request.message.question[0].rdtype == dns.rdatatype.IXFR:
                text = ixfr
            else:
                text = axfr
            r = dns.message.from_text(text, one_rr_per_rrset=True, origin=self.origin)
            r.id = request.message.id
            return r
        except Exception:
            pass


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
def test_sync_inbound_xfr():
    with XFRNanoNameserver() as ns:
        zone = dns.versioned.Zone("example")
        dns.query.inbound_xfr(
            ns.tcp_address[0],
            zone,
            port=ns.tcp_address[1],
            udp_mode=dns.query.UDPMode.TRY_FIRST,
        )
        dns.query.inbound_xfr(
            ns.tcp_address[0],
            zone,
            port=ns.tcp_address[1],
            udp_mode=dns.query.UDPMode.TRY_FIRST,
        )
        expected = dns.zone.from_text(ixfr_expected, "example")
        assert zone == expected


async def async_inbound_xfr():
    with XFRNanoNameserver() as ns:
        zone = dns.versioned.Zone("example")
        await dns.asyncquery.inbound_xfr(
            ns.tcp_address[0],
            zone,
            port=ns.tcp_address[1],
            udp_mode=dns.query.UDPMode.TRY_FIRST,
        )
        await dns.asyncquery.inbound_xfr(
            ns.tcp_address[0],
            zone,
            port=ns.tcp_address[1],
            udp_mode=dns.query.UDPMode.TRY_FIRST,
        )
        expected = dns.zone.from_text(ixfr_expected, "example")
        assert zone == expected


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
def test_asyncio_inbound_xfr():
    dns.asyncbackend.set_default_backend("asyncio")

    async def run():
        await async_inbound_xfr()

    asyncio.run(run())


#
# We don't need to do this as it's all generic code, but
# just for extra caution we do it for each backend.
#

try:
    import trio

    @pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
    def test_trio_inbound_xfr():
        dns.asyncbackend.set_default_backend("trio")

        async def run():
            await async_inbound_xfr()

        trio.run(run)

except ImportError:
    pass


class UDPXFRNanoNameserver(Server):
    def __init__(self):
        super().__init__(origin=dns.name.from_text("example"))
        self.did_truncation = False

    def handle(self, request):
        try:
            if request.message.question[0].rdtype == dns.rdatatype.IXFR:
                if self.did_truncation:
                    text = ixfr
                else:
                    text = retry_tcp_ixfr
                    self.did_truncation = True
            else:
                text = axfr
            r = dns.message.from_text(text, one_rr_per_rrset=True, origin=self.origin)
            r.id = request.message.id
            return r
        except Exception:
            pass


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
def test_sync_retry_tcp_inbound_xfr():
    with UDPXFRNanoNameserver() as ns:
        zone = dns.versioned.Zone("example")
        dns.query.inbound_xfr(
            ns.tcp_address[0],
            zone,
            port=ns.tcp_address[1],
            udp_mode=dns.query.UDPMode.TRY_FIRST,
        )
        dns.query.inbound_xfr(
            ns.tcp_address[0],
            zone,
            port=ns.tcp_address[1],
            udp_mode=dns.query.UDPMode.TRY_FIRST,
        )
        expected = dns.zone.from_text(ixfr_expected, "example")
        assert zone == expected


async def udp_async_inbound_xfr():
    with UDPXFRNanoNameserver() as ns:
        zone = dns.versioned.Zone("example")
        await dns.asyncquery.inbound_xfr(
            ns.tcp_address[0],
            zone,
            port=ns.tcp_address[1],
            udp_mode=dns.query.UDPMode.TRY_FIRST,
        )
        await dns.asyncquery.inbound_xfr(
            ns.tcp_address[0],
            zone,
            port=ns.tcp_address[1],
            udp_mode=dns.query.UDPMode.TRY_FIRST,
        )
        expected = dns.zone.from_text(ixfr_expected, "example")
        assert zone == expected


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
def test_asyncio_retry_tcp_inbound_xfr():
    dns.asyncbackend.set_default_backend("asyncio")

    async def run():
        await udp_async_inbound_xfr()

    try:
        runner = asyncio.run
    except AttributeError:

        def old_runner(awaitable):
            loop = asyncio.get_event_loop()
            return loop.run_until_complete(awaitable)

        runner = old_runner
    runner(run())


#
# The master's serial is older than ours, in a single-message IXFR response.
#
went_backwards_ixfr = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN IXFR
;ANSWER
@ 3600 IN SOA foo bar 5 2 3 4 5
"""

# Newer than what went_backwards_ixfr offers, so an IXFR from it goes backwards.
went_backwards_base = """@ 3600 IN SOA foo bar 6 2 3 4 5
@ 3600 IN NS ns1
"""


class WentBackwardsNanoNameserver(Server):
    def __init__(self):
        super().__init__(origin=dns.name.from_text("example"))

    def handle(self, request):
        try:
            r = dns.message.from_text(
                went_backwards_ixfr, one_rr_per_rrset=True, origin=self.origin
            )
            r.id = request.message.id
            return r
        except Exception:
            pass


def went_backwards_zone():
    return dns.zone.from_text(
        went_backwards_base, "example", zone_factory=dns.versioned.Zone
    )


def check_went_backwards_zone(zone):
    # Declining to raise ends the transfer without applying anything, so the
    # zone must be untouched.
    assert zone == dns.zone.from_text(went_backwards_base, "example")


#
# raise_on_serial_went_backwards has to reach dns.xfr.Inbound on both the UDP
# and the TCP path.  UDPMode.NEVER is TCP only; TRY_FIRST sends the IXFR over
# UDP first.
#
udp_modes = [dns.query.UDPMode.NEVER, dns.query.UDPMode.TRY_FIRST]


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
@pytest.mark.parametrize("udp_mode", udp_modes)
def test_sync_inbound_xfr_serial_went_backwards(udp_mode):
    with WentBackwardsNanoNameserver() as ns:
        where, port = ns.tcp_address
        with pytest.raises(dns.xfr.SerialWentBackwards):
            dns.query.inbound_xfr(
                where, went_backwards_zone(), port=port, udp_mode=udp_mode
            )
        zone = went_backwards_zone()
        dns.query.inbound_xfr(
            where,
            zone,
            port=port,
            udp_mode=udp_mode,
            raise_on_serial_went_backwards=False,
        )
        check_went_backwards_zone(zone)


async def async_inbound_xfr_serial_went_backwards(udp_mode):
    with WentBackwardsNanoNameserver() as ns:
        where, port = ns.tcp_address
        with pytest.raises(dns.xfr.SerialWentBackwards):
            await dns.asyncquery.inbound_xfr(
                where, went_backwards_zone(), port=port, udp_mode=udp_mode
            )
        zone = went_backwards_zone()
        await dns.asyncquery.inbound_xfr(
            where,
            zone,
            port=port,
            udp_mode=udp_mode,
            raise_on_serial_went_backwards=False,
        )
        check_went_backwards_zone(zone)


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
@pytest.mark.parametrize("udp_mode", udp_modes)
def test_asyncio_inbound_xfr_serial_went_backwards(udp_mode):
    dns.asyncbackend.set_default_backend("asyncio")

    async def run():
        await async_inbound_xfr_serial_went_backwards(udp_mode)

    asyncio.run(run())


try:
    import trio

    @pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
    @pytest.mark.parametrize("udp_mode", udp_modes)
    def test_trio_inbound_xfr_serial_went_backwards(udp_mode):
        dns.asyncbackend.set_default_backend("trio")

        async def run():
            await async_inbound_xfr_serial_went_backwards(udp_mode)

        trio.run(run)

except ImportError:
    pass


#
# If the request is TSIG-signed, the first and the last message of the response
# must be signed too (RFC 8945 section 5.3.1), and a transfer which is not must
# be rejected before anything is committed to the zone.
#
class TSIGXFRNanoNameserver(Server):
    def __init__(self, signed):
        super().__init__(origin=dns.name.from_text("example"), keyring=keyring)
        # Whether to sign each of the two messages of the response.
        self.signed = signed

    def handle(self, request):
        try:
            items = []
            for text, signed in zip((axfr1, axfr2), self.signed):
                r = dns.message.from_text(
                    text, one_rr_per_rrset=True, origin=self.origin
                )
                r.id = request.message.id
                if signed:
                    r.use_tsig(keyring, keyname)
                    r.request_mac = request.message.mac
                items.append(r)
            return items
        except Exception:
            pass


tsig_xfr_signed = [(True, True), (False, False), (False, True), (True, False)]


def tsig_xfr_zone_and_query():
    zone = dns.versioned.Zone("example")
    query, _ = dns.xfr.make_query(zone, keyring=keyring, keyname=keyname)
    return zone, query


def check_tsig_xfr_zone(zone, signed):
    if all(signed):
        assert zone == dns.zone.from_text(base, "example")
    else:
        # The zone must be untouched.
        assert zone == dns.versioned.Zone("example")


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
@pytest.mark.parametrize("signed", tsig_xfr_signed)
def test_sync_inbound_xfr_tsig(signed):
    with TSIGXFRNanoNameserver(signed) as ns:
        where, port = ns.tcp_address
        zone, query = tsig_xfr_zone_and_query()
        if all(signed):
            dns.query.inbound_xfr(where, zone, query, port=port)
        else:
            with pytest.raises(dns.exception.FormError):
                dns.query.inbound_xfr(where, zone, query, port=port)
        check_tsig_xfr_zone(zone, signed)


async def async_inbound_xfr_tsig(signed):
    with TSIGXFRNanoNameserver(signed) as ns:
        where, port = ns.tcp_address
        zone, query = tsig_xfr_zone_and_query()
        if all(signed):
            await dns.asyncquery.inbound_xfr(where, zone, query, port=port)
        else:
            with pytest.raises(dns.exception.FormError):
                await dns.asyncquery.inbound_xfr(where, zone, query, port=port)
        check_tsig_xfr_zone(zone, signed)


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
@pytest.mark.parametrize("signed", tsig_xfr_signed)
def test_asyncio_inbound_xfr_tsig(signed):
    dns.asyncbackend.set_default_backend("asyncio")

    async def run():
        await async_inbound_xfr_tsig(signed)

    asyncio.run(run())


try:
    import trio

    @pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
    @pytest.mark.parametrize("signed", tsig_xfr_signed)
    def test_trio_inbound_xfr_tsig(signed):
        dns.asyncbackend.set_default_backend("trio")

        async def run():
            await async_inbound_xfr_tsig(signed)

        trio.run(run)

except ImportError:
    pass


#
# Every message of the response must pass is_response() against the request
# (RFC 5936 section 2.2.1), otherwise a spoofed UDP IXFR response only has to
# hit the source port to be applied to the zone.
#
def _wrong_id(r):
    r.id ^= 1


def _no_qr(r):
    r.flags &= ~dns.flags.QR


def _wrong_opcode(r):
    r.set_opcode(dns.opcode.NOTIFY)


bad_responses = [_wrong_id, _no_qr, _wrong_opcode]


class BadResponseXFRNanoNameserver(Server):
    def __init__(self, mutate):
        super().__init__(origin=dns.name.from_text("example"))
        self.mutate = mutate

    def handle(self, request):
        try:
            r = dns.message.from_text(ixfr, one_rr_per_rrset=True, origin=self.origin)
            r.id = request.message.id
            self.mutate(r)
            return r
        except Exception:
            pass


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
@pytest.mark.parametrize("udp_mode", udp_modes)
@pytest.mark.parametrize("mutate", bad_responses)
def test_sync_inbound_xfr_bad_response(udp_mode, mutate):
    with BadResponseXFRNanoNameserver(mutate) as ns:
        where, port = ns.tcp_address
        zone = dns.zone.from_text(base, "example", zone_factory=dns.versioned.Zone)
        with pytest.raises(dns.query.BadResponse):
            dns.query.inbound_xfr(where, zone, port=port, udp_mode=udp_mode)
        assert zone == dns.zone.from_text(base, "example")


async def async_inbound_xfr_bad_response(udp_mode, mutate):
    with BadResponseXFRNanoNameserver(mutate) as ns:
        where, port = ns.tcp_address
        zone = dns.zone.from_text(base, "example", zone_factory=dns.versioned.Zone)
        with pytest.raises(dns.query.BadResponse):
            await dns.asyncquery.inbound_xfr(where, zone, port=port, udp_mode=udp_mode)
        assert zone == dns.zone.from_text(base, "example")


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
@pytest.mark.parametrize("udp_mode", udp_modes)
@pytest.mark.parametrize("mutate", bad_responses)
def test_asyncio_inbound_xfr_bad_response(udp_mode, mutate):
    dns.asyncbackend.set_default_backend("asyncio")

    async def run():
        await async_inbound_xfr_bad_response(udp_mode, mutate)

    asyncio.run(run())


try:
    import trio

    @pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
    @pytest.mark.parametrize("udp_mode", udp_modes)
    @pytest.mark.parametrize("mutate", bad_responses)
    def test_trio_inbound_xfr_bad_response(udp_mode, mutate):
        dns.asyncbackend.set_default_backend("trio")

        async def run():
            await async_inbound_xfr_bad_response(udp_mode, mutate)

        trio.run(run)

except ImportError:
    pass


#
# Messages after the first one may have an empty question section (RFC 5936
# section 2.2.1), and that is still accepted.
#
axfr_part1 = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
@ 3600 IN NS ns2
"""

axfr_part2 = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;ANSWER
bar.foo 300 IN MX 0 blaz.foo
ns1 3600 IN A 10.0.0.1
ns2 3600 IN A 10.0.0.2
@ 3600 IN SOA foo bar 1 2 3 4 5
"""


class EmptyQuestionXFRNanoNameserver(Server):
    def __init__(self, first_empty=False):
        super().__init__(origin=dns.name.from_text("example"))
        self.first_empty = first_empty

    def handle(self, request):
        try:
            messages = [
                dns.message.from_text(text, one_rr_per_rrset=True, origin=self.origin)
                for text in (axfr_part1, axfr_part2)
            ]
            for r in messages:
                r.id = request.message.id
            if self.first_empty:
                messages[0].question = []
            return messages
        except Exception:
            pass


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
def test_sync_inbound_xfr_empty_question():
    with EmptyQuestionXFRNanoNameserver() as ns:
        where, port = ns.tcp_address
        zone = dns.versioned.Zone("example")
        dns.query.inbound_xfr(where, zone, port=port)
        assert zone == dns.zone.from_text(base, "example")
    with EmptyQuestionXFRNanoNameserver(first_empty=True) as ns:
        where, port = ns.tcp_address
        zone = dns.versioned.Zone("example")
        with pytest.raises(dns.query.BadResponse):
            dns.query.inbound_xfr(where, zone, port=port)


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
def test_asyncio_inbound_xfr_empty_question():
    dns.asyncbackend.set_default_backend("asyncio")

    async def run():
        with EmptyQuestionXFRNanoNameserver() as ns:
            where, port = ns.tcp_address
            zone = dns.versioned.Zone("example")
            await dns.asyncquery.inbound_xfr(where, zone, port=port)
            assert zone == dns.zone.from_text(base, "example")
        with EmptyQuestionXFRNanoNameserver(first_empty=True) as ns:
            where, port = ns.tcp_address
            zone = dns.versioned.Zone("example")
            with pytest.raises(dns.query.BadResponse):
                await dns.asyncquery.inbound_xfr(where, zone, port=port)

    asyncio.run(run())


#
# --- Incremental acceptance of a multi-message AXFR -----------------------
#
# The tests below feed dns.xfr.Inbound a sequence of messages, without any
# network I/O, and check the receiving state, the commit boundary, and the
# rejection of malformed streams.  Nothing here opens a socket.
#

progressive_part1 = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
@ 3600 IN NS ns2
"""

progressive_part2 = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;ANSWER
bar.foo 300 IN MX 0 blaz.foo
ns1 3600 IN A 10.0.0.1
"""

progressive_part3 = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;ANSWER
ns2 3600 IN A 10.0.0.2
@ 3600 IN SOA foo bar 1 2 3 4 5
"""


def _messages(origin, *texts):
    return [
        dns.message.from_text(t, origin=origin, one_rr_per_rrset=True) for t in texts
    ]


def _zone_serial(zone):
    with zone.reader() as txn:
        rds = txn.get(dns.name.empty, "SOA")
        assert rds is not None
        return rds[0].serial


def _zone_names(zone):
    with zone.reader() as txn:
        return {name for name in txn.iterate_names()}


def test_progressive_axfr_phases_and_commit_boundary():
    z = dns.versioned.Zone("example.")
    m1, m2, m3 = _messages(
        z.origin, progressive_part1, progressive_part2, progressive_part3
    )
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        assert xfr.phase == dns.xfr.InboundPhase.START_SOA
        assert not xfr.complete
        assert not xfr.committed

        assert xfr.process_message(m1) is False
        assert xfr.phase == dns.xfr.InboundPhase.DATA
        assert not xfr.complete
        # The opening SOA and the two apex NS records have been seen.
        assert xfr.rrset_count == 3

        assert xfr.process_message(m2) is False
        assert xfr.phase == dns.xfr.InboundPhase.DATA
        assert xfr.rrset_count == 5
        assert not xfr.committed

        assert xfr.process_message(m3) is True
        assert xfr.phase == dns.xfr.InboundPhase.DONE
        assert xfr.complete
        assert xfr.committed
        assert xfr.serial_number == 1
        # Every stage is entered, in order, exactly once.
        assert xfr.history == [
            dns.xfr.InboundPhase.START_SOA,
            dns.xfr.InboundPhase.ZONE_IDENTITY,
            dns.xfr.InboundPhase.DATA,
            dns.xfr.InboundPhase.END_SOA,
            dns.xfr.InboundPhase.DONE,
        ]
    assert z == dns.zone.from_text(base, "example.")


def test_zone_identity_phase_before_any_data():
    z = dns.versioned.Zone("example.")
    m = dns.message.from_text(
        """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
""",
        origin=z.origin,
        one_rr_per_rrset=True,
    )
    xfr = dns.xfr.Inbound(z, dns.rdatatype.AXFR)
    assert xfr.process_message(m) is False
    # Only the opening SOA has arrived: the zone identity is known, but no
    # zone data has been accepted yet.
    assert xfr.phase == dns.xfr.InboundPhase.ZONE_IDENTITY
    assert xfr.history == [
        dns.xfr.InboundPhase.START_SOA,
        dns.xfr.InboundPhase.ZONE_IDENTITY,
    ]
    assert not xfr.committed
    with z.reader() as txn:
        assert txn.get(dns.name.empty, "SOA") is None
    xfr.__exit__(None, None, None)
    assert z == dns.versioned.Zone("example.")


def test_reader_sees_old_version_until_close():
    old = dns.zone.from_text(base, "example.")
    z = dns.versioned.Zone("example.")
    with z.writer() as txn:
        txn.add(
            "@", 3600, dns.rdata.from_text("IN", "SOA", "foo bar 1 2 3 4 5")
        )
    reader = z.reader()
    before = set(reader.iterate_names())
    m1, m2, m3 = _messages(
        z.origin, progressive_part1, progressive_part2, progressive_part3
    )
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        xfr.process_message(m1)
        xfr.process_message(m2)
        # The transfer has not closed, so the open reader still sees the old
        # version, and so does a fresh reader.
        assert set(reader.iterate_names()) == before
        with z.reader() as txn2:
            assert set(txn2.iterate_names()) == before
        xfr.process_message(m3)
        assert xfr.complete
        # A reader opened before the commit keeps the version it started with.
        assert set(reader.iterate_names()) == before
    reader.rollback()
    # A new reader now sees the committed version.
    with z.reader() as txn3:
        assert set(txn3.iterate_names()) == {
            dns.name.empty,
            dns.name.from_text("ns1", None),
            dns.name.from_text("ns2", None),
            dns.name.from_text("bar.foo", None),
        }
    assert z == dns.zone.from_text(base, "example.")


ns1 = dns.name.from_text("ns1", None)
ns2 = dns.name.from_text("ns2", None)
barfoo = dns.name.from_text("bar.foo", None)


def test_duplicate_closing_soa_in_same_message():
    z = dns.versioned.Zone("example.")
    m = dns.message.from_text(axfr, origin=z.origin, one_rr_per_rrset=True)
    soa = m.answer[-1]
    m.answer.append(soa)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        with pytest.raises(dns.xfr.DuplicateEnd):
            xfr.process_message(m)
        assert xfr.phase == dns.xfr.InboundPhase.FAILED
        assert xfr.failed
        assert not xfr.complete
        assert not xfr.committed
    # The zone was not partly replaced.
    assert z == dns.versioned.Zone("example.")


def test_message_after_closing_soa():
    z = dns.versioned.Zone("example.")
    m1 = dns.message.from_text(axfr, origin=z.origin, one_rr_per_rrset=True)
    m2 = dns.message.from_text(progressive_part3, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        assert xfr.process_message(m1) is True
        with pytest.raises(dns.xfr.DuplicateEnd):
            xfr.process_message(m2)
        # The transfer had already closed successfully; the extra message does
        # not undo the commit.
        assert xfr.committed
    assert z == dns.zone.from_text(base, "example.")


def test_message_after_closing_soa_can_be_ignored():
    z = dns.versioned.Zone("example.")
    m1 = dns.message.from_text(axfr, origin=z.origin, one_rr_per_rrset=True)
    m2 = dns.message.from_text(progressive_part3, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(
        z, dns.rdatatype.AXFR, raise_on_duplicate_end=False
    ) as xfr:
        assert xfr.process_message(m1) is True
        assert xfr.process_message(m2) is True
        assert xfr.complete
    assert z == dns.zone.from_text(base, "example.")


def test_answers_after_final_soa_rejected_without_partial_zone():
    z = dns.versioned.Zone("example.")
    m = dns.message.from_text(
        axfr_answers_after_final_soa, origin=z.origin, one_rr_per_rrset=True
    )
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        with pytest.raises(dns.xfr.DuplicateEnd):
            xfr.process_message(m)
        assert xfr.failed
    assert z == dns.versioned.Zone("example.")


def test_out_of_zone_name_rejected():
    z = dns.versioned.Zone("example.")
    text = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
evil.other. 300 IN A 10.9.9.9
@ 3600 IN SOA foo bar 1 2 3 4 5
"""
    m = dns.message.from_text(text, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        with pytest.raises(dns.xfr.OutOfZoneName) as excinfo:
            xfr.process_message(m)
        # OutOfZoneName is a ZoneIdentityError and a FormError.
        assert isinstance(excinfo.value, dns.xfr.ZoneIdentityError)
        assert isinstance(excinfo.value, dns.exception.FormError)
        assert xfr.failed
    assert z == dns.versioned.Zone("example.")


def test_out_of_zone_name_can_be_allowed():
    z = dns.versioned.Zone("example.")
    text = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
evil.other. 300 IN A 10.9.9.9
@ 3600 IN SOA foo bar 1 2 3 4 5
"""
    m = dns.message.from_text(text, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR, allow_out_of_zone=True) as xfr:
        assert xfr.process_message(m) is True
    ez = dns.zone.from_text(
        """@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
""",
        "example.",
    )
    assert z == ez


def test_wrong_class_rejected():
    z = dns.versioned.Zone("example.")
    text = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
foo 300 CH TXT "wrong"
@ 3600 IN SOA foo bar 1 2 3 4 5
"""
    m = dns.message.from_text(text, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        with pytest.raises(dns.xfr.ClassMismatch) as excinfo:
            xfr.process_message(m)
        assert isinstance(excinfo.value, dns.xfr.ZoneIdentityError)
        assert xfr.failed
    assert z == dns.versioned.Zone("example.")


def test_wrong_class_ch_zone():
    # The check is against the manager's class, not against IN specifically.
    z = dns.versioned.Zone("example.", dns.rdataclass.CH)
    text = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. CH AXFR
;ANSWER
@ 3600 CH SOA foo bar 1 2 3 4 5
@ 3600 IN A 10.0.0.1
@ 3600 CH SOA foo bar 1 2 3 4 5
"""
    m = dns.message.from_text(text, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        with pytest.raises(dns.xfr.ClassMismatch):
            xfr.process_message(m)
    assert z == dns.versioned.Zone("example.", dns.rdataclass.CH)


def test_aborted_stream_can_be_replaced_by_new_transfer():
    z = dns.versioned.Zone("example.")
    m1, m2, m3 = _messages(
        z.origin, progressive_part1, progressive_part2, progressive_part3
    )
    with pytest.raises(dns.exception.FormError):
        with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
            xfr.process_message(m1)
            xfr.process_message(m2)
            # The stream dies before the closing SOA: the context manager sees
            # the exception and rolls back.  Nothing is left behind.
            raise dns.exception.FormError("stream error")
    assert z == dns.versioned.Zone("example.")
    # A brand-new Inbound on the same manager can now do a full transfer.
    full = dns.message.from_text(axfr, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr2:
        assert xfr2.process_message(full) is True
        assert xfr2.committed
    assert z == dns.zone.from_text(base, "example.")


def test_failed_transfer_cannot_be_reused():
    z = dns.versioned.Zone("example.")
    m = dns.message.from_text(
        axfr_answers_after_final_soa, origin=z.origin, one_rr_per_rrset=True
    )
    good = dns.message.from_text(axfr, origin=z.origin, one_rr_per_rrset=True)
    xfr = dns.xfr.Inbound(z, dns.rdatatype.AXFR)
    with pytest.raises(dns.xfr.DuplicateEnd):
        xfr.process_message(m)
    assert xfr.failed
    with pytest.raises(dns.xfr.TransferAborted):
        xfr.process_message(good)
    assert z == dns.versioned.Zone("example.")


def test_context_exit_without_close_rolls_back():
    z = dns.versioned.Zone("example.")
    m1 = dns.message.from_text(
        progressive_part1, origin=z.origin, one_rr_per_rrset=True
    )
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        xfr.process_message(m1)
        assert xfr.phase == dns.xfr.InboundPhase.DATA
    # Leaving the context without the closing SOA abandons the transfer.
    assert xfr.failed
    assert z == dns.versioned.Zone("example.")


def test_split_boundaries_do_not_change_the_outcome():
    # The same records, delivered with different message boundaries, must give
    # the same zone: nothing may depend on a message happening to end on a
    # phase boundary.
    texts = [
        "@ 3600 IN SOA foo bar 1 2 3 4 5",
        "@ 3600 IN NS ns1",
        "@ 3600 IN NS ns2",
        "bar.foo 300 IN MX 0 blaz.foo",
        "ns1 3600 IN A 10.0.0.1",
        "ns2 3600 IN A 10.0.0.2",
        "@ 3600 IN SOA foo bar 1 2 3 4 5",
    ]

    def run(groupings):
        z = dns.versioned.Zone("example.")
        phase = dns.xfr.InboundPhase.START_SOA
        with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
            for group in groupings:
                body = ";ANSWER\n" + "\n".join(group) + "\n"
                m = dns.message.from_text(
                    "id 1\nopcode QUERY\nrcode NOERROR\nflags QR AA\n" + body,
                    origin=z.origin,
                    one_rr_per_rrset=True,
                )
                done = xfr.process_message(m)
                # Phases never move backwards.
                assert xfr.phase.value >= phase.value or xfr.phase == (
                    dns.xfr.InboundPhase.DONE
                )
                phase = xfr.phase
                assert done == xfr.complete
        return z

    whole = run([texts])
    pairs = run([[texts[0]], [texts[1], texts[2]], texts[3:5], texts[5:]])
    singles = run([[t] for t in texts])
    expected = dns.zone.from_text(base, "example.")
    assert whole == expected
    assert pairs == expected
    assert singles == expected


def test_first_message_question_name_mismatch():
    z = dns.versioned.Zone("example.")
    text = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
not-example. IN AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN SOA foo bar 1 2 3 4 5
"""
    m = dns.message.from_text(text, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        with pytest.raises(dns.xfr.ZoneIdentityError):
            xfr.process_message(m)


def test_continuation_question_name_mismatch():
    z = dns.versioned.Zone("example.")
    m1 = dns.message.from_text(
        progressive_part1, origin=z.origin, one_rr_per_rrset=True
    )
    m2 = dns.message.from_text(
        """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
other. IN AXFR
;ANSWER
ns1 3600 IN A 10.0.0.1
@ 3600 IN SOA foo bar 1 2 3 4 5
""",
        origin=z.origin,
        one_rr_per_rrset=True,
    )
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        xfr.process_message(m1)
        with pytest.raises(dns.xfr.ZoneIdentityError):
            xfr.process_message(m2)
        assert xfr.failed
    assert z == dns.versioned.Zone("example.")


def test_closing_soa_must_match_opening_soa():
    z = dns.versioned.Zone("example.")
    text = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
@ 3600 IN SOA foo bar 2 2 3 4 5
"""
    m = dns.message.from_text(text, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        # A different origin SOA is a second, non-final SOA in an AXFR, which
        # is malformed.
        with pytest.raises(dns.exception.FormError):
            xfr.process_message(m)
        assert xfr.failed
    assert z == dns.versioned.Zone("example.")


def test_truncated_stream_leaves_no_partial_zone_and_is_retryable():
    z = dns.versioned.Zone("example.")
    m1 = dns.message.from_text(
        progressive_part1, origin=z.origin, one_rr_per_rrset=True
    )
    # First attempt: stops after the first message.
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as attempt1:
        attempt1.process_message(m1)
    assert attempt1.failed
    assert z == dns.versioned.Zone("example.")
    # Second attempt on the very same manager, this time complete.
    m1b, m2, m3 = _messages(
        z.origin, progressive_part1, progressive_part2, progressive_part3
    )
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as attempt2:
        assert attempt2.process_message(m1b) is False
        assert attempt2.process_message(m2) is False
        assert attempt2.process_message(m3) is True
        assert attempt2.committed
    assert z == dns.zone.from_text(base, "example.")


def test_unversioned_zone_progressive_axfr():
    z = dns.zone.Zone("example.")
    m1, m2, m3 = _messages(
        z.origin, progressive_part1, progressive_part2, progressive_part3
    )
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        assert xfr.process_message(m1) is False
        assert xfr.process_message(m2) is False
        assert xfr.process_message(m3) is True
    assert z == dns.zone.from_text(base, "example.")


def test_progressive_axfr_convenience_properties():
    z = dns.versioned.Zone("example.")
    m = dns.message.from_text(axfr, origin=z.origin, one_rr_per_rrset=True)
    xfr = dns.xfr.Inbound(z, dns.rdatatype.AXFR)
    assert xfr.phase is dns.xfr.InboundPhase.START_SOA
    assert not xfr.complete and not xfr.failed and not xfr.committed
    assert xfr.process_message(m) is True
    assert xfr.phase is dns.xfr.InboundPhase.DONE
    assert xfr.complete and not xfr.failed and xfr.committed


#
# The out-of-zone rejection also applies when the transfer is driven through
# dns.query.inbound_xfr(); a failed transfer leaves the zone alone and the same
# manager accepts a new transfer afterwards.  dns.query.xfr() is a message
# generator for dns.zone.from_xfr(), which tolerates out-of-zone data, so it
# keeps working.
#
axfr_with_out_of_zone = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
ns1 3600 IN A 10.0.0.1
www.victim.com. 300 IN A 10.9.9.9
@ 3600 IN SOA foo bar 1 2 3 4 5
"""


class OutOfZoneXFRNanoNameserver(Server):
    def __init__(self, text):
        super().__init__(origin=dns.name.from_text("example"))
        self.text = text

    def handle(self, request):
        try:
            r = dns.message.from_text(
                self.text, one_rr_per_rrset=True, origin=self.origin
            )
            r.id = request.message.id
            return r
        except Exception:
            pass


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
def test_sync_inbound_xfr_rejects_out_of_zone_then_retries():
    with OutOfZoneXFRNanoNameserver(axfr_with_out_of_zone) as ns:
        where, port = ns.tcp_address
        zone = dns.versioned.Zone("example")
        with pytest.raises(dns.xfr.OutOfZoneName):
            dns.query.inbound_xfr(where, zone, port=port)
        # The failed attempt left the zone alone.
        assert zone == dns.versioned.Zone("example")
    with OutOfZoneXFRNanoNameserver(axfr) as ns:
        where, port = ns.tcp_address
        dns.query.inbound_xfr(where, zone, port=port)
        assert zone == dns.zone.from_text(base, "example")


async def async_inbound_xfr_rejects_out_of_zone_then_retries():
    with OutOfZoneXFRNanoNameserver(axfr_with_out_of_zone) as ns:
        where, port = ns.tcp_address
        zone = dns.versioned.Zone("example")
        with pytest.raises(dns.xfr.OutOfZoneName):
            await dns.asyncquery.inbound_xfr(where, zone, port=port)
        assert zone == dns.versioned.Zone("example")
    with OutOfZoneXFRNanoNameserver(axfr) as ns:
        where, port = ns.tcp_address
        await dns.asyncquery.inbound_xfr(where, zone, port=port)
        assert zone == dns.zone.from_text(base, "example")


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
def test_asyncio_inbound_xfr_rejects_out_of_zone_then_retries():
    dns.asyncbackend.set_default_backend("asyncio")
    asyncio.run(async_inbound_xfr_rejects_out_of_zone_then_retries())


@pytest.mark.skipif(not _nanonameserver_available, reason="requires nanonameserver")
def test_sync_query_xfr_generator_tolerates_out_of_zone():
    # dns.zone.from_xfr() ignores out-of-zone data, so the generator which
    # feeds it must not reject the transfer.
    with OutOfZoneXFRNanoNameserver(axfr_with_out_of_zone) as ns:
        where, port = ns.tcp_address
        xfr = dns.query.xfr(where, "example", port=port)
        zone = dns.zone.from_xfr(xfr)
        assert zone == dns.zone.from_text(base, "example")
        assert dns.name.from_text("www.victim.com.") not in zone


#
# The transport plumbing is exercised over a socketpair, which is local and
# involves no DNS server: the peer just writes a fixed sequence of transfer
# messages.  This covers the query-side arguments and the "stream dies" case.
#
def _serve_messages(b, messages, die_after=None, question_echo=None):
    """Read one query from *b*, then write *messages*, optionally dying."""

    def run():
        try:
            b.recv(65535)
            for i, m in enumerate(messages):
                if die_after is not None and i >= die_after:
                    break
                if question_echo is not None:
                    m.id = question_echo.id
                    if i > 0:
                        m.question = []
                wire = m.to_wire()
                b.sendall(struct.pack("!H", len(wire)) + wire)
        finally:
            b.close()

    t = threading.Thread(target=run)
    t.start()
    return t


def test_socket_stream_rejects_out_of_zone():
    z = dns.versioned.Zone("example.")
    q = dns.message.make_query("example", "AXFR")
    a, b = socket.socketpair()
    m = dns.message.from_text(
        axfr_with_out_of_zone, origin=z.origin, one_rr_per_rrset=True
    )
    t = _serve_messages(b, [m], question_echo=q)
    try:
        with pytest.raises(dns.xfr.OutOfZoneName):
            list(
                dns.query._inbound_xfr(
                    z, a, q, None, None, None, True, allow_out_of_zone=False
                )
            )
    finally:
        a.close()
        t.join()
    assert z == dns.versioned.Zone("example.")


def test_socket_stream_dies_before_closing_soa_then_retries():
    z = dns.versioned.Zone("example.")
    q = dns.message.make_query("example", "AXFR")
    texts = [progressive_part1, progressive_part2, progressive_part3]
    msgs = [
        dns.message.from_text(x, origin=z.origin, one_rr_per_rrset=True) for x in texts
    ]
    # The server sends the data but never the closing SOA and then hangs up.
    a, b = socket.socketpair()
    t = _serve_messages(b, msgs, die_after=2, question_echo=q)
    try:
        with pytest.raises(EOFError):
            list(
                dns.query._inbound_xfr(
                    z, a, q, None, None, None, True, allow_out_of_zone=False
                )
            )
    finally:
        a.close()
        t.join()
    # Leaving the generator normally (or via an error) releases the writer and
    # leaves the zone untouched, and a new transfer on the same manager works.
    assert z == dns.versioned.Zone("example.")
    a, b = socket.socketpair()
    m = dns.message.from_text(axfr, origin=z.origin, one_rr_per_rrset=True)
    t = _serve_messages(b, [m], question_echo=q)
    try:
        list(
            dns.query._inbound_xfr(
                z, a, q, None, None, None, True, allow_out_of_zone=False
            )
        )
    finally:
        a.close()
        t.join()
    assert z == dns.zone.from_text(base, "example.")


def test_socket_generator_path_tolerates_out_of_zone():
    # The dns.query.xfr() generator sets allow_out_of_zone so that
    # dns.zone.from_xfr() keeps ignoring out-of-zone data, as documented.
    z = dns.versioned.Zone("example.")
    q = dns.message.make_query("example", "AXFR")
    m = dns.message.from_text(
        axfr_with_out_of_zone, origin=z.origin, one_rr_per_rrset=True
    )
    a, b = socket.socketpair()
    t = _serve_messages(b, [m], question_echo=q)
    try:
        for _ in dns.query._inbound_xfr(
            z, a, q, None, None, None, True, allow_out_of_zone=True
        ):
            pass
    finally:
        a.close()
        t.join()
    ez = dns.zone.from_text(
        """@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
ns1 3600 IN A 10.0.0.1
""",
        "example.",
    )
    assert z == ez


def test_wrong_class_opening_soa():
    z = dns.versioned.Zone("example.")
    text = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. IN AXFR
;ANSWER
@ 3600 CH SOA foo bar 1 2 3 4 5
@ 3600 CH SOA foo bar 1 2 3 4 5
"""
    m = dns.message.from_text(text, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        with pytest.raises(dns.xfr.ClassMismatch):
            xfr.process_message(m)
        assert xfr.failed
    assert z == dns.versioned.Zone("example.")


def test_retransmitted_closing_message_is_duplicate_end():
    z = dns.versioned.Zone("example.")
    closing = dns.message.from_text(
        progressive_part3, origin=z.origin, one_rr_per_rrset=True
    )
    first = dns.message.from_text(
        progressive_part1, origin=z.origin, one_rr_per_rrset=True
    )
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        xfr.process_message(first)
        assert xfr.process_message(closing) is True
        # A retransmission of the closing message means the stream ended twice.
        with pytest.raises(dns.xfr.DuplicateEnd):
            xfr.process_message(closing)
        # The successful close is unaffected.
        assert xfr.committed
    assert z == dns.zone.from_text(
        """@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN NS ns1
@ 3600 IN NS ns2
ns2 3600 IN A 10.0.0.2
""",
        "example.",
    )


def test_question_class_mismatch():
    z = dns.versioned.Zone("example.")
    text = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. CH AXFR
;ANSWER
@ 3600 IN SOA foo bar 1 2 3 4 5
@ 3600 IN SOA foo bar 1 2 3 4 5
"""
    m = dns.message.from_text(text, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        with pytest.raises(dns.xfr.ClassMismatch):
            xfr.process_message(m)
        assert xfr.failed
    assert z == dns.versioned.Zone("example.")


def test_ch_zone_transfer_succeeds():
    z = dns.versioned.Zone("example.", dns.rdataclass.CH)
    text = """id 1
opcode QUERY
rcode NOERROR
flags QR AA
;QUESTION
example. CH AXFR
;ANSWER
@ 3600 CH SOA foo bar 1 2 3 4 5
@ 3600 CH TXT "hello"
@ 3600 CH SOA foo bar 1 2 3 4 5
"""
    m = dns.message.from_text(text, origin=z.origin, one_rr_per_rrset=True)
    with dns.xfr.Inbound(z, dns.rdatatype.AXFR) as xfr:
        assert xfr.process_message(m) is True
    assert z == dns.zone.from_text(
        """@ 3600 CH SOA foo bar 1 2 3 4 5
@ 3600 CH TXT "hello"
""",
        "example.",
        rdclass=dns.rdataclass.CH,
        check_origin=False,
    )


def test_transaction_setup_failure_releases_writer():
    z = dns.versioned.Zone("example.")
    m = dns.message.from_text(axfr, origin=z.origin, one_rr_per_rrset=True)

    class Boom(Exception):
        pass

    def setup(txn):
        raise Boom

    with dns.xfr.Inbound(z, dns.rdatatype.AXFR, transaction_setup=setup) as xfr:
        with pytest.raises(Boom):
            xfr.process_message(m)
        assert xfr.failed
    assert z == dns.versioned.Zone("example.")
    # The failed writer was released, so a new writer can be had.
    with z.writer() as txn:
        txn.add("@", 300, dns.rdata.from_text("IN", "SOA", ". . 1 2 3 4 5"))
