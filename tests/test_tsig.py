# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

import base64
import struct
import time
import unittest
from unittest.mock import Mock, patch

import dns.exception
import dns.message
import dns.name
import dns.rcode
import dns.rdtypes.ANY.TKEY
import dns.rdtypes.ANY.TSIG
import dns.tsig
import dns.tsigkeyring

keyring = dns.tsigkeyring.from_text({"keyname.": "NjHwPsMKjdN++dOfE5iAiQ=="})

keyname = dns.name.from_text("keyname")


class TSIGTestCase(unittest.TestCase):
    def test_get_context(self):
        key = dns.tsig.Key("foo.com", "abcd", "hmac-sha256")
        ctx = dns.tsig.get_context(key)
        self.assertEqual(ctx.name, "hmac-sha256")
        key = dns.tsig.Key("foo.com", "abcd", "hmac-sha512")
        ctx = dns.tsig.get_context(key)
        self.assertEqual(ctx.name, "hmac-sha512")
        bogus = dns.tsig.Key("foo.com", "abcd", "bogus")
        with self.assertRaises(NotImplementedError):
            dns.tsig.get_context(bogus)

    def test_tsig_message_properties(self):
        m = dns.message.make_query("example", "a")
        self.assertIsNone(m.keyname)
        self.assertIsNone(m.keyalgorithm)
        self.assertIsNone(m.tsig_error)
        m.use_tsig(keyring, keyname)
        self.assertEqual(m.keyname, keyname)
        self.assertEqual(m.keyalgorithm, dns.tsig.default_algorithm)
        self.assertEqual(m.tsig_error, dns.rcode.NOERROR)
        m = dns.message.make_query("example", "a")
        m.use_tsig(keyring, keyname, tsig_error=dns.rcode.BADKEY)
        self.assertEqual(m.tsig_error, dns.rcode.BADKEY)

    def test_verify_mac_for_context(self):
        key = dns.tsig.Key("foo.com", "abcd", "hmac-sha512")
        ctx = dns.tsig.get_context(key)
        bad_expected = b"xxxxxxxxxx"
        with self.assertRaises(dns.tsig.BadSignature):
            ctx.verify(bad_expected)

    def _tsig_start(self, wire, kr):
        # dns.tsig.validate() needs the exact byte offset of the TSIG
        # record, but nothing in the public API exposes it directly.
        # dns.message.from_wire() computes it correctly during parsing,
        # so capture the value it uses via the same call.
        captured = {}
        real_validate = dns.tsig.validate

        def spy(*args, **kwargs):
            captured["tsig_start"] = args[6]
            return real_validate(*args, **kwargs)

        with patch("dns.tsig.validate", side_effect=spy):
            dns.message.from_wire(wire, kr)
        return captured["tsig_start"]

    def test_validate(self):
        # make message and grab the TSIG
        m = dns.message.make_query("example", "a")
        m.use_tsig(keyring, keyname, algorithm=dns.tsig.HMAC_SHA256)
        w = m.to_wire()
        tsig = m.tsig[0]
        tsig_start = self._tsig_start(w, keyring)

        # get the time and use the correct key so only the timestamp is wrong
        now = int(time.time())
        correct_key = dns.tsig.Key(keyname, keyring[keyname], dns.tsig.HMAC_SHA256)

        # add enough to the time to take it over the fudge amount
        with self.assertRaises(dns.tsig.BadTime):
            dns.tsig.validate(
                w, correct_key, keyname, tsig, now + 1000, b"", tsig_start
            )

        key = dns.tsig.Key("foo.com", "abcd", "hmac-sha256")

        # change the key name
        with self.assertRaises(dns.tsig.BadKey):
            dns.tsig.validate(w, key, dns.name.from_text("bar.com"), tsig, now, b"", 0)

        # change the key algorithm
        key = dns.tsig.Key("foo.com", "abcd", "hmac-sha512")
        with self.assertRaises(dns.tsig.BadAlgorithm):
            dns.tsig.validate(w, key, dns.name.from_text("foo.com"), tsig, now, b"", 0)

    def test_validate_bad_signature_takes_precedence_over_bad_time(self):
        # https://github.com/rthalley/dnspython/issues/1287
        # RFC 8945 5.2: MAC validity must be established before other
        # errors like a bad timestamp are reported.
        m = dns.message.make_query("example", "a")
        m.use_tsig(keyring, keyname, algorithm=dns.tsig.HMAC_SHA256)
        w = m.to_wire()
        tsig = m.tsig[0]
        tsig_start = self._tsig_start(w, keyring)

        now = int(time.time())
        wrong_key = dns.tsig.Key(keyname, "abcd", "hmac-sha256")
        with self.assertRaises(dns.tsig.BadSignature):
            dns.tsig.validate(w, wrong_key, keyname, tsig, now + 1000, b"", tsig_start)

    def test_gssapi_context(self):
        def verify_signature(data, mac):
            if data == b"throw":
                raise Exception
            return None

        # mock out the gssapi context to return some dummy values
        gssapi_context_mock = Mock()
        gssapi_context_mock.get_signature.return_value = b"xxxxxxxxxxx"
        gssapi_context_mock.verify_signature.side_effect = verify_signature

        # create the key and add it to the keyring
        keyname = "gsstsigtest"
        key = dns.tsig.Key(keyname, gssapi_context_mock, "gss-tsig")
        ctx = dns.tsig.get_context(key)
        self.assertEqual(ctx.name, "gss-tsig")
        gsskeyname = dns.name.from_text(keyname)
        keyring[gsskeyname] = key

        # make sure we can get the keyring (no exception == success)
        text = dns.tsigkeyring.to_text(keyring)
        self.assertNotEqual(text, "")

        # test exceptional case for _verify_mac_for_context
        with self.assertRaises(dns.tsig.BadSignature):
            ctx.update(b"throw")
            ctx.verify(b"bogus")
        gssapi_context_mock.verify_signature.assert_called()
        self.assertEqual(gssapi_context_mock.verify_signature.call_count, 1)

        # simulate case where TKEY message is used to establish the context;
        # first, the query from the client
        tkey_message = dns.message.make_query(keyname, "tkey", "any")

        # test existent/non-existent keys in the keyring
        adapted_keyring = dns.tsig.GSSTSigAdapter(keyring)

        fetched_key = adapted_keyring(tkey_message, gsskeyname)
        self.assertEqual(fetched_key, key)
        key = adapted_keyring(None, gsskeyname)
        self.assertEqual(fetched_key, key)
        key = adapted_keyring(tkey_message, "dummy")
        self.assertEqual(key, None)

        # create a response, TKEY and turn it into bytes, simulating the server
        # sending the response to the query
        tkey_response = dns.message.make_response(tkey_message)
        key = base64.b64decode("KEYKEYKEYKEYKEYKEYKEYKEYKEYKEYKEYKEY")
        tkey = dns.rdtypes.ANY.TKEY.TKEY(
            dns.rdataclass.ANY,
            dns.rdatatype.TKEY,
            dns.name.from_text("gss-tsig."),
            1594203795,
            1594206664,
            3,
            0,
            key,
        )

        # add the TKEY answer and sign it
        tkey_response.set_rcode(dns.rcode.NOERROR)
        tkey_response.answer = [
            dns.rrset.from_rdata(dns.name.from_text(keyname), 0, tkey)
        ]
        tkey_response.use_tsig(
            keyring=dns.tsig.GSSTSigAdapter(keyring),
            keyname=gsskeyname,
            algorithm=dns.tsig.GSS_TSIG,
        )

        # "send" it to the client
        tkey_wire = tkey_response.to_wire()

        # grab the response from the "server" and simulate the client side
        dns.message.from_wire(tkey_wire, dns.tsig.GSSTSigAdapter(keyring))

        # assertions to make sure the "gssapi" functions were called
        gssapi_context_mock.get_signature.assert_called()
        self.assertEqual(gssapi_context_mock.get_signature.call_count, 1)
        gssapi_context_mock.verify_signature.assert_called()
        self.assertEqual(gssapi_context_mock.verify_signature.call_count, 2)
        gssapi_context_mock.step.assert_called()
        self.assertEqual(gssapi_context_mock.step.call_count, 1)

        # create example message and go to/from wire to simulate sign/verify
        # of regular messages
        a_message = dns.message.make_query("example", "a")
        a_message.use_tsig(dns.tsig.GSSTSigAdapter(keyring), gsskeyname)
        a_wire = a_message.to_wire()
        # not raising is passing
        dns.message.from_wire(a_wire, dns.tsig.GSSTSigAdapter(keyring))

        # assertions to make sure the "gssapi" functions were called again
        gssapi_context_mock.get_signature.assert_called()
        self.assertEqual(gssapi_context_mock.get_signature.call_count, 2)
        gssapi_context_mock.verify_signature.assert_called()
        self.assertEqual(gssapi_context_mock.verify_signature.call_count, 3)

    def test_sign_and_validate(self):
        m = dns.message.make_query("example", "a")
        m.use_tsig(keyring, keyname)
        w = m.to_wire()
        # not raising is passing
        dns.message.from_wire(w, keyring)

    def test_signature_is_invalid(self):
        m = dns.message.make_query("example", "a")
        m.use_tsig(keyring, keyname)
        w = m.to_wire()
        b = bytearray(w)
        # corrupt hash
        b[-7] = (b[-7] + 1) & 0xFF
        w = bytes(b)
        with self.assertRaises(dns.tsig.BadSignature):
            dns.message.from_wire(w, keyring)

    def test_signature_is_invalid_and_ignored(self):
        m = dns.message.make_query("example", "a")
        m.use_tsig(keyring, keyname)
        w = m.to_wire()
        b = bytearray(w)
        # corrupt hash
        b[-7] = (b[-7] + 1) & 0xFF
        w = bytes(b)
        m2 = dns.message.from_wire(w, False)
        self.assertIsNotNone(m2.tsig)

    def test_validate_with_bad_keyring(self):
        m = dns.message.make_query("example", "a")
        m.use_tsig(keyring, keyname)
        w = m.to_wire()

        # keyring == None is an error
        with self.assertRaises(dns.message.UnknownTSIGKey):
            dns.message.from_wire(w, None)
        # callable keyring that returns None is an error
        with self.assertRaises(dns.message.UnknownTSIGKey):
            dns.message.from_wire(w, lambda m, n: None)

    def test_sign_and_validate_with_other_data(self):
        m = dns.message.make_query("example", "a")
        m.use_tsig(keyring, keyname, other_data=b"other")
        w = m.to_wire()
        # not raising is passing
        dns.message.from_wire(w, keyring)

    def test_sign_respond_and_validate(self):
        mq = dns.message.make_query("example", "a")
        mq.use_tsig(keyring, keyname)
        wq = mq.to_wire()
        mq_with_tsig = dns.message.from_wire(wq, keyring)
        mr = dns.message.make_response(mq)
        mr.use_tsig(keyring, keyname)
        wr = mr.to_wire()
        dns.message.from_wire(wr, keyring, request_mac=mq_with_tsig.mac)

    def make_message_pair(self, qname="example", rdtype="A", tsig_error=0):
        q = dns.message.make_query(qname, rdtype)
        q.use_tsig(keyring=keyring, keyname=keyname)
        q.to_wire()  # to set q.mac
        r = dns.message.make_response(q, tsig_error=tsig_error)
        return (q, r)

    def test_peer_errors(self):
        items = [
            (dns.rcode.BADSIG, dns.tsig.PeerBadSignature),
            (dns.rcode.BADKEY, dns.tsig.PeerBadKey),
            (dns.rcode.BADTIME, dns.tsig.PeerBadTime),
            (dns.rcode.BADTRUNC, dns.tsig.PeerBadTruncation),
            (99, dns.tsig.PeerError),
        ]
        for err, ex in items:
            q, r = self.make_message_pair(tsig_error=err)
            w = r.to_wire()

            def bad():
                dns.message.from_wire(w, keyring=keyring, request_mac=q.mac)

            self.assertRaises(ex, bad)

    def _test_truncated_algorithm(self, alg, length):
        key = dns.tsig.Key("foo", b"abcdefg", algorithm=alg)
        q = dns.message.make_query("example", "a")
        q.use_tsig(key)
        q2 = dns.message.from_wire(q.to_wire(), keyring=key)

        self.assertTrue(q2.had_tsig)
        self.assertEqual(q2.tsig[0].algorithm, q.tsig[0].algorithm)
        self.assertEqual(len(q2.tsig[0].mac), length // 8)

    def test_hmac_sha256_128(self):
        self._test_truncated_algorithm(dns.tsig.HMAC_SHA256_128, 128)

    def test_hmac_sha384_192(self):
        self._test_truncated_algorithm(dns.tsig.HMAC_SHA384_192, 192)

    def test_hmac_sha512_256(self):
        self._test_truncated_algorithm(dns.tsig.HMAC_SHA512_256, 256)

    def _test_text_format(self, alg):
        key = dns.tsig.Key("foo", b"abcdefg", algorithm=alg)
        q = dns.message.make_query("example", "a")
        q.use_tsig(key)
        _ = q.to_wire()

        text = q.tsig[0].to_text()
        tsig2 = dns.rdata.from_text("ANY", "TSIG", text)
        self.assertEqual(tsig2, q.tsig[0])

        q = dns.message.make_query("example", "a")
        q.use_tsig(key, other_data=b"abc")
        q.use_tsig(key)
        _ = q.to_wire()

        text = q.tsig[0].to_text()
        tsig2 = dns.rdata.from_text("ANY", "TSIG", text)
        self.assertEqual(tsig2, q.tsig[0])

    def test_text_hmac_sha256_128(self):
        self._test_text_format(dns.tsig.HMAC_SHA256_128)

    def test_text_hmac_sha384_192(self):
        self._test_text_format(dns.tsig.HMAC_SHA384_192)

    def test_text_hmac_sha512_256(self):
        self._test_text_format(dns.tsig.HMAC_SHA512_256)

    def test_text_empty_mac(self):
        for error in (dns.rcode.BADKEY, dns.rcode.BADSIG):
            with self.subTest(error=error):
                tsig = dns.rdtypes.ANY.TSIG.TSIG(
                    dns.rdataclass.ANY,
                    dns.rdatatype.TSIG,
                    dns.tsig.HMAC_SHA256,
                    0,
                    300,
                    b"",
                    1234,
                    error,
                    b"",
                )
                wire = tsig.to_wire()
                tsig = dns.rdata.from_wire("ANY", "TSIG", wire, 0, len(wire))
                parsed = dns.rdata.from_text("ANY", "TSIG", tsig.to_text())
                self.assertEqual(parsed, tsig)
                self.assertEqual(parsed.to_wire(), wire)

    def test_text_empty_tkey_key(self):
        for other in (b"", b"other"):
            with self.subTest(other=other):
                tkey = dns.rdtypes.ANY.TKEY.TKEY(
                    dns.rdataclass.ANY,
                    dns.rdatatype.TKEY,
                    dns.tsig.HMAC_SHA256,
                    0,
                    0,
                    dns.rdtypes.ANY.TKEY.TKEY.KEY_DELETION,
                    0,
                    b"",
                    other,
                )
                wire = tkey.to_wire()
                tkey = dns.rdata.from_wire("ANY", "TKEY", wire, 0, len(wire))
                parsed = dns.rdata.from_text("ANY", "TKEY", tkey.to_text())
                self.assertEqual(parsed, tkey)
                self.assertEqual(parsed.to_wire(), wire)

    def test_non_gss_key_repr(self):
        key = dns.tsig.Key(
            "foo", b"0123456789abcdef" * 2, algorithm=dns.tsig.HMAC_SHA256
        )
        self.assertEqual(
            repr(key),
            "<DNS key name='foo.', algorithm='hmac-sha256.', "
            + "secret="
            + "'MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY='>",
        )

    def test_gss_key_repr(self):
        key = dns.tsig.Key("foo", None, algorithm=dns.tsig.GSS_TSIG)
        self.assertEqual(repr(key), "<DNS key name='foo.', algorithm='gss-tsig.'>")

    def _test_multi(self, pad):
        tm0 = """id 1
;QUESTION
example. IN AXFR
"""
        tm1 = """id 1
flags QR
;QUESTION
example. IN AXFR
;ANSWER
example. 300 IN SOA . . 1 2 3 4 5
example. 300 IN NS ns1.tld.
example. 300 IN NS ns2.tld.
"""
        tm2 = """id 1
flags QR
;ANSWER
example. 300 IN MX 10 mail.tld.
example. 300 IN SOA . . 1 2 3 4 5
"""
        m0 = dns.message.from_text(tm0)
        m0.use_edns(0, pad=pad)
        m0.use_tsig(keyring)
        w0 = m0.to_wire()
        m1 = dns.message.from_text(tm1)
        m1.use_edns(0, pad=pad)
        m1.use_tsig(keyring)
        m1.request_mac = m0.mac
        w1 = m1.to_wire(multi=True)
        if pad != 0:
            self.assertEqual(len(w1) % pad, 0)
        m2 = dns.message.from_text(tm2)
        m2.use_edns(0, pad=pad)
        m2.use_tsig(keyring)
        w2 = m2.to_wire(multi=True, tsig_ctx=m1.tsig_ctx)
        if pad != 0:
            self.assertEqual(len(w2) % pad, 0)
        m3 = dns.message.from_wire(w1, keyring=keyring, request_mac=m0.mac, multi=True)
        m4 = dns.message.from_wire(
            w2, keyring=keyring, multi=True, tsig_ctx=m3.tsig_ctx
        )
        # Not raising means we passed validation
        self.assertEqual(m1, m3)
        self.assertEqual(m2, m4)

    def test_multi(self):
        self._test_multi(0)

    def test_multi_with_pad(self):
        self._test_multi(468)

    def _make_signed_query_and_unsigned_response(self):
        q = dns.message.make_query("example", "a")
        q.use_tsig(keyring, keyname)
        q.to_wire()  # to set q.mac
        r = dns.message.make_response(q)
        r.tsig = None
        return (q, r.to_wire())

    def test_unsigned_response_not_multi_is_form_error(self):
        q, w = self._make_signed_query_and_unsigned_response()
        ctx = dns.tsig.get_context(q.keyring)
        with self.assertRaises(dns.exception.FormError) as cm:
            dns.message.from_wire(
                w, keyring=keyring, request_mac=q.mac, tsig_ctx=ctx, multi=False
            )
        self.assertEqual(
            str(cm.exception), "response requires a TSIG but does not have one"
        )

    def test_unsigned_response_without_context_is_form_error(self):
        # The ordinary request/response case: only request_mac is supplied,
        # with no ongoing TSIG context.
        q, w = self._make_signed_query_and_unsigned_response()
        with self.assertRaises(dns.exception.FormError):
            dns.message.from_wire(w, keyring=keyring, request_mac=q.mac)

    def test_unsigned_response_not_multi_continue_on_error(self):
        q, w = self._make_signed_query_and_unsigned_response()
        ctx = dns.tsig.get_context(q.keyring)
        m = dns.message.from_wire(
            w,
            keyring=keyring,
            request_mac=q.mac,
            tsig_ctx=ctx,
            multi=False,
            continue_on_error=True,
        )
        self.assertFalse(m.had_tsig)
        self.assertEqual(len(m.errors), 1)
        self.assertIsInstance(m.errors[0].exception, dns.exception.FormError)

    def test_unsigned_response_multi_updates_context(self):
        # In multi mode, a message without a TSIG is not an error; it is
        # instead digested into the context and covered by the next TSIG.
        q, w = self._make_signed_query_and_unsigned_response()
        ctx = Mock()
        m = dns.message.from_wire(
            w, keyring=keyring, request_mac=q.mac, tsig_ctx=ctx, multi=True
        )
        self.assertFalse(m.had_tsig)
        self.assertIs(m.tsig_ctx, ctx)
        ctx.update.assert_called_once_with(w)

    def test_make_response_to_unverified(self):
        # Ensure that we can make a response to an unverified query that
        # contained a TSIG.
        q1 = dns.message.make_query("example", "a")
        q1.use_tsig(keyring, keyname)
        wire = q1.to_wire()

        q2 = dns.message.from_wire(wire, keyring=False)
        m = dns.message.make_response(q2)

        self.assertIsNone(m.tsig)

    def test_render_message_with_existing_tsig(self):
        q1 = dns.message.make_query("example", "a")
        q1.use_tsig(keyring, keyname)
        wire = q1.to_wire()

        # The TSIG record parsed from wire format is rendered if
        # it is not verified.
        q2 = dns.message.from_wire(wire, keyring=False)
        self.assertEqual(q2.to_wire(), wire)

        # The TSIG record parsed from wire format is rendered if
        # it is verified.
        q3 = dns.message.from_wire(wire, keyring=keyring)
        self.assertEqual(q3.to_wire(), wire)

        # A new TSIG record is generated if we call use_tsig().
        #
        # Note that this specifies a new key because if the same key were used,
        # the same TSIG record might be generated, and the wire format would
        # be identical.
        q4 = dns.message.from_wire(wire, keyring=keyring)
        q4.use_tsig(dns.tsig.Key(keyname, "abcd"))
        self.assertNotEqual(q4.to_wire(), wire)


# Fixed timestamps so the tests never depend on the wall clock.
SIGN_NOW = 1700000000
VERIFY_NOW = 1700000010


class TSIGChainVerifierTestCase(unittest.TestCase):
    """Tests for the explicit multi-message TSIG chain verifier.

    All messages are produced locally with a local key and a patched,
    deterministic clock; nothing here uses the network or real credentials.
    """

    def _make_transfer(self, signed=True, unsigned_middle=False, sign_time=SIGN_NOW):
        """Build the signed request and a three-message AXFR-style transfer.

        Returns a dict with the request wire/MAC and the three response
        wires, all signed (or not) at *sign_time*.  When *unsigned_middle*
        is set, the middle message carries no TSIG and is instead digested
        into the chain covered by the final TSIG.  When *signed* is false,
        the final message has no TSIG (and the chain therefore cannot be
        fully verified).
        """
        tm0 = """id 1
;QUESTION
example. IN AXFR
"""
        tm1 = """id 1
flags QR
;QUESTION
example. IN AXFR
;ANSWER
example. 300 IN SOA . . 1 2 3 4 5
example. 300 IN NS ns1.tld.
example. 300 IN NS ns2.tld.
"""
        tmm = """id 1
flags QR
;ANSWER
example. 300 IN A 192.0.2.1
example. 300 IN A 192.0.2.2
"""
        tm2 = """id 1
flags QR
;ANSWER
example. 300 IN MX 10 mail.tld.
example. 300 IN SOA . . 1 2 3 4 5
"""
        with patch("time.time", return_value=sign_time):
            m0 = dns.message.from_text(tm0)
            m0.use_tsig(keyring)
            m0.to_wire()  # to set m0.mac

            # First response: signed, starts the multi-message digest chain.
            m1 = dns.message.from_text(tm1)
            m1.use_tsig(keyring)
            m1.request_mac = m0.mac
            w1 = m1.to_wire(multi=True)
            chain_ctx = m1.tsig_ctx

            # Middle response.
            if unsigned_middle:
                # A middle message without a TSIG.  In multi mode the receiver
                # digests its whole wire into the ongoing context, so the
                # following signed message must do the same when signing.
                mm = dns.message.from_text(tmm)
                w_mid = mm.to_wire()
                chain_ctx = dns.tsig.get_context(m1.keyring)
                chain_ctx.update(struct.pack("!H", len(m1.mac)))
                chain_ctx.update(m1.mac)
                chain_ctx.update(w_mid)
            else:
                mm = dns.message.from_text(tmm)
                mm.use_tsig(keyring)
                w_mid = mm.to_wire(multi=True, tsig_ctx=chain_ctx)
                chain_ctx = mm.tsig_ctx

            # Final response.
            m2 = dns.message.from_text(tm2)
            if signed:
                m2.use_tsig(keyring)
                w2 = m2.to_wire(multi=True, tsig_ctx=chain_ctx)
            else:
                w2 = m2.to_wire()

        return {
            "request_mac": m0.mac,
            "first": w1,
            "middle": w_mid,
            "final": w2,
        }

    def test_successful_chain_verification(self):
        transfer = self._make_transfer()
        verifier = dns.tsig.TSIGChainVerifier(
            keyring, request_mac=transfer["request_mac"], now=VERIFY_NOW
        )
        self.assertIs(verifier.state, dns.tsig.TSIGChainState.NEW)
        self.assertFalse(verifier.verified)
        self.assertIsNone(verifier.result)

        first_message = verifier.verify_message(transfer["first"])
        self.assertIs(verifier.state, dns.tsig.TSIGChainState.IN_PROGRESS)
        # An intermediate signature passing must not make the transfer trusted.
        self.assertFalse(verifier.verified)
        expected_first_mac = first_message.tsig[0].mac

        verifier.verify_message(transfer["middle"])
        verifier.verify_message(transfer["final"])
        self.assertFalse(verifier.verified)

        result = verifier.finish()
        self.assertIs(verifier.state, dns.tsig.TSIGChainState.FINISHED)
        self.assertTrue(verifier.verified)
        self.assertIs(verifier.result, result)
        self.assertEqual(result.message_count, 3)
        self.assertEqual(result.signed_count, 3)
        self.assertEqual(result.first_mac, expected_first_mac)
        self.assertEqual(result.last_mac, verifier.last_mac)

    def test_unsigned_intermediate_message_is_covered_by_chain(self):
        transfer = self._make_transfer(unsigned_middle=True)
        verifier = dns.tsig.TSIGChainVerifier(
            keyring, request_mac=transfer["request_mac"], now=VERIFY_NOW
        )
        verifier.verify_message(transfer["first"])
        verifier.verify_message(transfer["middle"])
        verifier.verify_message(transfer["final"])
        result = verifier.finish()
        self.assertTrue(verifier.verified)
        self.assertEqual(result.message_count, 3)
        # First and final are signed; the middle is not.
        self.assertEqual(result.signed_count, 2)

    def test_missing_final_signature(self):
        transfer = self._make_transfer(signed=False)
        verifier = dns.tsig.TSIGChainVerifier(
            keyring, request_mac=transfer["request_mac"], now=VERIFY_NOW
        )
        verifier.verify_message(transfer["first"])
        verifier.verify_message(transfer["middle"])
        verifier.verify_message(transfer["final"])
        with self.assertRaises(dns.tsig.MissingFinalSignature):
            verifier.finish()
        self.assertIs(verifier.state, dns.tsig.TSIGChainState.FAILED)
        self.assertFalse(verifier.verified)

    def test_missing_first_message(self):
        transfer = self._make_transfer()
        verifier = dns.tsig.TSIGChainVerifier(
            keyring, request_mac=transfer["request_mac"], now=VERIFY_NOW
        )
        # A truncated / empty envelope cannot establish the signed first
        # message of the transfer, so the required chain data is missing.
        with self.assertRaises(dns.tsig.MissingIntermediateData):
            verifier.verify_message(b"\x00\x01")
        self.assertIs(verifier.state, dns.tsig.TSIGChainState.FAILED)
        self.assertFalse(verifier.verified)

    def test_malformed_first_message_poisons_context(self):
        transfer = self._make_transfer()
        # A structurally complete header followed by trailing junk is not a
        # valid transfer message; it must be reported as missing chain data
        # and poison the verifier.
        malformed = b"\x00\x01\x80\x00\x00\x00\x00\x00\x00\x00\x00\x00junk"
        verifier = dns.tsig.TSIGChainVerifier(
            keyring, request_mac=transfer["request_mac"], now=VERIFY_NOW
        )
        with self.assertRaises(dns.tsig.MissingIntermediateData):
            verifier.verify_message(malformed)
        self.assertIs(verifier.state, dns.tsig.TSIGChainState.FAILED)
        # A poisoned context cannot be revived even by a good message.
        with self.assertRaises(dns.tsig.ChainVerificationError):
            verifier.verify_message(transfer["first"])

    def test_finish_without_any_message(self):
        verifier = dns.tsig.TSIGChainVerifier(keyring, now=VERIFY_NOW)
        with self.assertRaises(dns.tsig.MissingIntermediateData):
            verifier.finish()
        self.assertIs(verifier.state, dns.tsig.TSIGChainState.FAILED)

    def test_bad_key_id(self):
        transfer = self._make_transfer()
        # A keyring that does not contain the key name used in the messages.
        other_name = dns.name.from_text("other-key")
        other_keyring = dns.tsigkeyring.from_text({str(other_name): "abcd"})
        verifier = dns.tsig.TSIGChainVerifier(
            other_keyring, request_mac=transfer["request_mac"], now=VERIFY_NOW
        )
        with self.assertRaises(dns.message.UnknownTSIGKey):
            verifier.verify_message(transfer["first"])
        self.assertIs(verifier.state, dns.tsig.TSIGChainState.FAILED)

    def test_bad_time_outside_fudge(self):
        # Sign far in the past relative to the verification instant.
        transfer = self._make_transfer(sign_time=SIGN_NOW - 10000)
        verifier = dns.tsig.TSIGChainVerifier(
            keyring, request_mac=transfer["request_mac"], now=VERIFY_NOW
        )
        with self.assertRaises(dns.tsig.BadTime):
            verifier.verify_message(transfer["first"])
        self.assertIs(verifier.state, dns.tsig.TSIGChainState.FAILED)

    def test_bad_signature_poisons_context(self):
        transfer = self._make_transfer()
        wrong_key = dns.tsig.Key(keyname, "abcd", dns.tsig.HMAC_SHA256)
        verifier = dns.tsig.TSIGChainVerifier(
            wrong_key, request_mac=transfer["request_mac"], now=VERIFY_NOW
        )
        with self.assertRaises(dns.tsig.BadSignature):
            verifier.verify_message(transfer["first"])
        self.assertIs(verifier.state, dns.tsig.TSIGChainState.FAILED)
        # The failed context cannot be reused for a new transfer.
        with self.assertRaises(dns.tsig.ChainVerificationError):
            verifier.verify_message(transfer["first"])
        with self.assertRaises(dns.tsig.ChainVerificationError):
            verifier.finish()

    def test_finished_context_cannot_be_reused(self):
        transfer = self._make_transfer()
        verifier = dns.tsig.TSIGChainVerifier(
            keyring, request_mac=transfer["request_mac"], now=VERIFY_NOW
        )
        verifier.verify_message(transfer["first"])
        verifier.verify_message(transfer["middle"])
        verifier.verify_message(transfer["final"])
        verifier.finish()
        self.assertTrue(verifier.verified)
        with self.assertRaises(dns.tsig.ChainVerificationError):
            verifier.verify_message(transfer["first"])
        with self.assertRaises(dns.tsig.ChainVerificationError):
            verifier.finish()

    def test_retry_uses_a_new_instance(self):
        transfer = self._make_transfer(sign_time=SIGN_NOW - 10000)
        verifier = dns.tsig.TSIGChainVerifier(
            keyring, request_mac=transfer["request_mac"], now=VERIFY_NOW
        )
        with self.assertRaises(dns.tsig.BadTime):
            verifier.verify_message(transfer["first"])
        self.assertIs(verifier.state, dns.tsig.TSIGChainState.FAILED)

        # A fresh instance with an appropriate (deterministic) time succeeds.
        retry = dns.tsig.TSIGChainVerifier(
            keyring, request_mac=transfer["request_mac"], now=SIGN_NOW - 10000
        )
        # Rebuild a correctly-timed transfer for the retry rather than trust
        # the stale one; here the same wires verify because we pass a "now"
        # within the fudge window of their signing time.
        retry.verify_message(transfer["first"])
        retry.verify_message(transfer["middle"])
        retry.verify_message(transfer["final"])
        result = retry.finish()
        self.assertTrue(retry.verified)
        self.assertEqual(result.message_count, 3)

    def test_now_can_be_overridden_per_message(self):
        transfer = self._make_transfer()
        verifier = dns.tsig.TSIGChainVerifier(
            keyring, request_mac=transfer["request_mac"]
        )
        verifier.verify_message(transfer["first"], now=VERIFY_NOW)
        verifier.verify_message(transfer["middle"], now=VERIFY_NOW)
        verifier.verify_message(transfer["final"], now=VERIFY_NOW)
        result = verifier.finish()
        self.assertTrue(verifier.verified)
        self.assertEqual(result.signed_count, 3)

    def test_from_wire_now_parameter_is_deterministic(self):
        # The now= parameter of dns.message.from_wire drives the TSIG time
        # check without reading the wall clock.
        transfer = self._make_transfer(sign_time=SIGN_NOW - 10000)
        with self.assertRaises(dns.tsig.BadTime):
            dns.message.from_wire(
                transfer["first"],
                keyring=keyring,
                request_mac=transfer["request_mac"],
                multi=True,
                now=VERIFY_NOW,
            )
        # The same wire verifies when "now" is within the fudge window.
        m = dns.message.from_wire(
            transfer["first"],
            keyring=keyring,
            request_mac=transfer["request_mac"],
            multi=True,
            now=SIGN_NOW - 10000,
        )
        self.assertTrue(m.had_tsig)
