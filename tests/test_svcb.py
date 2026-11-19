# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

import io
import unittest

import dns.rdata
import dns.rdtypes.svcbbase
import dns.rrset
from dns.tokenizer import Tokenizer
from tests.util import here


class SVCBTestCase(unittest.TestCase):
    def check_valid_inputs(self, inputs):
        expected = inputs[0]
        for text in inputs:
            rr = dns.rdata.from_text("IN", "SVCB", text)
            new_text = rr.to_text()
            self.assertEqual(expected, new_text)

    def check_invalid_inputs(self, inputs):
        for text in inputs:
            with self.assertRaises((dns.exception.SyntaxError, ValueError)):
                dns.rdata.from_text("IN", "SVCB", text)

    def test_svcb_general_invalid(self):
        invalid_inputs = (
            # Duplicate keys
            "1 . alpn=h2 alpn=h3",
            "1 . alpn=h2 key1=h3",
            # Quoted keys
            '1 . "alpn=h2"',
            # Invalid space
            "1 . alpn= h2",
            "1 . alpn =h2",
            "1 . alpn = h2",
            '1 . alpn= "h2"',
            "1 . =alpn",
            # Escaped keys
            "1 . \\097lpn=h2",
            "1 . al\\pn=h2",
            "1 . key\\049=\\002h2",
            # Keys are lowercase, without underscores
            "1 . ALPN=h2",
            "1 . Key667=x",
            "1 . no_default_alpn alpn=h2",
        )
        self.check_invalid_inputs(invalid_inputs)

    def test_svcb_mandatory(self):
        valid_inputs = (
            '1 . mandatory="alpn,no-default-alpn" alpn="h2" no-default-alpn',
            "1 . mandatory=alpn,no-default-alpn alpn=h2 no-default-alpn",
            "1 . mandatory=key1,key2 alpn=h2 no-default-alpn",
            "1 . mandatory=alpn,no-default-alpn key1=\\002h2 key2",
            "1 . key0=\\000\\001\\000\\002 alpn=h2 no-default-alpn",
            "1 . alpn=h2 no-default-alpn mandatory=alpn,no-default-alpn",
        )
        self.check_valid_inputs(valid_inputs)

        # RFC 9460 Section 8 forbids escapes in the value.
        for text in ("1 . mandatory=\\097lpn alpn=h2", '1 . mandatory="alpn\\,port"'):
            with self.assertRaisesRegex(dns.exception.SyntaxError, "escape"):
                dns.rdata.from_text("IN", "SVCB", text)

        invalid_inputs = (
            # empty
            "1 . mandatory=",
            "1 . mandatory",
            '1 . mandatory=""',
            # unknown key
            "1 . mandatory=foo",
            # key 0
            "1 . mandatory=key0",
            "1 . mandatory=key0,alpn",
            # duplicate
            "1 . mandatory=alpn,alpn alpn=h2",
            # invalid escaping
            "1 . mandatory=\\alpn alpn=h2",
            # invalid key syntax
            "1 . mandatory=ALPN alpn=h2",
            "1 . mandatory=no_default_alpn alpn=h2 no-default-alpn",
            # empty wire format
            "1 . key0",
            "1 . key0=",
            '1 . key0=""',
            # 0 in wire format
            "1 . key0=\\000\\000",
            # invalid length in wire format
            "1 . key0=\\000",
            # out of order in wire format
            "1 . key0=\\000\\002\\000\\001 alpn=h2 no-default-alpn",
            # leading zeros
            "1 . mandatory=key1,key002 alpn=h2 no-default-alpn",
        )
        self.check_invalid_inputs(invalid_inputs)

        # A mandatory key that is listed but not present is a semantic
        # (self-consistency) failure, not a syntax one: the record still
        # parses and can be inspected, but validate() rejects it.
        rr = dns.rdata.from_text("IN", "SVCB", "1 . mandatory=alpn")
        with self.assertRaisesRegex(
            dns.rdtypes.svcbbase.InvalidSVCBRecord, "mandatory but is not present"
        ):
            rr.validate()
        # The raw record is still available to operational tooling.
        self.assertEqual(rr.to_text(), '1 . mandatory="alpn"')

        # An unsupported key that is marked mandatory makes the record
        # unusable: it must not be ignored so that a client would fall back
        # to the default port.
        rr = dns.rdata.from_text("IN", "SVCB", "1 . mandatory=key9999 key9999=abc")
        with self.assertRaisesRegex(
            dns.rdtypes.svcbbase.InvalidSVCBRecord, "not supported"
        ):
            rr.validate()
        # The unknown parameter keeps its original value.
        unknown = rr.params[dns.rdtypes.svcbbase.ParamKey.make("key9999")]
        self.assertEqual(unknown.value, b"abc")

    def test_svcb_alpn(self):
        valid_inputs_two_items = (
            '1 . alpn="h2,h3"',
            "1 . alpn=h2,h3",
            "1 . alpn=h\\050,h3",
            '1 . alpn="h\\050,h3"',
            "1 . alpn=\\h2,h3",
            '1 . alpn="h2\\,h3"',
            "1 . alpn=h2\\,h3",
            "1 . alpn=h2\\044h3",
            "1 . key1=\\002h2\\002h3",
        )
        self.check_valid_inputs(valid_inputs_two_items)

        valid_inputs_one_item = (
            '1 . alpn="h2\\\\,h3"',
            "1 . alpn=h2\\\\,h3",
            "1 . alpn=h2\\092\\044h3",
            "1 . key1=\\005h2,h3",
        )
        self.check_valid_inputs(valid_inputs_one_item)

        invalid_inputs = (
            "1 . alpn",
            "1 . alpn=",
            '1 . alpn=""',
            "1 . alpn=h2,,h3",
            "1 . alpn=01234567890abcdef01234567890abcdef01234567890abcdef"
            "01234567890abcdef01234567890abcdef01234567890abcdef"
            "01234567890abcdef01234567890abcdef01234567890abcdef"
            "01234567890abcdef01234567890abcdef01234567890abcdef"
            "01234567890abcdef01234567890abcdef01234567890abcdef"
            "01234567890abcdef",
            '1 . alpn=",h2,h3"',
            '1 . alpn="h2,h3,"',
            # list-level escapes other than "\," and "\\"
            '1 . alpn="h2\\\\x"',
            "1 . alpn=h2\\\\x",
            "1 . alpn=h2\\092x",
            "1 . alpn=h2\\\\",
            "1 . key1",
            "1 . key1=",
            '1 . key1=""',
            "1 . key1=\\000",
            "1 . key1=\\002x",
        )
        self.check_invalid_inputs(invalid_inputs)

    def test_svcb_alpn_escaping(self):
        # Bytes outside 0x20-0x7e are escaped once, at the character-string
        # level, as they don't need any escaping at the list level.
        valid_inputs_non_printable = (
            '1 . alpn="h\\0002,h3"',
            "1 . alpn=h\\0002,h3",
            "1 . key1=\\003h\\0002\\002h3",
        )
        self.check_valid_inputs(valid_inputs_non_printable)

        valid_inputs_high_bit = (
            '1 . alpn="\\255h2"',
            "1 . alpn=\\255h2",
            "1 . key1=\\003\\255h2",
        )
        self.check_valid_inputs(valid_inputs_high_bit)

        valid_inputs_quote = (
            '1 . alpn="h\\"2"',
            "1 . alpn=h\\0342",
            "1 . key1=\\003h\\0342",
        )
        self.check_valid_inputs(valid_inputs_quote)

        # to_text() output must parse back to the same value, both from
        # text and from wire.
        for ids in (
            (b"h\x002",),
            (b"\xff", b"h2"),
            (b"h,3", b"h\\3"),
            (b'h"2',),
            (b"h2", b"h3"),
        ):
            param = dns.rdtypes.svcbbase.ALPNParam(ids)
            rr = dns.rdata.from_text("IN", "SVCB", "1 . alpn=" + param.to_text())
            alpn = rr.params[dns.rdtypes.svcbbase.ParamKey.ALPN]
            self.assertEqual(alpn.ids, ids)
            rr2 = dns.rdata.from_text("IN", "SVCB", rr.to_generic().to_text())
            self.assertEqual(rr2, rr)

    def test_svcb_no_default_alpn(self):
        valid_inputs = (
            '1 . alpn="h2" no-default-alpn',
            '1 . alpn="h2" no-default-alpn=""',
            '1 . alpn="h2" key2',
            '1 . alpn="h2" key2=""',
        )
        self.check_valid_inputs(valid_inputs)

        invalid_inputs = (
            "1 . alpn=h2 no-default-alpn=foo",
            "1 . alpn=h2 no-default-alpn=",
            "1 . alpn=h2 key2=foo",
            "1 . alpn=h2 key2=",
        )
        self.check_invalid_inputs(invalid_inputs)

        # no-default-alpn without alpn is a semantic (self-consistency)
        # failure per RFC 9460 section 6.1: the records still parse for
        # inspection, but validate() rejects them.
        for text in (
            "1 . no-default-alpn",
            '1 . no-default-alpn=""',
            "1 . key2",
            '1 . key2=""',
        ):
            rr = dns.rdata.from_text("IN", "SVCB", text)
            with self.assertRaisesRegex(
                dns.rdtypes.svcbbase.InvalidSVCBRecord,
                "no-default-alpn is present but alpn is missing",
            ):
                rr.validate()

    def test_svcb_port(self):
        valid_inputs = (
            '1 . port="53"',
            "1 . port=53",
            "1 . key3=\\000\\053",
        )
        self.check_valid_inputs(valid_inputs)

        invalid_inputs = (
            "1 . port",
            "1 . port=",
            "1 . port=53x",
            "1 . port=x53",
            "1 . port=53,54",
            "1 . port=53\\,54",
            "1 . port=65536",
            "1 . key3",
            "1 . key3=",
            "1 . key3=\\000",
        )
        self.check_invalid_inputs(invalid_inputs)

    def test_svcb_ipv4hint(self):
        valid_inputs = (
            '1 . ipv4hint="0.0.0.0,1.1.1.1"',
            "1 . ipv4hint=0.0.0.0,1.1.1.1",
            "1 . key4=\\000\\000\\000\\000\\001\\001\\001\\001",
        )
        self.check_valid_inputs(valid_inputs)

        invalid_inputs = (
            "1 . ipv4hint",
            "1 . ipv4hint=",
            '1 . ipv4hint=""',
            "1 . ipv4hint=1234",
            "1 . ipv4hint=1\\.2.3.4",
            "1 . ipv4hint=1.2.3.4\\,2.3.4.5",
            "1 . key4=",
            '1 . key4=""',
            "1 . key4=123",
        )
        self.check_invalid_inputs(invalid_inputs)

    def test_svcb_ech(self):
        valid_inputs = (
            '1 . ech="AAT+DQAA"',
            "1 . ech=AAT+DQAA",
            "1 . key5=\\000\\004\\254\\013\\000\\000",
        )
        self.check_valid_inputs(valid_inputs)

        # RFC 9848 Figure 1
        rr = dns.rdata.from_text(
            "IN",
            "SVCB",
            "1 . ech=AEj+DQBEAQAgACAdd+scUi0IYFsXnUIU7ko2Nd9+F8M26pAGZVpz/KrWPgAEAAE"
            "AAWQVZWNoLXNpdGVzLmV4YW1wbGUubmV0AAA=",
        )
        self.assertEqual(len(rr.params[dns.rdtypes.svcbbase.ParamKey.ECH].ech), 74)
        wire = bytes.fromhex("0001 00 0005 0004 0002fe0d")
        with self.assertRaises(dns.exception.FormError):
            dns.rdata.from_wire("IN", "SVCB", wire, 0, len(wire))

        invalid_inputs = (
            "1 . ech",
            "1 . ech=",
            '1 . ech=""',
            "1 . ech=Zm9vMA",
            # not an ECHConfigList
            "1 . ech=Zm9vMA==",
            "1 . key5=foo0",
            # length prefix too large, too small
            "1 . ech=AAX+DQAA",
            "1 . ech=AAP+DQAA",
            # fewer than 4 octets after the length prefix
            "1 . ech=AAL+DQ==",
            "1 . key5=\\000\\000",
            "1 . ech=\\090m9vMA==",
            "1 . key5",
            "1 . key5=",
            '1 . key5=""',
        )
        self.check_invalid_inputs(invalid_inputs)

    def test_svcb_ipv6hint(self):
        valid_inputs = (
            '1 . ipv6hint="::4,1::"',
            "1 . ipv6hint=::4,1::",
            "1 . key6=\\000\\000\\000\\000\\000\\000\\000\\000"
            "\\000\\000\\000\\000\\000\\000\\000\\004"
            "\\000\\001\\000\\000\\000\\000\\000\\000"
            "\\000\\000\\000\\000\\000\\000\\000\\000",
        )
        self.check_valid_inputs(valid_inputs)

        invalid_inputs = (
            "1 . ipv6hint",
            "1 . ipv6hint=",
            '1 . ipv6hint=""',
            "1 . ipv6hint=1234",
            "1 . ipv6hint=1\\::2",
            "1 . ipv6hint=::1\\,::2",
            "1 . ipv6hint",
            "1 . key6",
            "1 . key6=",
            '1 . key6=""',
            "1 . key6=123",
        )
        self.check_invalid_inputs(invalid_inputs)

    def test_svcb_docpath(self):
        valid_inputs_two_items = (
            '1 . docpath="n,s"',
            "1 . docpath=n,s",
            "1 . docpath=\\110,s",
            '1 . docpath="\\110,s"',
            "1 . docpath=\\n,s",
            '1 . docpath="n\\,s"',
            "1 . docpath=n\\,s",
            "1 . docpath=n\\044s",
            "1 . key10=\\001n\\001s",
        )
        self.check_valid_inputs(valid_inputs_two_items)

        valid_inputs_one_item = (
            '1 . docpath="n\\\\,s"',
            "1 . docpath=n\\\\,s",
            "1 . docpath=n\\092\\044s",
            "1 . key10=\\003n,s",
        )
        self.check_valid_inputs(valid_inputs_one_item)

        valid_inputs_no_item = (
            "1 . docpath",
            '1 . docpath=""',
            '1 . key10=""',
            "1 . key10",
        )
        self.check_valid_inputs(valid_inputs_no_item)

        invalid_inputs = (
            "1 . docpath=n,,s",
            "1 . docpath=01234567890abcdef01234567890abcdef01234567890abcdef"
            "01234567890abcdef01234567890abcdef01234567890abcdef"
            "01234567890abcdef01234567890abcdef01234567890abcdef"
            "01234567890abcdef01234567890abcdef01234567890abcdef"
            "01234567890abcdef01234567890abcdef01234567890abcdef"
            "01234567890abcdef",
            '1 . docpath=",n,s"',
            '1 . docpath="n,s,"',
            '1 . docpath="n\\\\s"',
            "1 . docpath=n\\092s",
        )
        self.check_invalid_inputs(invalid_inputs)

    def test_svcb_docpath_escaping(self):
        # docpath shares ALPN's string list escaping.
        valid_inputs = (
            '1 . docpath="n\\000s,t"',
            "1 . docpath=n\\000s,t",
            "1 . key10=\\003n\\000s\\001t",
        )
        self.check_valid_inputs(valid_inputs)

    def test_svcb_unknown(self):
        valid_inputs_one_key = (
            '1 . key23="key45"',
            "1 . key23=key45",
            "1 . key23=key\\052\\053",
            '1 . key23="key\\052\\053"',
            "1 . key23=\\107\\101\\121\\052\\053",
        )
        self.check_valid_inputs(valid_inputs_one_key)

        valid_inputs_one_key_empty = (
            "1 . key23",
            '1 . key23=""',
        )
        self.check_valid_inputs(valid_inputs_one_key_empty)

        invalid_inputs_one_key = (
            "1 . key65536=foo",
            "1 . key24= key48",
        )
        self.check_invalid_inputs(invalid_inputs_one_key)

        valid_inputs_two_keys = (
            "1 . key24 key48",
            '1 . key24="" key48',
        )
        self.check_valid_inputs(valid_inputs_two_keys)

    def test_svcb_wire(self):
        valid_inputs = (
            '1 . mandatory="alpn,port" alpn="h2" port="257"',
            "\\# 24 0001 00 0000000400010003 00010003026832 000300020101",
        )
        self.check_valid_inputs(valid_inputs)

        valid_inputs = (
            '1 . alpn="co" docpath="n,s"',
            "\\# 18 0001 00 0001000302636f 000a0004016e0173",
        )
        self.check_valid_inputs(valid_inputs)

        valid_inputs = (
            '1 . alpn="co" docpath',
            "\\# 14 0001 00 0001000302636f 000a0000",
        )
        self.check_valid_inputs(valid_inputs)

        everything = (
            '100 foo.com. mandatory="alpn,port" alpn="h2,h3" '
            '             no-default-alpn port="12345" ech="AAT+DQAA" '
            "             ipv4hint=1.2.3.4,4.3.2.1 ipv6hint=1::2,3::4"
            '             key12345="foo"'
        )
        rr = dns.rdata.from_text("IN", "SVCB", everything)
        rr2 = dns.rdata.from_text("IN", "SVCB", rr.to_generic().to_text())
        self.assertEqual(rr, rr2)

        invalid_inputs = (
            # As above, but the keys are out of order.
            "\\# 24 0001 00 0000000400010003 000300020101 00010003026832",
            # Alias form shouldn't have parameters.
            "\\# 08 0000 000300020101",
        )
        self.check_invalid_inputs(invalid_inputs)

        # Mandatory keys that are not present, and no-default-alpn without
        # alpn, are semantic failures: the wire still parses so the record
        # can be inspected, but validate() rejects it.
        semantic_failures = (
            # mandatory lists key 2 (no-default-alpn), which is absent
            "\\# 24 0001 00 0000000400010002 00010003026832 000300020101",
            # mandatory lists key 4 (ipv4hint), which is absent
            "\\# 24 0001 00 0000000400010004 00010003026832 000300020101",
            # no-default-alpn requires alpn
            "\\# 07 0001 00 00020000",
        )
        for wire_hex in semantic_failures:
            wire = bytes.fromhex(wire_hex.split(" ", 2)[2].replace(" ", ""))
            rr = dns.rdata.from_wire("IN", "SVCB", wire, 0, len(wire))
            with self.assertRaises(dns.rdtypes.svcbbase.InvalidSVCBRecord):
                rr.validate()

    def test_svcb_empty_value(self):
        # Keys whose value must not be empty, as zero-length wire values.
        for key in (0, 1, 3, 4, 5, 6):
            wire = bytes.fromhex(f"0001 00 {key:04x} 0000")
            with self.assertRaises(dns.exception.FormError):
                dns.rdata.from_wire("in", "svcb", wire, 0, len(wire))
        # Keys whose value may be empty.
        for key in (8, 10, 12345):
            wire = bytes.fromhex(f"0001 00 {key:04x} 0000")
            rr = dns.rdata.from_wire("in", "svcb", wire, 0, len(wire))
            self.assertIsNone(rr.params[key])
            self.assertEqual(rr.to_wire(), wire)
        svcbbase = dns.rdtypes.svcbbase
        for cls, value in (
            (svcbbase.MandatoryParam, []),
            (svcbbase.ALPNParam, []),
            (svcbbase.IPv4HintParam, []),
            (svcbbase.IPv6HintParam, []),
            (svcbbase.ECHParam, b""),
        ):
            with self.assertRaises(ValueError):
                cls(value)
        for key in (0, 1, 3, 4, 5, 6):
            with self.assertRaises(ValueError):
                dns.rdata.from_text("IN", "SVCB", "1 .").replace(params={key: None})

    def test_svcb_duplicate_key_wire(self):
        # RFC 9460 Sec 2.2 requires SvcParamKeys to be in strictly increasing
        # numeric order, so a repeated key makes the RR malformed.  The
        # presentation parser rejects this (test_svcb_spec_failure_cases), but
        # the wire parser used to accept it and silently keep only the last
        # value; check that it is now a FormError.  Here PORT (key 3) appears
        # twice, as ports 53 and 54.
        wire = bytes.fromhex("0001" "00" "000300020035" "000300020036")
        with self.assertRaises(dns.exception.FormError):
            dns.rdata.from_wire("in", "svcb", wire, 0, len(wire))
        # The same rule applies to the keys inside the mandatory list (Sec 8):
        # here the mandatory value lists key 1 twice.
        wire = bytes.fromhex("0001" "00" "0000" "0004" "0001" "0001")
        with self.assertRaises(dns.exception.FormError):
            dns.rdata.from_wire("in", "svcb", wire, 0, len(wire))

    def test_misc_escape(self):
        rdata = dns.rdata.from_text("in", "svcb", "1 . alpn=\\010\\010")
        expected = '1 . alpn="\\010\\010"'
        self.assertEqual(rdata.to_text(), expected)
        self.assertEqual(dns.rdata.from_text("in", "svcb", expected), rdata)
        with self.assertRaises(dns.exception.SyntaxError):
            dns.rdata.from_text("in", "svcb", "1 . alpn=\\0")
        with self.assertRaises(dns.exception.SyntaxError):
            dns.rdata.from_text("in", "svcb", "1 . alpn=\\00")
        with self.assertRaises(dns.exception.SyntaxError):
            dns.rdata.from_text("in", "svcb", "1 . alpn=\\00q")
        with self.assertRaises(dns.exception.SyntaxError):
            dns.rdata.from_text("in", "svcb", "1 . alpn=\\256")
        # This doesn't usually get exercised, so we do it directly.
        gp = dns.rdtypes.svcbbase.GenericParam.from_value("\\001\\002")
        expected = '"\\001\\002"'
        self.assertEqual(gp.to_text(), expected)

    def test_non_ascii_digits_escape(self):
        digits = "\u0967"
        rdata = dns.rdata.from_text("in", "svcb", "1 . alpn=" + digits)
        self.assertEqual(rdata.to_text(), '1 . alpn="\\224\\165\\167"')
        self.assertEqual(
            dns.rdata.from_text("in", "svcb", rdata.to_text()).to_wire(),
            rdata.to_wire(),
        )
        self.assertRaises(
            dns.exception.SyntaxError,
            dns.rdata.from_text,
            "in",
            "svcb",
            "1 . alpn=\\12" + digits,
        )

    def test_svcb_spec_test_vectors(self):
        for rdtype in ("SVCB", "HTTPS"):
            with self.subTest(rdtype=rdtype):
                self.check_spec_test_vectors(rdtype)

    def check_spec_test_vectors(self, rdtype):
        text_file = here("svcb_test_vectors.text")
        text_tokenizer = Tokenizer(
            open(text_file, encoding="utf-8"), filename=text_file
        )
        generic_file = here("svcb_test_vectors.generic")
        generic_tokenizer = Tokenizer(
            open(generic_file, encoding="utf-8"), filename=generic_file
        )

        while True:
            while True:
                text_token = text_tokenizer.get()
                if text_token.is_eol():
                    continue
                break
            while True:
                generic_token = generic_tokenizer.get()
                if generic_token.is_eol():
                    continue
                break
            self.assertEqual(text_token.ttype, generic_token.ttype)
            if text_token.is_eof():
                break
            self.assertTrue(text_token.is_identifier)
            text_tokenizer.unget(text_token)
            generic_tokenizer.unget(generic_token)
            text_rdata = dns.rdata.from_text("IN", rdtype, text_tokenizer)
            generic_rdata = dns.rdata.from_text("IN", rdtype, generic_tokenizer)
            self.assertEqual(text_rdata, generic_rdata)

    def test_svcb_spec_failure_cases(self):
        failure_cases = (
            # This example has multiple instances of the same SvcParamKey
            "1 foo.example.com. key123=abc key123=def",
            # In the next examples the SvcParamKeys are missing their values.
            "1 foo.example.com. mandatory",
            "1 foo.example.com. alpn",
            "1 foo.example.com. port",
            "1 foo.example.com. ipv4hint",
            "1 foo.example.com. ipv6hint",
            # The "no-default-alpn" SvcParamKey value MUST be empty (Section 6.1).
            "1 foo.example.com. no-default-alpn=abc",
            # The "mandatory" SvcParamKey MUST not be included in mandatory list
            # (Section 7).
            "1 foo.example.com. mandatory=mandatory",
            # Here there are multiple instances of the same SvcParamKey in the
            # mandatory list (Section 7).
            "1 foo.example.com. mandatory=key123,key123 key123=abc",
        )
        self.check_invalid_inputs(failure_cases)

        # A mandatory SvcParam that is missing is a semantic failure
        # (Section 7/8), so the record parses but validate() rejects it.
        rr = dns.rdata.from_text("IN", "SVCB", "1 foo.example.com. mandatory=key123")
        with self.assertRaisesRegex(
            dns.rdtypes.svcbbase.InvalidSVCBRecord, "mandatory but is not present"
        ):
            rr.validate()

    def test_alias_mode(self):
        rd = dns.rdata.from_text("in", "svcb", "0 .")
        self.assertEqual(len(rd.params), 0)
        self.assertEqual(rd.target, dns.name.root)
        self.assertEqual(rd.to_text(), "0 .")
        rd = dns.rdata.from_text("in", "svcb", "0 elsewhere.")
        self.assertEqual(rd.target, dns.name.from_text("elsewhere."))
        self.assertEqual(len(rd.params), 0)
        # provoke 'parameters in AliasMode' from text.
        with self.assertRaises(dns.exception.SyntaxError):
            dns.rdata.from_text("in", "svcb", "0 elsewhere. alpn=h2")
        # provoke 'parameters in AliasMode' from wire too.
        wire = bytes.fromhex("0000000000000400010003")
        with self.assertRaises(dns.exception.FormError):
            dns.rdata.from_wire("in", "svcb", wire, 0, len(wire))

    def test_immutability(self):
        alpn = dns.rdtypes.svcbbase.ALPNParam.from_value(["h2", "h3"])
        with self.assertRaises(TypeError):
            alpn.ids[0] = "foo"
        with self.assertRaises(TypeError):
            del alpn.ids[0]
        with self.assertRaises(TypeError):
            alpn.ids = "foo"
        with self.assertRaises(TypeError):
            del alpn.ids

    def test_alias_not_compressed(self):
        rrs = dns.rrset.from_text("elsewhere.", 300, "in", "svcb", "0 elseWhere.")
        output = io.BytesIO()
        compress = {}
        rrs.to_wire(output, compress)
        wire = output.getvalue()
        # Just one of these assertions is enough, but we do both to show
        # the bug we're checking is fixed.
        assert not wire.endswith(b"\xc0\x00")
        assert wire.endswith(b"\x09elseWhere\x00")

    def test_svcb_validate(self):
        svcbbase = dns.rdtypes.svcbbase
        InvalidSVCBRecord = svcbbase.InvalidSVCBRecord
        ParamKey = svcbbase.ParamKey

        # A simple, valid ServiceMode record validates cleanly.
        rr = dns.rdata.from_text("IN", "SVCB", "1 . alpn=h2 port=53")
        self.assertIsNone(rr.validate())
        # Validation is read-only and does not mutate the record.
        self.assertEqual(rr.to_text(), '1 . alpn="h2" port="53"')

        # A record with every supported parameter validates.
        rr = dns.rdata.from_text(
            "IN",
            "SVCB",
            '100 foo.com. mandatory="alpn,port" alpn="h2,h3" '
            'no-default-alpn port="12345" ech="AAT+DQAA" '
            "ipv4hint=1.2.3.4 ipv6hint=1::2",
        )
        self.assertIsNone(rr.validate())

        # Unknown, non-mandatory parameters are tolerated and retain their
        # original value.
        rr = dns.rdata.from_text("IN", "SVCB", '1 . key12345="foo"')
        self.assertIsNone(rr.validate())
        self.assertEqual(rr.params[ParamKey.make("key12345")].value, b"foo")

        # An unsupported mandatory key makes the record unusable, even though
        # the key itself is present; it must not be ignored.
        rr = dns.rdata.from_text("IN", "SVCB", "1 . mandatory=key12345 key12345=foo")
        with self.assertRaisesRegex(InvalidSVCBRecord, "not supported"):
            rr.validate()
        # ... but the raw record remains inspectable.
        self.assertEqual(rr.to_text(), '1 . mandatory="key12345" key12345="foo"')

        # A mandatory key that is absent is a self-consistency failure.
        rr = dns.rdata.from_text("IN", "SVCB", "1 . mandatory=port")
        with self.assertRaisesRegex(InvalidSVCBRecord, "mandatory but is not present"):
            rr.validate()

        # Supported, present mandatory keys are fine, in any presentation
        # order.
        rr = dns.rdata.from_text(
            "IN",
            "SVCB",
            "1 . no-default-alpn mandatory=alpn,no-default-alpn alpn=h2",
        )
        self.assertIsNone(rr.validate())

        # no-default-alpn without alpn is rejected.
        rr = dns.rdata.from_text("IN", "SVCB", "1 . no-default-alpn")
        with self.assertRaisesRegex(
            InvalidSVCBRecord, "no-default-alpn is present but alpn is missing"
        ):
            rr.validate()

        # Records built directly through the Python API with the parameters
        # out of presentation order are still valid: presentation format
        # allows any order, and to_wire() emits keys in sorted order.
        rr = dns.rdata.from_text("IN", "SVCB", "1 . alpn=h2 port=53").replace(
            params={
                ParamKey.PORT: svcbbase.PortParam(53),
                ParamKey.ALPN: svcbbase.ALPNParam(["h2"]),
            }
        )
        self.assertIsNone(rr.validate())
        # The wire form is sorted even though the dict is not.
        wire = rr.to_wire()
        alpn_encoding = bytes.fromhex("00010003026832")
        port_encoding = bytes.fromhex("000300020035")
        self.assertIn(alpn_encoding, wire)
        self.assertIn(port_encoding, wire)
        self.assertLess(wire.index(alpn_encoding), wire.index(port_encoding))

        # AliasMode records have no parameters when parsed, and validate()
        # accepts them (including TargetName ".").
        for text in ("0 .", "0 elsewhere."):
            rr = dns.rdata.from_text("IN", "SVCB", text)
            self.assertIsNone(rr.validate())

        # An AliasMode record carrying parameters cannot be produced by the
        # parsers, but one built through the Python API is rejected by
        # validate() rather than silently accepted.
        rr = dns.rdata.from_text("IN", "SVCB", "0 .").replace(
            params={ParamKey.ALPN: svcbbase.ALPNParam(["h2"])}
        )
        with self.assertRaisesRegex(InvalidSVCBRecord, "AliasMode"):
            rr.validate()

        # A wire record that parses but is semantically invalid keeps its
        # exact wire form, so operational tooling can still examine it.
        wire = bytes.fromhex("0001 00 00020000")
        rr = dns.rdata.from_wire("IN", "SVCB", wire, 0, len(wire))
        with self.assertRaises(InvalidSVCBRecord):
            rr.validate()
        self.assertEqual(rr.to_wire(), wire)

        # The HTTPS RR type shares the same validation behavior.
        rr = dns.rdata.from_text("IN", "HTTPS", "1 . mandatory=key12345 key12345=x")
        with self.assertRaises(InvalidSVCBRecord):
            rr.validate()
        rr = dns.rdata.from_text("IN", "HTTPS", '1 . alpn="h2"')
        self.assertIsNone(rr.validate())
