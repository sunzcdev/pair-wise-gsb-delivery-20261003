import gzip
import hashlib
import pickle
import struct
import unittest
import zlib

from huey.exceptions import SerializerError
from huey.serializer import (
    COMPRESSION_GZIP,
    COMPRESSION_NONE,
    COMPRESSION_ZLIB,
    ENVELOPE_HEADER_SIZE,
    ENVELOPE_MAGIC,
    ENVELOPE_VERSION,
    FORMAT_PICKLE,
    Serializer,
    SignedSerializer,
    _ENVELOPE_HEADER,
)
from huey.tests.base import BaseTestCase


class TestSerializer(BaseTestCase):
    data = [
        None,
        0, 1,
        b'a' * 1024,
        ['k1', 'k2', 'k3'],
        {'k1': 'v1', 'k2': 'v2', 'k3': 'v3'}]

    def _test_serializer(self, s):
        for item in self.data:
            self.assertEqual(s.deserialize(s.serialize(item)), item)

    def test_serializer(self):
        self._test_serializer(Serializer())

    @unittest.skipIf(gzip is None, 'gzip module not installed')
    def test_serializer_gzip(self):
        self._test_serializer(Serializer(compression=True))

    @unittest.skipIf(zlib is None, 'zlib module not installed')
    def test_serializer_zlib(self):
        self._test_serializer(Serializer(compression=True, use_zlib=True))

    @unittest.skipIf(zlib is None, 'zlib module not installed')
    @unittest.skipIf(gzip is None, 'gzip module not installed')
    def test_mismatched_compression(self):
        for use_zlib in (False, True):
            s = Serializer()
            scomp = Serializer(compression=True, use_zlib=use_zlib)
            for item in self.data:
                self.assertEqual(scomp.deserialize(s.serialize(item)), item)


class TestEnvelopeRoundTrip(BaseTestCase):
    data = [None, 0, 1, 'hello', b'a' * 4096, [1, 2, 3],
            {'k': 'v', 'n': 42}, (1, 2, 3), {1, 2, 3}]

    def _roundtrip(self, **kwargs):
        s = Serializer(**kwargs)
        for item in self.data:
            blob = s.serialize(item)
            # Every newly-written message is an enveloped message.
            self.assertTrue(blob.startswith(ENVELOPE_MAGIC))
            self.assertEqual(s.deserialize(blob), item)

    def test_envelope_uncompressed(self):
        self._roundtrip()

    def test_envelope_gzip(self):
        self._roundtrip(compression=True)

    def test_envelope_zlib(self):
        self._roundtrip(compression=True, use_zlib=True)

    def _parse_header(self, blob):
        return _ENVELOPE_HEADER.unpack_from(blob, 0)

    def test_envelope_records_metadata(self):
        item = b'x' * 2048
        s = Serializer(compression=True)
        blob = s.serialize(item)
        magic, version, flags, orig_len, stored_len, checksum = (
            self._parse_header(blob))
        self.assertEqual(magic, ENVELOPE_MAGIC)
        self.assertEqual(version, ENVELOPE_VERSION)
        self.assertEqual(flags, FORMAT_PICKLE | COMPRESSION_GZIP)
        payload = pickle.dumps(item, s.pickle_protocol)
        self.assertEqual(orig_len, len(payload))
        self.assertEqual(stored_len, len(gzip.compress(payload, s.comp_level)))
        self.assertEqual(checksum, hashlib.sha256(payload).digest())
        self.assertEqual(len(blob), ENVELOPE_HEADER_SIZE + stored_len)

    def test_envelope_zlib_codec_recorded(self):
        s = Serializer(compression=True, use_zlib=True)
        blob = s.serialize(b'payload')
        magic, version, flags, orig_len, stored_len, checksum = (
            self._parse_header(blob))
        self.assertEqual(flags, FORMAT_PICKLE | COMPRESSION_ZLIB)

    def test_envelope_no_compression_codec(self):
        s = Serializer()
        blob = s.serialize(b'payload')
        magic, version, flags, orig_len, stored_len, checksum = (
            self._parse_header(blob))
        self.assertEqual(flags, FORMAT_PICKLE | COMPRESSION_NONE)
        self.assertEqual(orig_len, stored_len)


class TestEnvelopeDecodingPaths(BaseTestCase):
    """The decoder must follow the codec in the envelope, not reader config."""

    def test_reader_config_ignored_for_codec(self):
        # A gzip-written message must decode even when the reader is set up
        # for zlib (or no compression at all), and vice-versa.
        gz = Serializer(compression=True)
        zl = Serializer(compression=True, use_zlib=True)
        plain = Serializer()
        item = b'abc' * 500
        gz_blob = gz.serialize(item)
        zl_blob = zl.serialize(item)
        plain_blob = plain.serialize(item)
        for reader in (gz, zl, plain):
            self.assertEqual(reader.deserialize(gz_blob), item)
            self.assertEqual(reader.deserialize(zl_blob), item)
            self.assertEqual(reader.deserialize(plain_blob), item)

    def test_cross_compression_roundtrip(self):
        # Explicitly cover both directions of the gzip/zlib switch.
        gz = Serializer(compression=True, use_zlib=False)
        zl = Serializer(compression=True, use_zlib=True)
        for item in (None, 'text', b'b' * 1000, {'a': [1, 2]}):
            self.assertEqual(zl.deserialize(gz.serialize(item)), item)
            self.assertEqual(gz.deserialize(zl.serialize(item)), item)


class TestEnvelopeErrors(BaseTestCase):
    def setUp(self):
        super(TestEnvelopeErrors, self).setUp()
        self.s = Serializer()

    def test_empty_data_rejected(self):
        with self.assertRaises(SerializerError):
            self.s.deserialize(b'')
        with self.assertRaises(SerializerError):
            self.s.deserialize(None)

    def test_truncated_header_rejected(self):
        blob = self.s.serialize('hello')
        with self.assertRaises(SerializerError):
            self.s.deserialize(blob[:10])
        with self.assertRaises(SerializerError):
            self.s.deserialize(ENVELOPE_MAGIC)

    def test_truncated_body_rejected(self):
        blob = self.s.serialize(b'x' * 256)
        # Drop a byte off the end so the stored-length check fails.
        with self.assertRaises(SerializerError):
            self.s.deserialize(blob[:-1])

    def test_unknown_version_rejected(self):
        blob = bytearray(self.s.serialize('hello'))
        blob[4] = ENVELOPE_VERSION + 99  # version byte
        with self.assertRaises(SerializerError):
            self.s.deserialize(bytes(blob))

    def test_unknown_format_rejected(self):
        blob = bytearray(self.s.serialize('hello'))
        blob[5] = 0x90  # unknown high-nibble format, valid compression nibble
        with self.assertRaises(SerializerError):
            self.s.deserialize(bytes(blob))

    def test_unknown_compression_codec_rejected(self):
        blob = bytearray(self.s.serialize('hello'))
        blob[5] = FORMAT_PICKLE | 0x0f  # unknown low-nibble codec
        with self.assertRaises(SerializerError):
            self.s.deserialize(bytes(blob))

    def test_stored_length_mismatch_rejected(self):
        blob = bytearray(self.s.serialize(b'y' * 128))
        # Corrupt the stored-length field (bytes 14..21).
        struct.pack_into('>Q', blob, 14, 9999)
        with self.assertRaises(SerializerError):
            self.s.deserialize(bytes(blob))

    def test_original_length_mismatch_rejected(self):
        s = Serializer(compression=True)
        blob = bytearray(s.serialize(b'z' * 256))
        # Corrupt the original-length field (bytes 6..13).
        struct.pack_into('>Q', blob, 6, 9999)
        with self.assertRaises(SerializerError):
            s.deserialize(bytes(blob))

    def test_checksum_failure_rejected(self):
        blob = bytearray(self.s.serialize('hello'))
        blob[-1] ^= 0xff  # flip a payload bit
        with self.assertRaises(SerializerError):
            self.s.deserialize(bytes(blob))

    def test_checksum_failure_rejected_compressed(self):
        s = Serializer(compression=True, use_zlib=True)
        blob = bytearray(s.serialize(b'q' * 512))
        # Corrupt a checksum byte directly (bytes 22..53).
        blob[22] ^= 0xff
        with self.assertRaises(SerializerError):
            s.deserialize(bytes(blob))

    def test_header_field_tamper_rejected(self):
        # Flip the compression codec flag; the checksum/length guards must
        # reject this before any object is unpickled.
        s = Serializer(compression=True)
        blob = bytearray(s.serialize(b'm' * 256))
        blob[5] = FORMAT_PICKLE | COMPRESSION_ZLIB  # gzip -> zlib
        with self.assertRaises(SerializerError):
            s.deserialize(bytes(blob))

    def test_errors_raised_before_unpickle(self):
        # A corrupted payload that is also not valid pickle must surface as a
        # SerializerError (checksum), never a pickle error.
        blob = bytearray(self.s.serialize('hello'))
        blob[-1] ^= 0xff
        try:
            self.s.deserialize(bytes(blob))
        except SerializerError:
            pass
        except Exception as exc:  # pragma: no cover - defensive
            self.fail('expected SerializerError, got %r' % exc)
        else:  # pragma: no cover - defensive
            self.fail('expected SerializerError to be raised')


class TestLegacyCompatibility(BaseTestCase):
    def _legacy_blob(self, item, compression=False, use_zlib=False):
        data = pickle.dumps(item, pickle.HIGHEST_PROTOCOL)
        if compression:
            data = (zlib.compress if use_zlib else gzip.compress)(data, 6)
        return data

    def test_legacy_uncompressed_readable(self):
        s = Serializer()
        blob = self._legacy_blob({'legacy': True})
        self.assertFalse(blob.startswith(ENVELOPE_MAGIC))
        self.assertEqual(s.deserialize(blob), {'legacy': True})

    def test_legacy_gzip_readable(self):
        s = Serializer(compression=True)
        blob = self._legacy_blob('gzip-legacy', compression=True)
        self.assertEqual(s.deserialize(blob), 'gzip-legacy')

    def test_legacy_zlib_readable(self):
        s = Serializer(compression=True, use_zlib=True)
        blob = self._legacy_blob('zlib-legacy', compression=True,
                                 use_zlib=True)
        self.assertEqual(s.deserialize(blob), 'zlib-legacy')

    def test_legacy_disabled_rejects_unenveloped(self):
        s = Serializer(allow_legacy=False)
        blob = self._legacy_blob('nope')
        with self.assertRaises(SerializerError):
            s.deserialize(blob)

    def test_legacy_disabled_still_reads_envelope(self):
        strict = Serializer(allow_legacy=False)
        blob = strict.serialize('strict')
        self.assertEqual(strict.deserialize(blob), 'strict')

    def test_envelope_not_mistaken_for_legacy(self):
        # A legacy message whose *first byte* collides with the magic's first
        # byte must not be treated as an envelope: full magic matching only.
        s = Serializer(allow_legacy=False)
        collision = ENVELOPE_MAGIC[:1] + b'X-rest-of-legacy-pickle-ish-bytes'
        self.assertTrue(collision[0] == ENVELOPE_MAGIC[0])
        self.assertFalse(s._is_envelope(collision))
        with self.assertRaises(SerializerError):
            s.deserialize(collision)

    def test_magic_requires_full_prefix(self):
        s = Serializer()
        for n in range(0, 4):
            partial = ENVELOPE_MAGIC[:n] + b'Z' * (10 - n)
            self.assertFalse(s._is_envelope(partial))
        full = ENVELOPE_MAGIC + b'Z' * 10
        self.assertTrue(s._is_envelope(full))

    def test_default_config_preserves_legacy_reading(self):
        # Default constructor keeps backwards-compatible reading enabled.
        self.assertTrue(Serializer().allow_legacy)
        self.assertTrue(SignedSerializer(secret='k').allow_legacy)


class TestSignedSerializerEnvelope(BaseTestCase):
    def setUp(self):
        super(TestSignedSerializerEnvelope, self).setUp()
        self.s = SignedSerializer(secret='test-secret')

    def test_signed_envelope_roundtrip(self):
        for item in (None, 'a', b'b' * 100, [1, 2], {'k': 'v'}):
            blob = self.s.serialize(item)
            self.assertTrue(blob.startswith(ENVELOPE_MAGIC))
            self.assertEqual(self.s.deserialize(blob), item)

    def test_signed_envelope_with_compression(self):
        for use_zlib in (False, True):
            s = SignedSerializer(secret='k', compression=True,
                                 use_zlib=use_zlib)
            for item in ('text', b'x' * 500, {'a': 1}):
                self.assertEqual(s.deserialize(s.serialize(item)), item)

    def test_signature_covers_full_envelope(self):
        blob = bytearray(self.s.serialize('protected'))
        # Tamper with the flags byte (inside the envelope, within signature).
        blob[5] ^= 0xff
        with self.assertRaises(ValueError):
            self.s.deserialize(bytes(blob))

    def test_signature_covers_checksum_and_lengths(self):
        blob = bytearray(self.s.serialize(b'data' * 64))
        for index in (4, 6, 14, 22):  # version, orig-len, stored-len, checksum
            tampered = bytearray(blob)
            tampered[index] ^= 0xff
            with self.assertRaises((ValueError, SerializerError)):
                self.s.deserialize(bytes(tampered))

    def test_tampered_payload_rejected(self):
        blob = bytearray(self.s.serialize('hello'))
        # Signature sits at the very end; tamper with a payload byte instead.
        blob[ENVELOPE_HEADER_SIZE] ^= 0xff
        with self.assertRaises((ValueError, SerializerError)):
            self.s.deserialize(bytes(blob))

    def test_legacy_signed_message_readable(self):
        # Recreate an old-style signed (but un-enveloped) message.
        item = {'old': 'style'}
        raw = pickle.dumps(item, pickle.HIGHEST_PROTOCOL)
        legacy = self.s._sign(raw)
        self.assertFalse(legacy.startswith(ENVELOPE_MAGIC))
        self.assertEqual(self.s.deserialize(legacy), item)

    def test_legacy_signed_disabled(self):
        s = SignedSerializer(secret='k', allow_legacy=False)
        legacy = s._sign(pickle.dumps('old'))
        with self.assertRaises(SerializerError):
            s.deserialize(legacy)

    def test_secret_required(self):
        from huey.exceptions import ConfigurationError
        with self.assertRaises(ConfigurationError):
            SignedSerializer(secret='')


class TestSerializerErrorContract(BaseTestCase):
    def test_is_value_error(self):
        # Backwards-compatibility: callers catching ValueError still work.
        self.assertTrue(issubclass(SerializerError, ValueError))

    def test_all_guard_errors_are_serializer_error(self):
        s = Serializer()
        blob = bytearray(s.serialize('hello'))
        cases = [
            b'',
            blob[:10],
            bytes(blob[:-1]),
        ]
        for bad in cases:
            with self.assertRaises(SerializerError):
                s.deserialize(bad)
